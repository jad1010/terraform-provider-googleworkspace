// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"google.golang.org/api/chromepolicy/v1"
)

func testChromePolicyOrderingConfig(groupIds ...interface{}) map[string]interface{} {
	return map[string]interface{}{
		"policy_namespace": "chrome.users.apps",
		"policy_target_key": []interface{}{map[string]interface{}{
			"additional_target_key_names": map[string]interface{}{"app_id": testChromePolicyAppA},
		}},
		"group_ids": groupIds,
	}
}

// testChromePolicyOrderingGroups sets a policy of app A on each group, so that the fake API accepts an ordering of
// these groups.
func testChromePolicyOrderingGroups(api *fakeChromePolicyAPI, groupIds ...string) {
	for _, id := range groupIds {
		api.setVisibleOn("groups/"+id, testChromePolicyAppEntry(testChromePolicyAppA), map[string]interface{}{"appInstallType": "FORCED"})
	}
}

func TestChromePolicyGroupPriorityOrdering_lifecycle(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	testChromePolicyOrderingGroups(api, testChromePolicyGroup, testChromePolicyGroup2)
	client := newTestChromePolicyClient(t, api)
	r := resourceChromePolicyGroupPriorityOrdering()

	config := testChromePolicyOrderingConfig(testChromePolicyGroup, testChromePolicyGroup2)
	state, diags := testChromePolicyPlanApply(t, r, nil, config, client)
	requireNoDiags(t, diags)

	wantId := "namespace=chrome.users.apps::app_id=" + testChromePolicyAppA
	if state.ID != wantId {
		t.Errorf("ID = %q, want %q", state.ID, wantId)
	}
	orderingKey := testChromePolicyOrderingKey("", "chrome.users.apps", &chromepolicy.GoogleChromePolicyVersionsV1PolicyTargetKey{
		AdditionalTargetKeys: map[string]string{"app_id": testChromePolicyAppA},
	})
	api.mu.Lock()
	if got := api.orderings[orderingKey]; !reflect.DeepEqual(got, []string{testChromePolicyGroup, testChromePolicyGroup2}) {
		t.Errorf("ordering = %q, want [g1 g2]", got)
	}
	api.mu.Unlock()

	if _, diff := testChromePolicyRefreshPlan(t, r, state, config, client); diff != nil && !diff.Empty() {
		t.Errorf("plan after create = %#v, want no change", diff)
	}

	// A change outside Terraform is drift.
	api.mu.Lock()
	api.orderings[orderingKey] = []string{testChromePolicyGroup2, testChromePolicyGroup}
	api.mu.Unlock()
	refreshed, diff := testChromePolicyRefreshPlan(t, r, state, config, client)
	if got := refreshed.Attributes["group_ids.0"]; got != testChromePolicyGroup2 {
		t.Errorf("group_ids.0 after refresh = %q, want %s", got, testChromePolicyGroup2)
	}
	if diff == nil || diff.Empty() || diff.RequiresNew() {
		t.Fatalf("plan = %#v, want an in-place change", diff)
	}
	state, diags = r.Apply(context.Background(), refreshed, diff, client)
	requireNoDiags(t, diags)

	// Import gets the same state.
	d := r.Data(nil)
	d.SetId(wantId)
	imported, err := r.Importer.StateContext(context.Background(), d, client)
	if err != nil {
		t.Fatal(err)
	}
	requireNoDiags(t, r.ReadContext(context.Background(), imported[0], client))
	if _, diff := testChromePolicyRefreshPlan(t, r, imported[0].State(), config, client); diff != nil && !diff.Empty() {
		t.Errorf("plan after import = %#v, want no change", diff)
	}

	// Delete removes the resource from state only.
	diags = r.DeleteContext(context.Background(), r.Data(state), client)
	if diags.HasError() || len(diags) != 1 || diags[0].Severity != diag.Warning {
		t.Errorf("delete diagnostics = %#v, want one warning", diags)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.orderingUpdates != 2 {
		t.Errorf("updateGroupPriorityOrdering calls = %d, want 2", api.orderingUpdates)
	}
	if got := api.orderings[orderingKey]; !reflect.DeepEqual(got, []string{testChromePolicyGroup, testChromePolicyGroup2}) {
		t.Errorf("ordering after delete = %q, want [g1 g2]", got)
	}
}

func TestChromePolicyGroupPriorityOrdering_partialGroupListIsError(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	testChromePolicyOrderingGroups(api, testChromePolicyGroup, testChromePolicyGroup2)
	client := newTestChromePolicyClient(t, api)
	r := resourceChromePolicyGroupPriorityOrdering()

	// g2 also sets the policy, so a list without it is an error, not drift.
	_, diags := testChromePolicyPlanApply(t, r, nil, testChromePolicyOrderingConfig(testChromePolicyGroup), client)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "must include all group IDs") ||
		!strings.Contains(diags[0].Detail, "A common cause of this error") {
		t.Fatalf("diagnostics = %#v, want an error that tells to list every group", diags)
	}
}

func TestChromePolicyGroupPriorityOrderingRead_notConfiguredRemovesResource(t *testing.T) {
	t.Parallel()

	client := newTestChromePolicyClient(t, newFakeChromePolicyAPI(t))
	r := resourceChromePolicyGroupPriorityOrdering()

	d := r.Data(nil)
	d.SetId(testChromePolicyAppInstall + "::app_id=" + testChromePolicyAppA)
	imported, err := r.Importer.StateContext(context.Background(), d, client)
	if err != nil {
		t.Fatal(err)
	}
	if got := imported[0].Get("policy_schema"); got != testChromePolicyAppInstall {
		t.Errorf("policy_schema = %v, want %s", got, testChromePolicyAppInstall)
	}
	if got := imported[0].Get("policy_target_key.0.additional_target_key_names.app_id"); got != testChromePolicyAppA {
		t.Errorf("app_id = %v, want %s", got, testChromePolicyAppA)
	}

	// No group sets the policy.
	diags := r.ReadContext(context.Background(), imported[0], client)
	if diags.HasError() || len(diags) != 1 || diags[0].Severity != diag.Warning {
		t.Fatalf("diagnostics = %#v, want one warning", diags)
	}
	if imported[0].Id() != "" {
		t.Errorf("ID = %q, want the resource removed", imported[0].Id())
	}

	// The API requires the app, so an import ID without additional target keys is an error.
	for _, id := range []string{"chrome.users.Wallpaper", "chrome.users.Wallpaper:", ":orgunits/1", "namespace=:", "a:b:app_id", "namespace=chrome.users.apps::"} {
		d := r.Data(nil)
		d.SetId(id)
		if _, err := r.Importer.StateContext(context.Background(), d, client); err == nil {
			t.Errorf("import of %q: want an error", id)
		}
	}
}

func TestChromePolicyGroupPriorityOrderingRead_otherBadRequestIsError(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	testChromePolicyOrderingGroups(api, testChromePolicyGroup)
	api.fail = func(method string) (int, string) {
		if method == "policies/groups:listGroupPriorityOrdering" {
			return 400, "Invalid policy namespace"
		}
		return 0, ""
	}
	client := newTestChromePolicyClient(t, api)
	r := resourceChromePolicyGroupPriorityOrdering()

	d := r.Data(nil)
	d.SetId("namespace=chrome.users.apps::app_id=" + testChromePolicyAppA)
	imported, err := r.Importer.StateContext(context.Background(), d, client)
	if err != nil {
		t.Fatal(err)
	}

	diags := r.ReadContext(context.Background(), imported[0], client)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "Invalid policy namespace") {
		t.Fatalf("diagnostics = %#v, want the error of the API", diags)
	}
	if imported[0].Id() == "" {
		t.Error("the resource was removed, want it kept")
	}
}

func TestChromePolicyGroupPriorityOrderingPlan_validation(t *testing.T) {
	t.Parallel()

	r := resourceChromePolicyGroupPriorityOrdering()
	targetKey := []interface{}{map[string]interface{}{
		"additional_target_key_names": map[string]interface{}{"app_id": testChromePolicyAppA},
	}}

	for name, test := range map[string]struct {
		raw  map[string]interface{}
		want string
	}{
		"schema and namespace": {
			raw: map[string]interface{}{
				"policy_schema":     testChromePolicyAppInstall,
				"policy_namespace":  "chrome.users.apps",
				"policy_target_key": targetKey,
				"group_ids":         []interface{}{testChromePolicyGroup},
			},
			want: "only one of",
		},
		"neither schema nor namespace": {
			raw: map[string]interface{}{
				"policy_target_key": targetKey,
				"group_ids":         []interface{}{testChromePolicyGroup},
			},
			want: "one of",
		},
		"no policy target key": {
			raw: map[string]interface{}{
				"policy_namespace": "chrome.users.apps",
				"group_ids":        []interface{}{testChromePolicyGroup},
			},
			want: "policy_target_key",
		},
		"no additional target keys": {
			raw: map[string]interface{}{
				"policy_namespace":  "chrome.users.apps",
				"policy_target_key": []interface{}{map[string]interface{}{"target_resource": "orgunits/1"}},
				"group_ids":         []interface{}{testChromePolicyGroup},
			},
			want: "additional_target_key_names",
		},
		"empty additional target keys": {
			raw: map[string]interface{}{
				"policy_namespace": "chrome.users.apps",
				"policy_target_key": []interface{}{map[string]interface{}{
					"additional_target_key_names": map[string]interface{}{},
				}},
				"group_ids": []interface{}{testChromePolicyGroup},
			},
			want: "must have at least one key",
		},
	} {
		test := test
		t.Run(name, func(t *testing.T) {
			diags := r.Validate(terraform.NewResourceConfigRaw(test.raw))
			var messages []string
			for _, d := range diags {
				messages = append(messages, d.Summary+" "+d.Detail)
			}
			if got := strings.Join(messages, "; "); !diags.HasError() || !strings.Contains(got, test.want) {
				t.Errorf("validation = %q, want an error that contains %q", got, test.want)
			}
		})
	}
}
