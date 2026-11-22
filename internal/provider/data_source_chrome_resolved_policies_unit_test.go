// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"google.golang.org/api/chromepolicy/v1"
)

// fakeChromeResolveAPI serves policy schemas (from schemas, or else from testChromePolicySchemas) and policies:resolve
// responses by page token. policySchemas.list returns the schemas sorted by name, two per page, without the
// definitions of the schemas in listNoDefinition; with listErr set, it fails.
type fakeChromeResolveAPI struct {
	t *testing.T

	mu               sync.Mutex
	pages            map[string]*chromepolicy.GoogleChromePolicyVersionsV1ResolveResponse
	schemas          map[string]*chromepolicy.Proto2FileDescriptorProto
	listNoDefinition map[string]bool
	resolveErr       bool
	listErr          bool
	requests         []chromepolicy.GoogleChromePolicyVersionsV1ResolveRequest
	schemaGets       int
	getCalls         map[string]int
	listCalls        int
}

func (f *fakeChromeResolveAPI) definitions() map[string]*chromepolicy.Proto2FileDescriptorProto {
	if f.schemas != nil {
		return f.schemas
	}
	return testChromePolicySchemas
}

// testChromeGeneratedSchemas returns n schemas, each with one TYPE_INT64 field "count", and a resolved policy with
// the value i (as the API returns it, a string) for schema i.
func testChromeGeneratedSchemas(n int) (map[string]*chromepolicy.Proto2FileDescriptorProto, []string, []*chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy) {
	definitions := map[string]*chromepolicy.Proto2FileDescriptorProto{}
	var names []string
	var resolved []*chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy
	for i := 0; i < n; i++ {
		message := fmt.Sprintf("Generated%02d", i)
		name := "chrome.users." + message
		definitions[name] = &chromepolicy.Proto2FileDescriptorProto{
			Package:     "chrome.users",
			MessageType: []*chromepolicy.Proto2DescriptorProto{testChromePolicyMessage(message, testChromePolicyField("count", "TYPE_INT64", "LABEL_OPTIONAL", ""))},
		}
		names = append(names, name)
		resolved = append(resolved, testResolvedPolicy(name, fmt.Sprintf(`{"count":"%d"}`, i), testChromePolicyOrgUnit))
	}
	return definitions, names, resolved
}

func (f *fakeChromeResolveAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	customerPath := "/v1/customers/" + testChromePolicyCustomer
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodGet && r.URL.Path == customerPath+"/policySchemas":
		f.listCalls++
		if f.listErr {
			http.Error(w, `{"error":{"code":403,"message":"permission denied"}}`, http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("pageSize") != "1000" {
			f.t.Errorf("list pageSize = %q, want 1000", r.URL.Query().Get("pageSize"))
		}
		names := make([]string, 0, len(f.definitions()))
		for name := range f.definitions() {
			names = append(names, name)
		}
		sort.Strings(names)
		start := 0
		if token := r.URL.Query().Get("pageToken"); token != "" {
			start, _ = strconv.Atoi(token)
		}
		resp := &chromepolicy.GoogleChromePolicyVersionsV1ListPolicySchemasResponse{}
		for i := start; i < len(names) && i < start+2; i++ {
			listed := &chromepolicy.GoogleChromePolicyVersionsV1PolicySchema{SchemaName: names[i]}
			if !f.listNoDefinition[names[i]] {
				listed.Definition = f.definitions()[names[i]]
			}
			resp.PolicySchemas = append(resp.PolicySchemas, listed)
		}
		if start+2 < len(names) {
			resp.NextPageToken = strconv.Itoa(start + 2)
		}
		f.writeJSON(w, resp)

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, customerPath+"/policySchemas/"):
		name := strings.TrimPrefix(r.URL.Path, customerPath+"/policySchemas/")
		if f.getCalls == nil {
			f.getCalls = map[string]int{}
		}
		f.getCalls[name]++
		definition, ok := f.definitions()[name]
		if !ok {
			http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
			return
		}
		f.schemaGets++
		f.writeJSON(w, &chromepolicy.GoogleChromePolicyVersionsV1PolicySchema{SchemaName: name, Definition: definition})

	case r.Method == http.MethodPost && r.URL.Path == customerPath+"/policies:resolve":
		var req chromepolicy.GoogleChromePolicyVersionsV1ResolveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			f.t.Errorf("decode resolve request: %v", err)
		}
		f.requests = append(f.requests, req)
		if f.resolveErr {
			http.Error(w, `{"error":{"code":403,"message":"permission denied"}}`, http.StatusForbidden)
			return
		}
		resp, ok := f.pages[req.PageToken]
		if !ok {
			f.t.Errorf("unexpected page token %q", req.PageToken)
			resp = &chromepolicy.GoogleChromePolicyVersionsV1ResolveResponse{}
		}
		f.writeJSON(w, resp)

	default:
		f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"error":{"code":400,"message":"unexpected request"}}`, http.StatusBadRequest)
	}
}

func (f *fakeChromeResolveAPI) writeJSON(w http.ResponseWriter, v interface{}) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		f.t.Errorf("encode response: %v", err)
	}
}

func testResolvedPolicy(schemaName, value, sourceOrgUnit string) *chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy {
	resolved := &chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy{
		TargetKey: &chromepolicy.GoogleChromePolicyVersionsV1PolicyTargetKey{TargetResource: "orgunits/" + testChromePolicyOrgUnit},
		Value: &chromepolicy.GoogleChromePolicyVersionsV1PolicyValue{
			PolicySchema: schemaName,
			Value:        []byte(value),
		},
	}
	if sourceOrgUnit != "" {
		resolved.SourceKey = &chromepolicy.GoogleChromePolicyVersionsV1PolicyTargetKey{TargetResource: "orgunits/" + sourceOrgUnit}
	}
	return resolved
}

func testReadChromeResolvedPolicies(t *testing.T, api http.Handler, raw map[string]interface{}) (*schema.ResourceData, diag.Diagnostics) {
	t.Helper()
	r := dataSourceChromeResolvedPolicies()
	d := schema.TestResourceDataRaw(t, r.Schema, raw)
	diags := r.ReadContext(context.Background(), d, newTestChromeHandlerClient(t, api))
	return d, diags
}

func testResolvedPolicies(d *schema.ResourceData) []map[string]interface{} {
	var out []map[string]interface{}
	for _, p := range d.Get("resolved_policies").([]interface{}) {
		out = append(out, p.(map[string]interface{}))
	}
	return out
}

func TestChromeResolvedPoliciesRead_pagination(t *testing.T) {
	api := &fakeChromeResolveAPI{t: t, pages: map[string]*chromepolicy.GoogleChromePolicyVersionsV1ResolveResponse{
		"": {
			ResolvedPolicies: []*chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy{
				testResolvedPolicy(testChromePolicyConns, `{"maxConnectionsPerProxy":"33"}`, testChromePolicyOrgUnit),
			},
			NextPageToken: "page2",
		},
		"page2": {
			ResolvedPolicies: []*chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy{
				testResolvedPolicy(testChromePolicyUrls, `{"urlBlocklist":["a.example","b.example"],"maxUrls":"5"}`, testChromePolicyOrgUnit),
			},
		},
	}}

	d, diags := testReadChromeResolvedPolicies(t, api, map[string]interface{}{
		"org_unit_id":          "id:" + testChromePolicyOrgUnit,
		"policy_schema_filter": "chrome.users.*",
	})
	requireNoDiags(t, diags)

	if len(api.requests) != 2 {
		t.Fatalf("resolve calls = %d, want 2", len(api.requests))
	}
	for i, wantToken := range []string{"", "page2"} {
		req := api.requests[i]
		if req.PageToken != wantToken || req.PageSize != 1000 || req.PolicySchemaFilter != "chrome.users.*" ||
			req.PolicyTargetKey == nil || req.PolicyTargetKey.TargetResource != "orgunits/"+testChromePolicyOrgUnit ||
			len(req.PolicyTargetKey.AdditionalTargetKeys) != 0 {
			t.Errorf("request %d = %+v, target %+v", i, req, req.PolicyTargetKey)
		}
	}

	policies := testResolvedPolicies(d)
	if len(policies) != 2 {
		t.Fatalf("resolved_policies = %#v, want 2 policies", policies)
	}

	// Numbers that the API returns as strings have the types of the schema fields, as in the resource.
	want := []struct {
		schemaName   string
		value        string
		schemaValues map[string]interface{}
	}{
		{testChromePolicyConns, `{"maxConnectionsPerProxy":33}`, map[string]interface{}{"maxConnectionsPerProxy": "33"}},
		{testChromePolicyUrls, `{"maxUrls":5,"urlBlocklist":["a.example","b.example"]}`, map[string]interface{}{
			"maxUrls":      "5",
			"urlBlocklist": `["a.example","b.example"]`,
		}},
	}
	for i, w := range want {
		p := policies[i]
		if p["schema_name"] != w.schemaName || p["value"] != w.value || !reflect.DeepEqual(p["schema_values"], w.schemaValues) {
			t.Errorf("policy %d = %#v, want %s %s %v", i, p, w.schemaName, w.value, w.schemaValues)
		}
		if p["inherited"] != false || p["source_org_unit_id"] != testChromePolicyOrgUnit {
			t.Errorf("policy %d source = %v inherited = %v, want set on the org unit", i, p["source_org_unit_id"], p["inherited"])
		}
	}

	if d.Id() != testChromePolicyOrgUnit+"/chrome.users.*" {
		t.Errorf("id = %q", d.Id())
	}
}

func TestChromeResolvedPoliciesRead_inheritedAndDirect(t *testing.T) {
	api := &fakeChromeResolveAPI{t: t, pages: map[string]*chromepolicy.GoogleChromePolicyVersionsV1ResolveResponse{
		"": {ResolvedPolicies: []*chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy{
			testResolvedPolicy(testChromePolicyConns, `{"maxConnectionsPerProxy":"40"}`, testChromePolicyOrgUnit),
			testResolvedPolicy(testChromePolicyHomepage, `{"homepageLocation":"https://example.com"}`, "parent"),
			// No source key: the value is the customer default.
			testResolvedPolicy(testChromePolicyUrls, `{"maxUrls":"1"}`, ""),
		}},
	}}

	d, diags := testReadChromeResolvedPolicies(t, api, map[string]interface{}{
		"org_unit_id":          testChromePolicyOrgUnit,
		"policy_schema_filter": "chrome.users.*",
	})
	requireNoDiags(t, diags)

	policies := testResolvedPolicies(d)
	want := []struct {
		schemaName string
		source     string
		inherited  bool
	}{
		{testChromePolicyConns, testChromePolicyOrgUnit, false},
		{testChromePolicyHomepage, "parent", true},
		{testChromePolicyUrls, "", true},
	}
	if len(policies) != len(want) {
		t.Fatalf("resolved_policies = %#v, want %d policies", policies, len(want))
	}
	for i, w := range want {
		p := policies[i]
		if p["schema_name"] != w.schemaName || p["source_org_unit_id"] != w.source || p["inherited"] != w.inherited {
			t.Errorf("policy %d = %#v, want %s from %q inherited %v", i, p, w.schemaName, w.source, w.inherited)
		}
	}
}

func TestChromeResolvedPoliciesRead_empty(t *testing.T) {
	api := &fakeChromeResolveAPI{t: t, pages: map[string]*chromepolicy.GoogleChromePolicyVersionsV1ResolveResponse{
		"": {},
	}}

	d, diags := testReadChromeResolvedPolicies(t, api, map[string]interface{}{
		"org_unit_id":          testChromePolicyOrgUnit,
		"policy_schema_filter": testChromePolicyHomepage,
	})
	requireNoDiags(t, diags)

	if n := d.Get("resolved_policies.#").(int); n != 0 {
		t.Errorf("resolved_policies.# = %d, want 0", n)
	}
	if d.Id() == "" {
		t.Error("id is empty, want an id for a result with no policies")
	}
	if api.schemaGets != 0 {
		t.Errorf("schema gets = %d, want 0", api.schemaGets)
	}
}

func TestChromeResolvedPoliciesRead_additionalTargetKeys(t *testing.T) {
	resolved := testResolvedPolicy(testChromePolicyHomepage, `{"homepageLocation":"https://example.com"}`, testChromePolicyOrgUnit)
	resolved.TargetKey.AdditionalTargetKeys = map[string]string{"app_id": "chrome:abc"}
	api := &fakeChromeResolveAPI{t: t, pages: map[string]*chromepolicy.GoogleChromePolicyVersionsV1ResolveResponse{
		"": {ResolvedPolicies: []*chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy{resolved}},
	}}

	d, diags := testReadChromeResolvedPolicies(t, api, map[string]interface{}{
		"org_unit_id":            testChromePolicyOrgUnit,
		"policy_schema_filter":   testChromePolicyHomepage,
		"additional_target_keys": map[string]interface{}{"app_id": "chrome:abc"},
	})
	requireNoDiags(t, diags)

	if got := api.requests[0].PolicyTargetKey.AdditionalTargetKeys; !reflect.DeepEqual(got, map[string]string{"app_id": "chrome:abc"}) {
		t.Errorf("request additional target keys = %v", got)
	}
	policies := testResolvedPolicies(d)
	if len(policies) != 1 || !reflect.DeepEqual(policies[0]["additional_target_keys"], map[string]interface{}{"app_id": "chrome:abc"}) {
		t.Errorf("resolved_policies = %#v, want the app_id target key", policies)
	}
	if want := testChromePolicyOrgUnit + "/" + testChromePolicyHomepage + "/app_id=chrome:abc"; d.Id() != want {
		t.Errorf("id = %q, want %q", d.Id(), want)
	}
}

func TestChromeResolvedPoliciesRead_unknownFieldKeepsAPIValue(t *testing.T) {
	api := &fakeChromeResolveAPI{t: t, pages: map[string]*chromepolicy.GoogleChromePolicyVersionsV1ResolveResponse{
		"": {ResolvedPolicies: []*chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy{
			testResolvedPolicy(testChromePolicyHomepage, `{ "newField": "7", "homepageLocation": "https://example.com", "big": 12345678901234567890 }`, testChromePolicyOrgUnit),
		}},
	}}

	d, diags := testReadChromeResolvedPolicies(t, api, map[string]interface{}{
		"org_unit_id":          testChromePolicyOrgUnit,
		"policy_schema_filter": testChromePolicyHomepage,
	})
	testRequireRawValueWarning(t, diags, testChromePolicyHomepage)

	// The raw value is canonical JSON: sorted keys, no spaces, exact numbers.
	policies := testResolvedPolicies(d)
	if len(policies) != 1 || policies[0]["value"] != `{"big":12345678901234567890,"homepageLocation":"https://example.com","newField":"7"}` {
		t.Errorf("resolved_policies = %#v, want the canonical API value", policies)
	}

	// One schema name reads one schema, and does not list the schemas.
	if api.listCalls != 0 || api.schemaGets != 1 {
		t.Errorf("list calls = %d, schema gets = %d, want 0 and 1", api.listCalls, api.schemaGets)
	}
}

// testRequireRawValueWarning fails the test if diags is not one warning about the raw value of schemaName.
func testRequireRawValueWarning(t *testing.T, diags diag.Diagnostics, schemaName string) {
	t.Helper()
	if len(diags) != 1 || diags[0].Severity != diag.Warning ||
		!strings.Contains(diags[0].Summary, schemaName) || !strings.Contains(diags[0].Detail, "raw API values") {
		t.Fatalf("diags = %#v, want one warning about the raw value of %s", diags, schemaName)
	}
}

func TestChromeResolvedPoliciesRead_wildcardListsSchemas(t *testing.T) {
	definitions, names, resolved := testChromeGeneratedSchemas(chromePolicySchemaPrefetchMin)
	api := &fakeChromeResolveAPI{
		t:       t,
		schemas: definitions,
		// A listed schema without a definition is read with a request of its own.
		listNoDefinition: map[string]bool{names[0]: true},
		pages: map[string]*chromepolicy.GoogleChromePolicyVersionsV1ResolveResponse{
			"": {ResolvedPolicies: resolved},
		},
	}

	d, diags := testReadChromeResolvedPolicies(t, api, map[string]interface{}{
		"org_unit_id":          testChromePolicyOrgUnit,
		"policy_schema_filter": "chrome.users.*",
	})
	requireNoDiags(t, diags)

	// The fake lists two schemas per page.
	wantListCalls := (len(names) + 1) / 2
	if api.listCalls != wantListCalls || api.schemaGets != 1 || api.getCalls[names[0]] != 1 {
		t.Errorf("list calls = %d, schema gets = %v, want %d list calls and one get for %s",
			api.listCalls, api.getCalls, wantListCalls, names[0])
	}
	policies := testResolvedPolicies(d)
	if len(policies) != len(names) {
		t.Fatalf("resolved_policies = %d policies, want %d", len(policies), len(names))
	}
	for i, p := range policies {
		if want := fmt.Sprintf(`{"count":%d}`, i); p["value"] != want {
			t.Errorf("policy %d value = %v, want %s", i, p["value"], want)
		}
	}
}

func TestChromeResolvedPoliciesRead_fewSchemasReadEachSchema(t *testing.T) {
	definitions, names, resolved := testChromeGeneratedSchemas(chromePolicySchemaPrefetchMin - 1)
	api := &fakeChromeResolveAPI{t: t, schemas: definitions, pages: map[string]*chromepolicy.GoogleChromePolicyVersionsV1ResolveResponse{
		"": {ResolvedPolicies: resolved},
	}}

	_, diags := testReadChromeResolvedPolicies(t, api, map[string]interface{}{
		"org_unit_id":          testChromePolicyOrgUnit,
		"policy_schema_filter": "chrome.users.*",
	})
	requireNoDiags(t, diags)

	if api.listCalls != 0 || api.schemaGets != len(names) {
		t.Errorf("list calls = %d, schema gets = %d, want 0 and %d", api.listCalls, api.schemaGets, len(names))
	}
}

func TestChromeResolvedPoliciesRead_listErrorReadsEachSchema(t *testing.T) {
	definitions, names, resolved := testChromeGeneratedSchemas(chromePolicySchemaPrefetchMin)
	api := &fakeChromeResolveAPI{t: t, schemas: definitions, listErr: true, pages: map[string]*chromepolicy.GoogleChromePolicyVersionsV1ResolveResponse{
		"": {ResolvedPolicies: resolved},
	}}

	d, diags := testReadChromeResolvedPolicies(t, api, map[string]interface{}{
		"org_unit_id":          testChromePolicyOrgUnit,
		"policy_schema_filter": "chrome.users.*",
	})
	requireNoDiags(t, diags)

	if api.listCalls != 1 || api.schemaGets != len(names) {
		t.Errorf("list calls = %d, schema gets = %d, want 1 and %d", api.listCalls, api.schemaGets, len(names))
	}
	if p := testResolvedPolicies(d); len(p) != len(names) || p[0]["value"] != `{"count":0}` {
		t.Errorf("resolved_policies = %#v", p)
	}
}

func TestChromeResolvedPoliciesRead_schemaErrorWarns(t *testing.T) {
	missing := "chrome.users.NotInTheFake"
	api := &fakeChromeResolveAPI{t: t, pages: map[string]*chromepolicy.GoogleChromePolicyVersionsV1ResolveResponse{
		"": {ResolvedPolicies: []*chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy{
			testResolvedPolicy(testChromePolicyConns, `{"maxConnectionsPerProxy":"33"}`, testChromePolicyOrgUnit),
			testResolvedPolicy(missing, `{"value":"1"}`, testChromePolicyOrgUnit),
			testResolvedPolicyWithKeys(missing, `{"value":"2"}`, map[string]string{"app_id": "chrome:a2"}),
			testResolvedPolicyWithKeys(missing, `{"value":"3"}`, map[string]string{"app_id": "chrome:a3"}),
		}},
	}}

	d, diags := testReadChromeResolvedPolicies(t, api, map[string]interface{}{
		"org_unit_id":          testChromePolicyOrgUnit,
		"policy_schema_filter": "chrome.users.*",
	})
	// One warning for the schema, which lists each affected policy. The schema is read once.
	testRequireRawValueWarning(t, diags, missing)
	for _, target := range []string{"no additional target keys", "app_id=chrome:a2", "app_id=chrome:a3"} {
		if !strings.Contains(diags[0].Detail, target) {
			t.Errorf("warning detail %q does not name %s", diags[0].Detail, target)
		}
	}
	if api.getCalls[missing] != 1 {
		t.Errorf("schema reads of %s = %d, want 1", missing, api.getCalls[missing])
	}

	policies := testResolvedPolicies(d)
	if len(policies) != 4 || policies[0]["value"] != `{"maxConnectionsPerProxy":33}` || policies[1]["value"] != `{"value":"1"}` ||
		policies[3]["value"] != `{"value":"3"}` {
		t.Errorf("resolved_policies = %#v, want the converted value and the raw values", policies)
	}
}

func testResolvedPolicyWithKeys(schemaName, value string, keys map[string]string) *chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy {
	resolved := testResolvedPolicy(schemaName, value, testChromePolicyOrgUnit)
	resolved.TargetKey.AdditionalTargetKeys = keys
	return resolved
}

func TestChromeResolvedPoliciesRead_convertErrorWarns(t *testing.T) {
	api := &fakeChromeResolveAPI{t: t, pages: map[string]*chromepolicy.GoogleChromePolicyVersionsV1ResolveResponse{
		"": {ResolvedPolicies: []*chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy{
			testResolvedPolicy(testChromePolicyConns, `{"maxConnectionsPerProxy":"not a number"}`, testChromePolicyOrgUnit),
		}},
	}}

	d, diags := testReadChromeResolvedPolicies(t, api, map[string]interface{}{
		"org_unit_id":          testChromePolicyOrgUnit,
		"policy_schema_filter": testChromePolicyConns,
	})
	testRequireRawValueWarning(t, diags, testChromePolicyConns)

	if p := testResolvedPolicies(d); len(p) != 1 || p[0]["value"] != `{"maxConnectionsPerProxy":"not a number"}` {
		t.Errorf("resolved_policies = %#v, want the raw value", p)
	}
}

func TestChromeResolvedPolicies_orgUnitIdIsNotAPath(t *testing.T) {
	validate := dataSourceChromeResolvedPolicies().Schema["org_unit_id"].ValidateFunc
	for value, wantErr := range map[string]bool{"id:03ph8a2z": false, "03ph8a2z": false, "/Sales": true, "": true} {
		if _, errs := validate(value, "org_unit_id"); (len(errs) > 0) != wantErr {
			t.Errorf("validate(%q) errors = %v, want error %v", value, errs, wantErr)
		}
	}
}

func TestChromeResolvedPoliciesRead_apiErrorFails(t *testing.T) {
	api := &fakeChromeResolveAPI{t: t, resolveErr: true}

	_, diags := testReadChromeResolvedPolicies(t, api, map[string]interface{}{
		"org_unit_id":          testChromePolicyOrgUnit,
		"policy_schema_filter": "chrome.users.*",
	})
	if !diags.HasError() {
		t.Fatalf("diags = %#v, want an error", diags)
	}
}

// TestPrefetchChromePolicySchemas_concurrentReads runs the prefetch while other goroutines read the same cache. Run it
// with -race.
func TestPrefetchChromePolicySchemas_concurrentReads(t *testing.T) {
	// The readers cache at most 8 schemas before the prefetch starts, so the prefetch always lists the schemas.
	definitions, names, resolved := testChromeGeneratedSchemas(chromePolicySchemaPrefetchMin + 8)
	api := &fakeChromeResolveAPI{t: t, schemas: definitions}
	schemas, diags := newChromePolicySchemaCache(newTestChromeHandlerClient(t, api))
	requireNoDiags(t, diags)

	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		prefetchChromePolicySchemas(ctx, schemas, resolved)
	}()
	for i := 0; i < 8; i++ {
		name := names[i%len(names)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, diags := schemas.types(ctx, name, diag.Error); len(diags) > 0 {
				t.Errorf("types(%s) diags = %#v", name, diags)
			}
		}()
	}
	wg.Wait()

	if api.listCalls == 0 {
		t.Error("the prefetch did not list the schemas")
	}
	for _, name := range names {
		if !schemas.cached(name) {
			t.Errorf("schema %s is not in the cache", name)
		}
	}
}

// A schema that a data source reads, or that the prefetch lists, goes in the schema store of the provider. Other data
// sources and the resource, with copies of the client, then use it from the store and do not read it again.
func TestChromePolicyDataSources_shareSchemaStore(t *testing.T) {
	definitions, names, resolved := testChromeGeneratedSchemas(chromePolicySchemaPrefetchMin)
	definitions[testChromePolicyConns] = testChromePolicySchemas[testChromePolicyConns]
	// The resolved policies do not have this schema, so the prefetch does not add it to the store.
	other := "chrome.users.Other"
	definitions[other] = testChromePolicySchemas[testChromePolicyConns]
	api := &fakeChromeResolveAPI{t: t, schemas: definitions, pages: map[string]*chromepolicy.GoogleChromePolicyVersionsV1ResolveResponse{
		"": {ResolvedPolicies: append(resolved, testResolvedPolicy(testChromePolicyConns, `{"maxConnectionsPerProxy":"33"}`, testChromePolicyOrgUnit))},
	}}
	client := newTestChromeHandlerClient(t, api)
	client.chromePolicySchemas = newChromePolicySchemaStore()
	ctx := context.Background()

	// The prefetch lists the generated schemas. The fake also lists testChromePolicyConns.
	r := dataSourceChromeResolvedPolicies()
	d := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{
		"org_unit_id":          testChromePolicyOrgUnit,
		"policy_schema_filter": "chrome.users.*",
	})
	readClient := *client
	requireNoDiags(t, r.ReadContext(ctx, d, &readClient))
	if api.listCalls == 0 || api.schemaGets != 0 {
		t.Fatalf("list calls = %d, schema gets = %v, want a listing and no gets", api.listCalls, api.getCalls)
	}

	// The schema data source gets a listed schema with a GET, and the result replaces the entry in the store.
	s := dataSourceChromePolicySchema()
	ds := schema.TestResourceDataRaw(t, s.Schema, map[string]interface{}{"schema_name": names[0]})
	schemaClient := *client
	requireNoDiags(t, s.ReadContext(ctx, ds, &schemaClient))
	if api.getCalls[names[0]] != 1 || api.schemaGets != 1 || ds.Id() != names[0] {
		t.Errorf("schema gets = %v, id = %q, want one get of %s", api.getCalls, ds.Id(), names[0])
	}

	// A schema that the schema data source reads is in the store for the resolved policies data source and the
	// resource.
	ds = schema.TestResourceDataRaw(t, s.Schema, map[string]interface{}{"schema_name": other})
	requireNoDiags(t, s.ReadContext(ctx, ds, &schemaClient))
	if api.getCalls[other] != 1 || api.schemaGets != 2 {
		t.Fatalf("gets of %s = %d, schema gets = %v, want 1", other, api.getCalls[other], api.getCalls)
	}
	resourceClient := *client
	schemas, diags := newChromePolicySchemaCache(&resourceClient)
	requireNoDiags(t, diags)
	for _, name := range []string{other, names[1], testChromePolicyConns} {
		if _, err := schemas.get(ctx, name); err != nil {
			t.Errorf("get(%s) = %v", name, err)
		}
	}
	if api.schemaGets != 2 || api.getCalls[other] != 1 || api.getCalls[names[0]] != 1 {
		t.Errorf("schema gets = %v, want the resource reads served from the store", api.getCalls)
	}
}

// An int64 value above 2^53 keeps all its digits, as in the resource.
func TestChromeResolvedPoliciesRead_largeIntegerIsExact(t *testing.T) {
	definitions, names, _ := testChromeGeneratedSchemas(1)
	api := &fakeChromeResolveAPI{t: t, schemas: definitions, pages: map[string]*chromepolicy.GoogleChromePolicyVersionsV1ResolveResponse{
		"": {ResolvedPolicies: []*chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy{
			testResolvedPolicy(names[0], `{"count":"9007199254740993"}`, testChromePolicyOrgUnit),
		}},
	}}

	d, diags := testReadChromeResolvedPolicies(t, api, map[string]interface{}{
		"org_unit_id":          testChromePolicyOrgUnit,
		"policy_schema_filter": names[0],
	})
	requireNoDiags(t, diags)

	policies := testResolvedPolicies(d)
	if len(policies) != 1 || policies[0]["value"] != `{"count":9007199254740993}` ||
		!reflect.DeepEqual(policies[0]["schema_values"], map[string]interface{}{"count": "9007199254740993"}) {
		t.Errorf("resolved_policies = %#v, want the exact integer", policies)
	}
}

// addMissing keeps an entry that the store has, and adds an entry that it does not have.
func TestChromePolicySchemaStore_addMissingKeepsExisting(t *testing.T) {
	store := newChromePolicySchemaStore()
	first := &chromepolicy.GoogleChromePolicyVersionsV1PolicySchema{SchemaName: "first"}
	store.set("customers/c/policySchemas/a", first)

	store.addMissing(map[string]*chromepolicy.GoogleChromePolicyVersionsV1PolicySchema{
		"customers/c/policySchemas/a": {SchemaName: "other"},
		"customers/c/policySchemas/b": {SchemaName: "new"},
	})

	if got, ok := store.lookup("customers/c/policySchemas/a"); !ok || got != first {
		t.Errorf("lookup(a) = %v, %v, want the first schema kept", got, ok)
	}
	if got, ok := store.lookup("customers/c/policySchemas/b"); !ok || got.SchemaName != "new" {
		t.Errorf("lookup(b) = %v, %v, want the new schema added", got, ok)
	}
}

// set replaces an entry that the store has.
func TestChromePolicySchemaStore_setReplaces(t *testing.T) {
	store := newChromePolicySchemaStore()
	store.set("p", &chromepolicy.GoogleChromePolicyVersionsV1PolicySchema{SchemaName: "old"})
	store.set("p", &chromepolicy.GoogleChromePolicyVersionsV1PolicySchema{SchemaName: "new"})
	if got, _ := store.lookup("p"); got.SchemaName != "new" {
		t.Errorf("lookup(p) = %v, want the new schema", got)
	}
}
