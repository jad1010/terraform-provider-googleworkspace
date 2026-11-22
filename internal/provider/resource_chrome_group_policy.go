// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
)

func resourceChromeGroupPolicy() *schema.Resource {
	return &schema.Resource{
		Description: "Chrome Policy resource for a group in the Terraform Googleworkspace provider. Only policy schemas " +
			"that support groups can be set on a group: the API supports group targets only for the policies of apps " +
			"(`chrome.users.apps.*`), printers (`chrome.printers.*`), and print servers (`chrome.printerservers.*`). These " +
			"policies need `additional_target_keys`, for example `app_id` or `printer_id`. When more than one group sets the same policy, " +
			"`googleworkspace_chrome_policy_group_priority_ordering` sets which group has priority. Chrome Policy Schema " +
			"resides under the `https://www.googleapis.com/auth/chrome.management.policy` client scope.",

		CreateContext: resourceChromeGroupPolicyCreate,
		UpdateContext: resourceChromeGroupPolicyUpdate,
		ReadContext:   resourceChromeGroupPolicyRead,
		DeleteContext: resourceChromeGroupPolicyDelete,

		CustomizeDiff: resourceChromeGroupPolicyCustomizeDiff,

		Importer: &schema.ResourceImporter{
			StateContext: resourceChromeGroupPolicyImport,
		},

		Timeouts: &schema.ResourceTimeout{
			Create: schema.DefaultTimeout(5 * time.Minute),
			Update: schema.DefaultTimeout(5 * time.Minute),
		},

		Schema: map[string]*schema.Schema{
			"group_id": {
				Description:  "The ID of the target group on which this policy is applied.",
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validation.StringDoesNotContainAny("/"),
			},
			"additional_target_keys": chromePolicyAdditionalTargetKeysSchema(),
			"policies":               chromeGroupPolicyAttributeSchema("policies", "Policies to set for the group"),
			"applied_values":         chromeGroupPolicyAttributeSchema("applied_values", ""),
		},
	}
}

// chromeGroupPolicyAttributeSchema returns a copy of the schema of an attribute of googleworkspace_chrome_policy, so
// that both resources validate and store policies in the same way. A description that is not empty replaces the
// description of the copy.
func chromeGroupPolicyAttributeSchema(name, description string) *schema.Schema {
	attribute := *resourceChromePolicy().Schema[name]
	if description != "" {
		attribute.Description = description
	}
	return &attribute
}

// chromePolicyGroupTarget returns the group target of a googleworkspace_chrome_group_policy resource.
func chromePolicyGroupTarget(d chromePolicyResourceGetter) (chromePolicyTarget, error) {
	id := d.Id()
	if id == "" {
		id = d.Get("group_id").(string)
	}
	keys, err := chromePolicyAdditionalTargetKeys(d.Get("additional_target_keys"))
	return chromePolicyTarget{id: id, group: true, additionalKeys: keys}, err
}

func resourceChromeGroupPolicyCustomizeDiff(ctx context.Context, d *schema.ResourceDiff, meta interface{}) error {
	return chromePolicyCustomizeDiff(ctx, d, meta, true)
}

func resourceChromeGroupPolicyCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	target, err := chromePolicyGroupTarget(d)
	if err != nil {
		return diag.FromErr(err)
	}

	return applyChromePolicies(ctx, d, meta.(*apiClient), target, nil, d.Timeout(schema.TimeoutCreate))
}

func resourceChromeGroupPolicyUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	target, err := chromePolicyGroupTarget(d)
	if err != nil {
		return diag.FromErr(err)
	}

	return updateChromePolicies(ctx, d, meta.(*apiClient), target)
}

// resourceChromeGroupPolicyRead resolves each policy for the group. The API resolves the policies of a group target
// in the same way as for an org unit, and the source of a policy that is set on the group is the group.
func resourceChromeGroupPolicyRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	target, err := chromePolicyGroupTarget(d)
	if err != nil {
		return diag.FromErr(err)
	}

	return readChromePolicyResource(ctx, d, meta.(*apiClient), target)
}

func resourceChromeGroupPolicyDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	target, err := chromePolicyGroupTarget(d)
	if err != nil {
		return diag.FromErr(err)
	}

	return deleteChromePolicies(ctx, d, meta.(*apiClient), target)
}

// resourceChromeGroupPolicyImport imports the policies of a group. The ID is "<group_id>/<schema names>" or
// "<group_id>/<additional target keys>/<schema names>". The schema names are separated by commas, and the additional
// target keys are "<key>=<value>" pairs separated by "+". The value of a key can contain "/". The read that follows
// gets the values, and removes the resource if none of the policies is set on the group.
func resourceChromeGroupPolicyImport(ctx context.Context, d *schema.ResourceData, meta interface{}) ([]*schema.ResourceData, error) {
	importId := d.Id()
	first, last := strings.Index(importId, "/"), strings.LastIndex(importId, "/")
	if first <= 0 || last == len(importId)-1 {
		return nil, fmt.Errorf("invalid import ID %q: expected <group_id>/<schema names> or <group_id>/<key>=<value>[+<key>=<value>...]/<schema names>", importId)
	}

	groupId := importId[:first]

	var keys []interface{}
	if first != last {
		for _, pair := range strings.Split(importId[first+1:last], "+") {
			kv := strings.SplitN(pair, "=", 2)
			if len(kv) != 2 || kv[0] == "" || kv[1] == "" {
				return nil, fmt.Errorf("invalid additional target key %q in import ID %q: expected <key>=<value>", pair, importId)
			}
			keys = append(keys, map[string]interface{}{"target_key": kv[0], "target_value": kv[1]})
		}
	}
	if _, err := chromePolicyAdditionalTargetKeys(keys); err != nil {
		return nil, err
	}

	var policies []interface{}
	for _, schemaName := range strings.Split(importId[last+1:], ",") {
		schemaName = strings.TrimSpace(schemaName)
		if schemaName == "" {
			return nil, fmt.Errorf("invalid import ID %q: empty schema name", importId)
		}
		policies = append(policies, map[string]interface{}{
			"schema_name":   schemaName,
			"schema_values": map[string]interface{}{},
		})
	}

	d.SetId(groupId)
	if err := d.Set("group_id", groupId); err != nil {
		return nil, err
	}
	if err := d.Set("additional_target_keys", keys); err != nil {
		return nil, err
	}
	if err := d.Set("policies", policies); err != nil {
		return nil, err
	}

	return []*schema.ResourceData{d}, nil
}
