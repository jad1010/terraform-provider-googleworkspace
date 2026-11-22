// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

const (
	testChromePolicyAppA = "chrome:aaaa"
	testChromePolicyAppB = "chrome:bbbb"
)

func testChromePolicyAppKeys(app string) []interface{} {
	return []interface{}{map[string]interface{}{"target_key": "app_id", "target_value": app}}
}

func testChromePolicyAppEntry(app string) string {
	return testChromePolicyEntry(testChromePolicyAppInstall, map[string]string{"app_id": app})
}

func testChromePolicyAppConfig(app string, policies []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"org_unit_id":            testChromePolicyOrgUnit,
		"additional_target_keys": testChromePolicyAppKeys(app),
		"policies":               policies,
	}
}

func testChromePolicyPlanApply(t *testing.T, r *schema.Resource, state *terraform.InstanceState, raw map[string]interface{}, client *apiClient) (*terraform.InstanceState, diag.Diagnostics) {
	t.Helper()

	diff, err := r.Diff(context.Background(), state, terraform.NewResourceConfigRaw(raw), client)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return r.Apply(context.Background(), state, diff, client)
}

// testChromePolicyRefreshPlan refreshes state and plans the raw configuration. It returns the refreshed state and the
// plan.
func testChromePolicyRefreshPlan(t *testing.T, r *schema.Resource, state *terraform.InstanceState, raw map[string]interface{}, client *apiClient) (*terraform.InstanceState, *terraform.InstanceDiff) {
	t.Helper()

	state, diags := r.RefreshWithoutUpgrade(context.Background(), state, client)
	requireNoDiags(t, diags)
	diff, err := r.Diff(context.Background(), state, terraform.NewResourceConfigRaw(raw), client)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return state, diff
}

func testChromePolicyRequireRequests(t *testing.T, api *fakeChromePolicyAPI, want []string) {
	t.Helper()

	api.mu.Lock()
	defer api.mu.Unlock()
	if !reflect.DeepEqual(api.requests, want) {
		t.Errorf("write requests = %q, want %q", api.requests, want)
	}
}

func TestChromePolicyCreate_appScopedPolicy(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.hiddenResolves = 1
	// The policy of another app. Resolve returns it too.
	api.setVisible(testChromePolicyAppEntry(testChromePolicyAppB), map[string]interface{}{"appInstallType": "FORCED"})
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	config := testChromePolicyAppConfig(testChromePolicyAppA, []interface{}{
		testChromePolicy(testChromePolicyAppInstall, "appInstallType", `"ALLOWED"`),
	})

	state, diags := testChromePolicyPlanApply(t, r, nil, config, client)
	requireNoDiags(t, diags)

	if state.ID != testChromePolicyOrgUnit {
		t.Fatalf("ID = %q, want %q", state.ID, testChromePolicyOrgUnit)
	}
	keys, err := chromePolicyAdditionalTargetKeys(r.Data(state).Get("additional_target_keys"))
	if err != nil || keys["app_id"] != testChromePolicyAppA {
		t.Errorf("additional_target_keys = %v (%v), want app_id = %q", keys, err, testChromePolicyAppA)
	}
	if got := state.Attributes["policies.0.schema_values.appInstallType"]; got != `"ALLOWED"` {
		t.Errorf("appInstallType in state = %q, want \"ALLOWED\"", got)
	}
	testChromePolicyRequireRequests(t, api, []string{"orgunits:batchModify orgunits/ou1 app_id=" + testChromePolicyAppA})

	api.mu.Lock()
	if got := api.visible[testChromePolicyAppEntry(testChromePolicyAppB)]["appInstallType"]; got != "FORCED" {
		t.Errorf("value of the other app = %v, want FORCED", got)
	}
	api.mu.Unlock()

	if _, diff := testChromePolicyRefreshPlan(t, r, state, config, client); diff != nil && !diff.Empty() {
		t.Errorf("plan after the apply = %#v, want no change", diff)
	}
}

func TestChromePolicyRead_matchesResolvedPolicyByTargetKeys(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyAppEntry(testChromePolicyAppA), map[string]interface{}{"appInstallType": "ALLOWED"})
	api.setVisible(testChromePolicyAppEntry(testChromePolicyAppB), map[string]interface{}{"appInstallType": "FORCED"})
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	for app, want := range map[string]string{testChromePolicyAppA: `"ALLOWED"`, testChromePolicyAppB: `"FORCED"`} {
		d := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
			testChromePolicy(testChromePolicyAppInstall, "appInstallType", `"BLOCKED"`),
		})
		if err := d.Set("additional_target_keys", testChromePolicyAppKeys(app)); err != nil {
			t.Fatal(err)
		}

		requireNoDiags(t, r.ReadContext(context.Background(), d, client))

		if got := testChromePolicyValues(t, d)[testChromePolicyAppInstall]["appInstallType"]; got != want {
			t.Errorf("appInstallType for %s = %v, want %s", app, got, want)
		}
	}
}

func TestChromePolicyRead_appPolicyWithoutTargetKeysIsError(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyAppEntry(testChromePolicyAppA), map[string]interface{}{"appInstallType": "ALLOWED"})
	api.setVisible(testChromePolicyAppEntry(testChromePolicyAppB), map[string]interface{}{"appInstallType": "FORCED"})
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	d := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		testChromePolicy(testChromePolicyAppInstall, "appInstallType", `"ALLOWED"`),
	})

	diags := r.ReadContext(context.Background(), d, client)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "set additional_target_keys") {
		t.Fatalf("diagnostics = %#v, want an error that tells to set additional_target_keys", diags)
	}
}

func TestChromePolicyRefresh_appScopedDrift(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	config := testChromePolicyAppConfig(testChromePolicyAppA, []interface{}{
		testChromePolicy(testChromePolicyAppInstall, "appInstallType", `"ALLOWED"`),
	})
	state, diags := testChromePolicyPlanApply(t, r, nil, config, client)
	requireNoDiags(t, diags)

	// A change outside Terraform, for this app only.
	api.setVisible(testChromePolicyAppEntry(testChromePolicyAppA), map[string]interface{}{"appInstallType": "BLOCKED"})
	api.setVisible(testChromePolicyAppEntry(testChromePolicyAppB), map[string]interface{}{"appInstallType": "FORCED"})

	refreshed, diff := testChromePolicyRefreshPlan(t, r, state, config, client)
	if got := refreshed.Attributes["policies.0.schema_values.appInstallType"]; got != `"BLOCKED"` {
		t.Errorf("appInstallType after refresh = %q, want \"BLOCKED\"", got)
	}
	if diff == nil || diff.Empty() || diff.RequiresNew() {
		t.Errorf("plan = %#v, want an in-place change", diff)
	}

	// The policy of this app is removed outside Terraform.
	api.mu.Lock()
	delete(api.visible, testChromePolicyAppEntry(testChromePolicyAppA))
	api.mu.Unlock()

	refreshed, diags = r.RefreshWithoutUpgrade(context.Background(), state, client)
	if diags.HasError() || len(diags) != 1 || !strings.Contains(diags[0].Detail, "app_id="+testChromePolicyAppA) {
		t.Fatalf("diagnostics = %#v, want one warning that names the app", diags)
	}
	if refreshed != nil && refreshed.ID != "" {
		t.Errorf("state after refresh = %#v, want the resource removed", refreshed)
	}
}

func TestChromePolicyUpdate_appScopedResetSendsTargetKeys(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	state, diags := testChromePolicyPlanApply(t, r, nil, testChromePolicyAppConfig(testChromePolicyAppA, []interface{}{
		map[string]interface{}{
			"schema_name": testChromePolicyAppInstall,
			"schema_values": map[string]interface{}{
				"appInstallType":  `"FORCED"`,
				"installationUrl": `"https://example.com/app.crx"`,
			},
		},
	}), client)
	requireNoDiags(t, diags)

	// A field is removed, so the policy is inherited before the write.
	state, diags = testChromePolicyPlanApply(t, r, state, testChromePolicyAppConfig(testChromePolicyAppA, []interface{}{
		testChromePolicy(testChromePolicyAppInstall, "appInstallType", `"FORCED"`),
	}), client)
	requireNoDiags(t, diags)

	requireNoDiags(t, r.DeleteContext(context.Background(), r.Data(state), client))

	target := "orgunits/ou1 app_id=" + testChromePolicyAppA
	testChromePolicyRequireRequests(t, api, []string{
		"orgunits:batchModify " + target,
		"orgunits:batchInherit " + target,
		"orgunits:batchModify " + target,
		"orgunits:batchInherit " + target,
	})
}

func TestChromePolicyPlan_upgradeWithoutAdditionalTargetKeys(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyConns, map[string]interface{}{"maxConnectionsPerProxy": float64(33)})
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	config := []interface{}{testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33")}

	// State from an earlier version of the provider has no additional_target_keys and no applied_values.
	state := testChromePolicyData(t, r, testChromePolicyOrgUnit, config).State()
	for k := range state.Attributes {
		if strings.HasPrefix(k, "additional_target_keys") || strings.HasPrefix(k, "applied_values") {
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

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.modifyCalls != 0 || api.inheritCalls != 0 {
		t.Errorf("writes = %d batchModify, %d batchInherit, want none", api.modifyCalls, api.inheritCalls)
	}
}

func TestChromePolicyPlan_additionalTargetKeysForceNew(t *testing.T) {
	t.Parallel()

	client := newTestChromePolicyClient(t, newFakeChromePolicyAPI(t))
	r := testChromePolicyResource(time.Minute)
	policies := []interface{}{testChromePolicy(testChromePolicyAppInstall, "appInstallType", `"ALLOWED"`)}

	d := testChromePolicyData(t, r, testChromePolicyOrgUnit, policies)
	if err := d.Set("additional_target_keys", testChromePolicyAppKeys(testChromePolicyAppA)); err != nil {
		t.Fatal(err)
	}
	diff, err := r.Diff(context.Background(), d.State(), terraform.NewResourceConfigRaw(testChromePolicyAppConfig(testChromePolicyAppB, policies)), client)
	if err != nil {
		t.Fatal(err)
	}
	if diff == nil || !diff.RequiresNew() {
		t.Errorf("plan = %#v, want a new resource", diff)
	}
}

func TestChromePolicyPlan_rejectsInvalidTargets(t *testing.T) {
	t.Parallel()

	client := newTestChromePolicyClient(t, newFakeChromePolicyAPI(t))
	appPolicy := []interface{}{testChromePolicy(testChromePolicyAppInstall, "appInstallType", `"ALLOWED"`)}

	tests := map[string]struct {
		resource *schema.Resource
		raw      map[string]interface{}
		want     string
	}{
		"duplicate target key": {
			resource: testChromePolicyResource(time.Minute),
			raw: map[string]interface{}{
				"org_unit_id":            testChromePolicyOrgUnit,
				"additional_target_keys": append(testChromePolicyAppKeys(testChromePolicyAppA), testChromePolicyAppKeys(testChromePolicyAppB)...),
				"policies":               appPolicy,
			},
			want: `target_key "app_id" more than once`,
		},
		"app policy without app": {
			resource: testChromePolicyResource(time.Minute),
			raw:      map[string]interface{}{"org_unit_id": testChromePolicyOrgUnit, "policies": appPolicy},
			want:     "uses the additional target keys app_id, but additional_target_keys has none",
		},
	}

	for name, tt := range tests {
		_, err := tt.resource.Diff(context.Background(), nil, terraform.NewResourceConfigRaw(tt.raw), client)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: plan error = %v, want an error that contains %q", name, err, tt.want)
		}
	}
}

func TestChromePolicyCreate_contextCancelsRequests(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.firstModifyDelay = 2 * time.Second
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	d := testChromePolicyData(t, r, "", []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	diags := r.CreateContext(ctx, d, client)
	elapsed := time.Since(start)

	if !diags.HasError() {
		t.Fatalf("diagnostics = %#v, want an error", diags)
	}
	if elapsed >= 1500*time.Millisecond {
		t.Errorf("create took %s, want it to stop at the context deadline", elapsed)
	}

	// The request itself is canceled, not only the wait for it.
	for deadline := time.Now().Add(time.Second); atomic.LoadInt32(&api.modifyCanceled) == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&api.modifyCanceled) != 1 {
		t.Error("the batchModify request was not canceled at the context deadline")
	}
}

func TestChromePolicyRead_matchesResolvedPolicyOnSecondPage(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	// The API returns the policies of all apps, one on each page. The policy of app B is on the second page.
	api.resolvePageSize = 1
	api.resolveIgnoresKeys = true
	api.setVisible(testChromePolicyAppEntry(testChromePolicyAppA), map[string]interface{}{"appInstallType": "ALLOWED"})
	api.setVisible(testChromePolicyAppEntry(testChromePolicyAppB), map[string]interface{}{"appInstallType": "FORCED"})
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	d := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		testChromePolicy(testChromePolicyAppInstall, "appInstallType", `"BLOCKED"`),
	})
	if err := d.Set("additional_target_keys", testChromePolicyAppKeys(testChromePolicyAppB)); err != nil {
		t.Fatal(err)
	}

	requireNoDiags(t, r.ReadContext(context.Background(), d, client))

	if got := testChromePolicyValues(t, d)[testChromePolicyAppInstall]["appInstallType"]; got != `"FORCED"` {
		t.Errorf("appInstallType = %v, want \"FORCED\" from the second page", got)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.resolveCalls != 2 {
		t.Errorf("resolve calls = %d, want 2 (one for each page)", api.resolveCalls)
	}
}

func TestChromePolicyRead_twoPagesWithoutTargetKeysIsError(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.resolvePageSize = 1
	api.setVisible(testChromePolicyAppEntry(testChromePolicyAppA), map[string]interface{}{"appInstallType": "ALLOWED"})
	api.setVisible(testChromePolicyAppEntry(testChromePolicyAppB), map[string]interface{}{"appInstallType": "FORCED"})
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	d := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		testChromePolicy(testChromePolicyAppInstall, "appInstallType", `"ALLOWED"`),
	})

	// The error counts the policies of both pages.
	diags := r.ReadContext(context.Background(), d, client)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "resolved 2 policies") {
		t.Fatalf("diagnostics = %#v, want an error about 2 resolved policies", diags)
	}
}

func TestChromePolicyRead_oneAppPolicyWithoutTargetKeysIsNotAdopted(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	// Exactly one app has the policy, and the resource has no additional target keys.
	api.setVisible(testChromePolicyAppEntry(testChromePolicyAppA), map[string]interface{}{"appInstallType": "ALLOWED"})
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	d := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		testChromePolicy(testChromePolicyAppInstall, "appInstallType", `"BLOCKED"`),
	})

	diags := r.ReadContext(context.Background(), d, client)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "set additional_target_keys") {
		t.Fatalf("diagnostics = %#v, want an error that tells to set additional_target_keys", diags)
	}
	if got := testChromePolicyValues(t, d)[testChromePolicyAppInstall]["appInstallType"]; got != `"BLOCKED"` {
		t.Errorf("appInstallType in state = %v, want the prior value, not the value of the app", got)
	}
}

func TestChromePolicyRead_otherTargetResourceIsError(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	api.setVisible(testChromePolicyConns, map[string]interface{}{"maxConnectionsPerProxy": float64(33)})
	api.resolvedTargetResource = "orgunits/other"
	client := newTestChromePolicyClient(t, api)

	r := testChromePolicyResource(time.Minute)
	d := testChromePolicyData(t, r, testChromePolicyOrgUnit, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "10"),
	})

	diags := r.ReadContext(context.Background(), d, client)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, `"orgunits/other"`) ||
		strings.Contains(diags[0].Summary, "set additional_target_keys") {
		t.Fatalf("diagnostics = %#v, want an error that names the returned target resource", diags)
	}
	if got := testChromePolicyValues(t, d)[testChromePolicyConns]["maxConnectionsPerProxy"]; got != "10" {
		t.Errorf("maxConnectionsPerProxy in state = %v, want the prior value", got)
	}
}

func TestChromePolicyPlan_additionalTargetKeysOrderIsNoChange(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	client := newTestChromePolicyClient(t, api)
	r := testChromePolicyResource(time.Minute)

	block := func(key, value string) interface{} {
		return map[string]interface{}{"target_key": key, "target_value": value}
	}
	config := func(blocks ...interface{}) map[string]interface{} {
		return map[string]interface{}{
			"org_unit_id":            testChromePolicyOrgUnit,
			"additional_target_keys": blocks,
			"policies": []interface{}{
				testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
			},
		}
	}

	state, diags := testChromePolicyPlanApply(t, r, nil, config(block("app_id", testChromePolicyAppA), block("printer_id", "p1")), client)
	if diags.HasError() {
		t.Fatalf("apply diagnostics = %#v", diags)
	}

	// The same blocks in another order.
	if _, diff := testChromePolicyRefreshPlan(t, r, state, config(block("printer_id", "p1"), block("app_id", testChromePolicyAppA)), client); diff != nil && !diff.Empty() {
		t.Errorf("plan with the blocks in another order = %#v, want no change", diff)
	}
}

func TestChromePolicyCreate_unusedAdditionalTargetKeysWarn(t *testing.T) {
	t.Parallel()

	api := newFakeChromePolicyAPI(t)
	client := newTestChromePolicyClient(t, api)
	r := testChromePolicyResource(time.Minute)

	// No configured policy schema names additional target keys.
	_, diags := testChromePolicyPlanApply(t, r, nil, testChromePolicyAppConfig(testChromePolicyAppA, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
	}), client)

	if diags.HasError() || len(diags) != 1 || diags[0].Severity != diag.Warning ||
		diags[0].Summary != "additional_target_keys are not used by the policy schemas" {
		t.Fatalf("diagnostics = %#v, want one warning about the unused keys", diags)
	}

	// A policy schema that names the key gets no warning.
	_, diags = testChromePolicyPlanApply(t, r, nil, testChromePolicyAppConfig(testChromePolicyAppB, []interface{}{
		testChromePolicy(testChromePolicyAppInstall, "appInstallType", `"ALLOWED"`),
	}), client)
	requireNoDiags(t, diags)
}
