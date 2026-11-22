// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	ctyjson "github.com/hashicorp/go-cty/cty/json"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

const testGmailUserEmail = "user@example.com"

// unknownConfigValue is the legacy SDK representation of a value that is not
// known until apply.
const unknownConfigValue = "74D93920-ED26-11E3-AC10-0800200C9A66"

type fakeGmailRequest struct {
	method  string
	setting string
	body    map[string]interface{}
}

// fakeGmail is a fake Gmail API that keeps the IMAP, POP and automatic
// forwarding settings of one mailbox and records each request.
type fakeGmail struct {
	t *testing.T

	mu             sync.Mutex
	settings       map[string]map[string]interface{}
	requests       []fakeGmailRequest
	serviceCreated int

	// authenticatedUser is the user of the credentials. Like Gmail, the fake
	// resolves "me" to this user and rejects a request for another user.
	authenticatedUser string
	// dropWhenDisabled makes the fake omit the values of a disabled feature
	// from its responses.
	dropWhenDisabled bool
	// failPut makes the update of this setting fail.
	failPut string
	// notFound makes each request fail with 404.
	notFound bool
}

func newFakeGmail(t *testing.T) *fakeGmail {
	t.Helper()

	f := &fakeGmail{
		t:                 t,
		authenticatedUser: testGmailUserEmail,
		settings: map[string]map[string]interface{}{
			"imap":           {"enabled": true, "autoExpunge": true, "expungeBehavior": "archive"},
			"pop":            {"accessWindow": "allMail", "disposition": "leaveInInbox"},
			"autoForwarding": {"disposition": "leaveInInbox"},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(server.Close)

	previous := newGmailUserSettingsService
	t.Cleanup(func() { newGmailUserSettingsService = previous })
	newGmailUserSettingsService = func(ctx context.Context, _ *apiClient, userEmail string) (*gmail.Service, diag.Diagnostics) {
		f.mu.Lock()
		f.serviceCreated++
		f.mu.Unlock()
		if !strings.EqualFold(userEmail, testGmailUserEmail) {
			t.Errorf("Gmail service impersonates %q, want %q", userEmail, testGmailUserEmail)
		}
		svc, err := gmail.NewService(ctx, option.WithEndpoint(server.URL+"/"), option.WithHTTPClient(server.Client()))
		if err != nil {
			return nil, diag.FromErr(err)
		}
		return svc, nil
	}

	return f
}

func (f *fakeGmail) serveHTTP(w http.ResponseWriter, r *http.Request) {
	// The path is /gmail/v1/users/{userId}/settings/{setting}.
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/"), "/")
	if len(parts) != 3 || parts[1] != "settings" {
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		return
	}
	user, setting := parts[0], parts[2]

	f.mu.Lock()
	defer f.mu.Unlock()

	if user == "me" {
		f.t.Errorf("%s %s uses the user ID \"me\", want the email address of the user", r.Method, r.URL.Path)
		user = f.authenticatedUser
	}
	if !strings.EqualFold(user, f.authenticatedUser) {
		writeFakeGmailError(w, http.StatusForbidden, "Delegation denied for "+f.authenticatedUser)
		return
	}
	if f.notFound {
		writeFakeGmailError(w, http.StatusNotFound, "Not Found")
		return
	}
	if r.Method == http.MethodPut && setting == f.failPut {
		writeFakeGmailError(w, http.StatusBadRequest, "Invalid request")
		return
	}

	current, ok := f.settings[setting]
	if !ok {
		http.Error(w, "unexpected setting "+setting, http.StatusNotFound)
		return
	}

	request := fakeGmailRequest{method: r.Method, setting: setting}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPut:
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(raw, &request.body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// The update methods replace the whole setting.
		current = request.body
		f.settings[setting] = current
	default:
		http.Error(w, "unexpected method "+r.Method, http.StatusMethodNotAllowed)
		return
	}
	f.requests = append(f.requests, request)

	response := map[string]interface{}{}
	for key, value := range current {
		response[key] = value
	}
	if f.dropWhenDisabled {
		switch {
		case setting == "imap" && response["enabled"] != true:
			delete(response, "autoExpunge")
			delete(response, "expungeBehavior")
			delete(response, "maxFolderSize")
		case setting == "pop" && response["accessWindow"] == "disabled":
			delete(response, "disposition")
		case setting == "autoForwarding" && response["enabled"] != true:
			delete(response, "disposition")
			delete(response, "emailAddress")
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		f.t.Errorf("encoding response: %s", err)
	}
}

func writeFakeGmailError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{"code": code, "message": message},
	})
}

// calls returns the recorded requests as "METHOD setting" and clears them.
func (f *fakeGmail) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var calls []string
	for _, request := range f.requests {
		calls = append(calls, request.method+" "+request.setting)
	}
	f.requests = nil
	return calls
}

// lastPut returns the body of the last update of the setting.
func (f *fakeGmail) lastPut(t *testing.T, setting string) map[string]interface{} {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	for i := len(f.requests) - 1; i >= 0; i-- {
		if f.requests[i].method == http.MethodPut && f.requests[i].setting == setting {
			return f.requests[i].body
		}
	}
	t.Fatalf("no update of %s; requests: %v", setting, f.requests)
	return nil
}

func (f *fakeGmail) setSetting(setting string, value map[string]interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings[setting] = value
}

// gmailUserSettingsPlan validates the configuration and plans the change from
// the state, like Terraform does before an apply.
func gmailUserSettingsPlan(t *testing.T, r *schema.Resource, state *terraform.InstanceState, config map[string]interface{}) *terraform.InstanceDiff {
	t.Helper()

	c := terraform.NewResourceConfigRaw(config)
	if diags := r.Validate(c); diags.HasError() {
		t.Fatalf("unexpected validation error: %v", diags)
	}

	diff, err := r.Diff(context.Background(), state, c, &apiClient{})
	if err != nil {
		t.Fatalf("unexpected plan error: %s", err)
	}
	if diff == nil {
		return nil
	}

	// Terraform sends the configuration with the planned change.
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("encoding config: %s", err)
	}
	diff.RawConfig, err = ctyjson.Unmarshal(raw, r.CoreConfigSchema().ImpliedType())
	if err != nil {
		t.Fatalf("converting config: %s", err)
	}

	return diff
}

func gmailUserSettingsApply(t *testing.T, r *schema.Resource, state *terraform.InstanceState, config map[string]interface{}) *terraform.InstanceState {
	t.Helper()

	diff := gmailUserSettingsPlan(t, r, state, config)
	if diff == nil || diff.Empty() {
		t.Fatalf("expected a planned change for config %v", config)
	}

	newState, diags := r.Apply(context.Background(), state, diff, &apiClient{})
	if diags.HasError() {
		t.Fatalf("unexpected apply error: %v", diags)
	}
	return newState
}

func gmailUserSettingsRefresh(t *testing.T, r *schema.Resource, state *terraform.InstanceState) *terraform.InstanceState {
	t.Helper()

	newState, diags := r.RefreshWithoutUpgrade(context.Background(), state, &apiClient{})
	if diags.HasError() {
		t.Fatalf("unexpected refresh error: %v", diags)
	}
	return newState
}

func assertCalls(t *testing.T, f *fakeGmail, want ...string) {
	t.Helper()

	got := f.calls()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("API calls = %v, want %v", got, want)
	}
}

func assertAttributes(t *testing.T, state *terraform.InstanceState, want map[string]string) {
	t.Helper()

	for key, value := range want {
		got := state.Attributes[key]
		// The state omits the count of an empty block list.
		if got == "" && value == "0" && strings.HasSuffix(key, ".#") {
			continue
		}
		if got != value {
			t.Errorf("state %s = %q, want %q (state: %v)", key, got, value, state.Attributes)
		}
	}
}

// assertBody checks the update body. A false or zero value must be in the
// body, because the update replaces the whole setting.
func assertBody(t *testing.T, body map[string]interface{}, want map[string]interface{}) {
	t.Helper()

	for key, value := range want {
		got, ok := body[key]
		if !ok {
			t.Errorf("update body has no %q: %v", key, body)
			continue
		}
		if !reflect.DeepEqual(got, value) {
			t.Errorf("update body %s = %#v, want %#v", key, got, value)
		}
	}
}

func TestGmailUserSettingsCreate_imap(t *testing.T) {
	f := newFakeGmail(t)
	r := resourceGmailUserSettings()

	state := gmailUserSettingsApply(t, r, nil, map[string]interface{}{
		"user_email": testGmailUserEmail,
		"imap": []interface{}{map[string]interface{}{
			"enabled":         false,
			"max_folder_size": 1000,
		}},
	})

	body := f.lastPut(t, "imap")
	assertCalls(t, f, "GET imap", "PUT imap", "GET imap")
	// auto_expunge and expunge_behavior are not set: the current values are kept.
	assertBody(t, body, map[string]interface{}{
		"enabled":         false,
		"autoExpunge":     true,
		"expungeBehavior": "archive",
		"maxFolderSize":   float64(1000),
	})
	if state.ID != testGmailUserEmail {
		t.Errorf("ID = %q, want %q", state.ID, testGmailUserEmail)
	}
	assertAttributes(t, state, map[string]string{
		"imap.#":                  "1",
		"imap.0.enabled":          "false",
		"imap.0.auto_expunge":     "true",
		"imap.0.expunge_behavior": "archive",
		"imap.0.max_folder_size":  "1000",
		"pop.#":                   "0",
		"auto_forwarding.#":       "0",
	})
}

func TestGmailUserSettingsCreate_pop(t *testing.T) {
	f := newFakeGmail(t)
	r := resourceGmailUserSettings()

	state := gmailUserSettingsApply(t, r, nil, map[string]interface{}{
		"user_email": testGmailUserEmail,
		"pop": []interface{}{map[string]interface{}{
			"access_window": "disabled",
		}},
	})

	body := f.lastPut(t, "pop")
	assertCalls(t, f, "GET pop", "PUT pop", "GET pop")
	assertBody(t, body, map[string]interface{}{
		"accessWindow": "disabled",
		"disposition":  "leaveInInbox",
	})
	assertAttributes(t, state, map[string]string{
		"pop.#":               "1",
		"pop.0.access_window": "disabled",
		"pop.0.disposition":   "leaveInInbox",
		"imap.#":              "0",
		"auto_forwarding.#":   "0",
	})
}

func TestGmailUserSettingsCreate_autoForwarding(t *testing.T) {
	f := newFakeGmail(t)
	r := resourceGmailUserSettings()

	state := gmailUserSettingsApply(t, r, nil, map[string]interface{}{
		"user_email": testGmailUserEmail,
		"auto_forwarding": []interface{}{map[string]interface{}{
			"enabled":       true,
			"email_address": "forward@example.com",
			"disposition":   "trash",
		}},
	})

	body := f.lastPut(t, "autoForwarding")
	assertCalls(t, f, "GET autoForwarding", "PUT autoForwarding", "GET autoForwarding")
	assertBody(t, body, map[string]interface{}{
		"enabled":      true,
		"emailAddress": "forward@example.com",
		"disposition":  "trash",
	})
	assertAttributes(t, state, map[string]string{
		"auto_forwarding.#":               "1",
		"auto_forwarding.0.enabled":       "true",
		"auto_forwarding.0.email_address": "forward@example.com",
		"auto_forwarding.0.disposition":   "trash",
		"imap.#":                          "0",
		"pop.#":                           "0",
	})
}

func TestGmailUserSettingsUpdate_eachField(t *testing.T) {
	f := newFakeGmail(t)
	r := resourceGmailUserSettings()

	config := func(imap, pop, autoForwarding map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{
			"user_email":      testGmailUserEmail,
			"imap":            []interface{}{imap},
			"pop":             []interface{}{pop},
			"auto_forwarding": []interface{}{autoForwarding},
		}
	}
	imap := map[string]interface{}{"enabled": true, "auto_expunge": true, "expunge_behavior": "archive", "max_folder_size": 0}
	pop := map[string]interface{}{"access_window": "allMail", "disposition": "leaveInInbox"}
	autoForwarding := map[string]interface{}{"enabled": false, "disposition": "leaveInInbox"}

	state := gmailUserSettingsApply(t, r, nil, config(imap, pop, autoForwarding))
	assertCalls(t, f,
		"GET imap", "PUT imap", "GET pop", "PUT pop", "GET autoForwarding", "PUT autoForwarding",
		"GET imap", "GET pop", "GET autoForwarding")

	steps := []struct {
		name    string
		block   map[string]interface{}
		key     string
		value   interface{}
		setting string
		body    map[string]interface{}
		state   map[string]string
	}{
		{"imap enabled", imap, "enabled", false, "imap",
			map[string]interface{}{"enabled": false}, map[string]string{"imap.0.enabled": "false"}},
		{"imap auto_expunge", imap, "auto_expunge", false, "imap",
			map[string]interface{}{"autoExpunge": false}, map[string]string{"imap.0.auto_expunge": "false"}},
		{"imap expunge_behavior", imap, "expunge_behavior", "deleteForever", "imap",
			map[string]interface{}{"expungeBehavior": "deleteForever"}, map[string]string{"imap.0.expunge_behavior": "deleteForever"}},
		{"imap max_folder_size", imap, "max_folder_size", 10000, "imap",
			map[string]interface{}{"maxFolderSize": float64(10000)}, map[string]string{"imap.0.max_folder_size": "10000"}},
		{"imap max_folder_size no limit", imap, "max_folder_size", 0, "imap",
			map[string]interface{}{"maxFolderSize": float64(0)}, map[string]string{"imap.0.max_folder_size": "0"}},
		{"pop access_window", pop, "access_window", "fromNowOn", "pop",
			map[string]interface{}{"accessWindow": "fromNowOn"}, map[string]string{"pop.0.access_window": "fromNowOn"}},
		{"pop disposition", pop, "disposition", "markRead", "pop",
			map[string]interface{}{"disposition": "markRead"}, map[string]string{"pop.0.disposition": "markRead"}},
		{"auto_forwarding email_address", autoForwarding, "email_address", "forward@example.com", "autoForwarding",
			map[string]interface{}{"emailAddress": "forward@example.com", "enabled": false},
			map[string]string{"auto_forwarding.0.email_address": "forward@example.com"}},
		{"auto_forwarding enabled", autoForwarding, "enabled", true, "autoForwarding",
			map[string]interface{}{"enabled": true, "emailAddress": "forward@example.com"},
			map[string]string{"auto_forwarding.0.enabled": "true"}},
		{"auto_forwarding disposition", autoForwarding, "disposition", "archive", "autoForwarding",
			map[string]interface{}{"disposition": "archive"}, map[string]string{"auto_forwarding.0.disposition": "archive"}},
		{"auto_forwarding disabled", autoForwarding, "enabled", false, "autoForwarding",
			map[string]interface{}{"enabled": false}, map[string]string{"auto_forwarding.0.enabled": "false"}},
	}

	for _, step := range steps {
		step.block[step.key] = step.value
		state = gmailUserSettingsApply(t, r, state, config(imap, pop, autoForwarding))

		body := f.lastPut(t, step.setting)
		// Only the changed setting is written; the refresh reads all managed settings.
		got := f.calls()
		want := []string{"GET " + step.setting, "PUT " + step.setting, "GET imap", "GET pop", "GET autoForwarding"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: API calls = %v, want %v", step.name, got, want)
		}
		assertBody(t, body, step.body)
		assertAttributes(t, state, step.state)

		if diff := gmailUserSettingsPlan(t, r, state, config(imap, pop, autoForwarding)); diff != nil && !diff.Empty() {
			t.Fatalf("%s: unexpected change after apply: %#v", step.name, diff.Attributes)
		}
	}
}

func TestGmailUserSettingsUpdate_optionalValueRemovedFromConfigKeepsCurrentValue(t *testing.T) {
	f := newFakeGmail(t)
	r := resourceGmailUserSettings()

	state := gmailUserSettingsApply(t, r, nil, map[string]interface{}{
		"user_email": testGmailUserEmail,
		"imap": []interface{}{map[string]interface{}{
			"enabled":          true,
			"expunge_behavior": "trash",
		}},
	})
	f.calls()

	// Without expunge_behavior in the configuration, the plan keeps the
	// current value and the enabled change does not reset it.
	state = gmailUserSettingsApply(t, r, state, map[string]interface{}{
		"user_email": testGmailUserEmail,
		"imap":       []interface{}{map[string]interface{}{"enabled": false}},
	})
	assertBody(t, f.lastPut(t, "imap"), map[string]interface{}{"enabled": false, "expungeBehavior": "trash"})
	assertAttributes(t, state, map[string]string{"imap.0.expunge_behavior": "trash"})
}

func TestGmailUserSettingsUpdate_blockRemovedFromConfigIsNotManaged(t *testing.T) {
	f := newFakeGmail(t)
	r := resourceGmailUserSettings()

	state := gmailUserSettingsApply(t, r, nil, map[string]interface{}{
		"user_email": testGmailUserEmail,
		"imap":       []interface{}{map[string]interface{}{"enabled": false}},
		"pop":        []interface{}{map[string]interface{}{"access_window": "disabled"}},
	})
	f.calls()

	popOnly := map[string]interface{}{
		"user_email": testGmailUserEmail,
		"pop":        []interface{}{map[string]interface{}{"access_window": "disabled"}},
	}
	state = gmailUserSettingsApply(t, r, state, popOnly)
	// Removing the imap block does not write the IMAP settings.
	assertCalls(t, f, "GET pop")
	assertAttributes(t, state, map[string]string{"imap.#": "0", "pop.#": "1"})

	// A change to the IMAP settings outside Terraform causes no drift.
	f.setSetting("imap", map[string]interface{}{"enabled": true, "expungeBehavior": "trash"})
	state = gmailUserSettingsRefresh(t, r, state)
	assertCalls(t, f, "GET pop")
	assertAttributes(t, state, map[string]string{"imap.#": "0", "pop.#": "1"})
	if diff := gmailUserSettingsPlan(t, r, state, popOnly); diff != nil && !diff.Empty() {
		t.Fatalf("unexpected change: %#v", diff.Attributes)
	}
}

func TestGmailUserSettingsRead_detectsDrift(t *testing.T) {
	f := newFakeGmail(t)
	r := resourceGmailUserSettings()

	config := map[string]interface{}{
		"user_email": testGmailUserEmail,
		"pop":        []interface{}{map[string]interface{}{"access_window": "disabled"}},
	}
	state := gmailUserSettingsApply(t, r, nil, config)
	f.calls()

	f.setSetting("pop", map[string]interface{}{"accessWindow": "allMail", "disposition": "leaveInInbox"})
	state = gmailUserSettingsRefresh(t, r, state)
	assertCalls(t, f, "GET pop")
	assertAttributes(t, state, map[string]string{"pop.0.access_window": "allMail"})

	diff := gmailUserSettingsPlan(t, r, state, config)
	if diff == nil || diff.Attributes["pop.0.access_window"] == nil || diff.Attributes["pop.0.access_window"].New != "disabled" {
		t.Fatalf("expected a change of pop.0.access_window to disabled, got %#v", diff)
	}
}

func TestGmailUserSettingsImport_readsAllSettings(t *testing.T) {
	f := newFakeGmail(t)
	r := resourceGmailUserSettings()
	f.setSetting("autoForwarding", map[string]interface{}{"enabled": true, "emailAddress": "forward@example.com", "disposition": "archive"})

	d := r.Data(&terraform.InstanceState{ID: testGmailUserEmail})
	imported, err := r.Importer.StateContext(context.Background(), d, &apiClient{})
	if err != nil {
		t.Fatalf("unexpected import error: %s", err)
	}
	if len(imported) != 1 {
		t.Fatalf("imported %d resources, want 1", len(imported))
	}
	assertCalls(t, f, "GET imap", "GET pop", "GET autoForwarding")

	// Terraform refreshes the imported state.
	state := gmailUserSettingsRefresh(t, r, imported[0].State())
	assertCalls(t, f, "GET imap", "GET pop", "GET autoForwarding")
	assertAttributes(t, state, map[string]string{
		"user_email":                      testGmailUserEmail,
		"imap.#":                          "1",
		"imap.0.enabled":                  "true",
		"imap.0.auto_expunge":             "true",
		"imap.0.expunge_behavior":         "archive",
		"imap.0.max_folder_size":          "0",
		"pop.#":                           "1",
		"pop.0.access_window":             "allMail",
		"pop.0.disposition":               "leaveInInbox",
		"auto_forwarding.#":               "1",
		"auto_forwarding.0.enabled":       "true",
		"auto_forwarding.0.email_address": "forward@example.com",
		"auto_forwarding.0.disposition":   "archive",
	})
	if state.ID != testGmailUserEmail {
		t.Errorf("ID = %q, want %q", state.ID, testGmailUserEmail)
	}

	// A configuration with all three blocks and the current values has no change.
	diff := gmailUserSettingsPlan(t, r, state, map[string]interface{}{
		"user_email": testGmailUserEmail,
		"imap":       []interface{}{map[string]interface{}{"enabled": true}},
		"pop":        []interface{}{map[string]interface{}{"access_window": "allMail"}},
		"auto_forwarding": []interface{}{map[string]interface{}{
			"enabled":       true,
			"email_address": "forward@example.com",
		}},
	})
	if diff != nil && !diff.Empty() {
		t.Fatalf("unexpected change after import: %#v", diff.Attributes)
	}
}

func TestGmailUserSettingsImport_invalidID(t *testing.T) {
	f := newFakeGmail(t)
	r := resourceGmailUserSettings()

	d := r.Data(&terraform.InstanceState{ID: "not-an-email"})
	if _, err := r.Importer.StateContext(context.Background(), d, &apiClient{}); err == nil {
		t.Fatal("expected an import error")
	}
	assertCalls(t, f)
}

func TestGmailUserSettingsDelete_makesNoAPICall(t *testing.T) {
	f := newFakeGmail(t)
	r := resourceGmailUserSettings()

	state := gmailUserSettingsApply(t, r, nil, map[string]interface{}{
		"user_email":      testGmailUserEmail,
		"imap":            []interface{}{map[string]interface{}{"enabled": false}},
		"pop":             []interface{}{map[string]interface{}{"access_window": "disabled"}},
		"auto_forwarding": []interface{}{map[string]interface{}{"enabled": false}},
	})
	f.calls()
	f.mu.Lock()
	f.serviceCreated = 0
	f.mu.Unlock()

	newState, diags := r.Apply(context.Background(), state, &terraform.InstanceDiff{Destroy: true}, &apiClient{})
	if diags.HasError() {
		t.Fatalf("unexpected destroy error: %v", diags)
	}
	if newState != nil {
		t.Fatalf("expected no state after destroy, got %v", newState)
	}
	assertCalls(t, f)
	if f.serviceCreated != 0 {
		t.Fatalf("destroy created %d Gmail services, want 0", f.serviceCreated)
	}
}

func TestGmailUserSettings_userEmailCaseInsensitive(t *testing.T) {
	newFakeGmail(t)
	r := resourceGmailUserSettings()

	block := []interface{}{map[string]interface{}{"access_window": "disabled"}}
	state := gmailUserSettingsApply(t, r, nil, map[string]interface{}{"user_email": testGmailUserEmail, "pop": block})

	diff := gmailUserSettingsPlan(t, r, state, map[string]interface{}{"user_email": strings.ToUpper(testGmailUserEmail), "pop": block})
	if diff != nil && !diff.Empty() {
		t.Fatalf("unexpected change for a user_email that differs only in case: %#v", diff.Attributes)
	}
}

func TestGmailUserSettingsValidate(t *testing.T) {
	r := resourceGmailUserSettings()

	block := func(name string, values map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{
			"user_email": testGmailUserEmail,
			name:         []interface{}{values},
		}
	}

	tests := map[string]struct {
		config  map[string]interface{}
		wantErr bool
	}{
		"no setting block": {map[string]interface{}{"user_email": testGmailUserEmail}, true},
		"user_email me": {
			map[string]interface{}{"user_email": "me", "pop": []interface{}{map[string]interface{}{"access_window": "disabled"}}}, true},
		"user_email without domain": {
			map[string]interface{}{"user_email": "user", "pop": []interface{}{map[string]interface{}{"access_window": "disabled"}}}, true},
		"user_email with two @": {
			map[string]interface{}{"user_email": "a@b@example.com", "pop": []interface{}{map[string]interface{}{"access_window": "disabled"}}}, true},
		"imap minimal":                     {block("imap", map[string]interface{}{"enabled": true}), false},
		"imap expunge_behavior archive":    {block("imap", map[string]interface{}{"enabled": true, "expunge_behavior": "archive"}), false},
		"imap expunge_behavior trash":      {block("imap", map[string]interface{}{"enabled": true, "expunge_behavior": "trash"}), false},
		"imap expunge_behavior delete":     {block("imap", map[string]interface{}{"enabled": true, "expunge_behavior": "deleteForever"}), false},
		"imap expunge_behavior wrong case": {block("imap", map[string]interface{}{"enabled": true, "expunge_behavior": "Archive"}), true},
		"imap expunge_behavior unspecified": {
			block("imap", map[string]interface{}{"enabled": true, "expunge_behavior": "expungeBehaviorUnspecified"}), true},
		"imap max_folder_size 0":       {block("imap", map[string]interface{}{"enabled": true, "max_folder_size": 0}), false},
		"imap max_folder_size 2000":    {block("imap", map[string]interface{}{"enabled": true, "max_folder_size": 2000}), false},
		"imap max_folder_size 5000":    {block("imap", map[string]interface{}{"enabled": true, "max_folder_size": 5000}), false},
		"imap max_folder_size 3000":    {block("imap", map[string]interface{}{"enabled": true, "max_folder_size": 3000}), true},
		"imap without enabled":         {block("imap", map[string]interface{}{"auto_expunge": true}), true},
		"pop access_window allMail":    {block("pop", map[string]interface{}{"access_window": "allMail"}), false},
		"pop access_window fromNowOn":  {block("pop", map[string]interface{}{"access_window": "fromNowOn"}), false},
		"pop access_window wrong case": {block("pop", map[string]interface{}{"access_window": "allmail"}), true},
		"pop access_window unspecified": {
			block("pop", map[string]interface{}{"access_window": "accessWindowUnspecified"}), true},
		"pop disposition markRead":   {block("pop", map[string]interface{}{"access_window": "allMail", "disposition": "markRead"}), false},
		"pop disposition wrong case": {block("pop", map[string]interface{}{"access_window": "allMail", "disposition": "MarkRead"}), true},
		"pop without access_window":  {block("pop", map[string]interface{}{"disposition": "trash"}), true},
		"auto_forwarding disposition leaveInInbox": {
			block("auto_forwarding", map[string]interface{}{"enabled": false, "disposition": "leaveInInbox"}), false},
		"auto_forwarding disposition unknown": {
			block("auto_forwarding", map[string]interface{}{"enabled": false, "disposition": "delete"}), true},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			diags := r.Validate(terraform.NewResourceConfigRaw(test.config))
			if got := diags.HasError(); got != test.wantErr {
				t.Fatalf("validation error = %t, want %t: %v", got, test.wantErr, diags)
			}
		})
	}
}

func TestGmailUserSettingsPlan_autoForwardingEnabledNeedsEmailAddress(t *testing.T) {
	r := resourceGmailUserSettings()

	tests := map[string]struct {
		autoForwarding map[string]interface{}
		wantErr        bool
	}{
		"enabled without email_address":      {map[string]interface{}{"enabled": true}, true},
		"enabled with empty email_address":   {map[string]interface{}{"enabled": true, "email_address": ""}, true},
		"enabled with email_address":         {map[string]interface{}{"enabled": true, "email_address": "forward@example.com"}, false},
		"enabled with unknown email_address": {map[string]interface{}{"enabled": true, "email_address": unknownConfigValue}, false},
		"disabled without email_address":     {map[string]interface{}{"enabled": false}, false},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := r.Diff(context.Background(), nil, terraform.NewResourceConfigRaw(map[string]interface{}{
				"user_email":      testGmailUserEmail,
				"auto_forwarding": []interface{}{test.autoForwarding},
			}), &apiClient{})
			if got := err != nil; got != test.wantErr {
				t.Fatalf("plan error = %v, want error %t", err, test.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "email_address is required") {
				t.Fatalf("unexpected plan error: %s", err)
			}
		})
	}
}

func TestGmailUserSettings_requestsUseUserEmail(t *testing.T) {
	f := newFakeGmail(t)
	r := resourceGmailUserSettings()
	// The credentials authenticate a different user, for example because
	// they cannot impersonate. Gmail must reject the requests instead of
	// changing the mailbox of the authenticated user.
	f.authenticatedUser = "admin@example.com"

	config := map[string]interface{}{
		"user_email": testGmailUserEmail,
		"pop":        []interface{}{map[string]interface{}{"access_window": "disabled"}},
	}
	diff := gmailUserSettingsPlan(t, r, nil, config)
	state, diags := r.Apply(context.Background(), nil, diff, &apiClient{})
	if !diags.HasError() {
		t.Fatalf("expected an apply error, got state %v", state)
	}
	if !strings.Contains(fmt.Sprint(diags), "Delegation denied") {
		t.Fatalf("unexpected apply error: %v", diags)
	}
	assertCalls(t, f)
	f.mu.Lock()
	got := f.settings["pop"]["accessWindow"]
	f.mu.Unlock()
	if got != "allMail" {
		t.Fatalf("the POP settings of the authenticated user changed: accessWindow = %v", got)
	}
}

func TestGmailUserSettingsRead_disabledFeatureOmitsValues(t *testing.T) {
	f := newFakeGmail(t)
	f.dropWhenDisabled = true
	r := resourceGmailUserSettings()

	config := map[string]interface{}{
		"user_email": testGmailUserEmail,
		"imap": []interface{}{map[string]interface{}{
			"enabled":          false,
			"auto_expunge":     true,
			"expunge_behavior": "trash",
			"max_folder_size":  1000,
		}},
		"pop": []interface{}{map[string]interface{}{
			"access_window": "disabled",
			"disposition":   "archive",
		}},
		"auto_forwarding": []interface{}{map[string]interface{}{
			"enabled":       false,
			"email_address": "forward@example.com",
			"disposition":   "markRead",
		}},
	}
	want := map[string]string{
		"imap.0.enabled":                  "false",
		"imap.0.auto_expunge":             "true",
		"imap.0.expunge_behavior":         "trash",
		"imap.0.max_folder_size":          "1000",
		"pop.0.access_window":             "disabled",
		"pop.0.disposition":               "archive",
		"auto_forwarding.0.enabled":       "false",
		"auto_forwarding.0.email_address": "forward@example.com",
		"auto_forwarding.0.disposition":   "markRead",
	}

	state := gmailUserSettingsApply(t, r, nil, config)
	assertAttributes(t, state, want)
	if diff := gmailUserSettingsPlan(t, r, state, config); diff != nil && !diff.Empty() {
		t.Fatalf("unexpected change after apply: %#v", diff.Attributes)
	}

	state = gmailUserSettingsRefresh(t, r, state)
	assertAttributes(t, state, want)
	if diff := gmailUserSettingsPlan(t, r, state, config); diff != nil && !diff.Empty() {
		t.Fatalf("unexpected change after refresh: %#v", diff.Attributes)
	}
}

func TestGmailUserSettingsUpdate_failedWriteKeepsPriorState(t *testing.T) {
	f := newFakeGmail(t)
	r := resourceGmailUserSettings()

	state := gmailUserSettingsApply(t, r, nil, map[string]interface{}{
		"user_email": testGmailUserEmail,
		"pop":        []interface{}{map[string]interface{}{"access_window": "allMail", "disposition": "leaveInInbox"}},
	})
	f.calls()
	f.failPut = "pop"

	config := map[string]interface{}{
		"user_email": testGmailUserEmail,
		"pop":        []interface{}{map[string]interface{}{"access_window": "disabled", "disposition": "trash"}},
	}
	diff := gmailUserSettingsPlan(t, r, state, config)
	newState, diags := r.Apply(context.Background(), state, diff, &apiClient{})
	if !diags.HasError() {
		t.Fatal("expected an apply error")
	}
	assertAttributes(t, newState, map[string]string{
		"pop.0.access_window": "allMail",
		"pop.0.disposition":   "leaveInInbox",
	})

	// The next plan writes the settings again.
	if diff := gmailUserSettingsPlan(t, r, newState, config); diff == nil || diff.Empty() {
		t.Fatal("expected a planned change after the failed apply")
	}
}

func TestGmailUserSettingsUpdate_disabledForwardingWithoutAddressOmitsAddress(t *testing.T) {
	f := newFakeGmail(t)
	r := resourceGmailUserSettings()

	state := gmailUserSettingsApply(t, r, nil, map[string]interface{}{
		"user_email": testGmailUserEmail,
		"auto_forwarding": []interface{}{map[string]interface{}{
			"enabled":       true,
			"email_address": "forward@example.com",
		}},
	})
	f.calls()

	gmailUserSettingsApply(t, r, state, map[string]interface{}{
		"user_email":      testGmailUserEmail,
		"auto_forwarding": []interface{}{map[string]interface{}{"enabled": false}},
	})
	body := f.lastPut(t, "autoForwarding")
	assertBody(t, body, map[string]interface{}{"enabled": false, "disposition": "leaveInInbox"})
	if address, ok := body["emailAddress"]; ok {
		t.Fatalf("update body has emailAddress %v, want no address", address)
	}
}

func TestGmailUserSettings_emailAddressCaseInsensitive(t *testing.T) {
	newFakeGmail(t)
	r := resourceGmailUserSettings()

	block := func(address string) map[string]interface{} {
		return map[string]interface{}{
			"user_email": testGmailUserEmail,
			"auto_forwarding": []interface{}{map[string]interface{}{
				"enabled":       true,
				"email_address": address,
			}},
		}
	}
	state := gmailUserSettingsApply(t, r, nil, block("forward@example.com"))

	if diff := gmailUserSettingsPlan(t, r, state, block("Forward@Example.com")); diff != nil && !diff.Empty() {
		t.Fatalf("unexpected change for an email_address that differs only in case: %#v", diff.Attributes)
	}
}

func TestGmailUserSettingsRead_notFoundIsAnError(t *testing.T) {
	f := newFakeGmail(t)
	r := resourceGmailUserSettings()

	previousBackoff := notFoundBackoff
	t.Cleanup(func() { notFoundBackoff = previousBackoff })
	notFoundBackoff = nil

	state := gmailUserSettingsApply(t, r, nil, map[string]interface{}{
		"user_email": testGmailUserEmail,
		"pop":        []interface{}{map[string]interface{}{"access_window": "disabled"}},
	})
	f.mu.Lock()
	f.notFound = true
	f.mu.Unlock()

	newState, diags := r.RefreshWithoutUpgrade(context.Background(), state, &apiClient{})
	if !diags.HasError() {
		t.Fatalf("expected a refresh error, got state %v", newState)
	}
	if !strings.Contains(fmt.Sprint(diags), testGmailUserEmail) || !strings.Contains(fmt.Sprint(diags), "404") {
		t.Fatalf("expected an error that names the user and the 404, got %v", diags)
	}
	if newState == nil || newState.ID != testGmailUserEmail {
		t.Fatalf("expected the resource to stay in state, got %v", newState)
	}
}

func TestGmailUserSettingsCreate_unspecifiedValuesAreNotSent(t *testing.T) {
	f := newFakeGmail(t)
	r := resourceGmailUserSettings()
	f.setSetting("imap", map[string]interface{}{"enabled": true, "expungeBehavior": "expungeBehaviorUnspecified"})
	f.setSetting("pop", map[string]interface{}{"accessWindow": "accessWindowUnspecified", "disposition": "dispositionUnspecified"})
	f.setSetting("autoForwarding", map[string]interface{}{"disposition": "dispositionUnspecified"})

	gmailUserSettingsApply(t, r, nil, map[string]interface{}{
		"user_email":      testGmailUserEmail,
		"imap":            []interface{}{map[string]interface{}{"enabled": false}},
		"pop":             []interface{}{map[string]interface{}{"access_window": "disabled"}},
		"auto_forwarding": []interface{}{map[string]interface{}{"enabled": false}},
	})

	for setting, key := range map[string]string{
		"imap":           "expungeBehavior",
		"pop":            "disposition",
		"autoForwarding": "disposition",
	} {
		if value, ok := f.lastPut(t, setting)[key]; ok {
			t.Errorf("update of %s sends %s = %v, want no value", setting, key, value)
		}
	}
}
