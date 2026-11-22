// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/errwrap"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"google.golang.org/api/chromepolicy/v1"
	"google.golang.org/api/googleapi"
)

// chromePolicyOrderingNamespacePrefix starts the first part of an import ID that is a policy namespace, not a schema.
const chromePolicyOrderingNamespacePrefix = "namespace="

func resourceChromePolicyGroupPriorityOrdering() *schema.Resource {
	return &schema.Resource{
		Description: "Chrome Policy Group Priority Ordering resource in the Terraform Googleworkspace provider. This resource " +
			"manages the priority order of the groups that set a Chrome policy for an app: when groups set different values, " +
			"the value of the group that is first in `group_ids` applies. The API supports an ordering only for the policies " +
			"of an app: `policy_target_key` must have the app in `additional_target_key_names`. The API has no delete " +
			"operation for an ordering, so destroying this resource only removes it from the Terraform state, and the " +
			"ordering stays in Google Workspace. Chrome Policy Schema resides under the " +
			"`https://www.googleapis.com/auth/chrome.management.policy` client scope.\n\n" +
			"Each write must include every group that currently sets the policy for the app. A list without one of these " +
			"groups is an error of the API.\n\n" +
			"* To add a group, add its `googleworkspace_chrome_group_policy` and add the group to `group_ids` in the same " +
			"apply. Set `depends_on` to the group policies, so that the group policy is created before the ordering is " +
			"written.\n" +
			"* To remove a group, use two applies. First remove the `googleworkspace_chrome_group_policy` of the group from " +
			"the configuration and from `depends_on`, keep the group in `group_ids`, and apply. Because `group_ids` does " +
			"not change, this apply only destroys the group policy. Then remove the group from `group_ids` and apply " +
			"again. If the group is the last group in `group_ids`, remove the ordering resource from the configuration " +
			"instead: when no group sets the policy, the next refresh removes the ordering from state, and `group_ids` " +
			"needs at least one group. In one apply, Terraform writes the ordering before it destroys the group policy, " +
			"so the write leaves out a group that still sets the policy, and the API rejects it. This order follows the " +
			"API rule above, and is not tested against the live API.",

		CreateContext: resourceChromePolicyGroupPriorityOrderingWrite,
		UpdateContext: resourceChromePolicyGroupPriorityOrderingWrite,
		ReadContext:   resourceChromePolicyGroupPriorityOrderingRead,
		DeleteContext: resourceChromePolicyGroupPriorityOrderingDelete,

		Importer: &schema.ResourceImporter{
			StateContext: resourceChromePolicyGroupPriorityOrderingImport,
		},

		Schema: map[string]*schema.Schema{
			"policy_schema": {
				Description:  "The full qualified name of the policy schema, for example `chrome.users.apps.InstallType`. Set either `policy_schema` or `policy_namespace`.",
				Type:         schema.TypeString,
				Optional:     true,
				ForceNew:     true,
				ExactlyOneOf: []string{"policy_schema", "policy_namespace"},
			},
			"policy_namespace": {
				Description:  "The namespace of the policy type, for example `chrome.users.apps` for the policies of an app. Set either `policy_schema` or `policy_namespace`.",
				Type:         schema.TypeString,
				Optional:     true,
				ForceNew:     true,
				ExactlyOneOf: []string{"policy_schema", "policy_namespace"},
			},
			"policy_target_key": {
				Description: "The target of the ordering. The API requires the app in `additional_target_key_names`, for " +
					"example `app_id = \"chrome:<extension ID>\"`.",
				Type:     schema.TypeList,
				Required: true,
				ForceNew: true,
				MinItems: 1,
				MaxItems: 1,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"target_resource": {
							Description: "The target resource. The examples of the API leave it empty and set only the app in " +
								"`additional_target_key_names`.",
							Type:     schema.TypeString,
							Optional: true,
							ForceNew: true,
						},
						"additional_target_key_names": {
							Description: "A map of additional target key names to their values, for example " +
								"`{ app_id = \"chrome:<extension ID>\" }`. The API requires the app.",
							Type:     schema.TypeMap,
							Required: true,
							ForceNew: true,
							Elem:     &schema.Schema{Type: schema.TypeString},
							ValidateFunc: func(v interface{}, k string) ([]string, []error) {
								if names, _ := v.(map[string]interface{}); len(names) == 0 {
									return nil, []error{fmt.Errorf("%s must have at least one key, for example app_id: the API requires the app", k)}
								}
								return nil, nil
							},
						},
					},
				},
			},
			"group_ids": {
				Description: "The IDs of the groups, in priority order: the policy value of a group overrides the values " +
					"of the groups after it. The API requires every group that currently sets the policy for the app: a list " +
					"without one of them is an error of the API, not drift. See the description of the resource to add or " +
					"remove a group.",
				Type:     schema.TypeList,
				Required: true,
				MinItems: 1,
				Elem: &schema.Schema{
					Type:         schema.TypeString,
					ValidateFunc: validation.StringIsNotEmpty,
				},
			},
		},
	}
}

// chromePolicyOrderingTargetKey returns the policy target key of an ordering, or nil if it has none.
func chromePolicyOrderingTargetKey(d *schema.ResourceData) *chromepolicy.GoogleChromePolicyVersionsV1PolicyTargetKey {
	list, _ := d.Get("policy_target_key").([]interface{})
	if len(list) == 0 || list[0] == nil {
		return nil
	}
	raw := list[0].(map[string]interface{})
	key := &chromepolicy.GoogleChromePolicyVersionsV1PolicyTargetKey{}
	key.TargetResource, _ = raw["target_resource"].(string)
	if names, ok := raw["additional_target_key_names"].(map[string]interface{}); ok && len(names) > 0 {
		key.AdditionalTargetKeys = map[string]string{}
		for k, v := range names {
			key.AdditionalTargetKeys[k], _ = v.(string)
		}
	}
	return key
}

// chromePolicyOrderingId returns the ID of an ordering: "<policy schema>:<target resource>", with
// ":<key>=<value>[+<key>=<value>...]" when the target has additional target keys. The first part is
// "namespace=<policy namespace>" when the ordering has no policy schema.
func chromePolicyOrderingId(policySchema, policyNamespace string, key *chromepolicy.GoogleChromePolicyVersionsV1PolicyTargetKey) string {
	first := policySchema
	if first == "" {
		first = chromePolicyOrderingNamespacePrefix + policyNamespace
	}
	if key == nil {
		return first + ":"
	}
	id := first + ":" + key.TargetResource
	if len(key.AdditionalTargetKeys) > 0 {
		var pairs []string
		for k, v := range key.AdditionalTargetKeys {
			pairs = append(pairs, k+"="+v)
		}
		sort.Strings(pairs)
		id += ":" + strings.Join(pairs, "+")
	}
	return id
}

func resourceChromePolicyGroupPriorityOrderingWrite(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*apiClient)

	chromePolicyService, diags := client.NewChromePolicyService()
	if diags.HasError() {
		return diags
	}

	chromePoliciesService, diags := GetChromePoliciesService(chromePolicyService)
	if diags.HasError() {
		return diags
	}

	policySchema := d.Get("policy_schema").(string)
	policyNamespace := d.Get("policy_namespace").(string)
	targetKey := chromePolicyOrderingTargetKey(d)

	var groupIds []string
	for _, id := range d.Get("group_ids").([]interface{}) {
		groupIds = append(groupIds, id.(string))
	}

	log.Printf("[DEBUG] Updating Chrome Policy group priority ordering for %s", chromePolicyOrderingId(policySchema, policyNamespace, targetKey))

	err := retryTimeDuration(ctx, time.Minute, func() error {
		_, retryErr := chromePoliciesService.Groups.UpdateGroupPriorityOrdering(fmt.Sprintf("customers/%s", client.Customer), &chromepolicy.GoogleChromePolicyVersionsV1UpdateGroupPriorityOrderingRequest{
			PolicySchema:    policySchema,
			PolicyNamespace: policyNamespace,
			PolicyTargetKey: targetKey,
			GroupIds:        groupIds,
		}).Context(ctx).Do()
		return retryErr
	})
	if isApiErrorWithCode(err, 400) {
		return diag.Diagnostics{{
			Severity: diag.Error,
			Summary:  err.Error(),
			Detail: "A common cause of this error is a group_ids list that does not have exactly the groups that currently " +
				"set the policy for the app. To add or remove a group, see the documentation of " +
				"googleworkspace_chrome_policy_group_priority_ordering.",
		}}
	}
	if err != nil {
		return diag.FromErr(err)
	}

	// The state keeps the configured order. The next refresh reads the order from the API.
	d.SetId(chromePolicyOrderingId(policySchema, policyNamespace, targetKey))
	return nil
}

// chromePolicyOrderingNotConfigured matches the message of the API error for an ordering that no group sets. The fake
// API of the tests uses this message, but the exact message of the live API is not verified.
var chromePolicyOrderingNotConfigured = regexp.MustCompile(`(?i)\bnot configured on any groups?\b`)

// resourceChromePolicyGroupPriorityOrderingRead sets group_ids from the API. When no group sets the policy, the API
// returns an error with code 400 (see chromePolicyOrderingNotConfigured), and the resource is removed from state with
// a warning. Other errors are returned.
func resourceChromePolicyGroupPriorityOrderingRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*apiClient)

	chromePolicyService, diags := client.NewChromePolicyService()
	if diags.HasError() {
		return diags
	}

	chromePoliciesService, diags := GetChromePoliciesService(chromePolicyService)
	if diags.HasError() {
		return diags
	}

	var resp *chromepolicy.GoogleChromePolicyVersionsV1ListGroupPriorityOrderingResponse
	err := retryTimeDuration(ctx, time.Minute, func() error {
		var retryErr error
		resp, retryErr = chromePoliciesService.Groups.ListGroupPriorityOrdering(fmt.Sprintf("customers/%s", client.Customer), &chromepolicy.GoogleChromePolicyVersionsV1ListGroupPriorityOrderingRequest{
			PolicySchema:    d.Get("policy_schema").(string),
			PolicyNamespace: d.Get("policy_namespace").(string),
			PolicyTargetKey: chromePolicyOrderingTargetKey(d),
		}).Context(ctx).Do()
		return retryErr
	})
	notConfigured := err == nil && len(resp.GroupIds) == 0
	if gerr, ok := errwrap.GetType(err, &googleapi.Error{}).(*googleapi.Error); ok && gerr != nil && gerr.Code == 400 &&
		chromePolicyOrderingNotConfigured.MatchString(gerr.Message) {
		notConfigured = true
	} else if err != nil {
		return handleNotFoundError(err, d, fmt.Sprintf("Chrome Policy group priority ordering %s", d.Id()))
	}

	if notConfigured {
		log.Printf("[WARN] Removing Chrome Policy group priority ordering %s from state: no group sets the policy", d.Id())
		id := d.Id()
		d.SetId("")
		return diag.Diagnostics{{
			Severity: diag.Warning,
			Summary:  "Chrome Policy group priority ordering is removed from state",
			Detail: fmt.Sprintf("No group sets the policy of the ordering %s. Terraform removed the ordering from state; "+
				"the next plan creates it again if the configuration still has it.", id),
		}}
	}

	if err := d.Set("group_ids", resp.GroupIds); err != nil {
		return diag.FromErr(err)
	}
	return nil
}

// resourceChromePolicyGroupPriorityOrderingDelete removes the ordering from state only. The API has no delete
// operation, and an update requires at least one group. The ordering exists while groups set the policy.
func resourceChromePolicyGroupPriorityOrderingDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	log.Printf("[DEBUG] Removing Chrome Policy group priority ordering %s from state only", d.Id())
	d.SetId("")
	return diag.Diagnostics{{
		Severity: diag.Warning,
		Summary:  "Chrome Policy group priority ordering is not changed",
		Detail: "The Chrome Policy API cannot delete a group priority ordering. Terraform removed the resource from " +
			"state, and the ordering stays in Google Workspace while groups set the policy.",
	}}
}

// resourceChromePolicyGroupPriorityOrderingImport imports an ordering by its ID (see chromePolicyOrderingId), for
// example "namespace=chrome.users.apps::app_id=chrome:abc" or "chrome.users.apps.InstallType::app_id=chrome:abc". The
// ID must have additional target keys, because the API requires the app.
func resourceChromePolicyGroupPriorityOrderingImport(ctx context.Context, d *schema.ResourceData, meta interface{}) ([]*schema.ResourceData, error) {
	importId := d.Id()
	parts := strings.SplitN(importId, ":", 3)
	if len(parts) < 3 || parts[0] == "" || parts[0] == chromePolicyOrderingNamespacePrefix || parts[2] == "" {
		return nil, fmt.Errorf("invalid import ID %q: expected <policy schema>:<target resource>:<key>=<value>[+<key>=<value>...], "+
			"with namespace=<policy namespace> in place of the policy schema for an ordering by namespace. The API requires "+
			"the app in the additional target keys, for example app_id=chrome:<extension ID>", importId)
	}

	if strings.HasPrefix(parts[0], chromePolicyOrderingNamespacePrefix) {
		if err := d.Set("policy_namespace", strings.TrimPrefix(parts[0], chromePolicyOrderingNamespacePrefix)); err != nil {
			return nil, err
		}
	} else if err := d.Set("policy_schema", parts[0]); err != nil {
		return nil, err
	}

	names := map[string]interface{}{}
	for _, pair := range strings.Split(parts[2], "+") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 || kv[0] == "" || kv[1] == "" {
			return nil, fmt.Errorf("invalid additional target key %q in import ID %q: expected <key>=<value>", pair, importId)
		}
		names[kv[0]] = kv[1]
	}

	if err := d.Set("policy_target_key", []interface{}{map[string]interface{}{
		"target_resource":             parts[1],
		"additional_target_key_names": names,
	}}); err != nil {
		return nil, err
	}

	d.SetId(chromePolicyOrderingId(d.Get("policy_schema").(string), d.Get("policy_namespace").(string), chromePolicyOrderingTargetKey(d)))
	return []*schema.ResourceData{d}, nil
}
