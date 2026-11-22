// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"reflect"
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

// chromePolicyAdditionalTargetKeysSchema returns the schema of additional_target_keys. The block format follows the
// attribute names of the macadmins/googleworkspace fork. Compatibility with that fork is not tested.
func chromePolicyAdditionalTargetKeysSchema() *schema.Schema {
	return &schema.Schema{
		Description: "Additional target keys that identify the target of all policies in this resource, for example " +
			"`app_id` for the policies of an app (`chrome.users.apps.*`). Each `target_key` can occur only once. To set " +
			"the policies of more than one app, use one resource for each app. Changing this forces a new resource.",
		// A set, so that the order of the blocks does not matter.
		Type:     schema.TypeSet,
		Optional: true,
		ForceNew: true,
		Elem: &schema.Resource{
			Schema: map[string]*schema.Schema{
				"target_key": {
					Description:  "The name of the target key, for example `app_id`.",
					Type:         schema.TypeString,
					Required:     true,
					ForceNew:     true,
					ValidateFunc: validation.StringIsNotEmpty,
				},
				"target_value": {
					Description:  "The value of the target key, for example `chrome:ghbmnnjooekpmoecnnnilnnbdlolhkhi`.",
					Type:         schema.TypeString,
					Required:     true,
					ForceNew:     true,
					ValidateFunc: validation.StringIsNotEmpty,
				},
			},
		},
	}
}

// chromePolicyTarget is the target of the policies of a resource: an org unit or a group, and the additional target
// keys that identify the target further (for example, the app of an app policy).
type chromePolicyTarget struct {
	id             string
	group          bool
	additionalKeys map[string]string
}

// chromePolicyOrgUnitTarget returns the org unit target of a googleworkspace_chrome_policy resource.
func chromePolicyOrgUnitTarget(d chromePolicyResourceGetter) (chromePolicyTarget, error) {
	id := d.Id()
	if id == "" {
		id = strings.TrimPrefix(d.Get("org_unit_id").(string), "id:")
	}
	keys, err := chromePolicyAdditionalTargetKeys(d.Get("additional_target_keys"))
	return chromePolicyTarget{id: id, additionalKeys: keys}, err
}

// chromePolicyResourceGetter is the part of schema.ResourceData that reads the target of a resource.
type chromePolicyResourceGetter interface {
	Id() string
	Get(string) interface{}
}

// chromePolicyAdditionalTargetKeys returns a value of additional_target_keys (a set, or a list of blocks) as a map. It
// returns an error if a target key occurs more than once. A value that is not known yet is left out.
func chromePolicyAdditionalTargetKeys(raw interface{}) (map[string]string, error) {
	list, _ := raw.([]interface{})
	if set, ok := raw.(*schema.Set); ok {
		list = set.List()
	}
	keys := map[string]string{}
	for _, k := range list {
		entry, ok := k.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := entry["target_key"].(string)
		value, _ := entry["target_value"].(string)
		if name == "" || name == chromePolicyUnknownValue {
			continue
		}
		if _, ok := keys[name]; ok {
			return nil, fmt.Errorf("additional_target_keys has the target_key %q more than once. "+
				"To set policies for more than one target (for example, more than one app), use one resource for each target", name)
		}
		keys[name] = value
	}
	if len(keys) == 0 {
		return nil, nil
	}
	return keys, nil
}

func (t chromePolicyTarget) resource() string {
	if t.group {
		return "groups/" + t.id
	}
	return "orgunits/" + t.id
}

// key returns the policy target key of the target.
func (t chromePolicyTarget) key() *chromepolicy.GoogleChromePolicyVersionsV1PolicyTargetKey {
	key := &chromepolicy.GoogleChromePolicyVersionsV1PolicyTargetKey{TargetResource: t.resource()}
	if len(t.additionalKeys) > 0 {
		key.AdditionalTargetKeys = map[string]string{}
		for k, v := range t.additionalKeys {
			key.AdditionalTargetKeys[k] = v
		}
	}
	return key
}

// kind returns the type of the target for messages: "org unit" or "group".
func (t chromePolicyTarget) kind() string {
	if t.group {
		return "group"
	}
	return "org unit"
}

// String returns the target for messages, for example "org unit 123" or "group 456 (app_id=chrome:abc)".
func (t chromePolicyTarget) String() string {
	s := t.kind() + " " + t.id
	if len(t.additionalKeys) == 0 {
		return s
	}
	var keys []string
	for k, v := range t.additionalKeys {
		keys = append(keys, k+"="+v)
	}
	sort.Strings(keys)
	return s + " (" + strings.Join(keys, ", ") + ")"
}

// matches reports whether a resolved policy is for the target. The API can resolve a policy for more than one
// target, for example for each app when the request has no app. A resolved policy without a target key is for the
// target of the request.
func (t chromePolicyTarget) matches(resolved *chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy) bool {
	key := resolved.TargetKey
	if key == nil {
		return true
	}
	if key.TargetResource != "" && key.TargetResource != t.resource() {
		return false
	}
	if len(key.AdditionalTargetKeys) != len(t.additionalKeys) {
		return false
	}
	for k, v := range t.additionalKeys {
		if got, ok := key.AdditionalTargetKeys[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// setOn reports whether the resolved policy is set on the target. A policy that is inherited from a parent org unit
// is not. A policy without a source key is not either: the API documents that its value is the default value for the
// customer.
func (t chromePolicyTarget) setOn(resolved *chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy) bool {
	return resolved != nil && resolved.SourceKey != nil && resolved.SourceKey.TargetResource == t.resource()
}

// prepareModify returns a function that writes values to the target in one batch. The requests are prepared before
// the function is returned, so that only the API call can fail.
func (t chromePolicyTarget) prepareModify(ctx context.Context, chromePoliciesService *chromepolicy.CustomersPoliciesService, customer string, values []*chromepolicy.GoogleChromePolicyVersionsV1PolicyValue) (func() error, error) {
	var orgUnitRequests []*chromepolicy.GoogleChromePolicyVersionsV1ModifyOrgUnitPolicyRequest
	var groupRequests []*chromepolicy.GoogleChromePolicyVersionsV1ModifyGroupPolicyRequest
	for _, value := range values {
		var fields map[string]interface{}
		if err := json.Unmarshal(value.Value, &fields); err != nil {
			return nil, err
		}
		var keys []string
		for key := range fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		mask := strings.Join(keys, ",")

		if t.group {
			groupRequests = append(groupRequests, &chromepolicy.GoogleChromePolicyVersionsV1ModifyGroupPolicyRequest{
				PolicyTargetKey: t.key(),
				PolicyValue:     value,
				UpdateMask:      mask,
			})
		} else {
			orgUnitRequests = append(orgUnitRequests, &chromepolicy.GoogleChromePolicyVersionsV1ModifyOrgUnitPolicyRequest{
				PolicyTargetKey: t.key(),
				PolicyValue:     value,
				UpdateMask:      mask,
			})
		}
	}

	parent := fmt.Sprintf("customers/%s", customer)
	return func() error {
		return retryTimeDuration(ctx, time.Minute, func() error {
			var err error
			if t.group {
				_, err = chromePoliciesService.Groups.BatchModify(parent, &chromepolicy.GoogleChromePolicyVersionsV1BatchModifyGroupPoliciesRequest{Requests: groupRequests}).Context(ctx).Do()
			} else {
				_, err = chromePoliciesService.Orgunits.BatchModify(parent, &chromepolicy.GoogleChromePolicyVersionsV1BatchModifyOrgUnitPoliciesRequest{Requests: orgUnitRequests}).Context(ctx).Do()
			}
			return err
		})
	}, nil
}

// reset removes the policies from the target: an org unit inherits them from its parent (batchInherit), and they are
// deleted from a group (batchDelete). On a group, a policy that is not set, or a group that does not exist, is already
// reset (see isChromePolicyNotSet and isChromePolicyGroupNotFound). If such an error fails a batch of more than one
// policy, the policies are deleted one at a time.
func (t chromePolicyTarget) reset(ctx context.Context, chromePoliciesService *chromepolicy.CustomersPoliciesService, customer string, schemaNames []string) error {
	parent := fmt.Sprintf("customers/%s", customer)
	err := retryTimeDuration(ctx, time.Minute, func() error {
		var err error
		if t.group {
			var requests []*chromepolicy.GoogleChromePolicyVersionsV1DeleteGroupPolicyRequest
			for _, schemaName := range schemaNames {
				requests = append(requests, &chromepolicy.GoogleChromePolicyVersionsV1DeleteGroupPolicyRequest{
					PolicyTargetKey: t.key(),
					PolicySchema:    schemaName,
				})
			}
			_, err = chromePoliciesService.Groups.BatchDelete(parent, &chromepolicy.GoogleChromePolicyVersionsV1BatchDeleteGroupPoliciesRequest{Requests: requests}).Context(ctx).Do()
		} else {
			var requests []*chromepolicy.GoogleChromePolicyVersionsV1InheritOrgUnitPolicyRequest
			for _, schemaName := range schemaNames {
				requests = append(requests, &chromepolicy.GoogleChromePolicyVersionsV1InheritOrgUnitPolicyRequest{
					PolicyTargetKey: t.key(),
					PolicySchema:    schemaName,
				})
			}
			_, err = chromePoliciesService.Orgunits.BatchInherit(parent, &chromepolicy.GoogleChromePolicyVersionsV1BatchInheritOrgUnitPoliciesRequest{Requests: requests}).Context(ctx).Do()
		}
		return err
	})
	if !t.group || err == nil {
		return err
	}

	if isChromePolicyNotSet(err, schemaNames) && len(schemaNames) > 1 {
		for _, schemaName := range schemaNames {
			if err := t.reset(ctx, chromePoliciesService, customer, []string{schemaName}); err != nil {
				return err
			}
		}
		return nil
	}
	if isChromePolicyGroupNotFound(err, t.id) || isChromePolicyNotSet(err, schemaNames) {
		log.Printf("[WARN] Chrome Policies %s are already not set on %s: %s", strings.Join(schemaNames, ", "), t, err)
		return nil
	}
	return err
}

// The messages of the API errors for a group that does not exist, and for a group policy that is not set, are not
// verified against the live API. The predicates below match only an error with code 400 or 404 whose message says
// that something is not found and names the group ID or a schema name of the request, so that other errors (for
// example, a 404 for the customer, or an invalid schema name or target key) are not hidden.
var (
	chromePolicyGroupNotFoundMessage = regexp.MustCompile(`(?i)\bgroups?\b.*\b(not found|does not exist|doesn't exist)\b`)
	chromePolicyNotSetMessage        = regexp.MustCompile(`(?i)\bpolic(y|ies)\b.*\b(not found|does not exist|doesn't exist)\b`)
)

// chromePolicyErrorMatches reports whether err is an API error with code 400 or 404 whose message, or the message of
// one of its error items, matches message and names one of names (see chromePolicyMessageNames).
func chromePolicyErrorMatches(err error, message *regexp.Regexp, names []string) bool {
	gerr, ok := errwrap.GetType(err, &googleapi.Error{}).(*googleapi.Error)
	if !ok || gerr == nil || (gerr.Code != 400 && gerr.Code != 404) {
		return false
	}
	messages := []string{gerr.Message}
	for _, item := range gerr.Errors {
		messages = append(messages, item.Message)
	}
	for _, m := range messages {
		if !message.MatchString(m) {
			continue
		}
		for _, name := range names {
			if chromePolicyMessageNames(m, name) {
				return true
			}
		}
	}
	return false
}

// chromePolicyMessageNames reports whether message contains name as a whole name: not as a part of a longer ID or
// schema name. For example, "chrome.users.Foo" is not in "chrome.users.FooBar" or "chrome.users.Foo.Bar".
func chromePolicyMessageNames(message, name string) bool {
	if name == "" {
		return false
	}
	isNameChar := func(c byte) bool {
		return c == '_' || c == '-' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
	}
	for i := 0; ; {
		j := strings.Index(message[i:], name)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(name)
		before := start == 0 || (!isNameChar(message[start-1]) && message[start-1] != '.')
		after := end == len(message) || (!isNameChar(message[end]) &&
			!(message[end] == '.' && end+1 < len(message) && isNameChar(message[end+1])))
		if before && after {
			return true
		}
		i = start + 1
	}
}

// isChromePolicyGroupNotFound reports whether err is an API error for the group groupId that does not exist.
func isChromePolicyGroupNotFound(err error, groupId string) bool {
	return chromePolicyErrorMatches(err, chromePolicyGroupNotFoundMessage, []string{groupId})
}

// isChromePolicyNotSet reports whether err is an API error for a policy in schemaNames that is not set.
func isChromePolicyNotSet(err error, schemaNames []string) bool {
	return chromePolicyErrorMatches(err, chromePolicyNotSetMessage, schemaNames)
}

// validateChromePolicyTarget returns an error if a policy schema does not support groups and the target is a group,
// or if the schema names additional target keys and the target has other keys. A schema that names no additional
// target keys is not checked for keys. If the target has additional target keys and no policy schema names any, it
// returns a warning.
func validateChromePolicyTarget(ctx context.Context, policies []interface{}, schemas *chromePolicySchemaCache, target chromePolicyTarget) diag.Diagnostics {
	var got []string
	for k := range target.additionalKeys {
		got = append(got, k)
	}
	sort.Strings(got)

	namesKeys, allKnown := false, true
	for _, schemaName := range chromePolicyNames(policies) {
		schemaDef, err := schemas.get(ctx, schemaName)
		if err != nil {
			return diag.FromErr(err)
		}
		if schemaDef == nil {
			allKnown = false
			continue
		}

		if target.group && len(schemaDef.ValidTargetResources) > 0 && !stringInSlice(schemaDef.ValidTargetResources, "GROUP") {
			return diag.Errorf("the policy schema %s cannot be set on a group (valid target resources: %s)",
				schemaName, strings.Join(schemaDef.ValidTargetResources, ", "))
		}

		var want []string
		for _, key := range schemaDef.AdditionalTargetKeyNames {
			if key != nil && key.Key != "" {
				want = append(want, key.Key)
			}
		}
		if len(want) == 0 {
			continue
		}
		namesKeys = true
		sort.Strings(want)
		if !reflect.DeepEqual(want, got) {
			have := "none"
			if len(got) > 0 {
				have = strings.Join(got, ", ")
			}
			return diag.Errorf("the policy schema %s uses the additional target keys %s, but additional_target_keys has %s",
				schemaName, strings.Join(want, ", "), have)
		}
	}
	if len(got) > 0 && allKnown && !namesKeys {
		return diag.Diagnostics{{
			Severity: diag.Warning,
			Summary:  "additional_target_keys are not used by the policy schemas",
			Detail: fmt.Sprintf("additional_target_keys has %s, but none of the policy schemas %s names additional target keys. "+
				"The API can reject the keys or apply the policies to another target than you intend.",
				strings.Join(got, ", "), strings.Join(chromePolicyNames(policies), ", ")),
		}}
	}
	return nil
}
