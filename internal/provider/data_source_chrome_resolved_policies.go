// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"google.golang.org/api/chromepolicy/v1"
)

func dataSourceChromeResolvedPolicies() *schema.Resource {
	return &schema.Resource{
		Description: "Resolved Chrome Policies data source in the Terraform Googleworkspace provider. It reads the " +
			"effective (resolved) values of the Chrome policies for an org unit, also the values that the org unit " +
			"inherits from a parent org unit. A policy that is not set in the org unit or in a parent org unit is not " +
			"in the result, or is in the result with an empty `source_org_unit_id` (the default value for the " +
			"customer). Use it in a `check` block to verify a policy value or that a policy is not set. " +
			"The data source uses the `policies:resolve`, `policySchemas.get`, and `policySchemas.list` methods, which need the " +
			"`https://www.googleapis.com/auth/chrome.management.policy` client scope (in the default scopes) or the " +
			"`https://www.googleapis.com/auth/chrome.management.policy.readonly` client scope.",

		ReadContext: dataSourceChromeResolvedPoliciesRead,

		Schema: map[string]*schema.Schema{
			"org_unit_id": {
				Description: "The ID of the org unit for which the policies are resolved, not its path. The `id:` " +
					"prefix is optional.",
				Type:     schema.TypeString,
				Required: true,
				ValidateFunc: validation.All(
					validation.StringIsNotEmpty,
					validation.StringDoesNotContainAny("/"),
				),
			},
			"policy_schema_filter": {
				Description: "The schema name of one policy (for example `chrome.users.PasswordManager`), or a filter " +
					"with a wildcard in the leaf part of the name (for example `chrome.users.*` or " +
					"`chrome.users.apps.*`). A wildcard in the namespace (for example `chrome.*`) is not valid.",
				Type:         schema.TypeString,
				Required:     true,
				ValidateFunc: validation.StringIsNotEmpty,
			},
			"additional_target_keys": {
				Description: "Additional target keys for policies that need them, for example `app_id` for " +
					"`chrome.users.apps.*` policies. The `additional_target_key_names` attribute of " +
					"`googleworkspace_chrome_policy_schema` gives the key names.",
				Type:     schema.TypeMap,
				Optional: true,
				Elem:     &schema.Schema{Type: schema.TypeString},
			},
			"resolved_policies": {
				Description: "The resolved policies, in the order that the API returns them. Each element has these " +
					"attributes: `schema_name`; `value`, the resolved value as a JSON object of the schema fields in " +
					"canonical form (each value has the type of its schema field, for example integers are numbers, " +
					"not strings); `schema_values`, each field value JSON encoded in the same canonical form; " +
					"`source_org_unit_id`, the ID (without the `id:` prefix) of the org unit in which the policy is " +
					"set, empty if the API returns no source (the value is the default value for the customer); `inherited`, true if the policy is not set in " +
					"`org_unit_id` itself; and `additional_target_keys`, for example the `app_id` of an app policy. " +
					"The API can omit a field that has its default value, so a field can be absent from `value` and " +
					"`schema_values`. Read a field with `lookup(p.schema_values, \"field\", null)` or " +
					"`try(jsondecode(p.value).field, null)`, and compare decoded values (`jsondecode`), not JSON " +
					"strings. If a value cannot be converted (for example, the schema cannot be read or does not " +
					"describe a field), `value` and `schema_values` have the raw API value of that policy, with a " +
					"warning.",
				Type:     schema.TypeList,
				Computed: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"schema_name": {
							Description: "The full qualified name of the policy schema.",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"value": {
							Description: "The resolved value of the policy, as a JSON object of the schema fields " +
								"in canonical form. A field at its default value can be absent.",
							Type:     schema.TypeString,
							Computed: true,
						},
						"schema_values": {
							Description: "The resolved value of each field of the policy, JSON encoded in " +
								"canonical form. A field at its default value can be absent.",
							Type:     schema.TypeMap,
							Computed: true,
							Elem:     &schema.Schema{Type: schema.TypeString},
						},
						"source_org_unit_id": {
							Description: "The ID (without the `id:` prefix) of the org unit in which the policy is " +
								"set. If it is not `org_unit_id`, the org unit inherits the value. Empty if the API " +
								"returns no source: the value is the default value for the customer, and the policy " +
								"is not set in any org unit.",
							Type:     schema.TypeString,
							Computed: true,
						},
						"inherited": {
							Description: "True if the policy is not set in `org_unit_id` itself, and the value comes " +
								"from a parent org unit or from the customer default.",
							Type:     schema.TypeBool,
							Computed: true,
						},
						"additional_target_keys": {
							Description: "The additional target keys of the resolved policy, for example the `app_id` " +
								"of an app policy.",
							Type:     schema.TypeMap,
							Computed: true,
							Elem:     &schema.Schema{Type: schema.TypeString},
						},
					},
				},
			},
		},
	}
}

func dataSourceChromeResolvedPoliciesRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*apiClient)

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

	orgUnitId := strings.TrimPrefix(d.Get("org_unit_id").(string), "id:")
	filter := d.Get("policy_schema_filter").(string)

	additionalTargetKeys := map[string]string{}
	for k, v := range d.Get("additional_target_keys").(map[string]interface{}) {
		additionalTargetKeys[k] = v.(string)
	}

	targetKey := &chromepolicy.GoogleChromePolicyVersionsV1PolicyTargetKey{
		TargetResource: "orgunits/" + orgUnitId,
	}
	if len(additionalTargetKeys) > 0 {
		targetKey.AdditionalTargetKeys = additionalTargetKeys
	}

	var resolvedPolicies []*chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy
	pageToken := ""
	for {
		var resp *chromepolicy.GoogleChromePolicyVersionsV1ResolveResponse
		err := retryTimeDuration(ctx, time.Minute, func() error {
			var retryErr error
			resp, retryErr = chromePoliciesService.Resolve(fmt.Sprintf("customers/%s", client.Customer), &chromepolicy.GoogleChromePolicyVersionsV1ResolveRequest{
				PolicySchemaFilter: filter,
				PolicyTargetKey:    targetKey,
				PageSize:           1000,
				PageToken:          pageToken,
			}).Context(ctx).Do()
			return retryErr
		})
		if err != nil {
			return append(diags, diag.FromErr(err)...)
		}

		resolvedPolicies = append(resolvedPolicies, resp.ResolvedPolicies...)
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}

	if strings.Contains(filter, "*") {
		prefetchChromePolicySchemas(ctx, schemas, resolvedPolicies)
	}

	// rawValues has, by schema name, the resolved policies that have raw API values, for one warning per schema.
	rawValues := map[string]*chromeRawValuePolicies{}
	var rawValueSchemas []string
	// schemaErrs has the schemas that could not be read, so that each schema is read at most once more.
	schemaErrs := map[string]error{}

	policies := make([]interface{}, 0, len(resolvedPolicies))
	for _, resolved := range resolvedPolicies {
		if resolved == nil || resolved.Value == nil {
			continue
		}

		schemaValues, cause, err := flattenResolvedChromePolicyValues(ctx, resolved.Value, schemas, schemaErrs)
		if err != nil {
			return append(diags, diag.FromErr(err)...)
		}
		if cause != "" {
			raw, ok := rawValues[resolved.Value.PolicySchema]
			if !ok {
				raw = &chromeRawValuePolicies{}
				rawValues[resolved.Value.PolicySchema] = raw
				rawValueSchemas = append(rawValueSchemas, resolved.Value.PolicySchema)
			}
			raw.add(resolved.TargetKey, cause)
		}

		fields := map[string]json.RawMessage{}
		for k, v := range schemaValues {
			fields[k] = json.RawMessage(v.(string))
		}
		value, err := json.Marshal(fields)
		if err != nil {
			return append(diags, diag.FromErr(err)...)
		}

		sourceOrgUnitId := ""
		if resolved.SourceKey != nil {
			sourceOrgUnitId = strings.TrimPrefix(resolved.SourceKey.TargetResource, "orgunits/")
		}

		keys := map[string]interface{}{}
		if resolved.TargetKey != nil {
			for k, v := range resolved.TargetKey.AdditionalTargetKeys {
				keys[k] = v
			}
		}

		policies = append(policies, map[string]interface{}{
			"schema_name":            resolved.Value.PolicySchema,
			"value":                  string(value),
			"schema_values":          schemaValues,
			"source_org_unit_id":     sourceOrgUnitId,
			"inherited":              !chromePolicyTarget{id: orgUnitId}.setOn(resolved),
			"additional_target_keys": keys,
		})
	}

	for _, schemaName := range rawValueSchemas {
		raw := rawValues[schemaName]
		diags = append(diags, diag.Diagnostic{
			Severity: diag.Warning,
			Summary:  fmt.Sprintf("Chrome policy %s has raw API values", schemaName),
			Detail: fmt.Sprintf("The data source could not convert the value of Chrome policy %s to canonical form, "+
				"and returned the raw API values for these resolved policies: %s. For example, numbers can be "+
				"strings. Cause: %s", schemaName, strings.Join(raw.targets, "; "), strings.Join(raw.causes, "; ")),
		})
	}

	if err := d.Set("resolved_policies", policies); err != nil {
		return append(diags, diag.FromErr(err)...)
	}

	d.SetId(chromeResolvedPoliciesId(orgUnitId, filter, additionalTargetKeys))
	return diags
}

// chromeRawValuePolicies has the resolved policies of one schema that have raw API values, and the causes.
type chromeRawValuePolicies struct {
	targets []string
	causes  []string
}

func (r *chromeRawValuePolicies) add(targetKey *chromepolicy.GoogleChromePolicyVersionsV1PolicyTargetKey, cause string) {
	target := "no additional target keys"
	if targetKey != nil && len(targetKey.AdditionalTargetKeys) > 0 {
		target = joinChromeTargetKeys(targetKey.AdditionalTargetKeys)
	}
	r.targets = append(r.targets, target)
	for _, c := range r.causes {
		if c == cause {
			return
		}
	}
	r.causes = append(r.causes, cause)
}

// flattenResolvedChromePolicyValues returns the value of each field of a resolved policy, JSON encoded in canonical
// form: the conversion that the googleworkspace_chrome_policy resource uses for its state. If the value cannot be
// converted (for example, the schema cannot be read, or it does not describe a field), it returns the raw API values
// and the cause. Only a value that is not a JSON object is an error.
//
// schemaErrs has the errors of the schemas that could not be read, by schema name. A schema in schemaErrs is not read
// again, and a schema that cannot be read is added to it.
func flattenResolvedChromePolicyValues(ctx context.Context, value *chromepolicy.GoogleChromePolicyVersionsV1PolicyValue, schemas *chromePolicySchemaCache, schemaErrs map[string]error) (map[string]interface{}, string, error) {
	if len(value.Value) == 0 {
		return map[string]interface{}{}, "", nil
	}

	var cause string
	if err, ok := schemaErrs[value.PolicySchema]; ok {
		cause = fmt.Sprintf("cannot read the schema: %s", err)
	} else if _, err := schemas.get(ctx, value.PolicySchema); err != nil {
		schemaErrs[value.PolicySchema] = err
		cause = fmt.Sprintf("cannot read the schema: %s", err)
	} else {
		policies, _, flattenDiags := flattenChromePolicies(ctx, []*chromepolicy.GoogleChromePolicyVersionsV1PolicyValue{value}, schemas, nil, nil)
		if len(flattenDiags) == 0 && len(policies) == 1 {
			return policies[0]["schema_values"].(map[string]interface{}), "", nil
		}
		summaries := make([]string, 0, len(flattenDiags))
		for _, d := range flattenDiags {
			summaries = append(summaries, d.Summary)
		}
		cause = strings.Join(summaries, "; ")
	}

	var raw map[string]interface{}
	if err := unmarshalChromePolicyJSON(value.Value, &raw); err != nil {
		return nil, "", fmt.Errorf("chrome policy %s: the API value is not a JSON object: %w", value.PolicySchema, err)
	}
	schemaValues := map[string]interface{}{}
	for k, v := range raw {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, "", err
		}
		schemaValues[k] = string(b)
	}
	return schemaValues, cause, nil
}

// chromePolicySchemaPrefetchMin is the number of schemas that are not in the store, from which
// prefetchChromePolicySchemas lists the schemas. For fewer schemas, one request per schema costs less than a listing
// of all schemas.
const chromePolicySchemaPrefetchMin = 15

// prefetchChromePolicySchemas adds the schemas of the resolved policies to the schema store of the cache (the store that
// the provider shares between operations) from a listing of all policy schemas, in pages of 1000. For a wildcard
// filter, this replaces one request per schema with a few requests. It does nothing if fewer than
// chromePolicySchemaPrefetchMin schemas are not in the store. A listed schema without a definition with message types
// is not added to the store, so that it is read with a request of its own. A schema that the store already has is not
// replaced.
// The listing has no filter, because the API does not document a filter for a schema name prefix. A schema that the
// listing does not return is read later with a request of its own, and so is each schema if the listing fails.
func prefetchChromePolicySchemas(ctx context.Context, schemas *chromePolicySchemaCache, resolvedPolicies []*chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy) {
	needed := map[string]bool{}
	for _, resolved := range resolvedPolicies {
		if resolved == nil || resolved.Value == nil || resolved.Value.PolicySchema == "" {
			continue
		}
		if !schemas.cached(resolved.Value.PolicySchema) {
			needed[resolved.Value.PolicySchema] = true
		}
	}
	if len(needed) < chromePolicySchemaPrefetchMin {
		return
	}

	pageToken := ""
	for len(needed) > 0 {
		var resp *chromepolicy.GoogleChromePolicyVersionsV1ListPolicySchemasResponse
		err := retryTimeDuration(ctx, time.Minute, func() error {
			var retryErr error
			resp, retryErr = schemas.service.List(fmt.Sprintf("customers/%s", schemas.customer)).
				PageSize(1000).PageToken(pageToken).Context(ctx).Do()
			return retryErr
		})
		if err != nil {
			log.Printf("[WARN] Could not list Chrome policy schemas, reading each schema instead: %s", err)
			return
		}

		listed := map[string]*chromepolicy.GoogleChromePolicyVersionsV1PolicySchema{}
		for _, policySchema := range resp.PolicySchemas {
			if policySchema != nil && needed[policySchema.SchemaName] && policySchema.Definition != nil &&
				len(policySchema.Definition.MessageType) > 0 {
				listed[schemas.path(policySchema.SchemaName)] = policySchema
				delete(needed, policySchema.SchemaName)
			}
		}
		schemas.store.addMissing(listed)
		if resp.NextPageToken == "" {
			return
		}
		pageToken = resp.NextPageToken
	}
}

// cached reports whether the store of the cache has the schema named schemaName.
func (c *chromePolicySchemaCache) cached(schemaName string) bool {
	_, ok := c.store.lookup(c.path(schemaName))
	return ok
}

// chromeResolvedPoliciesId returns an ID from the org unit, the filter, and the additional target keys.
func chromeResolvedPoliciesId(orgUnitId, filter string, additionalTargetKeys map[string]string) string {
	id := orgUnitId + "/" + filter
	if len(additionalTargetKeys) == 0 {
		return id
	}
	return id + "/" + joinChromeTargetKeys(additionalTargetKeys)
}

// joinChromeTargetKeys returns the additional target keys as "key=value" pairs, sorted and separated by commas.
func joinChromeTargetKeys(additionalTargetKeys map[string]string) string {
	keys := make([]string, 0, len(additionalTargetKeys))
	for k, v := range additionalTargetKeys {
		keys = append(keys, k+"="+v)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
