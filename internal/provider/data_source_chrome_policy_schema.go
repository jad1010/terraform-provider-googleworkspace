// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"google.golang.org/api/chromepolicy/v1"
)

func dataSourceChromePolicySchema() *schema.Resource {
	return &schema.Resource{
		Description: "Chrome Policy Schema data source in the Terraform Googleworkspace provider. Chrome Policy Schema " +
			"resides under the `https://www.googleapis.com/auth/chrome.management.policy` client scope.",

		ReadContext: dataSourceChromePolicySchemaRead,

		Schema: map[string]*schema.Schema{
			// Intentionally ignoring field 'name' https://developers.google.com/chrome/policy/reference/rest/v1/customers.policySchemas#PolicySchema
			// it is a confusing field, that includes url segments the practitioner won't find useful.
			// Format: name=customers/{customer}/policySchemas/{schema_namespace}
			// Using the output field 'schema_name' instead as the field the practitioner specifies
			"schema_name": {
				Description: "The full qualified name of the policy schema",
				Type:        schema.TypeString,
				Required:    true,
			},
			"policy_description": {
				Description: "Description about the policy schema for user consumption.",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"additional_target_key_names": {
				Description: "Additional key names that will be used to identify the target of the policy value. When specifying a policyTargetKey, each of the additional keys specified here will have to be included in the additionalTargetKeys map.",
				Type:        schema.TypeList,
				Computed:    true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"key": {
							Description: "Key name.",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"key_description": {
							Description: "Key description.",
							Type:        schema.TypeString,
							Computed:    true,
						},
					},
				},
			},
			"definition": {
				Description: "Schema definition using proto descriptor.",
				Type:        schema.TypeList,
				Computed:    true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"name": {
							Description: "File name, relative to root of source tree",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"package": {
							Description: "e.g. 'foo', 'foo.bar', etc.",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"message_type": {
							Description: "All top-level definitions in this file, represented as a JSON string",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"enum_type": {
							Type:     schema.TypeList,
							Computed: true,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"name": {
										Type:     schema.TypeString,
										Computed: true,
									},
									"value": {
										Type:     schema.TypeList,
										Computed: true,
										Elem: &schema.Resource{
											Schema: map[string]*schema.Schema{
												"name": {
													Type:     schema.TypeString,
													Computed: true,
												},
												"number": {
													Type:     schema.TypeInt,
													Computed: true,
												},
											},
										},
									},
								},
							},
						},
						"syntax": {
							Description: "The syntax of the proto file. The supported values are 'proto' and 'proto3'.",
							Type:        schema.TypeString,
							Computed:    true,
						},
					},
				},
			},
			"field_descriptions": {
				Description: "Detailed description of each field that is part of the schema, represented as a JSON string.",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"access_restrictions": {
				Description: "Specific access restrictions related to this policy.",
				Type:        schema.TypeList,
				Computed:    true,
				Elem: &schema.Schema{
					Type: schema.TypeString,
				},
			},
			"notices": {
				Description: "Special notice messages related to setting certain values in certain fields in the schema.",
				Type:        schema.TypeList,
				Computed:    true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"field": {
							Description: "The field name associated with the notice.",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"notice_value": {
							Description: "The value of the field that has a notice. When setting the field to this value, the user may be required to acknowledge the notice message in order for the value to be set.",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"notice_message": {
							Description: "The notice message associate with the value of the field.",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"acknowledgement_required": {
							Description: "Whether the user needs to acknowledge the notice message before the value can be set.",
							Type:        schema.TypeBool,
							Computed:    true,
						},
					},
				},
			},
			"support_uri": {
				Description: "URI to related support article for this schema.",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"category_title": {
				Description: "Title of the category in which the setting belongs.",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"supported_platforms": {
				Description: "The platforms on which the policy applies, for example `CHROME_OS` or `CHROME_BROWSER`.",
				Type:        schema.TypeList,
				Computed:    true,
				Elem: &schema.Schema{
					Type: schema.TypeString,
				},
			},
			"policy_api_lifecycle": {
				Description: "The current lifecycle of the policy in the Chrome Policy API, empty if the API does not return it: " +
					"`stage` (for example `API_CURRENT` or `API_DEPRECATED`), `description`, `end_support` (the date, " +
					"`YYYY-MM-DD`, after which a change to a deprecated policy fails), `deprecated_in_favor_of` (the " +
					"schema names of the policies that replace a deprecated policy), and " +
					"`scheduled_to_deprecate_policies` (the schema names of the older policies that this policy replaces).",
				Type:     schema.TypeList,
				Computed: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"stage": {
							Description: "The lifecycle stage, for example `API_CURRENT` or `API_DEPRECATED`.",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"description": {
							Description: "Description of the current lifecycle stage.",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"end_support": {
							Description: "The date (`YYYY-MM-DD`) on which support for the policy ends. After this date, a change to the policy fails. Set only for a deprecated policy.",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"deprecated_in_favor_of": {
							Description: "The schema names of the policies that replace this deprecated policy.",
							Type:        schema.TypeList,
							Computed:    true,
							Elem:        &schema.Schema{Type: schema.TypeString},
						},
						"scheduled_to_deprecate_policies": {
							Description: "The schema names of the older policies that this policy replaces.",
							Type:        schema.TypeList,
							Computed:    true,
							Elem:        &schema.Schema{Type: schema.TypeString},
						},
					},
				},
			},
			"enum_fields": {
				Description: "The enum fields of the schema, also in nested messages, with the full names of their values. " +
					"Use a value name as the value of the field in `googleworkspace_chrome_policy`. Confirm enum value names " +
					"here instead of guessing a prefix. Each element has " +
					"`field` (the field path, with a dot between the names of nested fields), `enum_type` (the fully " +
					"qualified name of the enum type), and `values`. Each value has `value` (the full name), `number`, " +
					"and `description` (from the known value in the field descriptions that is the full name, or else the " +
					"longest known value that is the last part of the name after `_`; empty if there is none).",
				Type:     schema.TypeList,
				Computed: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"field": {
							Description: "The field path. Names of nested fields are separated by a dot.",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"enum_type": {
							Description: "The fully qualified name of the enum type.",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"values": {
							Description: "The values of the enum.",
							Type:        schema.TypeList,
							Computed:    true,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"value": {
										Description: "The full name of the enum value.",
										Type:        schema.TypeString,
										Computed:    true,
									},
									"number": {
										Description: "The number of the enum value.",
										Type:        schema.TypeInt,
										Computed:    true,
									},
									"description": {
										Description: "The description of the value from the field descriptions of the schema. Empty if the schema has no description for the value.",
										Type:        schema.TypeString,
										Computed:    true,
									},
								},
							},
						},
					},
				},
			},
			"field_known_values": {
				Description: "The known values of the fields of the schema, from the field descriptions, also for nested " +
					"fields and for fields that are not enums. Each element has `field` (the field path, with a dot " +
					"between the names of nested fields), `value`, and `description`.",
				Type:     schema.TypeList,
				Computed: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"field": {
							Description: "The field path. Names of nested fields are separated by a dot.",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"value": {
							Description: "The string form of the value.",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"description": {
							Description: "The description of the value.",
							Type:        schema.TypeString,
							Computed:    true,
						},
					},
				},
			},
		},
	}
}

func dataSourceChromePolicySchemaRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*apiClient)

	// The schema always comes from a GET, because a listed schema in the store may not have every field. The result
	// replaces the store entry for the other operations.
	schemas, diags := newChromePolicySchemaCache(client)
	if diags.HasError() {
		return diags
	}

	policySchema, err := schemas.refresh(ctx, d.Get("schema_name").(string))
	if err != nil {
		return diag.FromErr(err)
	}

	d.SetId(policySchema.SchemaName)
	d.Set("schema_name", policySchema.SchemaName)
	d.Set("policy_description", policySchema.PolicyDescription)
	d.Set("support_uri", policySchema.SupportUri)
	if err := d.Set("additional_target_key_names", flattenAdditionalTargetKeyNames(policySchema.AdditionalTargetKeyNames)); err != nil {
		return diag.FromErr(err)
	}
	if err := d.Set("definition", flattenDefinition(policySchema.Definition)); err != nil {
		return diag.FromErr(err)
	}

	// this attribute contains recursive types, so we store it as json
	fieldDescriptions, _ := json.MarshalIndent(policySchema.FieldDescriptions, "", "  ")
	d.Set("field_descriptions", string(fieldDescriptions))

	if err := d.Set("access_restrictions", policySchema.AccessRestrictions); err != nil {
		return diag.FromErr(err)
	}
	if err := d.Set("notices", flattenNotices(policySchema.Notices)); err != nil {
		return diag.FromErr(err)
	}
	if err := d.Set("category_title", policySchema.CategoryTitle); err != nil {
		return diag.FromErr(err)
	}
	if err := d.Set("supported_platforms", policySchema.SupportedPlatforms); err != nil {
		return diag.FromErr(err)
	}
	if err := d.Set("policy_api_lifecycle", flattenPolicyApiLifecycle(policySchema.PolicyApiLifecycle)); err != nil {
		return diag.FromErr(err)
	}
	if err := d.Set("enum_fields", flattenEnumFields(policySchema.Definition, policySchema.FieldDescriptions)); err != nil {
		return diag.FromErr(err)
	}
	if err := d.Set("field_known_values", flattenFieldKnownValues("", policySchema.FieldDescriptions)); err != nil {
		return diag.FromErr(err)
	}

	return nil
}

func flattenNotices(ns []*chromepolicy.GoogleChromePolicyVersionsV1PolicySchemaNoticeDescription) []interface{} {
	result := make([]interface{}, len(ns))

	for i, n := range ns {
		obj := make(map[string]interface{})
		obj["field"] = n.Field
		obj["notice_value"] = n.NoticeValue
		obj["notice_message"] = n.NoticeMessage
		obj["acknowledgement_required"] = n.AcknowledgementRequired
		result[i] = obj
	}

	return result
}

func flattenEnumType(es []*chromepolicy.Proto2EnumDescriptorProto) []interface{} {
	result := make([]interface{}, len(es))

	for i, e := range es {
		obj := make(map[string]interface{})

		obj["name"] = e.Name
		values := make([]interface{}, len(e.Value))
		for j, v := range e.Value {
			values[j] = map[string]interface{}{
				"name":   v.Name,
				"number": int(v.Number),
			}
		}
		obj["value"] = values

		result[i] = obj
	}

	return result
}

func flattenDefinition(d *chromepolicy.Proto2FileDescriptorProto) []interface{} {
	result := make([]interface{}, 1)
	obj := make(map[string]interface{})

	obj["name"] = d.Name
	obj["package"] = d.Package
	obj["syntax"] = d.Syntax
	obj["enum_type"] = flattenEnumType(d.EnumType)
	// this attribute contains recursive types, so we store it as json
	msgType, _ := json.MarshalIndent(d.MessageType, "", "  ")
	obj["message_type"] = string(msgType)

	result[0] = obj
	return result
}

func flattenAdditionalTargetKeyNames(as []*chromepolicy.GoogleChromePolicyVersionsV1AdditionalTargetKeyName) []interface{} {
	result := make([]interface{}, len(as))
	for i, a := range as {
		obj := make(map[string]interface{})
		obj["key"] = a.Key
		obj["key_description"] = a.KeyDescription
		result[i] = obj
	}
	return result
}

func flattenPolicyApiLifecycle(l *chromepolicy.GoogleChromePolicyVersionsV1PolicyApiLifecycle) []interface{} {
	if l == nil {
		return []interface{}{}
	}

	endSupport := ""
	if l.EndSupport != nil && l.EndSupport.Year > 0 {
		endSupport = fmt.Sprintf("%04d-%02d-%02d", l.EndSupport.Year, l.EndSupport.Month, l.EndSupport.Day)
	}

	return []interface{}{map[string]interface{}{
		"stage":                           l.PolicyApiLifecycleStage,
		"description":                     l.Description,
		"end_support":                     endSupport,
		"deprecated_in_favor_of":          l.DeprecatedInFavorOf,
		"scheduled_to_deprecate_policies": l.ScheduledToDeprecatePolicies,
	}}
}

// fieldDescriptionName returns the name of the field that fd describes.
func fieldDescriptionName(fd *chromepolicy.GoogleChromePolicyVersionsV1PolicySchemaFieldDescription) string {
	if fd.Name != "" {
		return fd.Name
	}
	return fd.Field
}

// findFieldDescription returns the description of the field named name, or nil.
func findFieldDescription(fds []*chromepolicy.GoogleChromePolicyVersionsV1PolicySchemaFieldDescription, name string) *chromepolicy.GoogleChromePolicyVersionsV1PolicySchemaFieldDescription {
	for _, fd := range fds {
		if fd != nil && fieldDescriptionName(fd) == name {
			return fd
		}
	}
	return nil
}

// flattenFieldKnownValues returns the known values of the fields in fds and in their nested fields.
// prefix is the path of the field that contains fds.
func flattenFieldKnownValues(prefix string, fds []*chromepolicy.GoogleChromePolicyVersionsV1PolicySchemaFieldDescription) []interface{} {
	result := []interface{}{}
	for _, fd := range fds {
		if fd == nil {
			continue
		}
		path := joinProtoName(prefix, fieldDescriptionName(fd))
		for _, kv := range fd.KnownValueDescriptions {
			if kv == nil {
				continue
			}
			result = append(result, map[string]interface{}{
				"field":       path,
				"value":       kv.Value,
				"description": kv.Description,
			})
		}
		result = append(result, flattenFieldKnownValues(path, fd.NestedFieldDescriptions)...)
	}
	return result
}

// enumValueDescription returns the description of the known value for the enum value named name. A known value matches
// if it is the name, or else if it is the last part of the name after "_" (for example "FALSE" for
// "PASSWORD_MANAGER_ENABLED_ENUM_FALSE"). Of the known values that match the last part, the longest one is used, so
// that "NOT_ALLOWED", not "ALLOWED", matches "X_NOT_ALLOWED". It returns "" if no known value matches.
func enumValueDescription(known []*chromepolicy.GoogleChromePolicyVersionsV1PolicySchemaFieldKnownValueDescription, name string) string {
	var best *chromepolicy.GoogleChromePolicyVersionsV1PolicySchemaFieldKnownValueDescription
	for _, kv := range known {
		if kv == nil {
			continue
		}
		if kv.Value == name {
			return kv.Description
		}
		if kv.Value != "" && strings.HasSuffix(name, "_"+kv.Value) && (best == nil || len(kv.Value) > len(best.Value)) {
			best = kv
		}
	}
	if best == nil {
		return ""
	}
	return best.Description
}

// flattenEnumFields returns the enum fields of the top-level messages of the definition and of the messages that
// their fields refer to. The description of a value is the description of the matching known value from the field
// descriptions (see enumValueDescription). If two fields have the same path, only the first one is in the result.
func flattenEnumFields(definition *chromepolicy.Proto2FileDescriptorProto, fds []*chromepolicy.GoogleChromePolicyVersionsV1PolicySchemaFieldDescription) []interface{} {
	result := []interface{}{}
	if definition == nil {
		return result
	}
	types := newChromePolicySchemaTypes(definition)

	// visiting has the messages on the current path, so that a recursive message is not followed again.
	visiting := map[string]bool{}
	// seen has the paths of the enum fields in the result. Two top-level messages can have a field with the same name.
	seen := map[string]bool{}
	var walk func(prefix, scope string, message *chromepolicy.Proto2DescriptorProto, fds []*chromepolicy.GoogleChromePolicyVersionsV1PolicySchemaFieldDescription)
	walk = func(prefix, scope string, message *chromepolicy.Proto2DescriptorProto, fds []*chromepolicy.GoogleChromePolicyVersionsV1PolicySchemaFieldDescription) {
		for _, f := range message.Field {
			if f == nil {
				continue
			}
			field := chromePolicyField{field: f, scope: scope}
			path := joinProtoName(prefix, f.Name)
			fd := findFieldDescription(fds, f.Name)

			switch types.kind(field) {
			case "TYPE_ENUM":
				enumName, ok := resolveName(f.TypeName, scope, func(name string) bool {
					_, ok := types.enums[name]
					return ok
				})
				if !ok {
					continue
				}
				if seen[path] {
					continue
				}
				seen[path] = true
				var known []*chromepolicy.GoogleChromePolicyVersionsV1PolicySchemaFieldKnownValueDescription
				if fd != nil {
					known = fd.KnownValueDescriptions
				}
				values := []interface{}{}
				for _, v := range types.enums[enumName].Value {
					if v == nil {
						continue
					}
					values = append(values, map[string]interface{}{
						"value":       v.Name,
						"number":      int(v.Number),
						"description": enumValueDescription(known, v.Name),
					})
				}
				result = append(result, map[string]interface{}{
					"field":     path,
					"enum_type": enumName,
					"values":    values,
				})
			case "TYPE_MESSAGE":
				name, nested := types.resolveMessage(field)
				if nested == nil || visiting[name] {
					continue
				}
				var nestedFds []*chromepolicy.GoogleChromePolicyVersionsV1PolicySchemaFieldDescription
				if fd != nil {
					nestedFds = fd.NestedFieldDescriptions
				}
				visiting[name] = true
				walk(path, name, nested, nestedFds)
				delete(visiting, name)
			}
		}
	}

	for _, message := range definition.MessageType {
		if message == nil {
			continue
		}
		name := joinProtoName(definition.Package, message.Name)
		visiting[name] = true
		walk("", name, message, fds)
		delete(visiting, name)
	}
	return result
}
