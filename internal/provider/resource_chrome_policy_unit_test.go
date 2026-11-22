// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"google.golang.org/api/chromepolicy/v1"
)

const (
	testChromePolicyCustomer   = "my_customer"
	testChromePolicyOrgUnit    = "ou1"
	testChromePolicyConns      = "chrome.users.MaxConnectionsPerProxy"
	testChromePolicyHomepage   = "chrome.users.HomepageLocation"
	testChromePolicyUrls       = "chrome.users.UrlBlocking"
	testChromePolicyBookmarks  = "chrome.users.ManagedBookmarksSetting"
	testChromePolicyShared     = "chrome.users.SharedFieldName"
	testChromePolicyPolicy     = "chrome.users.Policy"
	testChromePolicyNeverSeen  = -1
	testChromePolicyNoSource   = "-"
	testChromePolicyParent     = "orgunits/parent"
	testChromePolicyAppInstall = "chrome.users.apps.InstallType"
	testChromePolicyOrgOnly    = "chrome.devices.OrgUnitOnly"
)

// fakeChromePolicyAPI is a minimal Chrome Policy API for org units and groups.
// The values are stored by target and entry (see stateKey). An entry is the schema name, and for a target with
// additional target keys, the keys (see testChromePolicyEntry). A value on the default target (target, the org unit
// testChromePolicyOrgUnit unless a test sets it) has the entry as its key, so that tests can use the entry directly.
// Resolve returns the values of the requested target resource. If the request has additional target keys, it returns
// only the entry with those keys. Else it returns all entries of the schema, each with its own target key, as the API
// does for an app policy when the request has no app. With resolvePageSize, resolve returns pages of that size.
// resolvedTargetResource, if set, replaces the target resource in the target key of each resolved policy.
// With resolveIgnoresKeys, resolve returns all entries of the schema also for a request with additional target keys.
// A batchModify write becomes visible to resolve only after hiddenResolves resolve calls for that schema.
// With hiddenResolves set to testChromePolicyNeverSeen, a write never becomes visible.
// The first dropWrites batchModify calls return success but do not change a value.
// The batchModify calls with the numbers in failModifyCalls (from 1) fail, and the first call waits firstModifyDelay.
// modifyCanceled counts the first calls that the client canceled during that wait.
// If transform is set, it changes a value when the value becomes visible, as an API that normalizes values.
// sources has the source of a visible value by entry: a target resource, or testChromePolicyNoSource for no
// source key. Without an entry, the source is the target. A write that becomes visible has the target as source,
// or writeSource if it is set. A schema GET waits schemaDelay.
// requests has the method and the target of each write request, for example
// "groups:batchModify groups/g1 app_id=chrome:a". orderings has the group priority orderings by
// testChromePolicyOrderingKey. An ordering request needs a policy target key, and an update must list exactly the
// groups that have a visible value for the policy and the additional target keys (see orderingGroups).
// If fail is set, it is called for each POST request with the method (for example "policies:resolve" or
// "policies/groups:batchDelete"). If it returns a code that is not 0, the request fails with that code and message.
// With deleteUnsetFails, a groups:batchDelete request with a policy that is not set fails, and deletes nothing.
// A write sets only the fields in its update mask, as the API does, and modifyRequests has each write request.
// After a batchInherit, the next write of that schema starts from no value, also if a test sets a visible value again
// to simulate a stale read.
type fakeChromePolicyAPI struct {
	t *testing.T

	mu              sync.Mutex
	target          string
	visible         map[string]map[string]interface{}
	pending         map[string]map[string]interface{}
	pendingHidden   map[string]int
	hiddenResolves  int
	extraResolved   int
	dropWrites      int
	failModifyCalls map[int]bool
	transform       func(schemaName string, value map[string]interface{}) map[string]interface{}
	resolveCalls    int
	modifyCalls     int
	modifyRequests  []*chromepolicy.GoogleChromePolicyVersionsV1ModifyOrgUnitPolicyRequest
	inheritCalls    int
	inherited       []string
	reset           map[string]bool
	sources         map[string]string
	writeSource     string
	schemaDelay     time.Duration
	schemaCanceled  int32
	modifyCanceled  int32
	schemaGets      map[string]int
	requests        []string
	orderings       map[string][]string
	orderingUpdates int

	firstModifyDelay time.Duration
	modifyDelayed    int32
	// numbersAsNumbers makes resolve return numbers as JSON numbers, not as strings.
	numbersAsNumbers bool

	resolvePageSize        int
	resolvedTargetResource string
	resolveIgnoresKeys     bool
	fail                   func(method string) (int, string)
	deleteUnsetFails       bool
}

func newFakeChromePolicyAPI(t *testing.T) *fakeChromePolicyAPI {
	return &fakeChromePolicyAPI{
		t:             t,
		target:        "orgunits/" + testChromePolicyOrgUnit,
		visible:       map[string]map[string]interface{}{},
		pending:       map[string]map[string]interface{}{},
		pendingHidden: map[string]int{},
		schemaGets:    map[string]int{},
		orderings:     map[string][]string{},
	}
}

// testChromePolicyEntry returns the entry of the fake API for a schema and additional target keys. Without keys, the
// entry is the schema name.
func testChromePolicyEntry(schemaName string, keys map[string]string) string {
	if len(keys) == 0 {
		return schemaName
	}
	return schemaName + "|" + testChromePolicyKeysString(keys)
}

// testChromePolicyEntryKeys returns the schema name and the additional target keys of an entry.
func testChromePolicyEntryKeys(entry string) (string, map[string]string) {
	parts := strings.SplitN(entry, "|", 2)
	if len(parts) == 1 {
		return entry, nil
	}
	keys := map[string]string{}
	for _, pair := range strings.Split(parts[1], ",") {
		kv := strings.SplitN(pair, "=", 2)
		keys[kv[0]] = kv[1]
	}
	return parts[0], keys
}

func testChromePolicyKeysString(keys map[string]string) string {
	var pairs []string
	for k, v := range keys {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

// testChromePolicyTargetString returns a target key as "<target resource>" or "<target resource> <keys>".
func testChromePolicyTargetString(key *chromepolicy.GoogleChromePolicyVersionsV1PolicyTargetKey) string {
	if key == nil {
		return "<nil>"
	}
	if len(key.AdditionalTargetKeys) == 0 {
		return key.TargetResource
	}
	return key.TargetResource + " " + testChromePolicyKeysString(key.AdditionalTargetKeys)
}

// testChromePolicyOrderingKey returns the key of a group priority ordering in the fake API.
func testChromePolicyOrderingKey(schemaName, namespace string, key *chromepolicy.GoogleChromePolicyVersionsV1PolicyTargetKey) string {
	return schemaName + ";" + namespace + ";" + testChromePolicyTargetString(key)
}

func testChromePolicyMessage(name string, fields ...*chromepolicy.Proto2FieldDescriptorProto) *chromepolicy.Proto2DescriptorProto {
	return &chromepolicy.Proto2DescriptorProto{Name: name, Field: fields}
}

func testChromePolicyField(name, fieldType, label, typeName string) *chromepolicy.Proto2FieldDescriptorProto {
	return &chromepolicy.Proto2FieldDescriptorProto{Name: name, Type: fieldType, Label: label, TypeName: typeName}
}

// testChromePolicySchemas are the policy schema definitions that the fake API serves.
var testChromePolicySchemas = map[string]*chromepolicy.Proto2FileDescriptorProto{
	testChromePolicyConns: {
		Package: "chrome.users",
		MessageType: []*chromepolicy.Proto2DescriptorProto{
			testChromePolicyMessage("MaxConnectionsPerProxy", testChromePolicyField("maxConnectionsPerProxy", "TYPE_INT64", "LABEL_OPTIONAL", "")),
		},
	},
	testChromePolicyHomepage: {
		Package: "chrome.users",
		MessageType: []*chromepolicy.Proto2DescriptorProto{
			testChromePolicyMessage("HomepageLocation", testChromePolicyField("homepageLocation", "TYPE_STRING", "LABEL_OPTIONAL", "")),
		},
	},
	testChromePolicyUrls: {
		Package: "chrome.users",
		MessageType: []*chromepolicy.Proto2DescriptorProto{
			testChromePolicyMessage("UrlBlocking",
				testChromePolicyField("maxUrls", "TYPE_INT32", "LABEL_OPTIONAL", ""),
				testChromePolicyField("urlBlocklist", "TYPE_STRING", "LABEL_REPEATED", ""),
			),
		},
	},
	testChromePolicyBookmarks: {
		Package: "chrome.users",
		MessageType: []*chromepolicy.Proto2DescriptorProto{{
			Name: "ManagedBookmarksSetting",
			Field: []*chromepolicy.Proto2FieldDescriptorProto{
				// A fully qualified type name.
				testChromePolicyField("managedBookmarks", "TYPE_MESSAGE", "LABEL_OPTIONAL", ".chrome.users.ManagedBookmarksSetting.ManagedBookmarks"),
				// A type that is not in the schema definition.
				testChromePolicyField("refreshDelay", "TYPE_MESSAGE", "LABEL_OPTIONAL", ".google.protobuf.Duration"),
			},
			NestedType: []*chromepolicy.Proto2DescriptorProto{
				{
					Name: "ManagedBookmarks",
					Field: []*chromepolicy.Proto2FieldDescriptorProto{
						testChromePolicyField("toplevelName", "TYPE_STRING", "LABEL_OPTIONAL", ""),
						// A relative type name, found in the parent scope.
						testChromePolicyField("entries", "TYPE_MESSAGE", "LABEL_REPEATED", "BookmarkEntry"),
						// A map field.
						testChromePolicyField("labels", "TYPE_MESSAGE", "LABEL_REPEATED", "LabelsEntry"),
						// Not a map field: the map entry name of tags is TagsEntry.
						testChromePolicyField("tags", "TYPE_MESSAGE", "LABEL_REPEATED", "LabelsEntry"),
					},
					NestedType: []*chromepolicy.Proto2DescriptorProto{
						testChromePolicyMessage("LabelsEntry",
							testChromePolicyField("key", "TYPE_STRING", "LABEL_OPTIONAL", ""),
							testChromePolicyField("value", "TYPE_INT64", "LABEL_OPTIONAL", ""),
						),
					},
				},
				testChromePolicyMessage("BookmarkEntry",
					testChromePolicyField("name", "TYPE_STRING", "LABEL_OPTIONAL", ""),
					testChromePolicyField("url", "TYPE_STRING", "LABEL_OPTIONAL", ""),
					testChromePolicyField("visits", "TYPE_INT64", "LABEL_OPTIONAL", ""),
					testChromePolicyField("kind", "TYPE_ENUM", "LABEL_OPTIONAL", "Kind"),
					testChromePolicyField("weight", "TYPE_DOUBLE", "LABEL_OPTIONAL", ""),
				),
			},
			EnumType: []*chromepolicy.Proto2EnumDescriptorProto{{
				Name: "Kind",
				Value: []*chromepolicy.Proto2EnumValueDescriptorProto{
					{Name: "KIND_UNSPECIFIED", Number: 0},
					{Name: "KIND_URL", Number: 1},
					{Name: "KIND_FOLDER", Number: 2},
				},
			}},
		}},
	},
	// An app policy, which needs the additional target key app_id.
	testChromePolicyAppInstall: {
		Package: "chrome.users.apps",
		MessageType: []*chromepolicy.Proto2DescriptorProto{
			testChromePolicyMessage("InstallType",
				testChromePolicyField("appInstallType", "TYPE_STRING", "LABEL_OPTIONAL", ""),
				testChromePolicyField("installationUrl", "TYPE_STRING", "LABEL_OPTIONAL", ""),
			),
		},
	},
	// A policy that cannot be set on a group.
	testChromePolicyOrgOnly: {
		Package: "chrome.devices",
		MessageType: []*chromepolicy.Proto2DescriptorProto{
			testChromePolicyMessage("OrgUnitOnly", testChromePolicyField("enabled", "TYPE_BOOL", "LABEL_OPTIONAL", "")),
		},
	},
	// Two top-level messages with a field of the same name and different types.
	testChromePolicyShared: {
		Package: "chrome.users",
		MessageType: []*chromepolicy.Proto2DescriptorProto{
			testChromePolicyMessage("SharedFieldName", testChromePolicyField("shared", "TYPE_INT64", "LABEL_OPTIONAL", "")),
			testChromePolicyMessage("Other", testChromePolicyField("shared", "TYPE_STRING", "LABEL_OPTIONAL", "")),
		},
	},
	// The policy message is not the first top-level message.
	testChromePolicyPolicy: {
		Package: "chrome.users",
		MessageType: []*chromepolicy.Proto2DescriptorProto{
			testChromePolicyMessage("Helper", testChromePolicyField("value", "TYPE_STRING", "LABEL_OPTIONAL", "")),
			testChromePolicyMessage("Policy",
				testChromePolicyField("value", "TYPE_INT64", "LABEL_OPTIONAL", ""),
				testChromePolicyField("enabled", "TYPE_BOOL", "LABEL_OPTIONAL", ""),
			),
		},
	},
}

// testChromePolicySchemaKeyNames and testChromePolicySchemaTargets are the additional target key names and the valid
// target resources of the policy schemas that the fake API serves. A schema without an entry has none.
var (
	testChromePolicySchemaKeyNames = map[string][]*chromepolicy.GoogleChromePolicyVersionsV1AdditionalTargetKeyName{
		testChromePolicyAppInstall: {{Key: "app_id", KeyDescription: "The app."}},
	}
	testChromePolicySchemaTargets = map[string][]string{
		testChromePolicyAppInstall: {"ORG_UNIT", "GROUP"},
		testChromePolicyOrgOnly:    {"ORG_UNIT"},
	}
)

func (f *fakeChromePolicyAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, ":batchModify") {
		// The server sees that the client closed the connection only after the request body is read.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			f.t.Errorf("read request body: %v", err)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		// Only the first write waits. The wait stops if the request is canceled.
		if atomic.CompareAndSwapInt32(&f.modifyDelayed, 0, 1) {
			select {
			case <-time.After(f.firstModifyDelay):
			case <-r.Context().Done():
				atomic.AddInt32(&f.modifyCanceled, 1)
				return
			}
		}
	}
	if strings.Contains(r.URL.Path, "/policySchemas/") && f.schemaDelay > 0 {
		select {
		case <-time.After(f.schemaDelay):
		case <-r.Context().Done():
			atomic.AddInt32(&f.schemaCanceled, 1)
			return
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	customerPath := "/v1/customers/" + testChromePolicyCustomer

	if f.fail != nil && r.Method == http.MethodPost {
		if code, message := f.fail(strings.TrimPrefix(r.URL.Path, customerPath+"/")); code != 0 {
			body, _ := json.Marshal(map[string]interface{}{"error": map[string]interface{}{"code": code, "message": message}})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = w.Write(body)
			return
		}
	}

	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, customerPath+"/policySchemas/"):
		name := strings.TrimPrefix(r.URL.Path, customerPath+"/policySchemas/")
		definition, ok := testChromePolicySchemas[name]
		if !ok {
			http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
			return
		}
		f.schemaGets[name]++
		f.writeJSON(w, &chromepolicy.GoogleChromePolicyVersionsV1PolicySchema{
			SchemaName:               name,
			Definition:               definition,
			AdditionalTargetKeyNames: testChromePolicySchemaKeyNames[name],
			ValidTargetResources:     testChromePolicySchemaTargets[name],
		})

	case r.Method == http.MethodPost && r.URL.Path == customerPath+"/policies:resolve":
		var req chromepolicy.GoogleChromePolicyVersionsV1ResolveRequest
		f.readJSON(r, &req)
		f.resolveCalls++

		name := req.PolicySchemaFilter
		target := req.PolicyTargetKey.TargetResource
		for _, entry := range f.entries(f.pending, target, name, nil) {
			value := f.pending[entry]
			if f.pendingHidden[entry] == 0 {
				if f.transform != nil {
					value = f.transform(name, value)
				}
				f.visible[entry] = value
				delete(f.sources, entry)
				if f.writeSource != "" {
					if f.sources == nil {
						f.sources = map[string]string{}
					}
					f.sources[entry] = f.writeSource
				}
				delete(f.pending, entry)
			} else if f.pendingHidden[entry] > 0 {
				f.pendingHidden[entry]--
			}
		}

		filter := req.PolicyTargetKey.AdditionalTargetKeys
		if f.resolveIgnoresKeys {
			filter = nil
		}
		var resolved []*chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy
		for _, entry := range f.entries(f.visible, target, name, filter) {
			_, keys := testChromePolicyEntryKeys(f.splitStateKey(entry))
			targetKey := &chromepolicy.GoogleChromePolicyVersionsV1PolicyTargetKey{
				TargetResource:       target,
				AdditionalTargetKeys: keys,
			}
			if f.resolvedTargetResource != "" {
				targetKey.TargetResource = f.resolvedTargetResource
			}
			for i := 0; i <= f.extraResolved; i++ {
				sourceKey := targetKey
				switch source := f.sources[entry]; source {
				case "":
				case testChromePolicyNoSource:
					sourceKey = nil
				default:
					sourceKey = &chromepolicy.GoogleChromePolicyVersionsV1PolicyTargetKey{TargetResource: source, AdditionalTargetKeys: keys}
				}
				resolved = append(resolved, &chromepolicy.GoogleChromePolicyVersionsV1ResolvedPolicy{
					TargetKey: targetKey,
					SourceKey: sourceKey,
					Value: &chromepolicy.GoogleChromePolicyVersionsV1PolicyValue{
						PolicySchema: name,
						Value:        f.apiValue(f.visible[entry]),
					},
				})
			}
		}

		resp := &chromepolicy.GoogleChromePolicyVersionsV1ResolveResponse{ResolvedPolicies: resolved}
		if f.resolvePageSize > 0 {
			start, _ := strconv.Atoi(req.PageToken)
			end := start + f.resolvePageSize
			if end < len(resolved) {
				resp.NextPageToken = strconv.Itoa(end)
			} else {
				end = len(resolved)
			}
			resp.ResolvedPolicies = resolved[start:end]
		}
		f.writeJSON(w, resp)

	case r.Method == http.MethodPost && (r.URL.Path == customerPath+"/policies/orgunits:batchModify" || r.URL.Path == customerPath+"/policies/groups:batchModify"):
		// The request types of org units and groups have the same fields.
		var req chromepolicy.GoogleChromePolicyVersionsV1BatchModifyOrgUnitPoliciesRequest
		f.readJSON(r, &req)
		f.modifyCalls++
		f.modifyRequests = append(f.modifyRequests, req.Requests...)
		method := strings.TrimPrefix(r.URL.Path, customerPath+"/policies/")
		for _, mod := range req.Requests {
			f.requests = append(f.requests, method+" "+testChromePolicyTargetString(mod.PolicyTargetKey))
		}
		if f.failModifyCalls[f.modifyCalls] {
			http.Error(w, `{"error":{"code":400,"message":"write failed"}}`, http.StatusBadRequest)
			return
		}
		if f.dropWrites > 0 {
			f.dropWrites--
			f.writeJSON(w, struct{}{})
			return
		}

		for _, mod := range req.Requests {
			var value map[string]interface{}
			if err := unmarshalChromePolicyJSON(mod.PolicyValue.Value, &value); err != nil {
				f.t.Errorf("batchModify value: %v", err)
			}
			entry := f.stateKey(mod.PolicyTargetKey.TargetResource, testChromePolicyEntry(mod.PolicyValue.PolicySchema, mod.PolicyTargetKey.AdditionalTargetKeys))

			// The fields in the mask change the pending value, or else the visible value. A field in the mask that
			// is not in the value is cleared.
			current, ok := f.pending[entry]
			if !ok && !f.reset[entry] {
				current = f.visible[entry]
			}
			delete(f.reset, entry)
			merged := map[string]interface{}{}
			for k, v := range current {
				merged[k] = v
			}
			for _, k := range strings.Split(mod.UpdateMask, ",") {
				if v, ok := value[k]; ok {
					merged[k] = v
				} else {
					delete(merged, k)
				}
			}

			// A write again while a write is not visible yet does not restart the delay.
			if _, ok := f.pending[entry]; !ok {
				f.pendingHidden[entry] = f.hiddenResolves
			}
			f.pending[entry] = merged
		}
		f.writeJSON(w, struct{}{})

	case r.Method == http.MethodPost && (r.URL.Path == customerPath+"/policies/orgunits:batchInherit" || r.URL.Path == customerPath+"/policies/groups:batchDelete"):
		// The request types of org units and groups have the same fields.
		var req chromepolicy.GoogleChromePolicyVersionsV1BatchInheritOrgUnitPoliciesRequest
		f.readJSON(r, &req)
		f.inheritCalls++
		method := strings.TrimPrefix(r.URL.Path, customerPath+"/policies/")
		if f.deleteUnsetFails && method == "groups:batchDelete" {
			for _, inherit := range req.Requests {
				entry := f.stateKey(inherit.PolicyTargetKey.TargetResource, testChromePolicyEntry(inherit.PolicySchema, inherit.PolicyTargetKey.AdditionalTargetKeys))
				_, visible := f.visible[entry]
				_, pending := f.pending[entry]
				if !visible && !pending {
					f.requests = append(f.requests, method+" "+testChromePolicyTargetString(inherit.PolicyTargetKey)+" failed")
					http.Error(w, `{"error":{"code":400,"message":"Policy `+inherit.PolicySchema+` not found on the group"}}`, http.StatusBadRequest)
					return
				}
			}
		}

		for _, inherit := range req.Requests {
			f.requests = append(f.requests, method+" "+testChromePolicyTargetString(inherit.PolicyTargetKey))
			entry := f.stateKey(inherit.PolicyTargetKey.TargetResource, testChromePolicyEntry(inherit.PolicySchema, inherit.PolicyTargetKey.AdditionalTargetKeys))
			f.inherited = append(f.inherited, entry)
			if f.reset == nil {
				f.reset = map[string]bool{}
			}
			f.reset[entry] = true
			delete(f.visible, entry)
			delete(f.pending, entry)
		}
		f.writeJSON(w, struct{}{})

	case r.Method == http.MethodPost && r.URL.Path == customerPath+"/policies/groups:updateGroupPriorityOrdering":
		var req chromepolicy.GoogleChromePolicyVersionsV1UpdateGroupPriorityOrderingRequest
		f.readJSON(r, &req)
		f.orderingUpdates++
		f.requests = append(f.requests, "groups:updateGroupPriorityOrdering "+testChromePolicyTargetString(req.PolicyTargetKey))
		if req.PolicyTargetKey == nil {
			http.Error(w, `{"error":{"code":400,"message":"policyTargetKey is required"}}`, http.StatusBadRequest)
			return
		}
		if len(req.GroupIds) == 0 {
			http.Error(w, `{"error":{"code":400,"message":"Request must have at least one Group ID"}}`, http.StatusBadRequest)
			return
		}
		groups := f.orderingGroups(req.PolicySchema, req.PolicyNamespace, req.PolicyTargetKey)
		if len(groups) == 0 {
			http.Error(w, `{"error":{"code":400,"message":"The policy is not configured on any Groups"}}`, http.StatusBadRequest)
			return
		}
		sorted := append([]string(nil), req.GroupIds...)
		sort.Strings(sorted)
		if !reflect.DeepEqual(sorted, groups) {
			http.Error(w, `{"error":{"code":400,"message":"The request must include all group IDs that are applied on the app"}}`, http.StatusBadRequest)
			return
		}
		f.orderings[testChromePolicyOrderingKey(req.PolicySchema, req.PolicyNamespace, req.PolicyTargetKey)] = req.GroupIds
		f.writeJSON(w, struct{}{})

	case r.Method == http.MethodPost && r.URL.Path == customerPath+"/policies/groups:listGroupPriorityOrdering":
		var req chromepolicy.GoogleChromePolicyVersionsV1ListGroupPriorityOrderingRequest
		f.readJSON(r, &req)
		if req.PolicyTargetKey == nil {
			http.Error(w, `{"error":{"code":400,"message":"policyTargetKey is required"}}`, http.StatusBadRequest)
			return
		}
		groups := f.orderingGroups(req.PolicySchema, req.PolicyNamespace, req.PolicyTargetKey)
		if len(groups) == 0 {
			http.Error(w, `{"error":{"code":400,"message":"The policy is not configured on any Groups"}}`, http.StatusBadRequest)
			return
		}
		groupIds, ok := f.orderings[testChromePolicyOrderingKey(req.PolicySchema, req.PolicyNamespace, req.PolicyTargetKey)]
		if !ok {
			groupIds = groups
		}
		f.writeJSON(w, &chromepolicy.GoogleChromePolicyVersionsV1ListGroupPriorityOrderingResponse{
			GroupIds:        groupIds,
			PolicySchema:    req.PolicySchema,
			PolicyNamespace: req.PolicyNamespace,
			PolicyTargetKey: req.PolicyTargetKey,
		})

	default:
		f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"error":{"code":400,"message":"unexpected request"}}`, http.StatusBadRequest)
	}
}

// apiValue encodes a value as the API does: numbers, also in lists and nested messages, are returned as strings.
func (f *fakeChromePolicyAPI) apiValue(value map[string]interface{}) []byte {
	if f.numbersAsNumbers {
		b, err := json.Marshal(value)
		if err != nil {
			f.t.Errorf("marshal value: %v", err)
		}
		return b
	}
	b, err := json.Marshal(testChromePolicyStringNumbers(value))
	if err != nil {
		f.t.Errorf("marshal value: %v", err)
	}
	return b
}

func testChromePolicyStringNumbers(v interface{}) interface{} {
	switch v := v.(type) {
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	case []interface{}:
		out := make([]interface{}, len(v))
		for i, elem := range v {
			out[i] = testChromePolicyStringNumbers(elem)
		}
		return out
	case map[string]interface{}:
		out := map[string]interface{}{}
		for k, elem := range v {
			out[k] = testChromePolicyStringNumbers(elem)
		}
		return out
	default:
		return v
	}
}

func (f *fakeChromePolicyAPI) readJSON(r *http.Request, v interface{}) {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		f.t.Errorf("decode %s request: %v", r.URL.Path, err)
	}
}

func (f *fakeChromePolicyAPI) writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		f.t.Errorf("encode response: %v", err)
	}
}

// orderingGroups returns the IDs of the groups, in order, that have a visible value for the policy schema (or for a
// schema in the namespace) with the additional target keys of key.
func (f *fakeChromePolicyAPI) orderingGroups(policySchema, namespace string, key *chromepolicy.GoogleChromePolicyVersionsV1PolicyTargetKey) []string {
	var groups []string
	for stateKey := range f.visible {
		target := f.stateTarget(stateKey)
		name, keys := testChromePolicyEntryKeys(f.splitStateKey(stateKey))
		if !strings.HasPrefix(target, "groups/") || testChromePolicyKeysString(keys) != testChromePolicyKeysString(key.AdditionalTargetKeys) {
			continue
		}
		group := strings.TrimPrefix(target, "groups/")
		if (name == policySchema || (policySchema == "" && strings.HasPrefix(name, namespace+"."))) && !stringInSlice(groups, group) {
			groups = append(groups, group)
		}
	}
	sort.Strings(groups)
	return groups
}

// stateKey returns the key of a value of the fake API: the entry on the default target, or else
// "<target resource>@@<entry>".
func (f *fakeChromePolicyAPI) stateKey(targetResource, entry string) string {
	if targetResource == f.target {
		return entry
	}
	return targetResource + "@@" + entry
}

// splitStateKey returns the entry of a key of the fake API.
func (f *fakeChromePolicyAPI) splitStateKey(key string) string {
	if i := strings.Index(key, "@@"); i >= 0 {
		return key[i+2:]
	}
	return key
}

// stateTarget returns the target resource of a key of the fake API.
func (f *fakeChromePolicyAPI) stateTarget(key string) string {
	if i := strings.Index(key, "@@"); i >= 0 {
		return key[:i]
	}
	return f.target
}

// entries returns the keys in values for targetResource and schemaName, in order. If keys is not empty, it returns
// only the entry with these additional target keys.
func (f *fakeChromePolicyAPI) entries(values map[string]map[string]interface{}, targetResource, schemaName string, keys map[string]string) []string {
	var entries []string
	for key := range values {
		if f.stateTarget(key) != targetResource {
			continue
		}
		name, entryKeys := testChromePolicyEntryKeys(f.splitStateKey(key))
		if name != schemaName || (len(keys) > 0 && testChromePolicyKeysString(keys) != testChromePolicyKeysString(entryKeys)) {
			continue
		}
		entries = append(entries, key)
	}
	sort.Strings(entries)
	return entries
}

// modifyMasks returns the update masks of the write requests for schemaName, in order.
func (f *fakeChromePolicyAPI) modifyMasks(schemaName string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var masks []string
	for _, req := range f.modifyRequests {
		if req.PolicyValue.PolicySchema == schemaName {
			masks = append(masks, req.UpdateMask)
		}
	}
	return masks
}

// setVisible sets the value of an entry on the default target.
func (f *fakeChromePolicyAPI) setVisible(entry string, value map[string]interface{}) {
	f.setVisibleOn(f.target, entry, value)
}

// setVisibleOn sets the value of an entry on a target resource.
func (f *fakeChromePolicyAPI) setVisibleOn(targetResource, entry string, value map[string]interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.visible[f.stateKey(targetResource, entry)] = value
}

// chromePolicyRedirectTransport sends every request to the test server instead of the Google API host.
type chromePolicyRedirectTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (rt chromePolicyRedirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme = rt.target.Scheme
	r.URL.Host = rt.target.Host
	r.Host = rt.target.Host
	return rt.base.RoundTrip(r)
}

func newTestChromePolicyClient(t *testing.T, api *fakeChromePolicyAPI) *apiClient {
	ts := httptest.NewServer(api)
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

func testChromePolicyResource(timeout time.Duration) *schema.Resource {
	r := resourceChromePolicy()
	r.Timeouts = &schema.ResourceTimeout{
		Create: schema.DefaultTimeout(timeout),
		Update: schema.DefaultTimeout(timeout),
	}
	return r
}

func testChromePolicyData(t *testing.T, r *schema.Resource, id string, policies []interface{}) *schema.ResourceData {
	d := r.Data(nil)
	d.SetId(id)
	if err := d.Set("org_unit_id", testChromePolicyOrgUnit); err != nil {
		t.Fatal(err)
	}
	if err := d.Set("policies", policies); err != nil {
		t.Fatal(err)
	}
	return d
}

func testChromePolicy(schemaName, key, jsonValue string) map[string]interface{} {
	return map[string]interface{}{
		"schema_name":   schemaName,
		"schema_values": map[string]interface{}{key: jsonValue},
	}
}

func testChromePolicyValues(t *testing.T, d *schema.ResourceData) map[string]map[string]interface{} {
	t.Helper()
	out := map[string]map[string]interface{}{}
	for _, p := range d.Get("policies").([]interface{}) {
		policy := p.(map[string]interface{})
		out[policy["schema_name"].(string)] = policy["schema_values"].(map[string]interface{})
	}
	return out
}

func requireNoDiags(t *testing.T, diags diag.Diagnostics) {
	t.Helper()
	if len(diags) > 0 {
		t.Fatalf("unexpected diagnostics: %#v", diags)
	}
}

func TestChromePolicyRead_missingPolicyIsDrift(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyConns, map[string]interface{}{"maxConnectionsPerProxy": float64(33)})
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	d := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		// The first policy is missing, so the index of the second policy changes.
		testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://example.com"`),
		// An integer as a decimal string.
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", `"33"`),
	})

	diags := r.ReadContext(context.Background(), d, client)

	if diags.HasError() || len(diags) != 1 || diags[0].Severity != diag.Warning || !strings.Contains(diags[0].Summary, testChromePolicyHomepage) {
		t.Fatalf("diagnostics = %#v, want one warning that names %s", diags, testChromePolicyHomepage)
	}
	if d.Id() != testChromePolicyOrgUnit {
		t.Fatalf("resource was removed from state, ID = %q", d.Id())
	}
	got := testChromePolicyValues(t, d)
	if len(got) != 1 || got[testChromePolicyConns]["maxConnectionsPerProxy"] != `"33"` {
		t.Fatalf("policies in state = %#v, want only %s with the configured form \"33\"", got, testChromePolicyConns)
	}
}

func TestChromePolicyRead_noPoliciesSetRemovesResource(t *testing.T) {
	t.Parallel()

	client := newTestChromePolicyClient(t, newFakeChromePolicyAPI(t))

	r := testChromePolicyResource(time.Minute)
	d := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
	})

	diags := r.ReadContext(context.Background(), d, client)

	if diags.HasError() || len(diags) != 1 || !strings.Contains(diags[0].Summary, testChromePolicyConns) {
		t.Fatalf("diagnostics = %#v, want one warning that names %s", diags, testChromePolicyConns)
	}
	if d.Id() != "" {
		t.Fatalf("ID = %q, want the resource removed from state", d.Id())
	}
}

func TestChromePolicyRead_tooManyResolvedPoliciesIsError(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.extraResolved = 1
	api.setVisible(testChromePolicyConns, map[string]interface{}{"maxConnectionsPerProxy": float64(33)})
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	d := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
	})

	diags := r.ReadContext(context.Background(), d, client)
	if !diags.HasError() {
		t.Fatalf("expected an error, got %#v", diags)
	}
	if want := "unexpected number of resolved policies for schema " + testChromePolicyConns; !strings.Contains(diags[0].Summary, want) {
		t.Fatalf("error = %q, want it to contain %q", diags[0].Summary, want)
	}
}

func TestChromePolicyCreate_waitsForDelayedVisibility(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	// An earlier value is visible until the new write becomes visible.
	api.setVisible(testChromePolicyConns, map[string]interface{}{"maxConnectionsPerProxy": float64(10)})
	api.hiddenResolves = 2
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	d := testChromePolicyData(t, r, "", []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
	})

	requireNoDiags(t, r.CreateContext(context.Background(), d, client))

	if d.Id() != testChromePolicyOrgUnit {
		t.Fatalf("ID = %q, want %q", d.Id(), testChromePolicyOrgUnit)
	}
	if got := testChromePolicyValues(t, d)[testChromePolicyConns]["maxConnectionsPerProxy"]; got != "33" {
		t.Fatalf("maxConnectionsPerProxy in state = %v, want 33", got)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.modifyCalls != 1 {
		t.Errorf("batchModify calls = %d, want 1", api.modifyCalls)
	}
	// Two hidden resolves, one that shows the value, and one from the final read.
	if api.resolveCalls < 4 {
		t.Errorf("resolve calls = %d, want at least 4", api.resolveCalls)
	}
	// Create gets the schema once for the validation, all poll rounds, and the final read.
	if got := api.schemaGets[testChromePolicyConns]; got != 1 {
		t.Errorf("schema GET calls = %d, want 1", got)
	}
}

func TestChromePolicyCreate_timeoutWarnsAndKeepsState(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.hiddenResolves = testChromePolicyNeverSeen
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(3 * time.Second)
	d := testChromePolicyData(t, r, "", []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
	})

	diags := r.CreateContext(context.Background(), d, client)

	if diags.HasError() {
		t.Fatalf("expected no error, got %#v", diags)
	}
	if len(diags) != 1 || diags[0].Severity != diag.Warning {
		t.Fatalf("diagnostics = %#v, want one warning", diags)
	}
	if !strings.Contains(diags[0].Detail, testChromePolicyConns) {
		t.Errorf("warning detail = %q, want it to name %s", diags[0].Detail, testChromePolicyConns)
	}
	if d.Id() != testChromePolicyOrgUnit {
		t.Fatalf("ID = %q, want %q", d.Id(), testChromePolicyOrgUnit)
	}
	if got := testChromePolicyValues(t, d)[testChromePolicyConns]["maxConnectionsPerProxy"]; got != "33" {
		t.Fatalf("maxConnectionsPerProxy in state = %v, want the configured value 33", got)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	// The write is sent again once after half of the timeout.
	if api.modifyCalls != 2 {
		t.Errorf("batchModify calls = %d, want 2", api.modifyCalls)
	}
}

func TestValidatePolicyFieldValueType_numbers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		fieldType string
		value     interface{}
		want      bool
	}{
		{"TYPE_INT32", float64(5), true},
		{"TYPE_INT32", float64(-2147483648), true},
		{"TYPE_INT32", float64(2147483647), true},
		{"TYPE_INT32", float64(2147483648), false},
		{"TYPE_INT32", float64(-2147483649), false},
		{"TYPE_INT32", 5.5, false},
		{"TYPE_SINT32", float64(-5), true},
		{"TYPE_SFIXED32", float64(-5), true},
		{"TYPE_UINT32", float64(4294967295), true},
		{"TYPE_UINT32", float64(4294967296), false},
		{"TYPE_UINT32", float64(-1), false},
		{"TYPE_FIXED32", float64(7), true},
		{"TYPE_FIXED32", float64(-7), false},
		{"TYPE_INT64", float64(33), true},
		{"TYPE_INT64", float64(-33), true},
		{"TYPE_INT64", 33.5, false},
		{"TYPE_INT64", 1e19, false},
		{"TYPE_SINT64", float64(-1), true},
		{"TYPE_SFIXED64", float64(-1), true},
		{"TYPE_UINT64", 1e19, true},
		{"TYPE_UINT64", 2e19, false},
		{"TYPE_UINT64", float64(-1), false},
		{"TYPE_FIXED64", float64(-1), false},
		{"TYPE_INT64", "33", true},
		{"TYPE_INT64", "-33", true},
		{"TYPE_INT64", "33.5", false},
		{"TYPE_INT64", "9223372036854775808", false},
		{"TYPE_UINT64", "18446744073709551615", true},
		{"TYPE_UINT64", "-1", false},
		{"TYPE_INT32", "2147483648", false},
		{"TYPE_INT32", "x", false},
		{"TYPE_ENUM", "KIND_URL", true},
		{"TYPE_ENUM", float64(1), true},
		{"TYPE_ENUM", 1.5, false},
		{"TYPE_ENUM", true, false},
		{"TYPE_INT32", true, false},
		{"TYPE_FLOAT", 1.5, true},
		{"TYPE_FLOAT", 1e39, false},
		{"TYPE_DOUBLE", 1e39, true},
		{"TYPE_DOUBLE", "1.5", true},
		{"TYPE_DOUBLE", "-2.5e3", true},
		{"TYPE_DOUBLE", "NaN", true},
		{"TYPE_DOUBLE", "Infinity", true},
		{"TYPE_DOUBLE", "-Infinity", true},
		{"TYPE_DOUBLE", "inf", false},
		{"TYPE_DOUBLE", "0x10", false},
		{"TYPE_DOUBLE", "abc", false},
		{"TYPE_FLOAT", "1.5", true},
		{"TYPE_FLOAT", "1e39", false},
		{"TYPE_FLOAT", "NaN", true},
		{"TYPE_INT64", "1e3", true},
		{"TYPE_INT64", "7.0", true},
		{"TYPE_INT64", "1.5e0", false},
		{"TYPE_INT64", "0x10", false},
		{"TYPE_INT32", "3e9", false},
		{"TYPE_BOOL", true, true},
		{"TYPE_BOOL", false, true},
		{"TYPE_BOOL", "true", true},
		{"TYPE_BOOL", "false", true},
		{"TYPE_BOOL", "1", false},
		{"TYPE_BOOL", "t", false},
		{"TYPE_BOOL", "TRUE", false},
		{"TYPE_BOOL", float64(1), false},
		{"TYPE_STRING", "x", true},
		{"TYPE_STRING", float64(1), false},
		// Numbers decoded as json.Number.
		{"TYPE_STRING", json.Number("1"), false},
		{"TYPE_BYTES", json.Number("1"), false},
		{"TYPE_BOOL", json.Number("1"), false},
		{"TYPE_ENUM", json.Number("1"), true},
		{"TYPE_ENUM", json.Number("1.5"), false},
		{"TYPE_INT64", json.Number("9007199254740993"), true},
		{"TYPE_INT64", json.Number("1.5"), false},
		{"TYPE_INT32", json.Number("2147483648"), false},
		{"TYPE_UINT64", json.Number("18446744073709551615"), true},
		{"TYPE_DOUBLE", json.Number("1.5"), true},
		{"TYPE_FLOAT", json.Number("1e39"), false},
	}

	for _, test := range tests {
		if got := validatePolicyFieldValueType(test.fieldType, test.value); got != test.want {
			t.Errorf("validatePolicyFieldValueType(%s, %#v) = %v, want %v", test.fieldType, test.value, got, test.want)
		}
	}
}

func TestChromePolicyCreate_validatesRepeatedAndNestedValues(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		schemaName string
		key        string
		value      string
		// wantErr is empty for a valid value.
		wantErr string
	}{
		"int32 value": {
			schemaName: testChromePolicyUrls, key: "maxUrls", value: "5",
		},
		"int32 value with a fraction": {
			schemaName: testChromePolicyUrls, key: "maxUrls", value: "5.5",
			wantErr: "value provided for maxUrls is of incorrect type",
		},
		"repeated string": {
			schemaName: testChromePolicyUrls, key: "urlBlocklist", value: `["a.example.com","b.example.com"]`,
		},
		"repeated string with a number": {
			schemaName: testChromePolicyUrls, key: "urlBlocklist", value: `["a.example.com",3]`,
			wantErr: "value provided for urlBlocklist[1] is of incorrect type",
		},
		"top-level repeated string as one value": {
			schemaName: testChromePolicyUrls, key: "urlBlocklist", value: `"a.example.com"`,
		},
		"top-level repeated string as one value of the wrong type": {
			schemaName: testChromePolicyUrls, key: "urlBlocklist", value: `3`,
			wantErr: "value provided for urlBlocklist[0] is of incorrect type",
		},
		"nested message": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks",
			value: `{"toplevelName":"Stuff","entries":[{"name":"Example","url":"https://example.com","visits":7}]}`,
		},
		"nested string with a number": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"toplevelName":5}`,
			wantErr: "value provided for managedBookmarks.toplevelName is of incorrect type",
		},
		"repeated nested message with a bad value": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"entries":[{"name":"Example","visits":1.5}]}`,
			wantErr: "value provided for managedBookmarks.entries[0].visits is of incorrect type",
		},
		"repeated nested message that is not a list": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"entries":{"name":"Example"}}`,
			wantErr: "value provided for managedBookmarks.entries is of incorrect type",
		},
		"nested message that is not an object": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `"Stuff"`,
			wantErr: "value provided for managedBookmarks is of incorrect type",
		},
		"nested field that is not in the schema": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"unknownField":"x"}`,
		},
		"message type that is not in the schema": {
			schemaName: testChromePolicyBookmarks, key: "refreshDelay", value: `"3s"`,
		},
		"nested double as a string": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"entries":[{"name":"Example","weight":"1.5"}]}`,
		},
		"nested double NaN": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"entries":[{"name":"Example","weight":"NaN"}]}`,
		},
		"nested double Infinity": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"entries":[{"name":"Example","weight":"Infinity"}]}`,
		},
		"nested double -Infinity": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"entries":[{"name":"Example","weight":"-Infinity"}]}`,
		},
		"nested double that is not a number": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"entries":[{"weight":"inf"}]}`,
			wantErr: "value provided for managedBookmarks.entries[0].weight is of incorrect type",
		},
		"nested int64 with an exponent": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"entries":[{"name":"Example","visits":"1e3"}]}`,
		},
		"nested int64 with a fraction of zero": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"entries":[{"name":"Example","visits":"7.0"}]}`,
		},
		"nested int64 with an exponent and a fraction": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"entries":[{"visits":"1.5e0"}]}`,
			wantErr: "value provided for managedBookmarks.entries[0].visits is of incorrect type",
		},
		"top-level int64 as a string": {
			schemaName: testChromePolicyConns, key: "maxConnectionsPerProxy", value: `"33"`,
		},
		"top-level null": {
			schemaName: testChromePolicyUrls, key: "maxUrls", value: `null`,
		},
		"nested int64 as a string": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"entries":[{"name":"Example","visits":"7"}]}`,
		},
		"nested int64 string out of range": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"entries":[{"visits":"99999999999999999999"}]}`,
			wantErr: "value provided for managedBookmarks.entries[0].visits is of incorrect type",
		},
		"nested enum as a number": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"entries":[{"name":"Example","kind":1}]}`,
		},
		"nested enum as a name": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"entries":[{"name":"Example","kind":"KIND_FOLDER"}]}`,
		},
		"nested enum with a fraction": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"entries":[{"kind":1.5}]}`,
			wantErr: "value provided for managedBookmarks.entries[0].kind is of incorrect type",
		},
		"nested null": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"toplevelName":null,"entries":[{"name":"Example","url":null}]}`,
		},
		"map field": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"labels":{"a":5,"b":"6"}}`,
		},
		"map field with a bad value": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"labels":{"a":"x"}}`,
			wantErr: `value provided for managedBookmarks.labels["a"] is of incorrect type`,
		},
		"map field as a list": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"labels":[{"key":"a","value":5}]}`,
		},
		"map field as a list with a bad value": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"labels":[{"key":"a","value":"x"}]}`,
			wantErr: `value provided for managedBookmarks.labels["a"] is of incorrect type`,
		},
		"map field as a list with another field": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"labels":[{"key":"a","other":5}]}`,
			wantErr: "value provided for managedBookmarks.labels is of incorrect type",
		},
		"repeated Entry message that is not a map field": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"tags":[{"key":"a","value":5}]}`,
		},
		"repeated Entry message that is not a map field, as an object": {
			schemaName: testChromePolicyBookmarks, key: "managedBookmarks", value: `{"tags":{"a":5}}`,
			wantErr: "value provided for managedBookmarks.tags is of incorrect type",
		},
		"field of the policy message, not of the first message": {
			schemaName: testChromePolicyPolicy, key: "value", value: "5",
		},
		"field of the policy message, value of the wrong type": {
			schemaName: testChromePolicyPolicy, key: "value", value: `"x"`,
			wantErr: "value provided for value is of incorrect type (expected type: TYPE_INT64)",
		},
		"bool": {
			schemaName: testChromePolicyPolicy, key: "enabled", value: "true",
		},
		"bool as a string": {
			schemaName: testChromePolicyPolicy, key: "enabled", value: `"true"`,
		},
		"bool as another string": {
			schemaName: testChromePolicyPolicy, key: "enabled", value: `"1"`,
			wantErr: "value provided for enabled is of incorrect type",
		},
		"number for a string field": {
			schemaName: testChromePolicyHomepage, key: "homepageLocation", value: "5",
			wantErr: "value provided for homepageLocation is of incorrect type",
		},
		"shared field name, value for the first field": {
			schemaName: testChromePolicyShared, key: "shared", value: "5",
		},
		"shared field name, value for the second field": {
			schemaName: testChromePolicyShared, key: "shared", value: `"abc"`,
			wantErr: "value provided for shared is of incorrect type (expected type: TYPE_INT64)",
		},
	}

	for name, test := range tests {
		test := test
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client := newTestChromePolicyClient(t, newFakeChromePolicyAPI(t))
			r := testChromePolicyResource(2 * time.Second)
			d := testChromePolicyData(t, r, "", []interface{}{
				testChromePolicy(test.schemaName, test.key, test.value),
			})

			diags := r.CreateContext(context.Background(), d, client)

			if test.wantErr == "" {
				// A valid value is written, and the poll finds the same value.
				requireNoDiags(t, diags)
				return
			}
			if !diags.HasError() {
				t.Fatalf("expected an error that contains %q, got %#v", test.wantErr, diags)
			}
			if !strings.Contains(diags[0].Summary, test.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", diags[0].Summary, test.wantErr)
			}
		})
	}
}

func TestChromePolicyRead_convertsValuesByField(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyUrls, map[string]interface{}{
		"maxUrls":      float64(5),
		"urlBlocklist": []interface{}{"a.example.com"},
	})
	api.setVisible(testChromePolicyBookmarks, map[string]interface{}{
		"managedBookmarks": map[string]interface{}{
			"toplevelName": "Stuff",
			"entries":      []interface{}{map[string]interface{}{"name": "Example", "visits": float64(7)}},
		},
	})
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	// The values in state are different from the API values, so Read converts the API values.
	d := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		map[string]interface{}{
			"schema_name": testChromePolicyUrls,
			"schema_values": map[string]interface{}{
				"maxUrls":      "4",
				"urlBlocklist": `["b.example.com"]`,
			},
		},
		testChromePolicy(testChromePolicyBookmarks, "managedBookmarks", `{"toplevelName":"Stuff","entries":[{"name":"Example","visits":6}]}`),
	})
	want := map[string]map[string]string{
		testChromePolicyUrls:      {"maxUrls": "5", "urlBlocklist": `["a.example.com"]`},
		testChromePolicyBookmarks: {"managedBookmarks": `{"toplevelName":"Stuff","entries":[{"name":"Example","visits":7}]}`},
	}

	requireNoDiags(t, r.ReadContext(context.Background(), d, client))

	got := testChromePolicyValues(t, d)
	for schemaName, wantValues := range want {
		if len(got[schemaName]) != len(wantValues) {
			t.Errorf("%s in state = %#v, want %#v", schemaName, got[schemaName], wantValues)
		}
		for k, wantJson := range wantValues {
			if gotJson, _ := got[schemaName][k].(string); !testJSONEqual(t, gotJson, wantJson) {
				t.Errorf("%s.%s in state = %s, want %s", schemaName, k, gotJson, wantJson)
			}
		}
	}
}

func testJSONEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var aVal, bVal interface{}
	if err := json.Unmarshal([]byte(a), &aVal); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &bVal); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(aVal, bVal)
}

func TestChromePolicyCreate_resendsDroppedWrite(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	// The API returns success for the first write but does not apply it.
	api.dropWrites = 1
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(3 * time.Second)
	d := testChromePolicyData(t, r, "", []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
	})

	requireNoDiags(t, r.CreateContext(context.Background(), d, client))

	if got := testChromePolicyValues(t, d)[testChromePolicyConns]["maxConnectionsPerProxy"]; got != "33" {
		t.Fatalf("maxConnectionsPerProxy in state = %v, want 33", got)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.modifyCalls != 2 {
		t.Errorf("batchModify calls = %d, want 2", api.modifyCalls)
	}
}

// testChromePolicyApply plans and applies policies as Terraform does, with the timeouts of r in the context.
func testChromePolicyApply(t *testing.T, r *schema.Resource, state *terraform.InstanceState, policies []interface{}, client *apiClient) (*terraform.InstanceState, diag.Diagnostics) {
	t.Helper()

	diff, err := r.Diff(context.Background(), state, testChromePolicyConfig(policies), client)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return r.Apply(context.Background(), state, diff, client)
}

func testChromePolicyConfig(policies []interface{}) *terraform.ResourceConfig {
	return terraform.NewResourceConfigRaw(map[string]interface{}{
		"org_unit_id": testChromePolicyOrgUnit,
		"policies":    policies,
	})
}

func TestChromePolicyRead_flattenWarningKeepsState(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyConns, map[string]interface{}{"maxConnectionsPerProxy": float64(34), "unknownField": "x"})
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	d := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
	})

	diags := r.ReadContext(context.Background(), d, client)

	if diags.HasError() || len(diags) != 1 || !strings.Contains(diags[0].Summary, "unknownField") {
		t.Fatalf("diagnostics = %#v, want one warning about unknownField", diags)
	}
	if got := testChromePolicyValues(t, d)[testChromePolicyConns]["maxConnectionsPerProxy"]; got != "33" {
		t.Fatalf("maxConnectionsPerProxy in state = %v, want the prior value 33", got)
	}
}

func TestChromePolicyRead_keepsEquivalentForms(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	// The API form: names for enums, no null values, and numbers (which the fake returns as strings).
	api.setVisible(testChromePolicyBookmarks, map[string]interface{}{
		"managedBookmarks": map[string]interface{}{
			"toplevelName": "Stuff",
			"entries":      []interface{}{map[string]interface{}{"name": "Example", "visits": float64(7), "kind": "KIND_URL"}},
			"labels":       map[string]interface{}{"a": float64(5)},
		},
	})
	api.setVisible(testChromePolicyUrls, map[string]interface{}{"maxUrls": float64(5)})
	client := newTestChromePolicyClient(t, api)

	// The configured forms: decimal strings, an enum number, null values, and a default value that the API omits.
	bookmarks := `{"toplevelName":"Stuff","entries":[{"name":"Example","visits":"7","kind":1,"url":null}],"labels":{"a":"5"}}`
	r := testChromePolicyResource(time.Minute)
	d := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		testChromePolicy(testChromePolicyBookmarks, "managedBookmarks", bookmarks),
		map[string]interface{}{
			"schema_name":   testChromePolicyUrls,
			"schema_values": map[string]interface{}{"maxUrls": `"5"`, "urlBlocklist": `[]`},
		},
	})

	requireNoDiags(t, r.ReadContext(context.Background(), d, client))

	got := testChromePolicyValues(t, d)
	if got[testChromePolicyBookmarks]["managedBookmarks"] != bookmarks {
		t.Errorf("managedBookmarks in state = %v, want the configured form %s", got[testChromePolicyBookmarks]["managedBookmarks"], bookmarks)
	}
	if got[testChromePolicyUrls]["maxUrls"] != `"5"` || got[testChromePolicyUrls]["urlBlocklist"] != `[]` {
		t.Errorf("%s in state = %#v, want the configured forms", testChromePolicyUrls, got[testChromePolicyUrls])
	}
}

func TestChromePolicyCreate_normalizedValueConverges(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	// The API changes the value that it stores.
	api.transform = func(schemaName string, value map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{"homepageLocation": value["homepageLocation"].(string) + "/"}
	}
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(20 * time.Second)
	d := testChromePolicyData(t, r, "", []interface{}{
		testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://example.com"`),
	})

	start := time.Now()
	diags := r.CreateContext(context.Background(), d, client)
	elapsed := time.Since(start)

	if diags.HasError() || len(diags) != 1 || diags[0].Summary != "Chrome Policy values differ from the configuration" ||
		!strings.Contains(diags[0].Detail, testChromePolicyHomepage) {
		t.Fatalf("diagnostics = %#v, want one warning that names %s", diags, testChromePolicyHomepage)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Create took %s, want it to stop when the value does not change", elapsed)
	}
	if got := testChromePolicyValues(t, d)[testChromePolicyHomepage]["homepageLocation"]; got != `"https://example.com"` {
		t.Errorf("homepageLocation in state = %v, want the configured value", got)
	}
	form, ok := chromePolicyAppliedForms(d.Get("applied_values"))[testChromePolicyHomepage]
	if !ok || !sameChromePolicyJSON(form.Values, []byte(`{"homepageLocation":"https://example.com/"}`)) {
		t.Errorf("applied_values = %#v, want the API value for %s", d.Get("applied_values"), testChromePolicyHomepage)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	// The write, and the write sent again to confirm the new value.
	if api.modifyCalls != 2 {
		t.Errorf("batchModify calls = %d, want 2", api.modifyCalls)
	}
}

func TestChromePolicyCreate_omittedDefaultsMatch(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	// The API omits the default value visits=0.
	api.transform = func(schemaName string, value map[string]interface{}) map[string]interface{} {
		bookmarks := value["managedBookmarks"].(map[string]interface{})
		delete(bookmarks["entries"].([]interface{})[0].(map[string]interface{}), "visits")
		return value
	}
	client := newTestChromePolicyClient(t, api)

	value := `{"toplevelName":"Stuff","entries":[{"name":"Example","visits":0}]}`
	r := testChromePolicyResource(20 * time.Second)
	d := testChromePolicyData(t, r, "", []interface{}{
		testChromePolicy(testChromePolicyBookmarks, "managedBookmarks", value),
	})

	requireNoDiags(t, r.CreateContext(context.Background(), d, client))

	if got := testChromePolicyValues(t, d)[testChromePolicyBookmarks]["managedBookmarks"]; got != value {
		t.Errorf("managedBookmarks in state = %v, want the configured form %s", got, value)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.modifyCalls != 1 {
		t.Errorf("batchModify calls = %d, want 1", api.modifyCalls)
	}
}

func TestChromePolicyApply_resendUsesContextDeadline(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	// The first write takes half of the create timeout, and the API does not apply it.
	api.firstModifyDelay = 3 * time.Second
	api.dropWrites = 1
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(6 * time.Second)

	start := time.Now()
	state, diags := testChromePolicyApply(t, r, nil, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
	}, client)
	elapsed := time.Since(start)

	requireNoDiags(t, diags)
	if state == nil || state.ID != testChromePolicyOrgUnit {
		t.Fatalf("state = %#v, want ID %q", state, testChromePolicyOrgUnit)
	}
	if elapsed >= 6*time.Second {
		t.Errorf("apply took %s, want less than the create timeout", elapsed)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	// The poll has half of the time that remains before the context deadline. Then it sends the write again.
	if api.modifyCalls != 2 {
		t.Errorf("batchModify calls = %d, want 2", api.modifyCalls)
	}
}

func TestChromePolicyCreate_failedResendWarns(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.hiddenResolves = testChromePolicyNeverSeen
	api.failModifyCalls = map[int]bool{2: true}
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(3 * time.Second)
	d := testChromePolicyData(t, r, "", []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
	})

	diags := r.CreateContext(context.Background(), d, client)

	if diags.HasError() || len(diags) != 1 || !strings.Contains(diags[0].Detail, "that request failed") {
		t.Fatalf("diagnostics = %#v, want one warning about the failed request", diags)
	}
	if d.Id() != testChromePolicyOrgUnit {
		t.Fatalf("ID = %q, want %q", d.Id(), testChromePolicyOrgUnit)
	}
	if got := testChromePolicyValues(t, d)[testChromePolicyConns]["maxConnectionsPerProxy"]; got != "33" {
		t.Fatalf("maxConnectionsPerProxy in state = %v, want the configured value 33", got)
	}
}

func TestChromePolicyCreate_unknownFieldFromAPIWarns(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.transform = func(schemaName string, value map[string]interface{}) map[string]interface{} {
		value["unknownField"] = "x"
		return value
	}
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(20 * time.Second)
	d := testChromePolicyData(t, r, "", []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
	})

	diags := r.CreateContext(context.Background(), d, client)

	if diags.HasError() || len(diags) != 1 || !strings.Contains(diags[0].Summary, "unknownField") {
		t.Fatalf("diagnostics = %#v, want one warning about unknownField", diags)
	}
	if got := testChromePolicyValues(t, d)[testChromePolicyConns]["maxConnectionsPerProxy"]; got != "33" {
		t.Fatalf("maxConnectionsPerProxy in state = %v, want the configured value 33", got)
	}
}

func testChromePolicyBookmarksState(t *testing.T, r *schema.Resource, value string) *terraform.InstanceState {
	return testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		testChromePolicy(testChromePolicyBookmarks, "managedBookmarks", value),
	}).State()
}

func TestChromePolicyUpdate_invalidValueDoesNotInherit(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyBookmarks, map[string]interface{}{"managedBookmarks": map[string]interface{}{"toplevelName": "Stuff"}})
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	d := r.Data(testChromePolicyBookmarksState(t, r, `{"toplevelName":"Stuff"}`))
	if err := d.Set("policies", []interface{}{
		testChromePolicy(testChromePolicyBookmarks, "managedBookmarks", `{"toplevelName":5}`),
	}); err != nil {
		t.Fatal(err)
	}

	diags := r.UpdateContext(context.Background(), d, client)

	if !diags.HasError() || !strings.Contains(diags[0].Summary, "managedBookmarks.toplevelName") {
		t.Fatalf("diagnostics = %#v, want a validation error", diags)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.inheritCalls != 0 || api.modifyCalls != 0 {
		t.Errorf("batchInherit calls = %d, batchModify calls = %d, want no changes", api.inheritCalls, api.modifyCalls)
	}
	if _, ok := api.visible[testChromePolicyBookmarks]; !ok {
		t.Errorf("the policy was reset")
	}
}

func TestChromePolicyPlan_rejectsInvalidValue(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	client := newTestChromePolicyClient(t, api)
	r := testChromePolicyResource(time.Minute)
	state := testChromePolicyBookmarksState(t, r, `{"toplevelName":"Stuff"}`)

	_, err := r.Diff(context.Background(), state, testChromePolicyConfig([]interface{}{
		testChromePolicy(testChromePolicyBookmarks, "managedBookmarks", `{"toplevelName":5}`),
	}), client)

	if err == nil || !strings.Contains(err.Error(), "managedBookmarks.toplevelName") {
		t.Fatalf("plan error = %v, want a validation error", err)
	}
}

func TestChromePolicyPlan_skipsValidation(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		state    string
		policies []interface{}
	}{
		"schema cannot be fetched": {
			policies: []interface{}{testChromePolicy("chrome.users.NotInTheFake", "x", "1")},
		},
		"value not known yet": {
			policies: []interface{}{testChromePolicy(testChromePolicyBookmarks, "managedBookmarks", chromePolicyUnknownValue)},
		},
		"policies not changed": {
			// A value in state that is not valid does not stop a plan that does not change it.
			state:    `{"toplevelName":5}`,
			policies: []interface{}{testChromePolicy(testChromePolicyBookmarks, "managedBookmarks", `{"toplevelName":5}`)},
		},
	}

	for name, test := range tests {
		test := test
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client := newTestChromePolicyClient(t, newFakeChromePolicyAPI(t))
			r := testChromePolicyResource(time.Minute)

			var state *terraform.InstanceState
			if test.state != "" {
				state = testChromePolicyBookmarksState(t, r, test.state)
			}

			if _, err := r.Diff(context.Background(), state, testChromePolicyConfig(test.policies), client); err != nil {
				t.Fatalf("plan error = %v, want none", err)
			}
		})
	}
}

func TestChromePolicyUpdate_usesUpdateTimeout(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	client := newTestChromePolicyClient(t, api)

	r := resourceChromePolicy()
	r.Timeouts = &schema.ResourceTimeout{
		Create: schema.DefaultTimeout(time.Minute),
		Update: schema.DefaultTimeout(3 * time.Second),
	}

	state, diags := testChromePolicyApply(t, r, nil, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
	}, client)
	requireNoDiags(t, diags)

	api.mu.Lock()
	api.hiddenResolves = testChromePolicyNeverSeen
	api.mu.Unlock()

	// The update replaces the policy with another policy, which never becomes visible.
	start := time.Now()
	state, diags = testChromePolicyApply(t, r, state, []interface{}{
		testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://example.com"`),
	}, client)
	elapsed := time.Since(start)

	if diags.HasError() || len(diags) != 1 || diags[0].Summary != "Chrome Policy values are not visible yet" {
		t.Fatalf("diagnostics = %#v, want the timeout warning", diags)
	}
	if elapsed > 10*time.Second {
		t.Errorf("update took %s, want it to stop at the update timeout", elapsed)
	}
	if got := state.Attributes["policies.0.schema_values.homepageLocation"]; got != `"https://example.com"` {
		t.Errorf("homepageLocation in state = %q, want the configured value", got)
	}
}

func TestChromePolicyUpdate_resendsDroppedWrite(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	client := newTestChromePolicyClient(t, api)
	r := testChromePolicyResource(3 * time.Second)

	state, diags := testChromePolicyApply(t, r, nil, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
	}, client)
	requireNoDiags(t, diags)

	api.mu.Lock()
	api.dropWrites = 1
	api.mu.Unlock()

	state, diags = testChromePolicyApply(t, r, state, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "34"),
	}, client)

	requireNoDiags(t, diags)
	if got := state.Attributes["policies.0.schema_values.maxConnectionsPerProxy"]; got != "34" {
		t.Errorf("maxConnectionsPerProxy in state = %q, want 34", got)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	// One write for the create, and two for the update. The policy has the same fields, so it is not inherited.
	if api.inheritCalls != 0 || api.modifyCalls != 3 {
		t.Errorf("batchInherit calls = %d, batchModify calls = %d, want 0 and 3", api.inheritCalls, api.modifyCalls)
	}
}

func TestChromePolicyUpdate_inheritsOnlyRemovedPoliciesAndFields(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyConns, map[string]interface{}{"maxConnectionsPerProxy": float64(33)})
	api.setVisible(testChromePolicyHomepage, map[string]interface{}{"homepageLocation": "https://example.com"})
	api.setVisible(testChromePolicyUrls, map[string]interface{}{"maxUrls": float64(5), "urlBlocklist": []interface{}{"a.example.com"}})
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(20 * time.Second)
	state := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
		testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://example.com"`),
		map[string]interface{}{
			"schema_name":   testChromePolicyUrls,
			"schema_values": map[string]interface{}{"maxUrls": "5", "urlBlocklist": `["a.example.com"]`},
		},
	}).State()

	// conns has the same fields, homepage is removed, and urls has fewer fields.
	_, diags := testChromePolicyApply(t, r, state, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "34"),
		testChromePolicy(testChromePolicyUrls, "urlBlocklist", `["b.example.com"]`),
	}, client)
	requireNoDiags(t, diags)

	api.mu.Lock()
	defer api.mu.Unlock()
	got := append([]string(nil), api.inherited...)
	sort.Strings(got)
	if want := []string{testChromePolicyHomepage, testChromePolicyUrls}; !reflect.DeepEqual(got, want) {
		t.Errorf("inherited policies = %v, want %v", got, want)
	}
	if _, ok := api.visible[testChromePolicyHomepage]; ok {
		t.Errorf("the removed policy is still set")
	}
	if _, ok := api.visible[testChromePolicyUrls]["maxUrls"]; ok {
		t.Errorf("the removed field maxUrls is still set")
	}
}

func TestChromePolicyUpdate_restoresAfterFailedWrite(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		failModifyCalls map[int]bool
		wantErr         string
		wantRestored    bool
	}{
		"restore succeeds": {
			failModifyCalls: map[int]bool{1: true},
			wantErr:         "Terraform wrote the values from before the change again",
			wantRestored:    true,
		},
		"restore fails": {
			failModifyCalls: map[int]bool{1: true, 2: true},
			wantErr:         "Writing the values from before the change again also failed",
		},
	}

	for name, test := range tests {
		test := test
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			api := newFakeChromePolicyAPI(t)
			api.setVisible(testChromePolicyUrls, map[string]interface{}{"maxUrls": float64(5), "urlBlocklist": []interface{}{"a.example.com"}})
			api.failModifyCalls = test.failModifyCalls
			client := newTestChromePolicyClient(t, api)

			r := testChromePolicyResource(20 * time.Second)
			state := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
				map[string]interface{}{
					"schema_name":   testChromePolicyUrls,
					"schema_values": map[string]interface{}{"maxUrls": "5", "urlBlocklist": `["a.example.com"]`},
				},
			}).State()

			// maxUrls is removed, so the policy is inherited, and then the write fails.
			_, diags := testChromePolicyApply(t, r, state, []interface{}{
				testChromePolicy(testChromePolicyUrls, "urlBlocklist", `["b.example.com"]`),
			}, client)

			if !diags.HasError() || !strings.Contains(diags[0].Summary, "write failed") || !strings.Contains(diags[0].Summary, test.wantErr) {
				t.Fatalf("diagnostics = %#v, want an error with the write error and %q", diags, test.wantErr)
			}

			api.mu.Lock()
			defer api.mu.Unlock()
			restored, _ := json.Marshal(api.pending[testChromePolicyUrls])
			if test.wantRestored != (string(restored) == `{"maxUrls":"5","urlBlocklist":["a.example.com"]}`) {
				t.Errorf("value written after the failure = %s, want restored = %v", restored, test.wantRestored)
			}
		})
	}
}

func TestChromePolicyCreate_repeatedScalarIsSentAsList(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(20 * time.Second)
	d := testChromePolicyData(t, r, "", []interface{}{
		testChromePolicy(testChromePolicyUrls, "urlBlocklist", `"a.example.com"`),
	})

	requireNoDiags(t, r.CreateContext(context.Background(), d, client))

	// Read keeps the configured form, because the API value is a list with the same value.
	if got := testChromePolicyValues(t, d)[testChromePolicyUrls]["urlBlocklist"]; got != `"a.example.com"` {
		t.Errorf("urlBlocklist in state = %v, want the configured form", got)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if got := api.visible[testChromePolicyUrls]["urlBlocklist"]; !reflect.DeepEqual(got, []interface{}{"a.example.com"}) {
		t.Errorf("urlBlocklist written to the API = %#v, want a list with one value", got)
	}
}

func testChromePolicyNoAppliedValues(t *testing.T, state *terraform.InstanceState) {
	t.Helper()
	if state == nil {
		return
	}
	if n := state.Attributes["applied_values.%"]; n != "" && n != "0" {
		t.Errorf("applied_values in state = %v, want none", state.Attributes)
	}
}

func TestChromePolicyUpdate_delayedVisibilityWaitsForValue(t *testing.T) {
	t.Parallel()

	for _, hidden := range []int{2, 4, 8} {
		hidden := hidden
		t.Run(strconv.Itoa(hidden), func(t *testing.T) {
			t.Parallel()

			api := newFakeChromePolicyAPI(t)
			api.setVisible(testChromePolicyConns, map[string]interface{}{"maxConnectionsPerProxy": float64(33)})
			api.hiddenResolves = hidden
			client := newTestChromePolicyClient(t, api)

			r := testChromePolicyResource(12 * time.Second)
			state := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
				testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
			}).State()

			state, diags := testChromePolicyApply(t, r, state, []interface{}{
				testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "34"),
			}, client)

			if got := state.Attributes["policies.0.schema_values.maxConnectionsPerProxy"]; got != "34" {
				t.Errorf("maxConnectionsPerProxy in state = %q, want 34", got)
			}

			// The poll finds the new value before the timeout. It never accepts the old value before the timeout.
			if hidden <= 4 {
				requireNoDiags(t, diags)
				testChromePolicyNoAppliedValues(t, state)
				return
			}

			// At the timeout, the old value is still there after the write was sent again. It is not recorded.
			if diags.HasError() || len(diags) != 1 || diags[0].Summary != "Chrome Policy values did not change" {
				t.Fatalf("diagnostics = %#v, want the warning that the values did not change", diags)
			}
			testChromePolicyNoAppliedValues(t, state)
		})
	}
}

func TestChromePolicyUpdate_inheritedStaleReadWaitsForValue(t *testing.T) {
	t.Parallel()

	old := map[string]interface{}{"maxUrls": float64(5), "urlBlocklist": []interface{}{"a.example.com"}}
	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyUrls, old)
	api.hiddenResolves = 3
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(20 * time.Second)
	state := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		map[string]interface{}{
			"schema_name":   testChromePolicyUrls,
			"schema_values": map[string]interface{}{"maxUrls": "5", "urlBlocklist": `["a.example.com"]`},
		},
	}).State()

	// The reset is not visible for 1.5 seconds: the API still returns the old value.
	stop := time.After(1500 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				api.mu.Lock()
				if _, ok := api.visible[testChromePolicyUrls]; !ok && api.inheritCalls > 0 {
					api.visible[testChromePolicyUrls] = old
				}
				api.mu.Unlock()
			}
		}
	}()

	// maxUrls is removed, so the policy is inherited before the write.
	state, diags := testChromePolicyApply(t, r, state, []interface{}{
		testChromePolicy(testChromePolicyUrls, "urlBlocklist", `["b.example.com"]`),
	}, client)
	<-done

	requireNoDiags(t, diags)
	if got := state.Attributes["policies.0.schema_values.urlBlocklist"]; got != `["b.example.com"]` {
		t.Errorf("urlBlocklist in state = %q, want the new value", got)
	}
	if _, ok := state.Attributes["policies.0.schema_values.maxUrls"]; ok {
		t.Errorf("maxUrls is in state, want it removed")
	}
	testChromePolicyNoAppliedValues(t, state)
}

func TestChromePolicy_normalizedValueIsRecorded(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.transform = func(schemaName string, value map[string]interface{}) map[string]interface{} {
		if schemaName == testChromePolicyHomepage {
			return map[string]interface{}{"homepageLocation": value["homepageLocation"].(string) + "/"}
		}
		return value
	}
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(20 * time.Second)
	config := []interface{}{
		testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://example.com"`),
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
	}

	state, diags := testChromePolicyApply(t, r, nil, config, client)
	if diags.HasError() || len(diags) != 1 || diags[0].Summary != "Chrome Policy values differ from the configuration" {
		t.Fatalf("diagnostics = %#v, want the warning that the values differ", diags)
	}

	// The next refresh and plan show no change, so the next apply makes no write.
	state, diags = r.RefreshWithoutUpgrade(context.Background(), state, client)
	requireNoDiags(t, diags)
	if got := state.Attributes["policies.0.schema_values.homepageLocation"]; got != `"https://example.com"` {
		t.Errorf("homepageLocation in state after refresh = %q, want the configured value", got)
	}
	diff, err := r.Diff(context.Background(), state, testChromePolicyConfig(config), client)
	if err != nil {
		t.Fatal(err)
	}
	if diff != nil && !diff.Empty() {
		t.Errorf("plan = %#v, want no change", diff)
	}

	// An update of the other policy converges with the recorded form, without a warning and before the resend.
	start := time.Now()
	state, diags = testChromePolicyApply(t, r, state, []interface{}{
		config[0],
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "34"),
	}, client)
	requireNoDiags(t, diags)
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Errorf("update took %s, want it to stop before half of the timeout", elapsed)
	}
	if n := state.Attributes["applied_values.%"]; n != "1" {
		t.Errorf("applied_values in state = %v, want the recorded form kept", state.Attributes)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	// Two writes for the create (the second confirms the new value), and one for the update.
	if api.modifyCalls != 3 {
		t.Errorf("batchModify calls = %d, want 3", api.modifyCalls)
	}
}

func TestChromePolicyPlan_upgradeStateWithoutAppliedValues(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyConns, map[string]interface{}{"maxConnectionsPerProxy": float64(33)})
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	config := []interface{}{testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33")}

	// State from an earlier version of the provider has no applied_values.
	state := testChromePolicyData(t, r, testChromePolicyOrgUnit, config).State()
	for k := range state.Attributes {
		if strings.HasPrefix(k, "applied_values") {
			delete(state.Attributes, k)
		}
	}

	for _, refresh := range []bool{false, true} {
		planState := state
		if refresh {
			var diags diag.Diagnostics
			planState, diags = r.RefreshWithoutUpgrade(context.Background(), state, client)
			requireNoDiags(t, diags)
		}
		diff, err := r.Diff(context.Background(), planState, testChromePolicyConfig(config), client)
		if err != nil {
			t.Fatal(err)
		}
		if diff != nil && !diff.Empty() {
			t.Errorf("plan (refresh = %v) = %#v, want no change", refresh, diff)
		}
	}
}

func TestChromePolicyUpdate_failedWriteKeepsPriorState(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyUrls, map[string]interface{}{"maxUrls": float64(5), "urlBlocklist": []interface{}{"a.example.com"}})
	api.failModifyCalls = map[int]bool{1: true}
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(20 * time.Second)
	state := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		map[string]interface{}{
			"schema_name":   testChromePolicyUrls,
			"schema_values": map[string]interface{}{"maxUrls": "5", "urlBlocklist": `["a.example.com"]`},
		},
	}).State()

	newState, diags := testChromePolicyApply(t, r, state, []interface{}{
		testChromePolicy(testChromePolicyUrls, "urlBlocklist", `["b.example.com"]`),
	}, client)

	if !diags.HasError() {
		t.Fatalf("diagnostics = %#v, want an error", diags)
	}
	if newState == nil || newState.Attributes["policies.0.schema_values.maxUrls"] != "5" ||
		newState.Attributes["policies.0.schema_values.urlBlocklist"] != `["a.example.com"]` {
		t.Errorf("state after the failed update = %#v, want the prior state", newState)
	}
}

// testChromePolicyRefreshAndPlan refreshes state and reports whether the plan for policies has no change.
func testChromePolicyRefreshAndPlan(t *testing.T, r *schema.Resource, state *terraform.InstanceState, policies []interface{}, client *apiClient) (*terraform.InstanceState, bool) {
	t.Helper()
	state, diags := r.RefreshWithoutUpgrade(context.Background(), state, client)
	requireNoDiags(t, diags)
	diff, err := r.Diff(context.Background(), state, testChromePolicyConfig(policies), client)
	if err != nil {
		t.Fatal(err)
	}
	return state, diff == nil || diff.Empty()
}

func testChromePolicySlash(schemaName string, value map[string]interface{}) map[string]interface{} {
	if s, ok := value["homepageLocation"].(string); ok && !strings.HasSuffix(s, "/") {
		return map[string]interface{}{"homepageLocation": s + "/"}
	}
	return value
}

// testChromePolicyRequireUnchanged checks the result of an apply in which a policy kept the value from before the
// write: the warning, nothing recorded, the configured value in state, and a change in the next plan.
func testChromePolicyRequireUnchanged(t *testing.T, r *schema.Resource, state *terraform.InstanceState, diags diag.Diagnostics, config []interface{}, key, configured string, client *apiClient) {
	t.Helper()

	if diags.HasError() || len(diags) != 1 || diags[0].Summary != "Chrome Policy values did not change" ||
		!strings.Contains(diags[0].Detail, "Admin console") {
		t.Fatalf("diagnostics = %#v, want the warning that the values did not change", diags)
	}
	testChromePolicyNoAppliedValues(t, state)
	if got := state.Attributes["policies.0.schema_values."+key]; got != configured {
		t.Errorf("%s in state = %q, want the configured value %s", key, got, configured)
	}
	if _, empty := testChromePolicyRefreshAndPlan(t, r, state, config, client); empty {
		t.Error("plan after the apply has no change, want a change")
	}
}

func TestChromePolicyUpdate_upgradeNormalizedValueIsNotRecorded(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.transform = testChromePolicySlash
	api.setVisible(testChromePolicyHomepage, map[string]interface{}{"homepageLocation": "https://example.com/"})
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(4 * time.Second)
	config := []interface{}{testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://example.com"`)}

	// State from an earlier version: the API form of the value, and no applied_values.
	state := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://example.com/"`),
	}).State()
	delete(state.Attributes, "applied_values.%")

	state, empty := testChromePolicyRefreshAndPlan(t, r, state, config, client)
	if empty {
		t.Fatal("first plan has no change, want a change to the configured form")
	}

	state, diags := testChromePolicyApply(t, r, state, config, client)
	testChromePolicyRequireUnchanged(t, r, state, diags, config, "homepageLocation", `"https://example.com"`, client)
}

func TestChromePolicyUpdate_configChangeSameNormalizedIsNotRecorded(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.transform = testChromePolicySlash
	client := newTestChromePolicyClient(t, api)
	r := testChromePolicyResource(4 * time.Second)

	config := []interface{}{testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://example.com"`)}
	state, diags := testChromePolicyApply(t, r, nil, config, client)
	if diags.HasError() || len(diags) != 1 || diags[0].Summary != "Chrome Policy values differ from the configuration" {
		t.Fatalf("create diagnostics = %#v, want the warning that the values differ", diags)
	}

	// The configured text changes, but the API stores the same value as before.
	api.mu.Lock()
	api.transform = func(schemaName string, value map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{"homepageLocation": "https://example.com/"}
	}
	api.mu.Unlock()

	config = []interface{}{testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"HTTPS://EXAMPLE.COM"`)}
	state, empty := testChromePolicyRefreshAndPlan(t, r, state, config, client)
	if empty {
		t.Fatal("plan for the new configured text has no change, want a change")
	}

	state, diags = testChromePolicyApply(t, r, state, config, client)
	testChromePolicyRequireUnchanged(t, r, state, diags, config, "homepageLocation", `"HTTPS://EXAMPLE.COM"`, client)
}

func TestChromePolicyUpdate_droppedTwiceIsNotRecorded(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyConns, map[string]interface{}{"maxConnectionsPerProxy": float64(33)})
	// The write and the write that is sent again both return success, but the API applies neither.
	api.dropWrites = 2
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(3 * time.Second)
	state := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
	}).State()
	config := []interface{}{testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "34")}

	state, diags := testChromePolicyApply(t, r, state, config, client)
	testChromePolicyRequireUnchanged(t, r, state, diags, config, "maxConnectionsPerProxy", "34", client)

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.modifyCalls != 2 {
		t.Errorf("batchModify calls = %d, want 2", api.modifyCalls)
	}
}

func TestChromePolicyUpdate_failedResendKeepsNotVisibleWarning(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyConns, map[string]interface{}{"maxConnectionsPerProxy": float64(33)})
	api.dropWrites = 1
	api.failModifyCalls = map[int]bool{2: true}
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(3 * time.Second)
	state := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
	}).State()

	state, diags := testChromePolicyApply(t, r, state, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "34"),
	}, client)

	// The value from before the write, but the write was not sent again: the warning that the values are not visible.
	if diags.HasError() || len(diags) != 1 || diags[0].Summary != "Chrome Policy values are not visible yet" ||
		!strings.Contains(diags[0].Detail, "that request failed") {
		t.Fatalf("diagnostics = %#v, want the warning about the failed request", diags)
	}
	testChromePolicyNoAppliedValues(t, state)
}

func TestChromePolicyCreate_sourceTransition(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		// sourceBefore is the source of the value before the write, as in fakeChromePolicyAPI.sources.
		sourceBefore string
		// neverVisible is true if the write never becomes visible, so the API keeps the value and its source.
		neverVisible bool
		wantRecorded bool
	}{
		"inherited value, then set on the org unit": {
			sourceBefore: testChromePolicyParent,
			wantRecorded: true,
		},
		"no source key, then set on the org unit": {
			sourceBefore: testChromePolicyNoSource,
			wantRecorded: true,
		},
		"set on the org unit before and after": {},
		"stale read that still has the parent source": {
			sourceBefore: testChromePolicyParent,
			neverVisible: true,
		},
	}

	for name, test := range tests {
		test := test
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			api := newFakeChromePolicyAPI(t)
			api.transform = testChromePolicySlash
			// Before the write, the API has the value that it stores for the configured value.
			api.setVisible(testChromePolicyHomepage, map[string]interface{}{"homepageLocation": "https://example.com/"})
			if test.sourceBefore != "" {
				api.sources = map[string]string{testChromePolicyHomepage: test.sourceBefore}
			}
			if test.neverVisible {
				api.hiddenResolves = testChromePolicyNeverSeen
			}
			client := newTestChromePolicyClient(t, api)

			timeout := 3 * time.Second
			if test.wantRecorded {
				timeout = 20 * time.Second
			}
			r := testChromePolicyResource(timeout)
			config := []interface{}{testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://example.com"`)}

			start := time.Now()
			state, diags := testChromePolicyApply(t, r, nil, config, client)
			elapsed := time.Since(start)

			api.mu.Lock()
			writes := api.modifyCalls
			api.mu.Unlock()

			if !test.wantRecorded {
				testChromePolicyRequireUnchanged(t, r, state, diags, config, "homepageLocation", `"https://example.com"`, client)
				if elapsed < timeout/2 {
					t.Errorf("create took %s, want the poll to wait", elapsed)
				}
				return
			}

			// The source became the org unit, so the value comes from the write. It is recorded after the write is
			// sent again once to confirm it, before the timeout.
			if diags.HasError() || len(diags) != 1 || diags[0].Summary != "Chrome Policy values differ from the configuration" {
				t.Fatalf("diagnostics = %#v, want the warning that the values differ", diags)
			}
			if elapsed > 6*time.Second || writes != 2 {
				t.Errorf("create took %s with %d writes, want 2 writes before half of the timeout", elapsed, writes)
			}
			if n := state.Attributes["applied_values.%"]; n != "1" {
				t.Errorf("applied_values in state = %v, want the value recorded", state.Attributes)
			}
			if _, empty := testChromePolicyRefreshAndPlan(t, r, state, config, client); !empty {
				t.Error("plan after the create has a change, want none")
			}
		})
	}
}

func TestChromePolicyCreate_noSourceKeyAfterWriteIsNotAccepted(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.transform = testChromePolicySlash
	// The written value has no source key: the API documents this as the default value for the customer.
	api.writeSource = testChromePolicyNoSource
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(3 * time.Second)
	config := []interface{}{testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://example.com"`)}

	state, diags := testChromePolicyApply(t, r, nil, config, client)

	if diags.HasError() || len(diags) != 1 || diags[0].Summary != "Chrome Policy values are not visible yet" {
		t.Fatalf("diagnostics = %#v, want the warning that the values are not visible", diags)
	}
	testChromePolicyNoAppliedValues(t, state)
}

func TestChromePolicyUpdate_defaultValueIsNotRestored(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyUrls, map[string]interface{}{"maxUrls": float64(5), "urlBlocklist": []interface{}{"a.example.com"}})
	// Before the change, the value is the default value for the customer, not a value on the org unit.
	api.sources = map[string]string{testChromePolicyUrls: testChromePolicyNoSource}
	api.failModifyCalls = map[int]bool{1: true}
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(20 * time.Second)
	state := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		map[string]interface{}{
			"schema_name":   testChromePolicyUrls,
			"schema_values": map[string]interface{}{"maxUrls": "5", "urlBlocklist": `["a.example.com"]`},
		},
	}).State()

	_, diags := testChromePolicyApply(t, r, state, []interface{}{
		testChromePolicy(testChromePolicyUrls, "urlBlocklist", `["b.example.com"]`),
	}, client)

	if !diags.HasError() || !strings.Contains(diags[0].Summary, "were not set on the org unit before the change") {
		t.Fatalf("diagnostics = %#v, want an error that says nothing was restored", diags)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.modifyCalls != 1 {
		t.Errorf("batchModify calls = %d, want 1 (no restore of the default value)", api.modifyCalls)
	}
}

func TestChromePolicySchemaCache_contextAndConcurrency(t *testing.T) {
	t.Parallel()

	t.Run("cancel", func(t *testing.T) {
		t.Parallel()

		api := newFakeChromePolicyAPI(t)
		api.schemaDelay = 2 * time.Second
		schemas, diags := newChromePolicySchemaCache(newTestChromePolicyClient(t, api))
		requireNoDiags(t, diags)

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		start := time.Now()
		if _, err := schemas.get(ctx, testChromePolicyConns); err == nil {
			t.Fatal("get returned no error, want the context error")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("get took %s, want it to stop when the context is done", elapsed)
		}

		// The HTTP request itself is canceled, not only the wait for it.
		for i := 0; i < 50 && atomic.LoadInt32(&api.schemaCanceled) == 0; i++ {
			time.Sleep(20 * time.Millisecond)
		}
		if atomic.LoadInt32(&api.schemaCanceled) == 0 {
			t.Error("the schema request was not canceled")
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		t.Parallel()

		schemas, diags := newChromePolicySchemaCache(newTestChromePolicyClient(t, newFakeChromePolicyAPI(t)))
		requireNoDiags(t, diags)

		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for _, name := range []string{testChromePolicyConns, testChromePolicyHomepage, testChromePolicyUrls} {
					if _, err := schemas.get(context.Background(), name); err != nil {
						t.Errorf("get %s: %v", name, err)
					}
				}
			}()
		}
		wg.Wait()
	})
}

func TestClassifyChromePolicy(t *testing.T) {
	t.Parallel()

	newLocal := chromePolicyRound{set: true, local: true, stable: true}
	tests := map[string]struct {
		round chromePolicyRound
		want  chromePolicyState
	}{
		"not set":                       {chromePolicyRound{}, chromePolicyPending},
		"matches":                       {chromePolicyRound{set: true, matches: true, sameAsBefore: true}, chromePolicyMatches},
		"recorded form":                 {chromePolicyRound{set: true, recorded: true, sameAsBefore: true}, chromePolicyRecordedForm},
		"new local value, not stable":   {chromePolicyRound{set: true, local: true}, chromePolicyPending},
		"new local value, stable":       {newLocal, chromePolicyCandidate},
		"candidate, one round after":    {chromePolicyRound{set: true, local: true, stable: true, confirmedRounds: 1}, chromePolicyCandidate},
		"candidate, two rounds after":   {chromePolicyRound{set: true, local: true, stable: true, confirmedRounds: 2}, chromePolicyNewForm},
		"new value, not local":          {chromePolicyRound{set: true, stable: true}, chromePolicyPending},
		"value from before, local":      {chromePolicyRound{set: true, local: true, stable: true, sameAsBefore: true}, chromePolicyUnchanged},
		"value from before, now local":  {chromePolicyRound{set: true, local: true, stable: true, sameAsBefore: true, notLocalBefore: true}, chromePolicyCandidate},
		"value from before, not local":  {chromePolicyRound{set: true, stable: true, sameAsBefore: true, notLocalBefore: true}, chromePolicyUnchanged},
		"value from before, confirmed":  {chromePolicyRound{set: true, local: true, stable: true, sameAsBefore: true, notLocalBefore: true, confirmedRounds: 2}, chromePolicyNewForm},
		"matches before recorded check": {chromePolicyRound{set: true, matches: true, recorded: true}, chromePolicyMatches},
	}

	for name, test := range tests {
		if got := classifyChromePolicy(test.round); got != test.want {
			t.Errorf("%s: classifyChromePolicy(%+v) = %d, want %d", name, test.round, got, test.want)
		}
	}
}

func TestChromePolicyUpdate_foreignValueIsOverwrittenNotRecorded(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyHomepage, map[string]interface{}{"homepageLocation": "https://a.example.com"})
	// The API drops the write, and another writer sets a value during the poll.
	api.dropWrites = 1
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(30 * time.Second)
	state := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://a.example.com"`),
	}).State()
	go func() {
		time.Sleep(700 * time.Millisecond)
		api.setVisible(testChromePolicyHomepage, map[string]interface{}{"homepageLocation": "https://other.example.com"})
	}()

	config := []interface{}{testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://b.example.com"`)}
	state, diags := testChromePolicyApply(t, r, state, config, client)

	// The write that is sent again to confirm the other value overwrites it, so the configured value is found.
	requireNoDiags(t, diags)
	testChromePolicyNoAppliedValues(t, state)
	if _, empty := testChromePolicyRefreshAndPlan(t, r, state, config, client); !empty {
		t.Error("plan after the apply has a change, want none")
	}
}

func TestChromePolicyUpdate_restoreAfterDeadline(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyUrls, map[string]interface{}{"maxUrls": float64(5), "urlBlocklist": []interface{}{"a.example.com"}})
	// The write takes longer than the update timeout, so its request is canceled at the deadline.
	api.firstModifyDelay = 3 * time.Second
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Second)
	state := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		map[string]interface{}{
			"schema_name":   testChromePolicyUrls,
			"schema_values": map[string]interface{}{"maxUrls": "5", "urlBlocklist": `["a.example.com"]`},
		},
	}).State()

	start := time.Now()
	_, diags := testChromePolicyApply(t, r, state, []interface{}{
		testChromePolicy(testChromePolicyUrls, "urlBlocklist", `["b.example.com"]`),
	}, client)

	// The restore runs after the deadline, on its own context.
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "Terraform wrote the values from before the change again") ||
		!strings.Contains(diags[0].Summary, "can still apply that write later") {
		t.Fatalf("diagnostics = %#v, want an error that says the values were restored", diags)
	}
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Errorf("apply took %s, want the write request canceled at the deadline", elapsed)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	// The canceled write never reaches the fake API, so the one write is the restore of the value from before.
	restored, _ := json.Marshal(api.pending[testChromePolicyUrls])
	if api.modifyCalls != 1 || string(restored) != `{"maxUrls":"5","urlBlocklist":["a.example.com"]}` {
		t.Errorf("batchModify calls = %d, restored value = %s, want 1 call that restores the value from before", api.modifyCalls, restored)
	}
}

func TestChromePolicyApply_canceledDuringPollIsError(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.hiddenResolves = testChromePolicyNeverSeen
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(30 * time.Second)
	config := []interface{}{testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://b.example.com"`)}
	diff, err := r.Diff(context.Background(), nil, testChromePolicyConfig(config), client)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(time.Second)
		cancel()
	}()

	start := time.Now()
	state, diags := r.Apply(ctx, nil, diff, client)

	if !diags.HasError() || !strings.Contains(diags[0].Summary, "canceled") {
		t.Fatalf("diagnostics = %#v, want an error about the cancellation", diags)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("apply took %s, want it to stop when the context is canceled", elapsed)
	}
	testChromePolicyNoAppliedValues(t, state)
}

func TestChromePolicyUpdate_resetPolicyNotVisibleIsError(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyUrls, map[string]interface{}{"maxUrls": float64(5), "urlBlocklist": []interface{}{"a.example.com"}})
	api.hiddenResolves = testChromePolicyNeverSeen
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(3 * time.Second)
	state := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		map[string]interface{}{
			"schema_name":   testChromePolicyUrls,
			"schema_values": map[string]interface{}{"maxUrls": "5", "urlBlocklist": `["a.example.com"]`},
		},
	}).State()

	// maxUrls is removed, so the policy is reset before the write, and the write never becomes visible.
	newState, diags := testChromePolicyApply(t, r, state, []interface{}{
		testChromePolicy(testChromePolicyUrls, "urlBlocklist", `["b.example.com"]`),
	}, client)

	if !diags.HasError() || !strings.Contains(diags[0].Summary, "were reset before the write") {
		t.Fatalf("diagnostics = %#v, want an error about the reset policy", diags)
	}
	// The update keeps the prior state.
	if newState == nil || newState.Attributes["policies.0.schema_values.maxUrls"] != "5" {
		t.Errorf("state after the failed update = %#v, want the prior state", newState)
	}
}

func TestChromePolicyCreate_largeIntegerIsExact(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(5 * time.Second)
	// 2^53 + 1, which a float64 cannot hold.
	d := testChromePolicyData(t, r, "", []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "9007199254740993"),
	})

	requireNoDiags(t, r.CreateContext(context.Background(), d, client))

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.modifyRequests) != 1 {
		t.Fatalf("write requests = %d, want 1", len(api.modifyRequests))
	}
	if got := string(api.modifyRequests[0].PolicyValue.Value); got != `{"maxConnectionsPerProxy":9007199254740993}` {
		t.Errorf("value written to the API = %s, want the exact integer", got)
	}
}

func TestMapEntryName(t *testing.T) {
	t.Parallel()

	for fieldName, want := range map[string]string{
		"labels":   "LabelsEntry",
		"foo_bar":  "FooBarEntry",
		"fooBar":   "FooBarEntry",
		"_x__y":    "XYEntry",
		"field_1a": "Field1aEntry",
	} {
		if got := mapEntryName(fieldName); got != want {
			t.Errorf("mapEntryName(%q) = %q, want %q", fieldName, got, want)
		}
	}
}

func TestChromePolicySchemaTypes_normalizeBool(t *testing.T) {
	t.Parallel()

	types := newChromePolicySchemaTypes(testChromePolicySchemas[testChromePolicyPolicy])
	types.policyMessage = "Policy"
	field := types.topLevelField("enabled")

	for value, want := range map[interface{}]interface{}{"true": true, "false": false, "1": "1", "t": "t", true: true} {
		if got := types.normalize(*field, value); got != want {
			t.Errorf("normalize(%#v) = %#v, want %#v", value, got, want)
		}
	}
}

func TestChromePolicyCreate_duplicateSchemaNameIsError(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(2 * time.Second)
	d := testChromePolicyData(t, r, "", []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "34"),
	})

	diags := r.CreateContext(context.Background(), d, client)

	if !diags.HasError() || !strings.Contains(diags[0].Summary, testChromePolicyConns+") is set more than once") {
		t.Fatalf("diagnostics = %#v, want an error for the duplicate schema name", diags)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.modifyCalls != 0 {
		t.Errorf("batchModify calls = %d, want 0", api.modifyCalls)
	}
}

func TestChromePolicyPlan_reportsAllErrors(t *testing.T) {
	t.Parallel()

	client := newTestChromePolicyClient(t, newFakeChromePolicyAPI(t))
	r := testChromePolicyResource(time.Minute)

	_, err := r.Diff(context.Background(), nil, testChromePolicyConfig([]interface{}{
		testChromePolicy(testChromePolicyBookmarks, "managedBookmarks", `{"toplevelName":5}`),
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", `"x"`),
	}), client)

	if err == nil || !strings.Contains(err.Error(), "managedBookmarks.toplevelName") || !strings.Contains(err.Error(), "maxConnectionsPerProxy") {
		t.Fatalf("plan error = %v, want errors for both policies", err)
	}
	if !strings.Contains(err.Error(), "; ") {
		t.Errorf("plan error = %v, want the errors joined with \"; \"", err)
	}
}

func TestChromePolicyCreate_updateMaskIsSorted(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(5 * time.Second)
	d := testChromePolicyData(t, r, "", []interface{}{
		map[string]interface{}{
			"schema_name":   testChromePolicyUrls,
			"schema_values": map[string]interface{}{"urlBlocklist": `["a.example.com"]`, "maxUrls": "5"},
		},
	})

	requireNoDiags(t, r.CreateContext(context.Background(), d, client))

	if got := api.modifyMasks(testChromePolicyUrls); !reflect.DeepEqual(got, []string{"maxUrls,urlBlocklist"}) {
		t.Errorf("update masks = %q, want [maxUrls,urlBlocklist]", got)
	}
}

func TestChromePolicyCreate_writeSetsOnlyMaskedFields(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	// maxUrls is set on the org unit and is not in the configuration.
	api.setVisible(testChromePolicyUrls, map[string]interface{}{"maxUrls": float64(5), "urlBlocklist": []interface{}{"a.example.com"}})
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(5 * time.Second)
	d := testChromePolicyData(t, r, "", []interface{}{
		testChromePolicy(testChromePolicyUrls, "urlBlocklist", `["b.example.com"]`),
	})

	requireNoDiags(t, r.CreateContext(context.Background(), d, client))

	if got := api.modifyMasks(testChromePolicyUrls); !reflect.DeepEqual(got, []string{"urlBlocklist"}) {
		t.Errorf("update masks = %q, want [urlBlocklist]", got)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	want := map[string]interface{}{"maxUrls": float64(5), "urlBlocklist": []interface{}{"b.example.com"}}
	if got := api.visible[testChromePolicyUrls]; !reflect.DeepEqual(got, want) {
		t.Errorf("value in the API = %#v, want %#v", got, want)
	}
}

func TestChromePolicySchemaCache_sharedStore(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	client := newTestChromePolicyClient(t, api)
	client.chromePolicySchemas = newChromePolicySchemaStore()

	first, diags := newChromePolicySchemaCache(client)
	requireNoDiags(t, diags)
	// A copy of the client shares the store.
	copied := *client
	second, diags := newChromePolicySchemaCache(&copied)
	requireNoDiags(t, diags)

	if _, err := first.get(context.Background(), testChromePolicyConns); err != nil {
		t.Fatal(err)
	}
	if _, err := second.get(context.Background(), testChromePolicyConns); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		schemas := first
		if i%2 == 1 {
			schemas = second
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, name := range []string{testChromePolicyConns, testChromePolicyHomepage, testChromePolicyUrls} {
				if _, err := schemas.get(context.Background(), name); err != nil {
					t.Errorf("get %s: %v", name, err)
				}
			}
		}()
	}
	wg.Wait()

	api.mu.Lock()
	defer api.mu.Unlock()
	if got := api.schemaGets[testChromePolicyConns]; got != 1 {
		t.Errorf("schema GET calls for %s = %d, want 1", testChromePolicyConns, got)
	}
}

func TestChromePolicyUpdate_failedConfirmingResend(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		// failModifyCalls are the writes that fail. The first write is dropped, so writes 2 and later are resends.
		failModifyCalls map[int]bool
		timeout         time.Duration
		// wantConverged is true if a later resend succeeds in time and overwrites the other value.
		wantConverged bool
	}{
		"resend fails once, then succeeds": {
			failModifyCalls: map[int]bool{2: true},
			timeout:         30 * time.Second,
			wantConverged:   true,
		},
		"resends fail four times": {
			failModifyCalls: map[int]bool{2: true, 3: true, 4: true, 5: true},
			timeout:         30 * time.Second,
		},
		"all resends fail": {
			failModifyCalls: map[int]bool{2: true, 3: true, 4: true, 5: true, 6: true, 7: true, 8: true},
			timeout:         6 * time.Second,
		},
	}

	for name, test := range tests {
		test := test
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			api := newFakeChromePolicyAPI(t)
			api.setVisible(testChromePolicyHomepage, map[string]interface{}{"homepageLocation": "https://a.example.com"})
			// The API drops the write, and another writer sets a value during the poll.
			api.dropWrites = 1
			api.failModifyCalls = test.failModifyCalls
			client := newTestChromePolicyClient(t, api)

			r := testChromePolicyResource(test.timeout)
			state := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
				testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://a.example.com"`),
			}).State()
			go func() {
				time.Sleep(700 * time.Millisecond)
				api.setVisible(testChromePolicyHomepage, map[string]interface{}{"homepageLocation": "https://other.example.com"})
			}()

			config := []interface{}{testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://b.example.com"`)}
			state, diags := testChromePolicyApply(t, r, state, config, client)

			// A failed resend never confirms the other value, so it is never recorded.
			testChromePolicyNoAppliedValues(t, state)
			for _, d := range diags {
				if d.Summary == "Chrome Policy values differ from the configuration" {
					t.Fatalf("diagnostics = %#v, want the other value not accepted", diags)
				}
			}

			if test.wantConverged {
				requireNoDiags(t, diags)
				return
			}
			if diags.HasError() || len(diags) != 1 || diags[0].Summary != "Chrome Policy values are not visible yet" {
				t.Fatalf("diagnostics = %#v, want the warning that the values are not visible", diags)
			}
			if test.timeout < 10*time.Second && !strings.Contains(diags[0].Detail, "that request failed") {
				t.Errorf("warning detail = %q, want the error of the failed resend", diags[0].Detail)
			}
		})
	}
}

func TestChromePolicyUpdate_halfTimeResendAfterConfirmingResend(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyConns, map[string]interface{}{"maxConnectionsPerProxy": float64(33)})
	api.transform = func(schemaName string, value map[string]interface{}) map[string]interface{} {
		if schemaName == testChromePolicyConns {
			// The API keeps the value from before the write.
			return map[string]interface{}{"maxConnectionsPerProxy": float64(33)}
		}
		return testChromePolicySlash(schemaName, value)
	}
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(8 * time.Second)
	state := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
	}).State()

	// The homepage gets a new form early, which is confirmed with a resend. The other policy keeps its old value.
	_, diags := testChromePolicyApply(t, r, state, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "34"),
		testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://example.com"`),
	}, client)
	if diags.HasError() {
		t.Fatalf("diagnostics = %#v, want no error", diags)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	// The write, the confirming resend, and the half-time resend, which the earlier resend does not suppress.
	if api.modifyCalls != 3 {
		t.Errorf("batchModify calls = %d, want 3", api.modifyCalls)
	}
}

func TestChromePolicyRead_largeIntegerNumberIsExact(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.numbersAsNumbers = true
	// 2^53 + 1, which a float64 cannot hold.
	api.setVisible(testChromePolicyConns, map[string]interface{}{"maxConnectionsPerProxy": json.Number("9007199254740993")})
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	// A different value in state, so that Read converts the API value.
	d := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "1"),
	})

	requireNoDiags(t, r.ReadContext(context.Background(), d, client))

	if got := testChromePolicyValues(t, d)[testChromePolicyConns]["maxConnectionsPerProxy"]; got != "9007199254740993" {
		t.Errorf("maxConnectionsPerProxy in state = %v, want 9007199254740993", got)
	}
}
