// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"google.golang.org/api/chromepolicy/v1"
)

func resourceChromePolicy() *schema.Resource {
	return &schema.Resource{
		Description: "Chrome Policy resource in the Terraform Googleworkspace provider. Chrome Policy Schema " +
			"resides under the `https://www.googleapis.com/auth/chrome.management.policy` client scope.\n\n" +
			"Create and update wait until the written values are visible in the API, up to the `create` and `update` " +
			"timeouts (5 minutes by default). The write can be sent again: at half of that time if the values are not " +
			"visible, and when a new value that differs from the configuration is seen, to confirm it. A value from " +
			"another writer is overwritten only if that write succeeds and the API applies it. The apply can show these " +
			"warnings:\n\n" +
			"* \"Chrome Policy values are not visible yet\": the values were not visible at the timeout. The state keeps " +
			"the configured values, and the next plan shows a change if the values are still not set.\n" +
			"* \"Chrome Policy values did not change\": a policy still has the value from before the write, also after " +
			"the write was sent again. Nothing is recorded, and the next plan shows a change.\n" +
			"* \"Chrome Policy values differ from the configuration\": the API stores another form of the configured " +
			"values (for example, a normalized value). The provider records that form in `applied_values`, and the " +
			"plan shows no change while the API values and the configured values stay the same.",

		CreateContext: resourceChromePolicyCreate,
		UpdateContext: resourceChromePolicyUpdate,
		ReadContext:   resourceChromePolicyRead,
		DeleteContext: resourceChromePolicyDelete,

		CustomizeDiff: resourceChromePolicyCustomizeDiff,

		Timeouts: &schema.ResourceTimeout{
			Create: schema.DefaultTimeout(5 * time.Minute),
			Update: schema.DefaultTimeout(5 * time.Minute),
		},

		Schema: map[string]*schema.Schema{
			"org_unit_id": {
				Description:      "The target org unit on which this policy is applied.",
				Type:             schema.TypeString,
				Required:         true,
				ForceNew:         true,
				DiffSuppressFunc: diffSuppressOrgUnitId,
			},
			"additional_target_keys": chromePolicyAdditionalTargetKeysSchema(),
			"policies": {
				Description: "Policies to set for the org unit",
				Type:        schema.TypeList,
				Required:    true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"schema_name": {
							Description: "The full qualified name of the policy schema.",
							Type:        schema.TypeString,
							Required:    true,
						},
						"schema_values": {
							Description: "JSON encoded map that represents key/value pairs that " +
								"correspond to the given schema. ",
							Type:     schema.TypeMap,
							Required: true,
							Elem: &schema.Schema{
								Type: schema.TypeString,
								ValidateDiagFunc: validation.ToDiagFunc(
									validation.StringIsJSON,
								),
							},
						},
					},
				},
			},
			"applied_values": {
				Description: "The values that the API stored for a policy after the last write, by schema name, when " +
					"they differ from the configured values (for example, values that the API normalized). The provider " +
					"sets this attribute. While the API values and the configured values do not change, the plan shows " +
					"no change for these policies.",
				Type:     schema.TypeMap,
				Computed: true,
				Elem:     &schema.Schema{Type: schema.TypeString},
			},
		},
	}
}

// chromePolicyUnknownValue is the value that the SDK uses in a plan for a value that is not known yet.
const chromePolicyUnknownValue = "74D93920-ED26-11E3-AC10-0800200C9A66"

func resourceChromePolicyCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	target, err := chromePolicyOrgUnitTarget(d)
	if err != nil {
		return diag.FromErr(err)
	}

	log.Printf("[DEBUG] Creating Chrome Policy for %s", target)

	diags := applyChromePolicies(ctx, d, meta.(*apiClient), target, nil, d.Timeout(schema.TimeoutCreate))

	log.Printf("[DEBUG] Finished creating Chrome Policy for %s", target)
	return diags
}

func resourceChromePolicyUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	target, err := chromePolicyOrgUnitTarget(d)
	if err != nil {
		return diag.FromErr(err)
	}

	return updateChromePolicies(ctx, d, meta.(*apiClient), target)
}

// updateChromePolicies writes the changed policies of a resource to target.
func updateChromePolicies(ctx context.Context, d *schema.ResourceData, client *apiClient, target chromePolicyTarget) diag.Diagnostics {
	log.Printf("[DEBUG] Updating Chrome Policy for %s", target)

	// Update is achieved by inheriting defaults for the previous policySchemas, and then applying the new set
	old, _ := d.GetChange("policies")

	diags := applyChromePolicies(ctx, d, client, target, old.([]interface{}), d.Timeout(schema.TimeoutUpdate))
	if diags.HasError() {
		// Keep the prior state, because the API can still have the old values.
		// Follow-up: if the error comes after a successful write, the form recorded by that write is lost too.
		d.Partial(true)
	}

	log.Printf("[DEBUG] Finished Updating Chrome Policy for %s", target)
	return diags
}

// applyChromePolicies writes the configured policies to the target and waits until the values are visible.
// The configured values are validated before the first change.
//
// On update, prior has the policies from state. A policy that is removed from the configuration, or that has fewer
// fields in the configuration than in state, is reset (inherited on an org unit, deleted on a group) before the write.
// The update mask of a write cannot clear a field: each field in the mask must have a value. Other policies are only
// written, because the update mask then covers each field in state. If the write fails after a policy was reset, the
// values from before the change are written again once.
func applyChromePolicies(ctx context.Context, d *schema.ResourceData, client *apiClient, target chromePolicyTarget, prior []interface{}, timeout time.Duration) (diags diag.Diagnostics) {
	chromePolicyService, diags := client.NewChromePolicyService()
	if diags.HasError() {
		return diags
	}

	chromePoliciesService, diags := GetChromePoliciesService(chromePolicyService)
	if diags.HasError() {
		return diags
	}

	// All steps of the operation use the same schemas.
	schemas, diags := newChromePolicySchemaCache(client)
	if diags.HasError() {
		return diags
	}

	configured := d.Get("policies").([]interface{})

	diags = validateChromePolicies(ctx, configured, schemas)
	if diags.HasError() {
		return diags
	}

	targetDiags := validateChromePolicyTarget(ctx, configured, schemas, target)
	if targetDiags.HasError() {
		return targetDiags
	}
	// The warnings about the target are part of each result.
	defer func() {
		diags = append(targetDiags, diags...)
	}()

	configuredValues := map[string]map[string]interface{}{}
	for _, p := range configured {
		policy := p.(map[string]interface{})
		configuredValues[policy["schema_name"].(string)] = policy["schema_values"].(map[string]interface{})
	}

	inherited := map[string]bool{}
	var inherit []string
	for _, p := range prior {
		policy := p.(map[string]interface{})
		schemaName := policy["schema_name"].(string)
		if !chromePolicyKeysContain(configuredValues[schemaName], policy["schema_values"].(map[string]interface{})) && !inherited[schemaName] {
			inherited[schemaName] = true
			inherit = append(inherit, schemaName)
		}
	}

	// Prepare the write before the first change, so that only API calls can fail after a policy is inherited.
	writePolicies, diags := wrapRepeatedScalars(ctx, configured, schemas)
	if diags.HasError() {
		return diags
	}

	policies, diags := expandChromePoliciesValues(writePolicies)
	if diags.HasError() {
		return diags
	}

	batchModify, err := target.prepareModify(ctx, chromePoliciesService, client.Customer, policies)
	if err != nil {
		return diag.FromErr(err)
	}

	// The values before the change. The poll compares them with the values after the write, and they are written
	// again if the write fails after a policy was inherited.
	beforeResolved := map[string]*chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy{}
	before := map[string]map[string]interface{}{}
	for _, schemaName := range append(chromePolicyNames(configured), inherit...) {
		if _, ok := beforeResolved[schemaName]; ok {
			continue
		}

		resolved, err := resolveChromePolicy(ctx, chromePoliciesService, client.Customer, target, schemaName)
		if err != nil {
			return diag.FromErr(err)
		}
		beforeResolved[schemaName] = resolved
		if before[schemaName], err = resolvedChromePolicyValues(resolved); err != nil {
			return diag.FromErr(err)
		}
	}

	if len(inherit) > 0 {
		// The poll does not wait for the reset policies. A follow-up can poll them until they are no longer set on the
		// target.
		if err := target.reset(ctx, chromePoliciesService, client.Customer, inherit); err != nil {
			return diag.FromErr(err)
		}
	}

	if err := batchModify(); err != nil {
		if len(inherit) == 0 {
			return diag.FromErr(err)
		}
		return restoreChromePolicies(ctx, chromePoliciesService, client.Customer, target, inherit, beforeResolved, err)
	}

	d.SetId(target.id)

	// batchModify can return success before the values are visible, and it does not always apply them.
	// If the values do not become visible, keep the configured values in state and warn.
	// The next refresh then finds the missing values as drift.
	// The recorded forms in state. During apply, the planned value of applied_values is not known.
	priorForms, _ := d.GetChange("applied_values")
	forms := chromePolicyAppliedForms(priorForms)

	// A recorded form for the configured values of a policy is also a converged value.
	recorded := map[string]chromePolicyAppliedForm{}
	for schemaName, form := range forms {
		if values, ok := configuredValues[schemaName]; ok && form.ConfigHash == chromePolicyConfigHash(values) {
			recorded[schemaName] = form
		}
	}

	// The policies that were not set on the target before the write: not set, or inherited from a parent.
	notLocalBefore := map[string]bool{}
	for schemaName := range configuredValues {
		if !target.setOn(beforeResolved[schemaName]) {
			notLocalBefore[schemaName] = true
		}
	}

	diags, read, applied := waitForChromePolicies(ctx, chromePolicyPoll{
		service:        chromePoliciesService,
		schemas:        schemas,
		customer:       client.Customer,
		target:         target,
		configured:     configured,
		before:         before,
		notLocalBefore: notLocalBefore,
		reset:          inherited,
		recorded:       recorded,
		timeout:        timeout,
		resend:         batchModify,
	})

	// Record the values that the API stored in another form than the configured values, with the configured values
	// that produced them. A recorded form of an earlier write does not apply after this write.
	for schemaName, form := range forms {
		if values, ok := configuredValues[schemaName]; !ok || read || form.ConfigHash != chromePolicyConfigHash(values) {
			delete(forms, schemaName)
		}
	}
	for schemaName, values := range applied {
		forms[schemaName] = chromePolicyAppliedForm{ConfigHash: chromePolicyConfigHash(configuredValues[schemaName]), Values: values}
	}
	if err := setChromePolicyAppliedForms(d, forms); err != nil {
		return append(diags, diag.FromErr(err)...)
	}

	if !read {
		return diags
	}

	return append(diags, readChromePolicies(ctx, d, client.Customer, chromePoliciesService, schemas, target, forms)...)
}

// chromePolicyAppliedForm is the value of a policy that the API stored after a write, when it differs from the
// configured values (for example, a value that the API normalized), with the hash of the configured values.
type chromePolicyAppliedForm struct {
	ConfigHash string          `json:"config_hash"`
	Values     json.RawMessage `json:"values"`
}

// chromePolicyConfigHash returns a hash of the schema values of a policy.
func chromePolicyConfigHash(values map[string]interface{}) string {
	// json.Marshal sorts the keys of a map.
	data, err := json.Marshal(values)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// chromePolicyAppliedForms returns the recorded forms in a value of applied_values, by schema name. Entries that
// cannot be read are left out. State from an earlier version of the provider has no entries.
func chromePolicyAppliedForms(appliedValues interface{}) map[string]chromePolicyAppliedForm {
	forms := map[string]chromePolicyAppliedForm{}
	raw, _ := appliedValues.(map[string]interface{})
	for schemaName, v := range raw {
		var form chromePolicyAppliedForm
		if s, ok := v.(string); ok && json.Unmarshal([]byte(s), &form) == nil && form.ConfigHash != "" {
			forms[schemaName] = form
		}
	}
	return forms
}

func setChromePolicyAppliedForms(d *schema.ResourceData, forms map[string]chromePolicyAppliedForm) error {
	raw := map[string]interface{}{}
	for schemaName, form := range forms {
		data, err := json.Marshal(form)
		if err != nil {
			return err
		}
		raw[schemaName] = string(data)
	}
	return d.Set("applied_values", raw)
}

// sameChromePolicyJSON reports whether a and b encode the same JSON value.
func sameChromePolicyJSON(a, b []byte) bool {
	var aVal, bVal interface{}
	if unmarshalChromePolicyJSON(a, &aVal) != nil || unmarshalChromePolicyJSON(b, &bVal) != nil {
		return false
	}
	return reflect.DeepEqual(aVal, bVal)
}

// chromePolicyNames returns the schema names of policies.
func chromePolicyNames(policies []interface{}) []string {
	var names []string
	for _, p := range policies {
		names = append(names, p.(map[string]interface{})["schema_name"].(string))
	}
	return names
}

// chromePolicyKeysContain reports whether values has each key in keys. values is nil if the policy is not configured.
func chromePolicyKeysContain(values, keys map[string]interface{}) bool {
	if values == nil {
		return false
	}
	for k := range keys {
		if _, ok := values[k]; !ok {
			return false
		}
	}
	return true
}

// restoreChromePolicies writes the values from before the change again for the reset policies that were set on the
// target, after writeErr. It returns an error that tells if the values were restored.
//
// This assumes that a batchModify call that fails applies none of its requests. The API documentation does not say if
// a failed call can apply some of them.
func restoreChromePolicies(ctx context.Context, chromePoliciesService *chromepolicy.CustomersPoliciesService, customer string, target chromePolicyTarget, inherited []string, beforeResolved map[string]*chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy, writeErr error) diag.Diagnostics {
	var values []*chromepolicy.GoogleChromePolicyVersionsV1PolicyValue
	for _, schemaName := range inherited {
		resolved := beforeResolved[schemaName]
		got, err := resolvedChromePolicyValues(resolved)
		if err != nil || got == nil || !target.setOn(resolved) {
			continue
		}
		values = append(values, &chromepolicy.GoogleChromePolicyVersionsV1PolicyValue{PolicySchema: schemaName, Value: resolved.Value.Value})
	}

	summary := fmt.Sprintf("writing the Chrome Policies for %s failed after the policies %s were reset: %s. "+
		"The API can still apply that write later",
		target, strings.Join(inherited, ", "), writeErr)
	if len(values) == 0 {
		return diag.Errorf("%s. These policies were not set on the %s before the change", summary, target.kind())
	}

	// The restore also runs if the operation was canceled or its deadline passed.
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	restore, err := target.prepareModify(restoreCtx, chromePoliciesService, customer, values)
	if err == nil {
		err = restore()
	}
	if err != nil {
		return diag.Errorf("%s. Writing the values from before the change again also failed: %s", summary, err)
	}
	return diag.Errorf("%s. Terraform wrote the values from before the change again; if the first write is applied later, the next plan shows it", summary)
}

// wrapRepeatedScalars returns a copy of policies in which a value that is not a list, for a top-level repeated field,
// is a list with that value. Earlier versions of the provider did not accept a list for such a field.
func wrapRepeatedScalars(ctx context.Context, policies []interface{}, schemas *chromePolicySchemaCache) ([]interface{}, diag.Diagnostics) {
	var out []interface{}
	for _, p := range policies {
		policy := p.(map[string]interface{})
		schemaName := policy["schema_name"].(string)

		types, diags := schemas.types(ctx, schemaName, diag.Error)
		if len(diags) > 0 {
			return nil, diags
		}

		values := map[string]interface{}{}
		for k, v := range policy["schema_values"].(map[string]interface{}) {
			values[k] = v
			field := types.topLevelField(k)
			if field == nil {
				continue
			}
			var value interface{}
			if err := unmarshalChromePolicyJSON([]byte(v.(string)), &value); err != nil {
				return nil, diag.FromErr(err)
			}
			if types.isRepeatedScalar(*field, value) {
				list, err := json.Marshal([]interface{}{value})
				if err != nil {
					return nil, diag.FromErr(err)
				}
				values[k] = string(list)
			}
		}

		out = append(out, map[string]interface{}{"schema_name": schemaName, "schema_values": values})
	}
	return out, nil
}

// resourceChromePolicyCustomizeDiff validates changed policy values of an org unit resource at plan time.
func resourceChromePolicyCustomizeDiff(ctx context.Context, d *schema.ResourceDiff, meta interface{}) error {
	return chromePolicyCustomizeDiff(ctx, d, meta, false)
}

// chromePolicyCustomizeDiff validates changed policy values at plan time. group tells if the target is a group.
// If a value is not known yet, or a schema cannot be fetched, the values are validated at apply time only.
func chromePolicyCustomizeDiff(ctx context.Context, d *schema.ResourceDiff, meta interface{}, group bool) error {
	keys, err := chromePolicyAdditionalTargetKeys(d.Get("additional_target_keys"))
	if err != nil {
		return err
	}

	if d.Id() != "" && !d.HasChange("policies") {
		// State from an earlier version of the provider has no applied_values. The SDK then plans it as a new
		// computed value, which must not cause a change by itself.
		return d.Clear("applied_values")
	}

	if d.Id() != "" {
		// The apply records new forms of values.
		if err := d.SetNewComputed("applied_values"); err != nil {
			return err
		}
	}

	client, ok := meta.(*apiClient)
	if !ok || client == nil || !d.NewValueKnown("policies") {
		return nil
	}

	policies, ok := d.Get("policies").([]interface{})
	if !ok || !chromePolicyValuesKnown(policies) {
		return nil
	}

	schemas, diags := newChromePolicySchemaCache(client)
	if diags.HasError() {
		return nil
	}

	for _, p := range policies {
		schemaName := p.(map[string]interface{})["schema_name"].(string)
		if _, err := schemas.get(ctx, schemaName); err != nil {
			log.Printf("[WARN] Not validating Chrome Policy values at plan time: cannot get schema %s: %s", schemaName, err)
			return nil
		}
	}

	if diags := validateChromePolicies(ctx, policies, schemas); diags.HasError() {
		return errors.New(chromePolicyErrors(diags))
	}

	if !d.NewValueKnown("additional_target_keys") {
		return nil
	}
	if diags := validateChromePolicyTarget(ctx, policies, schemas, chromePolicyTarget{group: group, additionalKeys: keys}); diags.HasError() {
		return errors.New(diags[0].Summary)
	}
	return nil
}

// chromePolicyErrors joins the error diagnostics in diags, each as "summary: detail", or only the summary if there is
// no detail.
func chromePolicyErrors(diags diag.Diagnostics) string {
	var messages []string
	for _, d := range diags {
		if d.Severity != diag.Error {
			continue
		}
		message := d.Summary
		if d.Detail != "" {
			message += ": " + d.Detail
		}
		messages = append(messages, message)
	}
	return strings.Join(messages, "; ")
}

// chromePolicyValuesKnown reports whether all schema names and values in policies are known.
func chromePolicyValuesKnown(policies []interface{}) bool {
	for _, p := range policies {
		policy, ok := p.(map[string]interface{})
		if !ok {
			return false
		}
		schemaName, ok := policy["schema_name"].(string)
		if !ok || schemaName == "" || schemaName == chromePolicyUnknownValue {
			return false
		}
		values, ok := policy["schema_values"].(map[string]interface{})
		if !ok {
			return false
		}
		for _, v := range values {
			if s, ok := v.(string); !ok || s == chromePolicyUnknownValue {
				return false
			}
		}
	}
	return true
}

func resourceChromePolicyRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	target, err := chromePolicyOrgUnitTarget(d)
	if err != nil {
		return diag.FromErr(err)
	}

	return readChromePolicyResource(ctx, d, meta.(*apiClient), target)
}

// readChromePolicyResource refreshes the policies of a resource from target.
func readChromePolicyResource(ctx context.Context, d *schema.ResourceData, client *apiClient, target chromePolicyTarget) diag.Diagnostics {
	chromePolicyService, diags := client.NewChromePolicyService()
	if diags.HasError() {
		return diags
	}

	chromePoliciesService, diags := GetChromePoliciesService(chromePolicyService)
	if diags.HasError() {
		return diags
	}

	schemas, diags := newChromePolicySchemaCache(client)
	if diags.HasError() {
		return diags
	}

	log.Printf("[DEBUG] Getting Chrome Policy for %s", target)

	diags = readChromePolicies(ctx, d, client.Customer, chromePoliciesService, schemas, target, chromePolicyAppliedForms(d.Get("applied_values")))

	log.Printf("[DEBUG] Finished getting Chrome Policy for %s", target)
	return diags
}

// readChromePolicies sets the policies in state from the API.
// A policy that is not set is removed from state with a warning, so that the next plan sets it again.
// forms has the recorded forms of values, by schema name.
func readChromePolicies(ctx context.Context, d *schema.ResourceData, customer string, chromePoliciesService *chromepolicy.CustomersPoliciesService, schemas *chromePolicySchemaCache, target chromePolicyTarget, forms map[string]chromePolicyAppliedForm) diag.Diagnostics {
	var diags diag.Diagnostics

	prior := map[string]map[string]interface{}{}
	policiesObj := []*chromepolicy.GoogleChromePolicyVersionsV1PolicyValue{}
	for _, p := range d.Get("policies").([]interface{}) {
		policy := p.(map[string]interface{})
		schemaName := policy["schema_name"].(string)
		prior[schemaName] = policy["schema_values"].(map[string]interface{})

		resolved, err := resolveChromePolicy(ctx, chromePoliciesService, customer, target, schemaName)
		if target.group && isChromePolicyGroupNotFound(err, target.id) {
			log.Printf("[WARN] Removing Chrome Policy for %s because the group does not exist: %s", target, err)
			d.SetId("")
			return append(diags, diag.Diagnostic{
				Severity: diag.Warning,
				Summary:  fmt.Sprintf("Chrome Policy group %s does not exist", target.id),
				Detail: fmt.Sprintf("The Chrome Policy API returned that the %s does not exist: %s. Terraform removes the "+
					"resource from state.", target, err),
			})
		}
		if err != nil {
			return append(diags, diag.FromErr(err)...)
		}

		// A group gets a policy only from itself. A value with another source, for example an org unit, is not set
		// on the group. On an org unit, an inherited value is kept, as before.
		if resolved == nil || resolved.Value == nil || (target.group && !target.setOn(resolved)) {
			// For example, a write that the API returned success for but did not apply.
			diags = append(diags, diag.Diagnostic{
				Severity: diag.Warning,
				Summary:  fmt.Sprintf("Chrome Policy %s is not set", schemaName),
				Detail: fmt.Sprintf("The Chrome Policy %s is not set for %s. "+
					"Terraform removes it from state, and the next plan sets it again.", schemaName, target),
			})
			continue
		}

		policiesObj = append(policiesObj, resolved.Value)
	}

	if len(policiesObj) == 0 {
		log.Printf("[WARN] Removing Chrome Policy for %s because none of its policies are set", target)
		d.SetId("")
		return diags
	}

	policies, keptForms, flattenDiags := flattenChromePolicies(ctx, policiesObj, schemas, prior, forms)
	diags = append(diags, flattenDiags...)
	if flattenDiags.HasError() || policies == nil {
		// The values cannot be read. Keep the policies in state.
		return diags
	}

	if err := d.Set("policies", policies); err != nil {
		return append(diags, diag.FromErr(err)...)
	}

	// A recorded form that does not match the API value or the values in state does not apply any more.
	for schemaName := range forms {
		if !keptForms[schemaName] {
			delete(forms, schemaName)
		}
	}
	if err := setChromePolicyAppliedForms(d, forms); err != nil {
		return append(diags, diag.FromErr(err)...)
	}

	return diags
}

func resourceChromePolicyDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	target, err := chromePolicyOrgUnitTarget(d)
	if err != nil {
		return diag.FromErr(err)
	}

	return deleteChromePolicies(ctx, d, meta.(*apiClient), target)
}

// deleteChromePolicies resets the policies of a resource on target: an org unit inherits them, and they are deleted
// from a group.
func deleteChromePolicies(ctx context.Context, d *schema.ResourceData, client *apiClient, target chromePolicyTarget) diag.Diagnostics {
	chromePolicyService, diags := client.NewChromePolicyService()
	if diags.HasError() {
		return diags
	}

	chromePoliciesService, diags := GetChromePoliciesService(chromePolicyService)
	if diags.HasError() {
		return diags
	}

	log.Printf("[DEBUG] Deleting Chrome Policy for %s", target)

	// Delete does not wait for the reset policies. A follow-up can poll them until they are no longer set on the
	// target.
	if err := target.reset(ctx, chromePoliciesService, client.Customer, chromePolicyNames(d.Get("policies").([]interface{}))); err != nil {
		return diag.FromErr(err)
	}

	log.Printf("[DEBUG] Finished deleting Chrome Policy for %s", target)
	return nil
}

// resolveChromePolicy returns the resolved policy schemaName for the target, or nil if the policy is not set.
// The API can return a policy for more than one target, for example for each app. Only the policy for the target, with
// the same additional target keys, is returned.
func resolveChromePolicy(ctx context.Context, chromePoliciesService *chromepolicy.CustomersPoliciesService, customer string, target chromePolicyTarget, schemaName string) (*chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy, error) {
	var resolved, matched []*chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy
	pageToken := ""
	for {
		var resp *chromepolicy.GoogleChromePolicyVersionsV1ResolveResponse
		err := retryTimeDuration(ctx, time.Minute, func() error {
			var retryErr error
			resp, retryErr = chromePoliciesService.Resolve(fmt.Sprintf("customers/%s", customer), &chromepolicy.GoogleChromePolicyVersionsV1ResolveRequest{
				PolicySchemaFilter: schemaName,
				PolicyTargetKey:    target.key(),
				PageToken:          pageToken,
			}).Context(ctx).Do()
			return retryErr
		})
		if err != nil {
			return nil, err
		}

		for _, policy := range resp.ResolvedPolicies {
			if policy == nil {
				continue
			}
			resolved = append(resolved, policy)
			if target.matches(policy) {
				matched = append(matched, policy)
			}
		}

		if resp.NextPageToken == "" || resp.NextPageToken == pageToken {
			break
		}
		pageToken = resp.NextPageToken
	}

	switch len(matched) {
	case 0:
		if len(resolved) == 0 {
			return nil, nil
		}
		// A resolved policy that does not match has a target key (see matches).
		withKeys := false
		for _, policy := range resolved {
			if len(policy.TargetKey.AdditionalTargetKeys) > 0 {
				withKeys = true
			}
		}
		if !withKeys {
			return nil, fmt.Errorf("the API resolved the policy %s for the target resource %q without additional target keys, not for %s",
				schemaName, resolved[0].TargetKey.TargetResource, target)
		}
		if len(target.additionalKeys) == 0 {
			return nil, fmt.Errorf("the API resolved %d policies for schema %s in %s, each for other additional target keys "+
				"(for example, one for each app): set additional_target_keys to select one", len(resolved), schemaName, target)
		}
		// The policy is set only for other additional target keys, for example for other apps.
		return nil, nil
	case 1:
		return matched[0], nil
	default:
		return nil, fmt.Errorf("unexpected number of resolved policies for schema %s in %s: got %d, expected 1 "+
			"(set additional_target_keys for policies that require them)", schemaName, target, len(matched))
	}
}

// resolvedChromePolicyValues returns the values of a resolved policy, or nil if it is not set.
// Numbers are json.Number, so that large integers stay exact.
func resolvedChromePolicyValues(resolved *chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy) (map[string]interface{}, error) {
	if resolved == nil || resolved.Value == nil || len(resolved.Value.Value) == 0 {
		return nil, nil
	}

	var values map[string]interface{}
	if err := unmarshalChromePolicyJSON(resolved.Value.Value, &values); err != nil {
		return nil, err
	}
	return values, nil
}

// unmarshalChromePolicyJSON unmarshals data with numbers as json.Number.
func unmarshalChromePolicyJSON(data []byte, v interface{}) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(v)
}

// chromePolicyState is the state of a policy in a round of the poll after a write.
type chromePolicyState int

const (
	// chromePolicyPending: the policy has no value that can be accepted yet.
	chromePolicyPending chromePolicyState = iota
	// chromePolicyMatches: the values are equal to the configured values, after normalization.
	chromePolicyMatches
	// chromePolicyRecordedForm: the value is the recorded form for the configured values.
	chromePolicyRecordedForm
	// chromePolicyCandidate: a new value on the target that differs from the configuration. Before it is accepted,
	// the write is sent again, and the value must not change in two rounds after that.
	chromePolicyCandidate
	// chromePolicyNewForm: a candidate that did not change in two rounds after the write was sent again. It is
	// probably the form in which the API stores the configured value.
	chromePolicyNewForm
	// chromePolicyUnchanged: the value from before the write.
	chromePolicyUnchanged
)

// chromePolicyRound is what a round of the poll found for one policy.
type chromePolicyRound struct {
	// set is true if the policy has a value.
	set bool
	// matches is true if the values are equal to the configured values, after normalization.
	matches bool
	// recorded is true if the value is the recorded form for the configured values.
	recorded bool
	// local is true if the value is set on the target (its source key is the target).
	local bool
	// sameAsBefore is true if the value is the value from before the write.
	sameAsBefore bool
	// notLocalBefore is true if the policy was not set on the target before the write.
	notLocalBefore bool
	// stable is true if the value is the same as in the previous round.
	stable bool
	// confirmedRounds is the number of rounds, after the write was sent again for this value, in which the value did
	// not change.
	confirmedRounds int
}

// classifyChromePolicy returns the state of a policy in a round of the poll.
//
// A value that is new on the target (different from the value before the write, or set on the target for the first
// time) and that differs from the configuration is only a candidate: another writer, or a write that was not
// applied, can also cause it. The write is sent again, and only a value that stays the same after that is the result
// of the write.
func classifyChromePolicy(r chromePolicyRound) chromePolicyState {
	switch {
	case !r.set:
		return chromePolicyPending
	case r.matches:
		return chromePolicyMatches
	case r.recorded:
		return chromePolicyRecordedForm
	case r.local && r.stable && (!r.sameAsBefore || r.notLocalBefore):
		if r.confirmedRounds >= 2 {
			return chromePolicyNewForm
		}
		return chromePolicyCandidate
	case r.sameAsBefore:
		return chromePolicyUnchanged
	}
	return chromePolicyPending
}

// chromePolicyPoll has the inputs of waitForChromePolicies.
type chromePolicyPoll struct {
	service    *chromepolicy.CustomersPoliciesService
	schemas    *chromePolicySchemaCache
	customer   string
	target     chromePolicyTarget
	configured []interface{}
	// before has the values before the write, by schema name.
	before map[string]map[string]interface{}
	// notLocalBefore has the schema names that were not set on the target before the write.
	notLocalBefore map[string]bool
	// reset has the schema names that were reset before the write (inherited on an org unit, deleted on a group).
	reset map[string]bool
	// recorded has the recorded forms for the configured values, by schema name.
	recorded map[string]chromePolicyAppliedForm
	timeout  time.Duration
	// resend sends the write again. The write is idempotent.
	resend func() error
}

// chromePolicyCandidateValue is a candidate value of a policy, and the number of resends when it appeared.
type chromePolicyCandidateValue struct {
	value     map[string]interface{}
	resends   int
	confirmed int
}

// waitForChromePolicies polls policies:resolve until each configured policy has converged (see classifyChromePolicy).
// It returns false if the caller must keep the configured values in state, and not read them from the API.
// It also returns the values (as the API returned them) of the policies that converged with other values than the
// configured values.
//
// The poll stops at the timeout or at the context deadline, if that is earlier. If a policy has not converged after
// half of that time, the write is sent again, because the API sometimes returns success for a write that it does not
// apply. At the timeout, a policy that still has the value from before the write, after the write was sent again, gets
// its own warning, and nothing is recorded for it. A policy that was reset before the write and has not converged is an
// error, because the target does not have the values that it had before the change. If the context is canceled, it returns an error.
func waitForChromePolicies(ctx context.Context, poll chromePolicyPoll) (diag.Diagnostics, bool, map[string]json.RawMessage) {
	start := time.Now()
	deadline := start.Add(poll.timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	budget := deadline.Sub(start)
	resendAt := start.Add(budget / 2)

	// The refresh function can still run in a goroutine of the retry after the retry returns, so it holds mu.
	var mu sync.Mutex
	var pending, changed []string
	for _, p := range poll.configured {
		pending = append(pending, p.(map[string]interface{})["schema_name"].(string))
	}
	unchanged := map[string]bool{}
	applied := map[string]json.RawMessage{}
	previous := map[string]map[string]interface{}{}
	candidates := map[string]*chromePolicyCandidateValue{}
	// resends counts the resends that succeeded. Only these can confirm a candidate value.
	resends := 0
	halfTimeResent := false
	var resendErr error
	var stopDiags diag.Diagnostics

	sendAgain := func(reason string) {
		log.Printf("[WARN] Sending the Chrome Policy write for %s again after %s: %s",
			poll.target, time.Since(start).Round(time.Second), reason)
		if resendErr = poll.resend(); resendErr != nil {
			log.Printf("[WARN] Sending the Chrome Policy write again for %s failed: %s", poll.target, resendErr)
			return
		}
		resends++
	}

	err := errors.New("timed out while waiting for Chrome Policies: no time left")
	if budget > 0 {
		err = retryTimeDuration(ctx, budget, func() error {
			mu.Lock()
			defer mu.Unlock()

			if !halfTimeResent && len(previous) > 0 && !time.Now().Before(resendAt) {
				halfTimeResent = true
				sendAgain("the values of " + strings.Join(pending, ", ") + " are not visible")
			}

			var roundPending, roundChanged []string
			roundApplied := map[string]json.RawMessage{}
			roundUnchanged := map[string]bool{}
			confirm := false
			for _, p := range poll.configured {
				policy := p.(map[string]interface{})
				schemaName := policy["schema_name"].(string)

				types, diags := poll.schemas.types(ctx, schemaName, diag.Warning)
				if len(diags) > 0 {
					// The values cannot be compared.
					stopDiags = diags
					return nil
				}

				resolved, err := resolveChromePolicy(ctx, poll.service, poll.customer, poll.target, schemaName)
				if err != nil {
					return err
				}
				got, err := resolvedChromePolicyValues(resolved)
				if err != nil {
					return err
				}

				candidate := candidates[schemaName]
				if candidate != nil && !reflect.DeepEqual(got, candidate.value) {
					delete(candidates, schemaName)
					candidate = nil
				}
				if candidate != nil && resends > candidate.resends {
					candidate.confirmed++
				}

				round := chromePolicyRound{
					set:            got != nil,
					local:          poll.target.setOn(resolved),
					sameAsBefore:   reflect.DeepEqual(got, poll.before[schemaName]),
					notLocalBefore: poll.notLocalBefore[schemaName],
					stable:         reflect.DeepEqual(got, previous[schemaName]),
				}
				if got != nil {
					round.matches = types.valuesMatch(policy["schema_values"].(map[string]interface{}), got)
					form, ok := poll.recorded[schemaName]
					round.recorded = ok && sameChromePolicyJSON(form.Values, resolved.Value.Value)
				}
				if candidate != nil {
					round.confirmedRounds = candidate.confirmed
				}

				switch classifyChromePolicy(round) {
				case chromePolicyMatches:
					continue
				case chromePolicyRecordedForm:
					roundApplied[schemaName] = poll.recorded[schemaName].Values
					continue
				case chromePolicyNewForm:
					roundChanged = append(roundChanged, schemaName)
					roundApplied[schemaName] = append(json.RawMessage(nil), resolved.Value.Value...)
					continue
				case chromePolicyCandidate:
					if candidate == nil {
						candidate = &chromePolicyCandidateValue{value: got, resends: resends}
						candidates[schemaName] = candidate
					}
					// The write must be sent again after the value appeared. If that failed, try again.
					if resends == candidate.resends {
						confirm = true
					}
				case chromePolicyUnchanged:
					roundUnchanged[schemaName] = true
				}

				previous[schemaName] = got
				roundPending = append(roundPending, schemaName)
			}

			pending, changed, applied, unchanged = roundPending, roundChanged, roundApplied, roundUnchanged
			if confirm {
				// A new value that differs from the configuration must not change after the write is sent again.
				sendAgain("confirm the new values")
			}
			if len(pending) > 0 {
				return fmt.Errorf("timed out while waiting for Chrome Policies %s to be set for %s", strings.Join(pending, ", "), poll.target)
			}
			return nil
		})
	}

	mu.Lock()
	defer mu.Unlock()
	elapsed := time.Since(start).Round(time.Second)

	if errors.Is(ctx.Err(), context.Canceled) {
		return diag.Errorf("the operation was canceled while waiting for the Chrome Policies of %s to be visible, after %s. "+
			"The write was sent; the next plan shows the values that the API has", poll.target, elapsed), false, nil
	}

	if len(stopDiags) > 0 {
		return stopDiags, false, nil
	}

	if err != nil {
		if !IsNotConsistent(err) && !errors.Is(err, context.DeadlineExceeded) {
			return diag.FromErr(err), false, nil
		}

		var diags diag.Diagnostics
		var resetNames, unchangedNames, notVisible []string
		for _, schemaName := range pending {
			switch {
			case poll.reset[schemaName]:
				resetNames = append(resetNames, schemaName)
			case unchanged[schemaName] && resends > 0 && resendErr == nil:
				unchangedNames = append(unchangedNames, schemaName)
			default:
				notVisible = append(notVisible, schemaName)
			}
		}

		if len(resetNames) > 0 {
			detail := "The org unit can now use the inherited values for these policies."
			if poll.target.group {
				detail = "The group does not set these policies now."
			}
			diags = append(diags, diag.Diagnostic{
				Severity: diag.Error,
				Summary: fmt.Sprintf("Chrome Policies %s on %s were reset before the write, and the configured values are not visible after %s",
					strings.Join(resetNames, ", "), poll.target, elapsed),
				Detail: detail + " Apply again, or check the policies in the Admin console.",
			})
		}

		if len(unchangedNames) > 0 {
			diags = append(diags, diag.Diagnostic{
				Severity: diag.Warning,
				Summary:  "Chrome Policy values did not change",
				Detail: fmt.Sprintf("After the write, and after Terraform sent it again, %s on %s still have the value from before the write. "+
					"Either the API stores the configured value in this form, or the write was not applied. Terraform did not record it; the next plan shows a change. "+
					"If the plan shows the value you intend, set the configuration to that form. If not, apply again or check the Admin console.",
					strings.Join(unchangedNames, ", "), poll.target),
			})
		}

		if len(notVisible) > 0 {
			detail := fmt.Sprintf("The Chrome Policy API accepted the change for %s, but these policies did not have the configured values after %s: %s. "+
				"Terraform saved the configured values. If the values are still not set, the next plan shows a change to set them again.",
				poll.target, elapsed, strings.Join(notVisible, ", "))
			if resendErr != nil {
				detail += fmt.Sprintf(" Terraform sent the write again, and that request failed: %s.", resendErr)
			}
			diags = append(diags, diag.Diagnostic{
				Severity: diag.Warning,
				Summary:  "Chrome Policy values are not visible yet",
				Detail:   detail,
			})
		}

		// The time is used up, so the values are not read again. The state keeps the configured values.
		return append(diags, chromePolicyChangedWarning(changed, poll.target)...), false, applied
	}

	return chromePolicyChangedWarning(changed, poll.target), true, applied
}

// chromePolicyChangedWarning returns a warning for the policies whose values differ from the configuration after a
// write, or nothing if there are none.
func chromePolicyChangedWarning(changed []string, target chromePolicyTarget) diag.Diagnostics {
	if len(changed) == 0 {
		return nil
	}
	return diag.Diagnostics{{
		Severity: diag.Warning,
		Summary:  "Chrome Policy values differ from the configuration",
		Detail: fmt.Sprintf("After the write, the API returns values for %s on %s that differ from the configuration and do not change. "+
			"The API can normalize values. Terraform records these values in applied_values, and shows no change for these "+
			"policies while the API values and the configured values stay the same.", strings.Join(changed, ", "), target),
	}}
}

// Chrome Policies

// chromePolicySchemaStore holds policy schemas from the API, by the path of the schema. The provider keeps one store
// for all operations, and copies of an apiClient share it. It is safe for concurrent use.
type chromePolicySchemaStore struct {
	mu      sync.Mutex
	schemas map[string]*chromepolicy.GoogleChromePolicyVersionsV1PolicySchema
}

func newChromePolicySchemaStore() *chromePolicySchemaStore {
	return &chromePolicySchemaStore{schemas: map[string]*chromepolicy.GoogleChromePolicyVersionsV1PolicySchema{}}
}

// lookup returns the schema at path, and false if the store does not have it.
func (s *chromePolicySchemaStore) lookup(path string) (*chromepolicy.GoogleChromePolicyVersionsV1PolicySchema, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	schemaDef, ok := s.schemas[path]
	return schemaDef, ok
}

// addMissing adds the schemas, by path, that the store does not have yet. A schema that the store has is not replaced.
func (s *chromePolicySchemaStore) addMissing(schemas map[string]*chromepolicy.GoogleChromePolicyVersionsV1PolicySchema) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for path, schemaDef := range schemas {
		if _, ok := s.schemas[path]; !ok {
			s.schemas[path] = schemaDef
		}
	}
}

// set stores the schema at path, in place of any entry.
func (s *chromePolicySchemaStore) set(path string, schemaDef *chromepolicy.GoogleChromePolicyVersionsV1PolicySchema) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schemas[path] = schemaDef
}

// chromePolicySchemaCache gets each policy schema from the API once. It keeps the schemas in the store of the client,
// or in its own store if the client has none. It is safe for concurrent use.
type chromePolicySchemaCache struct {
	customer string
	service  *chromepolicy.CustomersPolicySchemasService
	store    *chromePolicySchemaStore
}

func newChromePolicySchemaCache(client *apiClient) (*chromePolicySchemaCache, diag.Diagnostics) {
	chromePolicyService, diags := client.NewChromePolicyService()
	if diags.HasError() {
		return nil, diags
	}

	chromePolicySchemasService, diags := GetChromePolicySchemasService(chromePolicyService)
	if diags.HasError() {
		return nil, diags
	}

	store := client.chromePolicySchemas
	if store == nil {
		store = newChromePolicySchemaStore()
	}

	return &chromePolicySchemaCache{
		customer: client.Customer,
		service:  chromePolicySchemasService,
		store:    store,
	}, diags
}

// path returns the path of the schema named schemaName, which is also its key in the store.
func (c *chromePolicySchemaCache) path(schemaName string) string {
	return fmt.Sprintf("customers/%s/policySchemas/%s", c.customer, schemaName)
}

func (c *chromePolicySchemaCache) get(ctx context.Context, schemaName string) (*chromepolicy.GoogleChromePolicyVersionsV1PolicySchema, error) {
	if schemaDef, ok := c.store.lookup(c.path(schemaName)); ok {
		return schemaDef, nil
	}

	return c.refresh(ctx, schemaName)
}

// refresh gets the schema from the API, even if the store has it, and stores the result in place of any entry.
func (c *chromePolicySchemaCache) refresh(ctx context.Context, schemaName string) (*chromepolicy.GoogleChromePolicyVersionsV1PolicySchema, error) {
	path := c.path(schemaName)

	var schemaDef *chromepolicy.GoogleChromePolicyVersionsV1PolicySchema
	err := retryTimeDuration(ctx, time.Minute, func() error {
		var retryErr error

		schemaDef, retryErr = c.service.Get(path).Context(ctx).Do()
		return retryErr
	})
	if err != nil {
		return nil, err
	}

	c.store.set(path, schemaDef)
	return schemaDef, nil
}

// types returns the types of the policy schema. If the schema has no definition, it returns a diagnostic with the
// severity.
func (c *chromePolicySchemaCache) types(ctx context.Context, schemaName string, severity diag.Severity) (*chromePolicySchemaTypes, diag.Diagnostics) {
	schemaDef, err := c.get(ctx, schemaName)
	if err != nil {
		return nil, diag.FromErr(err)
	}

	if schemaDef == nil || schemaDef.Definition == nil || schemaDef.Definition.MessageType == nil {
		return nil, diag.Diagnostics{{
			Summary:  fmt.Sprintf("schema definition (%s) is empty", schemaName),
			Severity: severity,
		}}
	}

	types := newChromePolicySchemaTypes(schemaDef.Definition)
	// The message of a policy has the last part of the schema name, for example "Foo" for "chrome.users.Foo".
	types.policyMessage = schemaName[strings.LastIndex(schemaName, ".")+1:]
	return types, nil
}

// validateChromePolicies returns an error for each schema name that is set more than once, and for each value that does
// not match its schema.
func validateChromePolicies(ctx context.Context, policies []interface{}, schemas *chromePolicySchemaCache) diag.Diagnostics {
	var diags diag.Diagnostics
	seen := map[string]bool{}

	// Validate config against schemas
	for _, policy := range policies {
		schemaName := policy.(map[string]interface{})["schema_name"].(string)

		if seen[schemaName] {
			diags = append(diags, diag.Diagnostic{
				Summary:  fmt.Sprintf("policy schema (%s) is set more than once in policies", schemaName),
				Severity: diag.Error,
			})
			continue
		}
		seen[schemaName] = true

		types, typesDiags := schemas.types(ctx, schemaName, diag.Error)
		if len(typesDiags) > 0 {
			diags = append(diags, typesDiags...)
			continue
		}

		policyDef := policy.(map[string]interface{})["schema_values"].(map[string]interface{})

		for polKey, polJsonVal := range policyDef {
			field := types.topLevelField(polKey)
			if field == nil {
				diags = append(diags, diag.Diagnostic{
					Summary:  fmt.Sprintf("field name (%s) is not found in this schema definition (%s)", polKey, schemaName),
					Severity: diag.Error,
				})
				continue
			}

			// json.Number keeps large integers exact.
			var polVal interface{}
			err := unmarshalChromePolicyJSON([]byte(polJsonVal.(string)), &polVal)
			if err != nil {
				diags = append(diags, diag.FromErr(err)...)
				continue
			}

			// A value that is not a list, for a top-level repeated field, is written as a list with one element.
			if types.isRepeatedScalar(*field, polVal) {
				polVal = []interface{}{polVal}
			}

			if err := types.validateValue(polKey, *field, polVal); err != nil {
				diags = append(diags, diag.Diagnostic{
					Summary:  err.Error(),
					Severity: diag.Error,
				})
			}
		}
	}

	return diags
}

// chromePolicySchemaTypes finds the messages and enums of a policy schema definition by their fully qualified names.
//
// Values use the protobuf JSON mapping, which the API accepts: integers can be numbers or decimal strings, enums can
// be names or numbers, a field can be null, and a map field is a JSON object.
type chromePolicySchemaTypes struct {
	definition *chromepolicy.Proto2FileDescriptorProto
	messages   map[string]*chromepolicy.Proto2DescriptorProto
	enums      map[string]*chromepolicy.Proto2EnumDescriptorProto
	// policyMessage is the name of the top-level message of the policy. topLevelField looks in it first.
	policyMessage string
}

// chromePolicyField is a schema field and the fully qualified name of the message that contains it.
type chromePolicyField struct {
	field *chromepolicy.Proto2FieldDescriptorProto
	scope string
}

func newChromePolicySchemaTypes(definition *chromepolicy.Proto2FileDescriptorProto) *chromePolicySchemaTypes {
	types := &chromePolicySchemaTypes{
		definition: definition,
		messages:   map[string]*chromepolicy.Proto2DescriptorProto{},
		enums:      map[string]*chromepolicy.Proto2EnumDescriptorProto{},
	}
	types.addEnums(definition.Package, definition.EnumType)
	types.addMessages(definition.Package, definition.MessageType)
	return types
}

func (t *chromePolicySchemaTypes) addMessages(scope string, messages []*chromepolicy.Proto2DescriptorProto) {
	for _, message := range messages {
		if message == nil {
			continue
		}
		name := joinProtoName(scope, message.Name)
		t.messages[name] = message
		t.addEnums(name, message.EnumType)
		t.addMessages(name, message.NestedType)
	}
}

func (t *chromePolicySchemaTypes) addEnums(scope string, enums []*chromepolicy.Proto2EnumDescriptorProto) {
	for _, enum := range enums {
		if enum != nil {
			t.enums[joinProtoName(scope, enum.Name)] = enum
		}
	}
}

func joinProtoName(scope, name string) string {
	if scope == "" {
		return name
	}
	return scope + "." + name
}

// topLevelField returns the field named name in the top-level message of the policy (see policyMessage), or else the
// first field named name in the top-level messages of the definition, or nil.
// Validation, conversion, and comparison all use this field.
func (t *chromePolicySchemaTypes) topLevelField(name string) *chromePolicyField {
	for _, message := range t.definition.MessageType {
		if message == nil || t.policyMessage == "" || message.Name != t.policyMessage {
			continue
		}
		for _, field := range message.Field {
			if field != nil && field.Name == name {
				return &chromePolicyField{field: field, scope: joinProtoName(t.definition.Package, message.Name)}
			}
		}
	}
	for _, message := range t.definition.MessageType {
		if message == nil {
			continue
		}
		for _, field := range message.Field {
			if field != nil && field.Name == name {
				return &chromePolicyField{field: field, scope: joinProtoName(t.definition.Package, message.Name)}
			}
		}
	}
	return nil
}

// resolveName finds the fully qualified name of typeName, which a field of the message named scope refers to.
// It uses the protobuf scope rules: a name that starts with "." is fully qualified, and other names are looked up in
// scope, then in each parent scope. It returns false if exists is false for each name.
func resolveName(typeName, scope string, exists func(string) bool) (string, bool) {
	if strings.HasPrefix(typeName, ".") {
		name := strings.TrimPrefix(typeName, ".")
		return name, exists(name)
	}

	for {
		name := joinProtoName(scope, typeName)
		if exists(name) {
			return name, true
		}
		if scope == "" {
			return "", false
		}
		if i := strings.LastIndex(scope, "."); i >= 0 {
			scope = scope[:i]
		} else {
			scope = ""
		}
	}
}

// resolveMessage returns the fully qualified name and the message that field refers to, or nil.
func (t *chromePolicySchemaTypes) resolveMessage(field chromePolicyField) (string, *chromepolicy.Proto2DescriptorProto) {
	name, ok := resolveName(field.field.TypeName, field.scope, func(name string) bool {
		_, ok := t.messages[name]
		return ok
	})
	if !ok {
		return "", nil
	}
	return name, t.messages[name]
}

// resolveEnum returns the enum that field refers to, or nil.
func (t *chromePolicySchemaTypes) resolveEnum(field chromePolicyField) *chromepolicy.Proto2EnumDescriptorProto {
	name, ok := resolveName(field.field.TypeName, field.scope, func(name string) bool {
		_, ok := t.enums[name]
		return ok
	})
	if !ok {
		return nil
	}
	return t.enums[name]
}

// kind returns the type of field. If a field has a type name but no type, the type name can refer to a message or to
// an enum. If the type name is not in the definition, kind returns "".
func (t *chromePolicySchemaTypes) kind(field chromePolicyField) string {
	if field.field.Type != "" || field.field.TypeName == "" {
		return field.field.Type
	}
	if _, message := t.resolveMessage(field); message != nil {
		return "TYPE_MESSAGE"
	}
	if t.resolveEnum(field) != nil {
		return "TYPE_ENUM"
	}
	return ""
}

// mapEntry returns the key and value fields if field is a protobuf map field: a repeated field of a nested message
// that has the map entry name of the field (see mapEntryName) and has only a key field and a value field.
func (t *chromePolicySchemaTypes) mapEntry(field chromePolicyField) (key, value chromePolicyField, ok bool) {
	if field.field.Label != "LABEL_REPEATED" || t.kind(field) != "TYPE_MESSAGE" {
		return key, value, false
	}
	scope, message := t.resolveMessage(field)
	if message == nil || message.Name != mapEntryName(field.field.Name) || len(message.Field) != 2 {
		return key, value, false
	}
	keyField, valueField := messageField(message, "key"), messageField(message, "value")
	if keyField == nil || valueField == nil {
		return key, value, false
	}
	return chromePolicyField{field: keyField, scope: scope}, chromePolicyField{field: valueField, scope: scope}, true
}

// mapEntryName returns the name of the entry message that protoc makes for the map field fieldName: the field name in
// CamelCase, with the suffix "Entry". For example, "labels" becomes "LabelsEntry", and "foo_bar" becomes "FooBarEntry".
func mapEntryName(fieldName string) string {
	var b strings.Builder
	capNext := true
	for _, c := range fieldName {
		switch {
		case c == '_':
			capNext = true
		case capNext:
			if 'a' <= c && c <= 'z' {
				c -= 'a' - 'A'
			}
			b.WriteRune(c)
			capNext = false
		default:
			b.WriteRune(c)
		}
	}
	return b.String() + "Entry"
}

// chromePolicyMapEntries returns the entries of a value of a map field, by key. The value can be a JSON object, or a
// list of objects that each have a key field and an optional value field. It returns false for other values.
func chromePolicyMapEntries(value interface{}) (map[string]interface{}, bool) {
	switch v := value.(type) {
	case map[string]interface{}:
		return v, true
	case []interface{}:
		entries := map[string]interface{}{}
		for _, elem := range v {
			entry, ok := elem.(map[string]interface{})
			if !ok || len(entry) > 2 {
				return nil, false
			}
			if _, ok := entry["value"]; len(entry) == 2 && !ok {
				return nil, false
			}
			var key string
			switch k := entry["key"].(type) {
			case string:
				key = k
			case json.Number:
				key = k.String()
			case float64:
				key = strconv.FormatFloat(k, 'f', -1, 64)
			case bool:
				key = strconv.FormatBool(k)
			default:
				return nil, false
			}
			entries[key] = entry["value"]
		}
		return entries, true
	}
	return nil, false
}

// isRepeatedScalar reports whether value is one value, not a list, for a repeated field that is not a map field.
func (t *chromePolicySchemaTypes) isRepeatedScalar(field chromePolicyField, value interface{}) bool {
	if field.field.Label != "LABEL_REPEATED" || value == nil {
		return false
	}
	if _, _, ok := t.mapEntry(field); ok {
		return false
	}
	_, isList := value.([]interface{})
	return !isList
}

// messageField returns the field of message whose name or JSON name is key, or nil.
func messageField(message *chromepolicy.Proto2DescriptorProto, key string) *chromepolicy.Proto2FieldDescriptorProto {
	for _, field := range message.Field {
		if field != nil && (field.Name == key || field.JsonName == key) {
			return field
		}
	}
	return nil
}

// validateValue returns an error if value does not match field. The error uses path as the name of the value.
func (t *chromePolicySchemaTypes) validateValue(path string, field chromePolicyField, value interface{}) error {
	if value == nil {
		return nil
	}

	if key, valueField, ok := t.mapEntry(field); ok {
		entries, ok := chromePolicyMapEntries(value)
		if !ok {
			return fmt.Errorf("value provided for %s is of incorrect type (expected type: map, as a JSON object or a list of key and value objects)", path)
		}
		for k, v := range entries {
			if !validatePolicyFieldValueType(key.field.Type, k) && !(key.field.Type == "TYPE_BOOL" && (k == "true" || k == "false")) {
				return fmt.Errorf("key %q provided for %s is of incorrect type (expected type: %s)", k, path, key.field.Type)
			}
			if err := t.validateSingleValue(fmt.Sprintf("%s[%q]", path, k), valueField, v); err != nil {
				return err
			}
		}
		return nil
	}

	if field.field.Label != "LABEL_REPEATED" {
		return t.validateSingleValue(path, field, value)
	}

	list, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("value provided for %s is of incorrect type (expected type: repeated %s)", path, field.field.Type)
	}
	for i, elem := range list {
		if err := t.validateSingleValue(fmt.Sprintf("%s[%d]", path, i), field, elem); err != nil {
			return err
		}
	}
	return nil
}

func (t *chromePolicySchemaTypes) validateSingleValue(path string, field chromePolicyField, value interface{}) error {
	if value == nil {
		return nil
	}

	incorrectType := fmt.Errorf("value provided for %s is of incorrect type (expected type: %s)", path, field.field.Type)
	kind := t.kind(field)

	switch {
	case kind == "":
		log.Printf("[DEBUG] Not validating %s: type %s is not in the policy schema definition", path, field.field.TypeName)
		return nil
	case kind != "TYPE_MESSAGE" || field.field.TypeName == "":
		if !validatePolicyFieldValueType(kind, value) {
			return incorrectType
		}
		return nil
	}

	scope, message := t.resolveMessage(field)
	if message == nil {
		// For example, a well-known type such as google.protobuf.Duration, which is not an object in JSON.
		log.Printf("[DEBUG] Not validating %s: type %s is not in the policy schema definition", path, field.field.TypeName)
		return nil
	}

	values, ok := value.(map[string]interface{})
	if !ok {
		return incorrectType
	}
	for key, v := range values {
		nested := messageField(message, key)
		if nested == nil {
			log.Printf("[DEBUG] Not validating %s.%s: the field is not in type %s", path, key, scope)
			continue
		}
		if err := t.validateValue(path+"."+key, chromePolicyField{field: nested, scope: scope}, v); err != nil {
			return err
		}
	}
	return nil
}

// valuesMatch reports whether got has each value in want (JSON encoded) after normalization.
func (t *chromePolicySchemaTypes) valuesMatch(want map[string]interface{}, got map[string]interface{}) bool {
	for k, wantJson := range want {
		var wantVal interface{}
		if err := unmarshalChromePolicyJSON([]byte(wantJson.(string)), &wantVal); err != nil {
			return false
		}
		if !t.equivalent(k, wantVal, got[k]) {
			return false
		}
	}
	return true
}

// equivalent reports whether a and b are the same value of the top-level field key.
func (t *chromePolicySchemaTypes) equivalent(key string, a, b interface{}) bool {
	field := t.topLevelField(key)
	if field == nil {
		return reflect.DeepEqual(a, b)
	}
	a, b = t.normalize(*field, a), t.normalize(*field, b)
	if t.isDefault(*field, a) && t.isDefault(*field, b) {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// normalize returns a canonical form of a value of field, so that forms of the same value are equal: integers become
// decimal strings, numbers become float64 (rounded to float32 for TYPE_FLOAT), enum numbers become names, and fields
// with default values are removed from nested messages.
func (t *chromePolicySchemaTypes) normalize(field chromePolicyField, value interface{}) interface{} {
	if value == nil {
		return nil
	}

	if _, valueField, ok := t.mapEntry(field); ok {
		// The list form becomes the object form.
		entries, ok := chromePolicyMapEntries(value)
		if !ok {
			return value
		}
		out := map[string]interface{}{}
		for k, v := range entries {
			out[k] = t.normalizeSingle(valueField, v)
		}
		return out
	}

	if field.field.Label != "LABEL_REPEATED" {
		return t.normalizeSingle(field, value)
	}

	// One value is the same as a list with that value.
	list, ok := value.([]interface{})
	if !ok {
		list = []interface{}{value}
	}

	out := make([]interface{}, len(list))
	for i, elem := range list {
		out[i] = t.normalizeSingle(field, elem)
	}
	return out
}

func (t *chromePolicySchemaTypes) normalizeSingle(field chromePolicyField, value interface{}) interface{} {
	if value == nil {
		return nil
	}

	switch kind := t.kind(field); kind {
	case "TYPE_MESSAGE":
		scope, message := t.resolveMessage(field)
		values, ok := value.(map[string]interface{})
		if message == nil || !ok {
			return value
		}
		out := map[string]interface{}{}
		for key, v := range values {
			nested := messageField(message, key)
			if nested == nil {
				out[key] = v
				continue
			}
			nestedField := chromePolicyField{field: nested, scope: scope}
			if normalized := t.normalize(nestedField, v); !t.isDefault(nestedField, normalized) {
				out[nested.Name] = normalized
			}
		}
		return out
	case "TYPE_ENUM":
		number, ok := chromePolicyInteger("TYPE_INT32", value)
		if !ok {
			return value
		}
		if enum := t.resolveEnum(field); enum != nil {
			for _, v := range enum.Value {
				if v != nil && strconv.FormatInt(v.Number, 10) == number {
					return v.Name
				}
			}
		}
		return number
	case "TYPE_BOOL":
		switch value {
		case "true":
			return true
		case "false":
			return false
		}
		return value
	case "TYPE_FLOAT", "TYPE_DOUBLE":
		n, ok := chromePolicyNumber(value)
		if !ok {
			return value
		}
		switch {
		case math.IsNaN(n):
			// NaN is not equal to NaN.
			return "NaN"
		case math.IsInf(n, 1):
			return "Infinity"
		case math.IsInf(n, -1):
			return "-Infinity"
		case kind == "TYPE_FLOAT":
			return float64(float32(n))
		}
		return n
	default:
		if chromePolicyIntegerTypes[kind] {
			if number, ok := chromePolicyInteger(kind, value); ok {
				return number
			}
		}
		if n, ok := value.(json.Number); ok {
			return n.String()
		}
		return value
	}
}

// isDefault reports whether a normalized value of field is the protobuf default value, which the API can omit.
func (t *chromePolicySchemaTypes) isDefault(field chromePolicyField, normalized interface{}) bool {
	switch v := normalized.(type) {
	case nil:
		return true
	case bool:
		return !v
	case float64:
		return v == 0
	case []interface{}:
		return len(v) == 0
	case map[string]interface{}:
		return len(v) == 0
	case string:
		kind := t.kind(field)
		if chromePolicyIntegerTypes[kind] {
			return v == "0"
		}
		if kind == "TYPE_ENUM" {
			if enum := t.resolveEnum(field); enum != nil {
				for _, value := range enum.Value {
					if value != nil && value.Number == 0 {
						return v == value.Name
					}
				}
			}
			return v == "0"
		}
		return v == ""
	}
	return false
}

var chromePolicyIntegerTypes = map[string]bool{
	"TYPE_INT64": true, "TYPE_SFIXED64": true, "TYPE_SINT64": true, "TYPE_UINT64": true, "TYPE_FIXED64": true,
	"TYPE_INT32": true, "TYPE_SFIXED32": true, "TYPE_SINT32": true, "TYPE_UINT32": true, "TYPE_FIXED32": true,
}

// chromePolicyInteger returns value as a decimal string if it is an integer in the range of fieldType.
// The value can be a JSON number or a decimal string.
func chromePolicyInteger(fieldType string, value interface{}) (string, bool) {
	var s string
	switch v := value.(type) {
	case string:
		s = v
	case json.Number:
		s = v.String()
	case float64:
		if v != math.Trunc(v) || math.IsInf(v, 0) {
			return "", false
		}
		s = strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return "", false
	}

	// An integer can also have a fraction of zero or an exponent, for example "1e3".
	if !chromePolicyIntegerPattern.MatchString(s) {
		if !chromePolicyDecimalPattern.MatchString(s) {
			return "", false
		}
		n, err := strconv.ParseFloat(s, 64)
		if err != nil || n != math.Trunc(n) {
			return "", false
		}
		s = strconv.FormatFloat(n, 'f', -1, 64)
	}

	bits := 64
	if strings.HasSuffix(fieldType, "32") {
		bits = 32
	}

	if fieldType == "TYPE_UINT64" || fieldType == "TYPE_FIXED64" || fieldType == "TYPE_UINT32" || fieldType == "TYPE_FIXED32" {
		n, err := strconv.ParseUint(s, 10, bits)
		if err != nil {
			return "", false
		}
		return strconv.FormatUint(n, 10), true
	}

	n, err := strconv.ParseInt(s, 10, bits)
	if err != nil {
		return "", false
	}
	return strconv.FormatInt(n, 10), true
}

var (
	chromePolicyIntegerPattern = regexp.MustCompile(`^-?[0-9]+$`)
	chromePolicyDecimalPattern = regexp.MustCompile(`^-?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+)?$`)
)

// chromePolicyNumber returns value as a float64 if it is a JSON number, a decimal string, or one of the strings
// "NaN", "Infinity", and "-Infinity".
func chromePolicyNumber(value interface{}) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case json.Number:
		n, err := v.Float64()
		return n, err == nil
	case string:
		switch v {
		case "NaN":
			return math.NaN(), true
		case "Infinity":
			return math.Inf(1), true
		case "-Infinity":
			return math.Inf(-1), true
		}
		if !chromePolicyDecimalPattern.MatchString(v) {
			return 0, false
		}
		n, err := strconv.ParseFloat(v, 64)
		return n, err == nil
	}
	return 0, false
}

// convertValue converts a value of field from the API to the JSON types of the schema.
// The API returns numbers as strings, also in lists, maps, and nested messages.
func (t *chromePolicySchemaTypes) convertValue(field chromePolicyField, value interface{}) (interface{}, error) {
	if _, valueField, ok := t.mapEntry(field); ok {
		entries, ok := value.(map[string]interface{})
		if !ok {
			return value, nil
		}
		out := map[string]interface{}{}
		for k, v := range entries {
			converted, err := t.convertSingleValue(valueField, v)
			if err != nil {
				return nil, err
			}
			out[k] = converted
		}
		return out, nil
	}

	list, ok := value.([]interface{})
	if field.field.Label != "LABEL_REPEATED" || !ok {
		return t.convertSingleValue(field, value)
	}

	out := make([]interface{}, len(list))
	for i, elem := range list {
		converted, err := t.convertSingleValue(field, elem)
		if err != nil {
			return nil, err
		}
		out[i] = converted
	}
	return out, nil
}

func (t *chromePolicySchemaTypes) convertSingleValue(field chromePolicyField, value interface{}) (interface{}, error) {
	kind := t.kind(field)
	if kind != "TYPE_MESSAGE" {
		return convertPolicyFieldValueType(kind, value)
	}

	scope, message := t.resolveMessage(field)
	values, ok := value.(map[string]interface{})
	if message == nil || !ok {
		return value, nil
	}

	out := map[string]interface{}{}
	for key, v := range values {
		nested := messageField(message, key)
		if nested == nil {
			out[key] = v
			continue
		}
		converted, err := t.convertValue(chromePolicyField{field: nested, scope: scope}, v)
		if err != nil {
			return nil, err
		}
		out[key] = converted
	}
	return out, nil
}

// This will take a value and validate whether the type is correct
func validatePolicyFieldValueType(fieldType string, fieldValue interface{}) bool {
	valid := false

	// JSON numbers are float64 or json.Number. A json.Number is not a string value.
	switch fieldType {
	case "TYPE_BOOL":
		// true or false, also as a string.
		_, isBool := fieldValue.(bool)
		valid = isBool || fieldValue == "true" || fieldValue == "false"
	// A number can also be a decimal string, or "NaN", "Infinity", or "-Infinity".
	case "TYPE_FLOAT":
		n, ok := chromePolicyNumber(fieldValue)
		valid = ok && (math.IsNaN(n) || math.IsInf(n, 0) || math.Abs(n) <= math.MaxFloat32)
	case "TYPE_DOUBLE":
		_, valid = chromePolicyNumber(fieldValue)
	case "TYPE_INT64", "TYPE_SFIXED64", "TYPE_SINT64", "TYPE_UINT64", "TYPE_FIXED64",
		"TYPE_INT32", "TYPE_SFIXED32", "TYPE_SINT32", "TYPE_UINT32", "TYPE_FIXED32":
		// A number or a decimal string, in the range of the type.
		_, valid = chromePolicyInteger(fieldType, fieldValue)
	case "TYPE_MESSAGE":
		// validateValue checks the fields of nested messages.
		valid = reflect.ValueOf(fieldValue).Kind() == reflect.Map
	case "TYPE_ENUM":
		// A name, or a number.
		_, isName := fieldValue.(string)
		_, isFloat := fieldValue.(float64)
		_, isNumber := fieldValue.(json.Number)
		_, isInteger := chromePolicyInteger("TYPE_INT32", fieldValue)
		valid = isName || ((isFloat || isNumber) && isInteger)
	case "TYPE_STRING":
		fallthrough
	default:
		_, valid = fieldValue.(string)
	}

	return valid
}

// The API returns numeric values as strings. This will convert it to the appropriate type
func convertPolicyFieldValueType(fieldType string, fieldValue interface{}) (interface{}, error) {
	// A JSON number is already a number, and json.Number keeps its exact value when it is encoded again.
	if n, ok := fieldValue.(json.Number); ok {
		return n, nil
	}

	// If it's not of type string, then we'll assume it's the right type
	if reflect.ValueOf(fieldValue).Kind() != reflect.String {
		return fieldValue, nil
	}

	var err error
	var value interface{}

	switch fieldType {
	case "TYPE_BOOL":
		value, err = strconv.ParseBool(fieldValue.(string))
	case "TYPE_FLOAT", "TYPE_DOUBLE":
		switch fieldValue.(string) {
		case "NaN", "Infinity", "-Infinity":
			// JSON has no number for these values.
			value = fieldValue
		default:
			value, err = strconv.ParseFloat(fieldValue.(string), 64)
		}
	case "TYPE_INT64", "TYPE_SFIXED64", "TYPE_SINT64":
		value, err = strconv.ParseInt(fieldValue.(string), 10, 64)
	case "TYPE_UINT64", "TYPE_FIXED64":
		value, err = strconv.ParseUint(fieldValue.(string), 10, 64)
	case "TYPE_INT32", "TYPE_SFIXED32", "TYPE_SINT32":
		value, err = strconv.ParseInt(fieldValue.(string), 10, 32)
	case "TYPE_UINT32", "TYPE_FIXED32":
		value, err = strconv.ParseUint(fieldValue.(string), 10, 32)
	case "TYPE_ENUM":
		fallthrough
	case "TYPE_MESSAGE":
		fallthrough
	case "TYPE_STRING":
		fallthrough
	default:
		value = fieldValue
	}

	return value, err
}

func expandChromePoliciesValues(policies []interface{}) ([]*chromepolicy.GoogleChromePolicyVersionsV1PolicyValue, diag.Diagnostics) {
	var diags diag.Diagnostics
	result := []*chromepolicy.GoogleChromePolicyVersionsV1PolicyValue{}

	for _, p := range policies {
		policy := p.(map[string]interface{})

		schemaName := policy["schema_name"].(string)
		schemaValues := policy["schema_values"].(map[string]interface{})

		policyValuesObj := map[string]interface{}{}

		for k, v := range schemaValues {
			// json.Number keeps large integers exact.
			var polVal interface{}
			err := unmarshalChromePolicyJSON([]byte(v.(string)), &polVal)
			if err != nil {
				return nil, diag.FromErr(err)
			}

			policyValuesObj[k] = polVal
		}

		// create the json object and assign to the schema
		schemaValuesJson, err := json.Marshal(policyValuesObj)
		if err != nil {
			return nil, diag.FromErr(err)
		}

		policyObj := chromepolicy.GoogleChromePolicyVersionsV1PolicyValue{
			PolicySchema: schemaName,
			Value:        schemaValuesJson,
		}

		result = append(result, &policyObj)
	}

	return result, diags
}

// flattenChromePolicies converts the policy values from the API to the form in state. prior has the schema values in
// state by schema name. A prior value that is the same value as the API value is kept, so that the form of a value in
// the configuration (for example, an integer as a string) does not cause a change.
//
// forms has the recorded forms of values, by schema name. If the API value is a recorded form, and the values in state
// are the configured values that produced it, the values in state are kept. The returned set has the schema names of
// the forms that were used.
func flattenChromePolicies(ctx context.Context, policiesObj []*chromepolicy.GoogleChromePolicyVersionsV1PolicyValue, schemas *chromePolicySchemaCache, prior map[string]map[string]interface{}, forms map[string]chromePolicyAppliedForm) ([]map[string]interface{}, map[string]bool, diag.Diagnostics) {
	var policies []map[string]interface{}
	keptForms := map[string]bool{}

	for _, polObj := range policiesObj {
		priorValues := prior[polObj.PolicySchema]
		if form, ok := forms[polObj.PolicySchema]; ok && priorValues != nil &&
			form.ConfigHash == chromePolicyConfigHash(priorValues) && sameChromePolicyJSON(form.Values, polObj.Value) {
			keptForms[polObj.PolicySchema] = true
			schemaValues := map[string]interface{}{}
			for k, v := range priorValues {
				schemaValues[k] = v
			}
			policies = append(policies, map[string]interface{}{
				"schema_name":   polObj.PolicySchema,
				"schema_values": schemaValues,
			})
			continue
		}

		types, diags := schemas.types(ctx, polObj.PolicySchema, diag.Warning)
		if len(diags) > 0 {
			return nil, nil, diags
		}

		var schemaValuesObj map[string]interface{}
		// Numbers are json.Number, so that an int64 that the API returns as a JSON number stays exact.
		if err := unmarshalChromePolicyJSON(polObj.Value, &schemaValuesObj); err != nil {
			return nil, nil, diag.FromErr(err)
		}

		// The same values with exact numbers, to compare with the prior values.
		var exactValues map[string]interface{}
		if err := unmarshalChromePolicyJSON(polObj.Value, &exactValues); err != nil {
			return nil, nil, diag.FromErr(err)
		}

		samePrior := func(k string) (string, bool) {
			priorJson, ok := priorValues[k].(string)
			if !ok {
				return "", false
			}
			var priorVal interface{}
			if err := unmarshalChromePolicyJSON([]byte(priorJson), &priorVal); err != nil {
				return "", false
			}
			return priorJson, types.equivalent(k, priorVal, exactValues[k])
		}

		schemaValues := map[string]interface{}{}
		for k, v := range schemaValuesObj {
			field := types.topLevelField(k)
			if field == nil {
				return nil, nil, diag.Diagnostics{{
					Summary:  fmt.Sprintf("field name (%s) is not found in this schema definition (%s)", k, polObj.PolicySchema),
					Severity: diag.Warning,
				}}
			}

			if priorJson, ok := samePrior(k); ok {
				schemaValues[k] = priorJson
				continue
			}

			val, err := types.convertValue(*field, v)
			if err != nil {
				return nil, nil, diag.FromErr(err)
			}

			jsonVal, err := json.Marshal(val)
			if err != nil {
				return nil, nil, diag.FromErr(err)
			}
			schemaValues[k] = string(jsonVal)
		}

		// The API can omit a value that is the default value. Keep such a value from state.
		for k := range priorValues {
			if _, ok := schemaValues[k]; ok {
				continue
			}
			if priorJson, ok := samePrior(k); ok {
				schemaValues[k] = priorJson
			}
		}

		policies = append(policies, map[string]interface{}{
			"schema_name":   polObj.PolicySchema,
			"schema_values": schemaValues,
		})
	}

	return policies, keptForms, nil
}
