// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"google.golang.org/api/chromepolicy/v1"
)

const testChromePolicyPasswordManager = "chrome.users.PasswordManager"

// testChromePolicySchemaDetails are full policy schemas, by schema name.
var testChromePolicySchemaDetails = map[string]*chromepolicy.GoogleChromePolicyVersionsV1PolicySchema{
	testChromePolicyPasswordManager: {
		SchemaName:         testChromePolicyPasswordManager,
		PolicyDescription:  "Password manager.",
		SupportUri:         "https://support.google.com/chrome/a/answer/1",
		CategoryTitle:      "Security",
		SupportedPlatforms: []string{"CHROME_OS", "CHROME_BROWSER"},
		PolicyApiLifecycle: &chromepolicy.GoogleChromePolicyVersionsV1PolicyApiLifecycle{
			PolicyApiLifecycleStage: "API_DEPRECATED",
			Description:             "Use the new policy.",
			EndSupport:              &chromepolicy.GoogleTypeDate{Year: 2027, Month: 3, Day: 1},
			DeprecatedInFavorOf:     []string{"chrome.users.PasswordManagerV2"},
		},
		Definition: &chromepolicy.Proto2FileDescriptorProto{
			Package: "chrome.users",
			Syntax:  "proto2",
			EnumType: []*chromepolicy.Proto2EnumDescriptorProto{{
				Name: "Mode",
				Value: []*chromepolicy.Proto2EnumValueDescriptorProto{
					{Name: "MODE_UNSPECIFIED", Number: 0},
					{Name: "MODE_STRICT", Number: 1},
				},
			}},
			MessageType: []*chromepolicy.Proto2DescriptorProto{{
				Name: "PasswordManager",
				Field: []*chromepolicy.Proto2FieldDescriptorProto{
					// An enum in the message.
					testChromePolicyField("passwordManagerEnabled", "TYPE_ENUM", "LABEL_OPTIONAL", "PasswordManagerEnabledEnum"),
					testChromePolicyField("settings", "TYPE_MESSAGE", "LABEL_OPTIONAL", "Settings"),
					testChromePolicyField("flag", "TYPE_BOOL", "LABEL_OPTIONAL", ""),
				},
				EnumType: []*chromepolicy.Proto2EnumDescriptorProto{{
					Name: "PasswordManagerEnabledEnum",
					Value: []*chromepolicy.Proto2EnumValueDescriptorProto{
						{Name: "PASSWORD_MANAGER_ENABLED_ENUM_UNSET", Number: 0},
						{Name: "PASSWORD_MANAGER_ENABLED_ENUM_FALSE", Number: 1},
						{Name: "PASSWORD_MANAGER_ENABLED_ENUM_TRUE", Number: 2},
					},
				}},
				NestedType: []*chromepolicy.Proto2DescriptorProto{testChromePolicyMessage("Settings",
					// A top-level enum, by its fully qualified name.
					testChromePolicyField("mode", "TYPE_ENUM", "LABEL_OPTIONAL", ".chrome.users.Mode"),
					// A recursive message is not followed again.
					testChromePolicyField("again", "TYPE_MESSAGE", "LABEL_OPTIONAL", "Settings"),
				)},
			}, testChromePolicyMessage("Other",
				// The same path as a field of the first message: only the first one is in enum_fields.
				testChromePolicyField("passwordManagerEnabled", "TYPE_ENUM", "LABEL_OPTIONAL", ".chrome.users.Mode"),
			)},
		},
		FieldDescriptions: []*chromepolicy.GoogleChromePolicyVersionsV1PolicySchemaFieldDescription{
			{
				Name: "passwordManagerEnabled",
				KnownValueDescriptions: []*chromepolicy.GoogleChromePolicyVersionsV1PolicySchemaFieldKnownValueDescription{
					{Value: "PASSWORD_MANAGER_ENABLED_ENUM_FALSE", Description: "Never allow use of password manager."},
					// The last part of the value name only.
					{Value: "TRUE", Description: "Always allow use of password manager."},
				},
			},
			{
				// The deprecated field attribute, without name.
				Field: "settings",
				NestedFieldDescriptions: []*chromepolicy.GoogleChromePolicyVersionsV1PolicySchemaFieldDescription{{
					Name: "mode",
					KnownValueDescriptions: []*chromepolicy.GoogleChromePolicyVersionsV1PolicySchemaFieldKnownValueDescription{
						{Value: "MODE_STRICT", Description: "Strict mode."},
					},
				}},
			},
			{
				Name: "flag",
				KnownValueDescriptions: []*chromepolicy.GoogleChromePolicyVersionsV1PolicySchemaFieldKnownValueDescription{
					{Value: "true", Description: "On."},
				},
			},
		},
	},
	testChromePolicyHomepage: {
		SchemaName: testChromePolicyHomepage,
		Definition: testChromePolicySchemas[testChromePolicyHomepage],
	},
}

// newTestChromeHandlerClient returns a client that sends every request to handler.
func newTestChromeHandlerClient(t *testing.T, handler http.Handler) *apiClient {
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	target, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}

	return &apiClient{
		client:   &http.Client{Transport: chromePolicyRedirectTransport{target: target, base: ts.Client().Transport}},
		Customer: testChromePolicyCustomer,
	}
}

func testReadChromePolicySchema(t *testing.T, schemaName string) *schema.ResourceData {
	t.Helper()
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/v1/customers/"+testChromePolicyCustomer+"/policySchemas/")
		policySchema, ok := testChromePolicySchemaDetails[name]
		if r.Method != http.MethodGet || !ok {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(policySchema); err != nil {
			t.Errorf("encode response: %v", err)
		}
	})

	r := dataSourceChromePolicySchema()
	d := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"schema_name": schemaName})
	requireNoDiags(t, r.ReadContext(context.Background(), d, newTestChromeHandlerClient(t, api)))
	return d
}

func TestChromePolicySchemaRead_platformsAndLifecycle(t *testing.T) {
	d := testReadChromePolicySchema(t, testChromePolicyPasswordManager)

	checks := map[string]interface{}{
		// Existing attributes do not change.
		"schema_name":                   testChromePolicyPasswordManager,
		"policy_description":            "Password manager.",
		"support_uri":                   "https://support.google.com/chrome/a/answer/1",
		"definition.0.enum_type.0.name": "Mode",
		// New attributes.
		"category_title":                                           "Security",
		"supported_platforms":                                      []interface{}{"CHROME_OS", "CHROME_BROWSER"},
		"policy_api_lifecycle.#":                                   1,
		"policy_api_lifecycle.0.stage":                             "API_DEPRECATED",
		"policy_api_lifecycle.0.description":                       "Use the new policy.",
		"policy_api_lifecycle.0.end_support":                       "2027-03-01",
		"policy_api_lifecycle.0.deprecated_in_favor_of":            []interface{}{"chrome.users.PasswordManagerV2"},
		"policy_api_lifecycle.0.scheduled_to_deprecate_policies.#": 0,
	}
	for key, want := range checks {
		if got := d.Get(key); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
}

func TestChromePolicySchemaRead_noLifecycle(t *testing.T) {
	d := testReadChromePolicySchema(t, testChromePolicyHomepage)

	for _, key := range []string{"policy_api_lifecycle.#", "supported_platforms.#", "enum_fields.#", "field_known_values.#"} {
		if got := d.Get(key).(int); got != 0 {
			t.Errorf("%s = %d, want 0", key, got)
		}
	}
}

func TestChromePolicySchemaRead_enumFields(t *testing.T) {
	d := testReadChromePolicySchema(t, testChromePolicyPasswordManager)

	want := []interface{}{
		map[string]interface{}{
			"field":     "passwordManagerEnabled",
			"enum_type": "chrome.users.PasswordManager.PasswordManagerEnabledEnum",
			"values": []interface{}{
				map[string]interface{}{"value": "PASSWORD_MANAGER_ENABLED_ENUM_UNSET", "number": 0, "description": ""},
				map[string]interface{}{"value": "PASSWORD_MANAGER_ENABLED_ENUM_FALSE", "number": 1, "description": "Never allow use of password manager."},
				map[string]interface{}{"value": "PASSWORD_MANAGER_ENABLED_ENUM_TRUE", "number": 2, "description": "Always allow use of password manager."},
			},
		},
		map[string]interface{}{
			"field":     "settings.mode",
			"enum_type": "chrome.users.Mode",
			"values": []interface{}{
				map[string]interface{}{"value": "MODE_UNSPECIFIED", "number": 0, "description": ""},
				map[string]interface{}{"value": "MODE_STRICT", "number": 1, "description": "Strict mode."},
			},
		},
	}
	if got := d.Get("enum_fields"); !reflect.DeepEqual(got, want) {
		t.Errorf("enum_fields = %#v\nwant %#v", got, want)
	}
}

func TestChromePolicySchemaRead_fieldKnownValues(t *testing.T) {
	d := testReadChromePolicySchema(t, testChromePolicyPasswordManager)

	want := []interface{}{
		map[string]interface{}{"field": "passwordManagerEnabled", "value": "PASSWORD_MANAGER_ENABLED_ENUM_FALSE", "description": "Never allow use of password manager."},
		map[string]interface{}{"field": "passwordManagerEnabled", "value": "TRUE", "description": "Always allow use of password manager."},
		map[string]interface{}{"field": "settings.mode", "value": "MODE_STRICT", "description": "Strict mode."},
		map[string]interface{}{"field": "flag", "value": "true", "description": "On."},
	}
	if got := d.Get("field_known_values"); !reflect.DeepEqual(got, want) {
		t.Errorf("field_known_values = %#v\nwant %#v", got, want)
	}
}

func TestEnumValueDescription(t *testing.T) {
	known := []*chromepolicy.GoogleChromePolicyVersionsV1PolicySchemaFieldKnownValueDescription{
		nil,
		{Value: "FALSE", Description: "suffix false"},
		{Value: "MODE_ENUM_FALSE", Description: "exact false"},
		{Value: "TRUE", Description: "suffix true"},
		{Value: "", Description: "empty"},
		// Overlapping suffixes, in both orders.
		{Value: "ALLOWED", Description: "allowed"},
		{Value: "NOT_ALLOWED", Description: "not allowed"},
		{Value: "BLOCKED_ALL", Description: "blocked all"},
		{Value: "ALL", Description: "all"},
	}
	for name, want := range map[string]string{
		"MODE_ENUM_FALSE":       "exact false", // An exact match wins over a suffix match.
		"MODE_ENUM_TRUE":        "suffix true",
		"MODE_ENUM_UNSET":       "",
		"NOTTRUE":               "", // A suffix matches only after "_".
		"X_ENUM_NOT_ALLOWED":    "not allowed",
		"X_ENUM_ALLOWED":        "allowed",
		"X_ENUM_BLOCKED_ALL":    "blocked all",
		"X_ENUM_ALL":            "all",
		"NOT_ALLOWED":           "not allowed",
		"X_ENUM_IS_NOT_ALLOWED": "not allowed",
	} {
		if got := enumValueDescription(known, name); got != want {
			t.Errorf("enumValueDescription(%q) = %q, want %q", name, got, want)
		}
	}
}
