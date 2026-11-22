// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

const (
	testChromePolicyGroup  = "g1"
	testChromePolicyGroup2 = "g2"
)

func testChromeGroupPolicyConfig(keys []interface{}, policies []interface{}) map[string]interface{} {
	raw := map[string]interface{}{
		"group_id": testChromePolicyGroup,
		"policies": policies,
	}
	if keys != nil {
		raw["additional_target_keys"] = keys
	}
	return raw
}

// newFakeChromeGroupPolicyAPI returns a fake API whose default target is the group testChromePolicyGroup.
func newFakeChromeGroupPolicyAPI(t *testing.T) *fakeChromePolicyAPI {
	api := newFakeChromePolicyAPI(t)
	api.target = "groups/" + testChromePolicyGroup
	return api
}

func testChromeGroupPolicyResource(timeout time.Duration) *schema.Resource {
	r := resourceChromeGroupPolicy()
	r.Timeouts = &schema.ResourceTimeout{
		Create: schema.DefaultTimeout(timeout),
		Update: schema.DefaultTimeout(timeout),
	}
	return r
}

// testChromePolicyPlanApply plans the raw configuration and applies it, as Terraform does.
func TestChromeGroupPolicy_lifecycle(t *testing.T) {
	t.Parallel()

	api := newFakeChromeGroupPolicyAPI(t)
	api.hiddenResolves = 2
	client := newTestChromePolicyClient(t, api)

	r := testChromeGroupPolicyResource(time.Minute)
	config := testChromeGroupPolicyConfig(nil, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
		testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://example.com"`),
	})

	// Create waits until the values are visible.
	state, diags := testChromePolicyPlanApply(t, r, nil, config, client)
	requireNoDiags(t, diags)
	if state.ID != testChromePolicyGroup {
		t.Fatalf("ID = %q, want %q", state.ID, testChromePolicyGroup)
	}
	if _, diff := testChromePolicyRefreshPlan(t, r, state, config, client); diff != nil && !diff.Empty() {
		t.Errorf("plan after create = %#v, want no change", diff)
	}

	// A removed policy is deleted from the group before the write.
	config = testChromeGroupPolicyConfig(nil, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "34"),
	})
	state, diags = testChromePolicyPlanApply(t, r, state, config, client)
	requireNoDiags(t, diags)
	if got := state.Attributes["policies.#"]; got != "1" {
		t.Errorf("policies.# = %q, want 1", got)
	}

	// A change outside Terraform is drift.
	api.setVisible(testChromePolicyConns, map[string]interface{}{"maxConnectionsPerProxy": float64(40)})
	refreshed, diff := testChromePolicyRefreshPlan(t, r, state, config, client)
	if got := refreshed.Attributes["policies.0.schema_values.maxConnectionsPerProxy"]; got != "40" {
		t.Errorf("maxConnectionsPerProxy after refresh = %q, want 40", got)
	}
	if diff == nil || diff.Empty() || diff.RequiresNew() {
		t.Errorf("plan = %#v, want an in-place change", diff)
	}

	requireNoDiags(t, r.DeleteContext(context.Background(), r.Data(refreshed), client))

	testChromePolicyRequireRequests(t, api, []string{
		"groups:batchModify groups/g1",
		"groups:batchModify groups/g1",
		"groups:batchDelete groups/g1",
		"groups:batchModify groups/g1",
		"groups:batchDelete groups/g1",
	})

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.visible) != 0 || len(api.pending) != 0 {
		t.Errorf("values after delete = %#v, pending %#v, want none", api.visible, api.pending)
	}
}

func TestChromeGroupPolicyCreate_resendsDroppedWrite(t *testing.T) {
	t.Parallel()

	api := newFakeChromeGroupPolicyAPI(t)
	api.dropWrites = 1
	client := newTestChromePolicyClient(t, api)

	r := testChromeGroupPolicyResource(3 * time.Second)
	state, diags := testChromePolicyPlanApply(t, r, nil, testChromeGroupPolicyConfig(nil, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
	}), client)
	requireNoDiags(t, diags)

	if got := state.Attributes["policies.0.schema_values.maxConnectionsPerProxy"]; got != "33" {
		t.Errorf("maxConnectionsPerProxy in state = %q, want 33", got)
	}
	testChromePolicyRequireRequests(t, api, []string{"groups:batchModify groups/g1", "groups:batchModify groups/g1"})
}

func TestChromeGroupPolicyCreate_appScoped(t *testing.T) {
	t.Parallel()

	api := newFakeChromeGroupPolicyAPI(t)
	api.setVisible(testChromePolicyAppEntry(testChromePolicyAppB), map[string]interface{}{"appInstallType": "FORCED"})
	client := newTestChromePolicyClient(t, api)

	r := testChromeGroupPolicyResource(time.Minute)
	config := testChromeGroupPolicyConfig(testChromePolicyAppKeys(testChromePolicyAppA), []interface{}{
		testChromePolicy(testChromePolicyAppInstall, "appInstallType", `"ALLOWED"`),
	})
	state, diags := testChromePolicyPlanApply(t, r, nil, config, client)
	requireNoDiags(t, diags)

	if got := state.Attributes["policies.0.schema_values.appInstallType"]; got != `"ALLOWED"` {
		t.Errorf("appInstallType in state = %q, want \"ALLOWED\"", got)
	}
	if _, diff := testChromePolicyRefreshPlan(t, r, state, config, client); diff != nil && !diff.Empty() {
		t.Errorf("plan after the apply = %#v, want no change", diff)
	}
	testChromePolicyRequireRequests(t, api, []string{"groups:batchModify groups/g1 app_id=" + testChromePolicyAppA})
}

func TestChromeGroupPolicyImport(t *testing.T) {
	t.Parallel()

	api := newFakeChromeGroupPolicyAPI(t)
	api.setVisible(testChromePolicyConns, map[string]interface{}{"maxConnectionsPerProxy": float64(33)})
	webApp := "web:https://example.com/app"
	api.setVisible(testChromePolicyEntry(testChromePolicyAppInstall, map[string]string{"app_id": webApp}), map[string]interface{}{"appInstallType": "FORCED"})
	client := newTestChromePolicyClient(t, api)

	r := testChromeGroupPolicyResource(time.Minute)

	importState := func(id string) (*schema.ResourceData, error) {
		d := r.Data(nil)
		d.SetId(id)
		imported, err := r.Importer.StateContext(context.Background(), d, client)
		if err != nil {
			return nil, err
		}
		requireNoDiags(t, r.ReadContext(context.Background(), imported[0], client))
		return imported[0], nil
	}

	d, err := importState("g1/" + testChromePolicyConns)
	if err != nil {
		t.Fatal(err)
	}
	if d.Id() != testChromePolicyGroup || d.Get("group_id") != testChromePolicyGroup {
		t.Errorf("ID = %q, group_id = %v, want %s", d.Id(), d.Get("group_id"), testChromePolicyGroup)
	}
	if got := testChromePolicyValues(t, d)[testChromePolicyConns]["maxConnectionsPerProxy"]; got != "33" {
		t.Errorf("maxConnectionsPerProxy = %v, want 33", got)
	}

	// The value of a key can contain "/".
	d, err = importState("g1/app_id=" + webApp + "/" + testChromePolicyAppInstall)
	if err != nil {
		t.Fatal(err)
	}
	if keys, err := chromePolicyAdditionalTargetKeys(d.Get("additional_target_keys")); err != nil || keys["app_id"] != webApp {
		t.Errorf("additional_target_keys = %v (%v), want app_id = %s", keys, err, webApp)
	}
	if got := testChromePolicyValues(t, d)[testChromePolicyAppInstall]["appInstallType"]; got != `"FORCED"` {
		t.Errorf("appInstallType = %v, want \"FORCED\"", got)
	}

	for _, id := range []string{"g1", "/" + testChromePolicyConns, "g1/", "g1/app_id/" + testChromePolicyConns, "g1/a=1+a=2/" + testChromePolicyConns} {
		if _, err := importState(id); err == nil {
			t.Errorf("import of %q: want an error", id)
		}
	}
}

func TestChromeGroupPolicyPlan_rejectsSchemaWithoutGroups(t *testing.T) {
	t.Parallel()

	client := newTestChromePolicyClient(t, newFakeChromeGroupPolicyAPI(t))
	r := testChromeGroupPolicyResource(time.Minute)

	_, err := r.Diff(context.Background(), nil, terraform.NewResourceConfigRaw(testChromeGroupPolicyConfig(nil, []interface{}{
		testChromePolicy(testChromePolicyOrgOnly, "enabled", "true"),
	})), client)
	if want := "cannot be set on a group"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("plan error = %v, want an error that contains %q", err, want)
	}
}

// testChromeGroupPolicyState returns the state of a group policy resource with policies, without API calls.
func testChromeGroupPolicyState(t *testing.T, r *schema.Resource, policies []interface{}) *terraform.InstanceState {
	t.Helper()

	d := r.Data(nil)
	d.SetId(testChromePolicyGroup)
	if err := d.Set("group_id", testChromePolicyGroup); err != nil {
		t.Fatal(err)
	}
	if err := d.Set("policies", policies); err != nil {
		t.Fatal(err)
	}
	return d.State()
}

func TestChromeGroupPolicyCreate_foreignValueIsOverwrittenNotRecorded(t *testing.T) {
	t.Parallel()

	api := newFakeChromeGroupPolicyAPI(t)
	// The API drops the write, and another writer sets a value for the app on the group during the poll.
	api.dropWrites = 1
	client := newTestChromePolicyClient(t, api)
	go func() {
		time.Sleep(700 * time.Millisecond)
		api.setVisible(testChromePolicyAppEntry(testChromePolicyAppA), map[string]interface{}{"appInstallType": "BLOCKED"})
	}()

	// The write is sent again at half of the timeout (30 seconds) if no value appears first.
	r := testChromeGroupPolicyResource(time.Minute)
	config := testChromeGroupPolicyConfig(testChromePolicyAppKeys(testChromePolicyAppA), []interface{}{
		testChromePolicy(testChromePolicyAppInstall, "appInstallType", `"ALLOWED"`),
	})
	start := time.Now()
	state, diags := testChromePolicyPlanApply(t, r, nil, config, client)

	// The write that is sent again to confirm the other value overwrites it, so the configured value is found before
	// the resend at half of the timeout.
	requireNoDiags(t, diags)
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Errorf("apply took %s, want the write sent again when the other value appears", elapsed)
	}
	testChromePolicyNoAppliedValues(t, state)
	if got := state.Attributes["policies.0.schema_values.appInstallType"]; got != `"ALLOWED"` {
		t.Errorf("appInstallType in state = %q, want \"ALLOWED\"", got)
	}
	if _, diff := testChromePolicyRefreshPlan(t, r, state, config, client); diff != nil && !diff.Empty() {
		t.Errorf("plan after the apply = %#v, want no change", diff)
	}
	write := "groups:batchModify groups/g1 app_id=" + testChromePolicyAppA
	testChromePolicyRequireRequests(t, api, []string{write, write})
}

func TestChromeGroupPolicyCreate_normalizedValueIsConfirmed(t *testing.T) {
	t.Parallel()

	api := newFakeChromeGroupPolicyAPI(t)
	// The API changes the value that it stores.
	api.transform = func(schemaName string, value map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{"appInstallType": value["appInstallType"].(string) + "_STORED"}
	}
	client := newTestChromePolicyClient(t, api)

	r := testChromeGroupPolicyResource(20 * time.Second)
	config := testChromeGroupPolicyConfig(testChromePolicyAppKeys(testChromePolicyAppA), []interface{}{
		testChromePolicy(testChromePolicyAppInstall, "appInstallType", `"ALLOWED"`),
	})
	state, diags := testChromePolicyPlanApply(t, r, nil, config, client)

	if diags.HasError() || len(diags) != 1 || diags[0].Summary != "Chrome Policy values differ from the configuration" ||
		!strings.Contains(diags[0].Detail, "group "+testChromePolicyGroup) {
		t.Fatalf("diagnostics = %#v, want one warning that names the group", diags)
	}
	if n := state.Attributes["applied_values.%"]; n != "1" {
		t.Errorf("applied_values in state = %v, want the API value", state.Attributes)
	}
	// The write, and the write sent again to confirm the new value.
	write := "groups:batchModify groups/g1 app_id=" + testChromePolicyAppA
	testChromePolicyRequireRequests(t, api, []string{write, write})
}

func TestChromeGroupPolicyCreate_canceledDuringPollIsError(t *testing.T) {
	t.Parallel()

	api := newFakeChromeGroupPolicyAPI(t)
	api.hiddenResolves = testChromePolicyNeverSeen
	client := newTestChromePolicyClient(t, api)

	r := testChromeGroupPolicyResource(30 * time.Second)
	diff, err := r.Diff(context.Background(), nil, terraform.NewResourceConfigRaw(testChromeGroupPolicyConfig(nil, []interface{}{
		testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://b.example.com"`),
	})), client)
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

	if !diags.HasError() || !strings.Contains(diags[0].Summary, "canceled") || !strings.Contains(diags[0].Summary, "group "+testChromePolicyGroup) {
		t.Fatalf("diagnostics = %#v, want an error about the cancellation for the group", diags)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("apply took %s, want it to stop when the context is canceled", elapsed)
	}
	testChromePolicyNoAppliedValues(t, state)
}

func TestChromeGroupPolicyUpdate_resetPolicyNotVisibleIsError(t *testing.T) {
	t.Parallel()

	api := newFakeChromeGroupPolicyAPI(t)
	api.setVisible(testChromePolicyUrls, map[string]interface{}{"maxUrls": float64(5), "urlBlocklist": []interface{}{"a.example.com"}})
	api.hiddenResolves = testChromePolicyNeverSeen
	client := newTestChromePolicyClient(t, api)

	r := testChromeGroupPolicyResource(3 * time.Second)
	state := testChromeGroupPolicyState(t, r, []interface{}{
		map[string]interface{}{
			"schema_name":   testChromePolicyUrls,
			"schema_values": map[string]interface{}{"maxUrls": "5", "urlBlocklist": `["a.example.com"]`},
		},
	})

	// maxUrls is removed, so the policy is deleted from the group before the write, and the write never becomes
	// visible.
	newState, diags := testChromePolicyPlanApply(t, r, state, testChromeGroupPolicyConfig(nil, []interface{}{
		testChromePolicy(testChromePolicyUrls, "urlBlocklist", `["b.example.com"]`),
	}), client)

	if !diags.HasError() || !strings.Contains(diags[0].Summary, "were reset before the write") ||
		!strings.Contains(diags[0].Summary, "group "+testChromePolicyGroup) || !strings.Contains(diags[0].Detail, "group") {
		t.Fatalf("diagnostics = %#v, want an error about the reset policy of the group", diags)
	}
	// The update keeps the prior state.
	if newState == nil || newState.Attributes["policies.0.schema_values.maxUrls"] != "5" {
		t.Errorf("state after the failed update = %#v, want the prior state", newState)
	}
	testChromePolicyRequireRequests(t, api, []string{"groups:batchDelete groups/g1", "groups:batchModify groups/g1", "groups:batchModify groups/g1"})
}

func TestChromeGroupPolicyUpdate_restoreAfterDeadline(t *testing.T) {
	t.Parallel()

	api := newFakeChromeGroupPolicyAPI(t)
	api.setVisible(testChromePolicyUrls, map[string]interface{}{"maxUrls": float64(5), "urlBlocklist": []interface{}{"a.example.com"}})
	// The write takes longer than the update timeout, so its request is canceled at the deadline.
	api.firstModifyDelay = 3 * time.Second
	client := newTestChromePolicyClient(t, api)

	r := testChromeGroupPolicyResource(time.Second)
	state := testChromeGroupPolicyState(t, r, []interface{}{
		map[string]interface{}{
			"schema_name":   testChromePolicyUrls,
			"schema_values": map[string]interface{}{"maxUrls": "5", "urlBlocklist": `["a.example.com"]`},
		},
	})

	start := time.Now()
	_, diags := testChromePolicyPlanApply(t, r, state, testChromeGroupPolicyConfig(nil, []interface{}{
		testChromePolicy(testChromePolicyUrls, "urlBlocklist", `["b.example.com"]`),
	}), client)

	// The restore runs after the deadline, on its own context.
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "Terraform wrote the values from before the change again") ||
		!strings.Contains(diags[0].Summary, "group "+testChromePolicyGroup) {
		t.Fatalf("diagnostics = %#v, want an error that says the values of the group were restored", diags)
	}
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Errorf("apply took %s, want the write request canceled at the deadline", elapsed)
	}
	if n := atomic.LoadInt32(&api.modifyCanceled); n != 1 {
		t.Errorf("canceled writes = %d, want 1", n)
	}

	// The canceled write never reaches the fake API, so the one write is the restore of the value from before.
	testChromePolicyRequireRequests(t, api, []string{"groups:batchDelete groups/g1", "groups:batchModify groups/g1"})
	api.mu.Lock()
	defer api.mu.Unlock()
	restored, _ := json.Marshal(api.pending[testChromePolicyUrls])
	if string(restored) != `{"maxUrls":"5","urlBlocklist":["a.example.com"]}` {
		t.Errorf("restored value = %s, want the value from before", restored)
	}
}

func TestChromeGroupPolicyRead_valueFromOrgUnitIsNotSetOnGroup(t *testing.T) {
	t.Parallel()

	api := newFakeChromeGroupPolicyAPI(t)
	api.setVisible(testChromePolicyConns, map[string]interface{}{"maxConnectionsPerProxy": float64(33)})
	api.setVisible(testChromePolicyHomepage, map[string]interface{}{"homepageLocation": "https://example.com"})
	// Resolve returns a value for the group whose source is an org unit.
	api.sources = map[string]string{testChromePolicyConns: testChromePolicyParent}
	client := newTestChromePolicyClient(t, api)

	r := testChromeGroupPolicyResource(time.Minute)
	state := testChromeGroupPolicyState(t, r, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
		testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://example.com"`),
	})

	refreshed, diags := r.RefreshWithoutUpgrade(context.Background(), state, client)
	if diags.HasError() || len(diags) != 1 || diags[0].Summary != "Chrome Policy "+testChromePolicyConns+" is not set" {
		t.Fatalf("diagnostics = %#v, want one warning that %s is not set", diags, testChromePolicyConns)
	}
	if got := refreshed.Attributes["policies.#"]; got != "1" {
		t.Errorf("policies.# = %q, want 1: only the policy that is set on the group", got)
	}
	if got := refreshed.Attributes["policies.0.schema_name"]; got != testChromePolicyHomepage {
		t.Errorf("policies.0.schema_name = %q, want %s", got, testChromePolicyHomepage)
	}
}

// testChromeGroupPolicyMissingGroup makes resolve and batchDelete fail with code and message, as the API does for a
// group that does not exist.
func testChromeGroupPolicyMissingGroup(api *fakeChromePolicyAPI, code int, message string) {
	api.fail = func(method string) (int, string) {
		if method == "policies:resolve" || method == "policies/groups:batchDelete" {
			return code, message
		}
		return 0, ""
	}
}

func TestChromeGroupPolicy_missingGroupIsGone(t *testing.T) {
	t.Parallel()

	api := newFakeChromeGroupPolicyAPI(t)
	testChromeGroupPolicyMissingGroup(api, 404, "Group "+testChromePolicyGroup+" not found")
	client := newTestChromePolicyClient(t, api)

	r := testChromeGroupPolicyResource(time.Minute)
	policies := []interface{}{testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33")}

	// The read removes the resource with a warning.
	refreshed, diags := r.RefreshWithoutUpgrade(context.Background(), testChromeGroupPolicyState(t, r, policies), client)
	if diags.HasError() || len(diags) != 1 || diags[0].Severity != diag.Warning || !strings.Contains(diags[0].Summary, "does not exist") {
		t.Fatalf("read diagnostics = %#v, want one warning that the group does not exist", diags)
	}
	if refreshed != nil && refreshed.ID != "" {
		t.Errorf("state after the read = %#v, want the resource removed", refreshed)
	}

	// The delete succeeds.
	requireNoDiags(t, r.DeleteContext(context.Background(), r.Data(testChromeGroupPolicyState(t, r, policies)), client))
}

func TestChromeGroupPolicy_otherNotFoundIsError(t *testing.T) {
	t.Parallel()

	api := newFakeChromeGroupPolicyAPI(t)
	// A 404 that does not name a group, for example for the customer.
	testChromeGroupPolicyMissingGroup(api, 404, "Requested entity was not found.")
	client := newTestChromePolicyClient(t, api)

	r := testChromeGroupPolicyResource(time.Minute)
	policies := []interface{}{testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33")}

	d := r.Data(testChromeGroupPolicyState(t, r, policies))
	if diags := r.ReadContext(context.Background(), d, client); !diags.HasError() || d.Id() == "" {
		t.Errorf("read diagnostics = %#v, ID = %q, want an error and the resource kept", diags, d.Id())
	}
	if diags := r.DeleteContext(context.Background(), r.Data(testChromeGroupPolicyState(t, r, policies)), client); !diags.HasError() {
		t.Errorf("delete diagnostics = %#v, want an error", diags)
	}
}

func TestChromeGroupPolicyDelete_policyNotSetIsDone(t *testing.T) {
	t.Parallel()

	api := newFakeChromeGroupPolicyAPI(t)
	api.deleteUnsetFails = true
	// Only one of the two policies in state is still set on the group.
	api.setVisible(testChromePolicyHomepage, map[string]interface{}{"homepageLocation": "https://example.com"})
	client := newTestChromePolicyClient(t, api)

	r := testChromeGroupPolicyResource(time.Minute)
	state := testChromeGroupPolicyState(t, r, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
		testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://example.com"`),
	})

	requireNoDiags(t, r.DeleteContext(context.Background(), r.Data(state), client))

	// The batch fails on the policy that is not set, and each policy is then deleted alone.
	testChromePolicyRequireRequests(t, api, []string{
		"groups:batchDelete groups/g1 failed",
		"groups:batchDelete groups/g1 failed",
		"groups:batchDelete groups/g1",
	})
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.visible) != 0 {
		t.Errorf("values after delete = %#v, want none", api.visible)
	}
}

// testChromeGroupPolicyRequireError requires that a read and a delete of a group policy for testChromePolicyConns fail,
// and that the read keeps the resource, when the fake API is set up to fail.
func testChromeGroupPolicyRequireError(t *testing.T, api *fakeChromePolicyAPI) {
	t.Helper()

	client := newTestChromePolicyClient(t, api)
	r := testChromeGroupPolicyResource(time.Minute)
	policies := []interface{}{testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33")}

	d := r.Data(testChromeGroupPolicyState(t, r, policies))
	if diags := r.ReadContext(context.Background(), d, client); !diags.HasError() || d.Id() == "" {
		t.Errorf("read diagnostics = %#v, ID = %q, want an error and the resource kept", diags, d.Id())
	}
	if diags := r.DeleteContext(context.Background(), r.Data(testChromeGroupPolicyState(t, r, policies)), client); !diags.HasError() {
		t.Errorf("delete diagnostics = %#v, want an error", diags)
	}
}

func TestChromeGroupPolicy_unrelatedNotFoundMessagesAreErrors(t *testing.T) {
	t.Parallel()

	// Each message says that something is not set or not found, but not that the group or a policy of the request is
	// missing.
	for _, message := range []string{
		"Invalid policy target: additionalTargetKeys app_id is not set",
		"Policy schema chrome.users.apps.Foo not found",
		"Group policy chrome.users.apps.InstallType not found",
	} {
		for _, code := range []int{400, 404} {
			message, code := message, code
			t.Run(strconv.Itoa(code)+" "+message, func(t *testing.T) {
				t.Parallel()

				api := newFakeChromeGroupPolicyAPI(t)
				testChromeGroupPolicyMissingGroup(api, code, message)
				testChromeGroupPolicyRequireError(t, api)
			})
		}
	}
}

func TestChromeGroupPolicy_forbiddenWithGroupMessageIsError(t *testing.T) {
	t.Parallel()

	// Only codes 400 and 404 can mean that the group does not exist.
	api := newFakeChromeGroupPolicyAPI(t)
	testChromeGroupPolicyMissingGroup(api, 403, "Group "+testChromePolicyGroup+" not found")
	testChromeGroupPolicyRequireError(t, api)
}

func TestChromeGroupPolicyDelete_failedSingleDeleteIsError(t *testing.T) {
	t.Parallel()

	api := newFakeChromeGroupPolicyAPI(t)
	var deletes int32
	api.fail = func(method string) (int, string) {
		if method != "policies/groups:batchDelete" {
			return 0, ""
		}
		// The batch fails because the first policy is not set, the delete of that policy alone fails in the same
		// way, and the delete of the second policy fails on the server.
		switch atomic.AddInt32(&deletes, 1) {
		case 1, 2:
			return 400, "Policy " + testChromePolicyConns + " not found"
		default:
			return 500, "Internal error"
		}
	}
	client := newTestChromePolicyClient(t, api)

	r := testChromeGroupPolicyResource(time.Minute)
	state := testChromeGroupPolicyState(t, r, []interface{}{
		testChromePolicy(testChromePolicyConns, "maxConnectionsPerProxy", "33"),
		testChromePolicy(testChromePolicyHomepage, "homepageLocation", `"https://example.com"`),
	})

	diags := r.DeleteContext(context.Background(), r.Data(state), client)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "Internal error") {
		t.Fatalf("delete diagnostics = %#v, want the error of the failed delete", diags)
	}
	if n := atomic.LoadInt32(&deletes); n != 3 {
		t.Errorf("batchDelete calls = %d, want 3", n)
	}
}
