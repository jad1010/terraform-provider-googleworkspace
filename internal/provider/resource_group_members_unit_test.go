// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"

	directory "google.golang.org/api/admin/directory/v1"
)

// fakeDirectory is a minimal Directory API members endpoint. The member list
// that a list call returns comes from listView, so that a test can make the
// listing lag behind the writes.
type fakeDirectory struct {
	mu sync.Mutex

	members  []*directory.Member
	nextID   int
	pageSize int

	// listView returns the members that list call n (from 0) shows. A list
	// call is one paged listing; it can make more than one request.
	listView func(n int, members []*directory.Member) []*directory.Member
	// listStatus, when not zero, is the HTTP status of each list request.
	listStatus int
	// deleteStatus, by member key, is the HTTP status of a delete.
	deleteStatus map[string]int
	// insertStatus, by email, is the HTTP status of an insert.
	insertStatus map[string]int
	// emptyInsertID makes an insert return no member ID.
	emptyInsertID bool
	// aliases maps a member alias in lower case to the primary email. The
	// API resolves an alias to its member, and lists the primary email.
	aliases map[string]string
	// groups maps a group key in lower case to the group's unique ID.
	groups map[string]string
	// groupStatus, when not zero, is the HTTP status of a group get, with
	// groupMessage (or the status text) as the error message.
	groupStatus  int
	groupMessage string
	// getView returns the response to a get. Nil means the stored member.
	getView func(n int, m *directory.Member) (*directory.Member, int)

	listCalls    int
	listRequests int
	inserts      int
	updates      int
	deletes      int
	gets         int
	groupGets    int
	pageTokens   []string
}

func newFakeDirectory(members ...*directory.Member) *fakeDirectory {
	return &fakeDirectory{members: members, pageSize: 2}
}

// meta returns a provider meta value whose HTTP client sends every request
// to the fake.
func (f *fakeDirectory) meta(t *testing.T) *apiClient {
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	target, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &apiClient{client: &http.Client{Transport: redirectTransport{target: target}}}
}

type redirectTransport struct {
	target *url.URL
}

func (rt redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Scheme = rt.target.Scheme
	r.URL.Host = rt.target.Host
	r.Host = ""
	return http.DefaultTransport.RoundTrip(r)
}

func (f *fakeDirectory) find(key string) int {
	if primary, ok := f.aliases[strings.ToLower(key)]; ok {
		key = primary
	}
	for i, m := range f.members {
		if m.Id == key || (m.Email != "" && strings.EqualFold(m.Email, key)) {
			return i
		}
	}
	return -1
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]interface{}{"code": status, "message": http.StatusText(status)},
	})
}

func (f *fakeDirectory) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Paths: /admin/directory/v1/groups/{group}[/members[/{member}]]
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/admin/directory/v1/"), "/")
	if len(parts) == 2 && parts[0] == "groups" && r.Method == http.MethodGet {
		f.groupGets++
		key, _ := url.PathUnescape(parts[1])
		id, ok := f.groups[strings.ToLower(key)]
		switch {
		case f.groupStatus != 0 && f.groupMessage != "":
			writeJSON(w, f.groupStatus, map[string]interface{}{
				"error": map[string]interface{}{"code": f.groupStatus, "message": f.groupMessage},
			})
		case f.groupStatus != 0:
			writeAPIError(w, f.groupStatus)
		case !ok:
			writeAPIError(w, http.StatusNotFound)
		default:
			writeJSON(w, http.StatusOK, &directory.Group{Id: id})
		}
		return
	}
	if len(parts) < 3 || parts[0] != "groups" || parts[2] != "members" {
		writeAPIError(w, http.StatusNotFound)
		return
	}
	memberKey := ""
	if len(parts) == 4 {
		memberKey, _ = url.PathUnescape(parts[3])
	}

	switch {
	case r.Method == http.MethodGet && memberKey == "":
		f.listRequests++
		if f.listStatus != 0 {
			f.listCalls++
			writeAPIError(w, f.listStatus)
			return
		}
		token := r.URL.Query().Get("pageToken")
		f.pageTokens = append(f.pageTokens, token)
		start := 0
		if token == "" {
			f.listCalls++
		} else {
			start, _ = strconv.Atoi(token)
		}
		view := f.members
		if f.listView != nil {
			view = f.listView(f.listCalls-1, f.members)
		}
		end := start + f.pageSize
		resp := &directory.Members{}
		if end < len(view) {
			resp.NextPageToken = strconv.Itoa(end)
		} else {
			end = len(view)
		}
		if start < end {
			resp.Members = view[start:end]
		}
		writeJSON(w, http.StatusOK, resp)

	case r.Method == http.MethodPost && memberKey == "":
		f.inserts++
		m := &directory.Member{}
		if err := json.NewDecoder(r.Body).Decode(m); err != nil {
			writeAPIError(w, http.StatusBadRequest)
			return
		}
		if status := f.insertStatus[m.Email]; status != 0 {
			writeAPIError(w, status)
			return
		}
		if f.find(m.Email) >= 0 {
			writeJSON(w, http.StatusConflict, map[string]interface{}{
				"error": map[string]interface{}{
					"code":    http.StatusConflict,
					"message": "Member already exists.",
					"errors":  []interface{}{map[string]interface{}{"reason": "duplicate", "message": "Member already exists."}},
				},
			})
			return
		}
		f.nextID++
		m.Id = fmt.Sprintf("ID%d", f.nextID)
		m.Email = strings.ToLower(m.Email)
		if primary, ok := f.aliases[m.Email]; ok {
			m.Email = primary
		}
		m.Etag = "etag-1"
		m.Status = "ACTIVE"
		f.members = append(f.members, m)
		if f.emptyInsertID {
			c := *m
			c.Id = ""
			m = &c
		}
		writeJSON(w, http.StatusOK, m)

	case r.Method == http.MethodGet:
		f.gets++
		i := f.find(memberKey)
		var m *directory.Member
		if i >= 0 {
			m = f.members[i]
		}
		status := http.StatusOK
		if f.getView != nil {
			m, status = f.getView(f.gets-1, m)
		}
		switch {
		case status != http.StatusOK:
			writeAPIError(w, status)
		case m == nil:
			writeAPIError(w, http.StatusNotFound)
		case r.Header.Get("If-None-Match") != "" && r.Header.Get("If-None-Match") == m.Etag:
			w.WriteHeader(http.StatusNotModified)
		default:
			writeJSON(w, http.StatusOK, m)
		}

	case r.Method == http.MethodPut:
		f.updates++
		i := f.find(memberKey)
		if i < 0 {
			writeAPIError(w, http.StatusNotFound)
			return
		}
		m := &directory.Member{}
		if err := json.NewDecoder(r.Body).Decode(m); err != nil {
			writeAPIError(w, http.StatusBadRequest)
			return
		}
		cur := f.members[i]
		if m.Role != "" {
			cur.Role = m.Role
		}
		if m.Type != "" {
			cur.Type = m.Type
		}
		if m.DeliverySettings != "" {
			cur.DeliverySettings = m.DeliverySettings
		}
		cur.Etag = "etag-2"
		writeJSON(w, http.StatusOK, cur)

	case r.Method == http.MethodDelete:
		f.deletes++
		if status := f.deleteStatus[memberKey]; status != 0 {
			writeAPIError(w, status)
			return
		}
		i := f.find(memberKey)
		if i < 0 {
			writeAPIError(w, http.StatusNotFound)
			return
		}
		f.members = append(f.members[:i:i], f.members[i+1:]...)
		w.WriteHeader(http.StatusNoContent)

	default:
		writeAPIError(w, http.StatusMethodNotAllowed)
	}
}

// fastGroupMembersPoll makes the group_members consistency wait poll every
// millisecond. Tests that use it must not run in parallel.
func fastGroupMembersPoll(t *testing.T) {
	old := groupMembersPollInterval
	groupMembersPollInterval = time.Millisecond
	t.Cleanup(func() { groupMembersPollInterval = old })
}

// operationContext returns a context with the deadline that the SDK sets for
// an operation with the given timeout.
func operationContext(t *testing.T, timeout time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}

func warnings(diags diag.Diagnostics) []diag.Diagnostic {
	var w []diag.Diagnostic
	for _, d := range diags {
		if d.Severity == diag.Warning {
			w = append(w, d)
		}
	}
	return w
}

// stateMembers returns the members in state by email.
func stateMembers(t *testing.T, d *schema.ResourceData) map[string]map[string]interface{} {
	t.Helper()
	got := map[string]map[string]interface{}{}
	for _, raw := range d.Get("members").(*schema.Set).List() {
		m := raw.(map[string]interface{})
		key := m["email"].(string)
		if key == "" {
			key = "id:" + m["id"].(string)
		}
		got[key] = m
	}
	return got
}

func groupMembersConfig(members ...map[string]interface{}) map[string]interface{} {
	list := make([]interface{}, len(members))
	for i, m := range members {
		list[i] = m
	}
	return map[string]interface{}{"group_id": "group@example.com", "members": list}
}

// planGroupMembersUpdate returns the ResourceData that Update gets for a
// change from state to the raw configuration.
func planGroupMembersUpdate(t *testing.T, state *terraform.InstanceState, raw map[string]interface{}) (*schema.ResourceData, *terraform.InstanceDiff) {
	t.Helper()
	d, diff, err := planGroupMembersChange(state, raw, nil)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	return d, diff
}

// planGroupMembersChange is planGroupMembersUpdate with a provider meta value
// for CustomizeDiff, and with the Diff error returned.
func planGroupMembersChange(state *terraform.InstanceState, raw map[string]interface{}, meta interface{}) (*schema.ResourceData, *terraform.InstanceDiff, error) {
	r := resourceGroupMembers()
	diff, err := r.Diff(context.Background(), state, terraform.NewResourceConfigRaw(raw), meta)
	if err != nil {
		return nil, nil, err
	}
	d, err := schema.InternalMap(r.Schema).Data(state, diff)
	return d, diff, err
}

func TestResourceGroupMembersCreate_waitsOnlyForDeclaredMembers(t *testing.T) {
	fastGroupMembersPoll(t)

	extra := &directory.Member{Email: "extra@example.com", Id: "X", Role: "MEMBER", Type: "USER", Status: "ACTIVE"}
	customer := &directory.Member{Id: "C0123", Role: "MEMBER", Type: "CUSTOMER"}
	f := newFakeDirectory(extra, customer)
	// The first two listings lag: they show neither new member, then
	// Alice with the default role and no Bob.
	f.listView = func(n int, members []*directory.Member) []*directory.Member {
		switch n {
		case 0:
			return members[:2]
		case 1:
			alice := *members[2]
			alice.Role = "MEMBER"
			return []*directory.Member{members[0], members[1], &alice}
		}
		return members
	}

	d := schema.TestResourceDataRaw(t, resourceGroupMembers().Schema, groupMembersConfig(
		map[string]interface{}{"email": "Alice@Example.com", "role": "OWNER"},
		map[string]interface{}{"email": "bob@example.com"},
	))

	diags := resourceGroupMembersCreate(operationContext(t, 20*time.Minute), d, f.meta(t))
	if len(diags) != 0 {
		t.Fatalf("Create() diagnostics = %#v, want none", diags)
	}

	if f.inserts != 2 {
		t.Errorf("inserts = %d, want 2", f.inserts)
	}
	// Two stale listings, numConsistent consistent ones, and one from Read.
	if want := 2 + numConsistent + 1; f.listCalls != want {
		t.Errorf("list calls = %d, want %d", f.listCalls, want)
	}
	// Each full listing of four members takes two pages of two.
	if want := 2 + 2*(numConsistent+1); f.listRequests < want {
		t.Errorf("list requests = %d, want at least %d (pagination)", f.listRequests, want)
	}
	if d.Id() != "groups/group@example.com" {
		t.Errorf("Id() = %q", d.Id())
	}

	got := stateMembers(t, d)
	if len(got) != 4 {
		t.Fatalf("state members = %v, want 4", got)
	}
	// The spelling from the configuration is kept.
	if m, ok := got["Alice@Example.com"]; !ok || m["role"] != "OWNER" || m["id"] != "ID1" {
		t.Errorf("Alice in state = %v", m)
	}
	if _, ok := got["bob@example.com"]; !ok {
		t.Errorf("Bob is not in state: %v", got)
	}
	// Members that the configuration does not declare are recorded as drift.
	if _, ok := got["extra@example.com"]; !ok {
		t.Errorf("extra member is not in state: %v", got)
	}
	if _, ok := got["id:C0123"]; !ok {
		t.Errorf("CUSTOMER member is not in state: %v", got)
	}
}

func TestResourceGroupMembersCreate_consistencyTimeoutWarns(t *testing.T) {
	fastGroupMembersPoll(t)

	f := newFakeDirectory()
	// Bob never shows in the listing.
	f.listView = func(_ int, members []*directory.Member) []*directory.Member {
		var view []*directory.Member
		for _, m := range members {
			if m.Email != "bob@example.com" {
				view = append(view, m)
			}
		}
		return view
	}

	d := schema.TestResourceDataRaw(t, resourceGroupMembers().Schema, groupMembersConfig(
		map[string]interface{}{"email": "alice@example.com"},
		map[string]interface{}{"email": "bob@example.com"},
	))

	// A short deadline gives the wait half of it: 200ms.
	diags := resourceGroupMembersCreate(operationContext(t, 400*time.Millisecond), d, f.meta(t))
	if diags.HasError() {
		t.Fatalf("Create() diagnostics = %#v, want no error", diags)
	}
	w := warnings(diags)
	if len(w) != 1 || !strings.Contains(w[0].Detail, "bob@example.com (not listed)") ||
		strings.Contains(w[0].Detail, "alice@example.com") {
		t.Fatalf("warnings = %#v, want one that names only bob@example.com", w)
	}
	if d.Id() != "groups/group@example.com" {
		t.Errorf("Id() = %q, want the resource kept in state", d.Id())
	}
	if f.inserts != 2 {
		t.Errorf("inserts = %d, want 2", f.inserts)
	}
	if _, ok := stateMembers(t, d)["alice@example.com"]; !ok {
		t.Errorf("Read did not run after the warning")
	}
}

func TestResourceGroupMembersCreate_listErrorFails(t *testing.T) {
	fastGroupMembersPoll(t)

	f := newFakeDirectory()
	f.listStatus = http.StatusBadRequest

	d := schema.TestResourceDataRaw(t, resourceGroupMembers().Schema, groupMembersConfig(
		map[string]interface{}{"email": "alice@example.com"},
	))

	diags := resourceGroupMembersCreate(operationContext(t, 20*time.Minute), d, f.meta(t))
	if !diags.HasError() {
		t.Fatalf("Create() diagnostics = %#v, want an error", diags)
	}
	if len(warnings(diags)) != 0 {
		t.Errorf("warnings = %#v, want none", warnings(diags))
	}
	if f.listCalls != 1 {
		t.Errorf("list calls = %d, want 1", f.listCalls)
	}
	// No state: a tainted resource would be destroyed with every member.
	if d.Id() != "" {
		t.Errorf("Id() = %q, want no state after a failed create", d.Id())
	}
}

func TestResourceGroupMembersUpdate_waitsForWrites(t *testing.T) {
	fastGroupMembersPoll(t)

	f := newFakeDirectory()
	meta := f.meta(t)

	d := schema.TestResourceDataRaw(t, resourceGroupMembers().Schema, groupMembersConfig(
		map[string]interface{}{"email": "alice@example.com"},
		map[string]interface{}{"email": "bob@example.com"},
	))
	if diags := resourceGroupMembersCreate(operationContext(t, 20*time.Minute), d, meta); len(diags) != 0 {
		t.Fatalf("Create() diagnostics = %#v", diags)
	}
	state := d.State()

	// A member added outside Terraform after the last refresh.
	f.members = append(f.members, &directory.Member{Email: "extra@example.com", Id: "X", Role: "MEMBER", Type: "USER"})

	// Alice changes role (and the case of her email), Bob is removed and
	// Carol is added. The first listing still shows Bob and Alice's old
	// role, and not Carol.
	var stale []*directory.Member
	for _, m := range f.members {
		c := *m
		stale = append(stale, &c)
	}
	listsBefore := f.listCalls
	f.listView = func(n int, members []*directory.Member) []*directory.Member {
		if n == listsBefore {
			return stale
		}
		return members
	}
	f.inserts = 0

	d, _ = planGroupMembersUpdate(t, state, groupMembersConfig(
		map[string]interface{}{"email": "Alice@Example.com", "role": "OWNER"},
		map[string]interface{}{"email": "carol@example.com"},
	))
	diags := resourceGroupMembersUpdate(operationContext(t, 20*time.Minute), d, meta)
	if len(diags) != 0 {
		t.Fatalf("Update() diagnostics = %#v, want none", diags)
	}

	if f.inserts != 1 || f.updates != 1 || f.deletes != 1 {
		t.Errorf("inserts, updates, deletes = %d, %d, %d; want 1, 1, 1", f.inserts, f.updates, f.deletes)
	}
	// One stale listing, numConsistent consistent ones, and one from Read.
	if want := listsBefore + 1 + numConsistent + 1; f.listCalls != want {
		t.Errorf("list calls = %d, want %d", f.listCalls, want)
	}

	got := stateMembers(t, d)
	if m, ok := got["Alice@Example.com"]; !ok || m["role"] != "OWNER" || m["id"] != "ID1" {
		t.Errorf("Alice in state = %v (all: %v)", m, got)
	}
	if _, ok := got["bob@example.com"]; ok {
		t.Errorf("Bob is still in state")
	}
	if _, ok := got["carol@example.com"]; !ok {
		t.Errorf("Carol is not in state")
	}
	if _, ok := got["extra@example.com"]; !ok {
		t.Errorf("extra member is not in state")
	}
}

func TestResourceGroupMembersUpdate_deletedMemberStillListedWarns(t *testing.T) {
	fastGroupMembersPoll(t)

	f := newFakeDirectory()
	meta := f.meta(t)

	d := schema.TestResourceDataRaw(t, resourceGroupMembers().Schema, groupMembersConfig(
		map[string]interface{}{"email": "alice@example.com"},
		map[string]interface{}{"email": "bob@example.com"},
	))
	if diags := resourceGroupMembersCreate(operationContext(t, 20*time.Minute), d, meta); len(diags) != 0 {
		t.Fatalf("Create() diagnostics = %#v", diags)
	}
	state := d.State()
	bob := *f.members[1]

	// The listing keeps showing Bob after his delete.
	f.listView = func(_ int, members []*directory.Member) []*directory.Member {
		return append(append([]*directory.Member{}, members...), &bob)
	}

	d, _ = planGroupMembersUpdate(t, state, groupMembersConfig(
		map[string]interface{}{"email": "alice@example.com"},
	))
	diags := resourceGroupMembersUpdate(operationContext(t, 400*time.Millisecond), d, meta)
	if diags.HasError() {
		t.Fatalf("Update() diagnostics = %#v, want no error", diags)
	}
	w := warnings(diags)
	if len(w) != 1 || !strings.Contains(w[0].Detail, "bob@example.com (still listed)") {
		t.Fatalf("warnings = %#v, want one that names bob@example.com", w)
	}
	if f.deletes != 1 {
		t.Errorf("deletes = %d, want 1", f.deletes)
	}
}

func TestResourceGroupMembers_emailCaseIsNotADiff(t *testing.T) {
	fastGroupMembersPoll(t)

	f := newFakeDirectory()
	d := schema.TestResourceDataRaw(t, resourceGroupMembers().Schema, groupMembersConfig(
		map[string]interface{}{"email": "alice@example.com", "role": "OWNER"},
	))
	if diags := resourceGroupMembersCreate(operationContext(t, 20*time.Minute), d, f.meta(t)); len(diags) != 0 {
		t.Fatalf("Create() diagnostics = %#v", diags)
	}

	_, diff := planGroupMembersUpdate(t, d.State(), groupMembersConfig(
		map[string]interface{}{"email": "Alice@EXAMPLE.com", "role": "OWNER"},
	))
	if diff != nil && !diff.Empty() {
		t.Errorf("Diff() = %#v, want no diff for a change of email case", diff.Attributes)
	}

	// When another member changes, the SDK diffs every element of the set.
	// The case of Alice's email must still not show as a change.
	_, diff = planGroupMembersUpdate(t, d.State(), groupMembersConfig(
		map[string]interface{}{"email": "Alice@EXAMPLE.com", "role": "OWNER"},
		map[string]interface{}{"email": "bob@example.com"},
	))
	if diff == nil || diff.Empty() {
		t.Fatalf("Diff() is empty, want Bob added")
	}
	for k, attr := range diff.Attributes {
		if strings.HasSuffix(k, ".email") && strings.EqualFold(attr.Old, "alice@example.com") && attr.New != attr.Old {
			t.Errorf("Diff() %s = %q => %q, want no change", k, attr.Old, attr.New)
		}
		if attr.RequiresNew {
			t.Errorf("Diff() %s requires a new resource", k)
		}
	}
}

func TestResourceGroupMembersRead_keepsConfiguredEmailCase(t *testing.T) {
	t.Parallel()

	f := newFakeDirectory(&directory.Member{Email: "alice@example.com", Id: "A", Role: "MEMBER", Type: "USER"})
	d := schema.TestResourceDataRaw(t, resourceGroupMembers().Schema, groupMembersConfig(
		map[string]interface{}{"email": "Alice@Example.com", "delivery_settings": "DIGEST"},
	))

	if diags := resourceGroupMembersRead(context.Background(), d, f.meta(t)); len(diags) != 0 {
		t.Fatalf("Read() diagnostics = %#v", diags)
	}
	m, ok := stateMembers(t, d)["Alice@Example.com"]
	if !ok {
		t.Fatalf("state members = %v, want Alice@Example.com", stateMembers(t, d))
	}
	if m["delivery_settings"] != "DIGEST" {
		t.Errorf("delivery_settings = %v, want DIGEST", m["delivery_settings"])
	}
}

// createdGroupMembers creates three members through the fake and returns
// the ResourceData that Delete gets.
func createdGroupMembers(t *testing.T, f *fakeDirectory, meta *apiClient) *schema.ResourceData {
	t.Helper()
	d := schema.TestResourceDataRaw(t, resourceGroupMembers().Schema, groupMembersConfig(
		map[string]interface{}{"email": "alice@example.com"},
		map[string]interface{}{"email": "bob@example.com"},
		map[string]interface{}{"email": "carol@example.com"},
	))
	if diags := resourceGroupMembersCreate(operationContext(t, 20*time.Minute), d, meta); len(diags) != 0 {
		t.Fatalf("Create() diagnostics = %#v", diags)
	}
	d, err := schema.InternalMap(resourceGroupMembers().Schema).Data(d.State(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestResourceGroupMembersDelete_continuesPastMemberAlreadyGone(t *testing.T) {
	fastGroupMembersPoll(t)

	f := newFakeDirectory()
	meta := f.meta(t)
	d := createdGroupMembers(t, f, meta)

	// Bob was removed outside Terraform.
	i := f.find("bob@example.com")
	f.members = append(f.members[:i:i], f.members[i+1:]...)

	diags := resourceGroupMembersDelete(context.Background(), d, meta)
	if len(diags) != 0 {
		t.Fatalf("Delete() diagnostics = %#v, want none", diags)
	}
	if f.deletes != 3 {
		t.Errorf("deletes = %d, want 3", f.deletes)
	}
	if len(f.members) != 0 {
		t.Errorf("members left in the group = %d, want 0", len(f.members))
	}
	// A 404 for one member must not take the "resource is gone" path, which
	// clears the ID and stops the loop.
	if d.Id() == "" {
		t.Errorf("Id() was cleared by the 404 for one member")
	}
}

func TestResourceGroupMembersDelete_attemptsAllMembersAndReportsFailures(t *testing.T) {
	fastGroupMembersPoll(t)

	f := newFakeDirectory()
	meta := f.meta(t)
	d := createdGroupMembers(t, f, meta)

	// Deleting Bob fails.
	bobID := f.members[f.find("bob@example.com")].Id
	f.deleteStatus = map[string]int{bobID: http.StatusForbidden}

	diags := resourceGroupMembersDelete(context.Background(), d, meta)
	if !diags.HasError() || len(diags) != 1 {
		t.Fatalf("Delete() diagnostics = %#v, want one error", diags)
	}
	if !strings.Contains(diags[0].Summary, "bob@example.com ("+bobID+")") || !strings.Contains(diags[0].Detail, "403") {
		t.Errorf("error = %q: %q, want it to name Bob and the API error", diags[0].Summary, diags[0].Detail)
	}
	if f.deletes != 3 {
		t.Errorf("deletes = %d, want 3", f.deletes)
	}
	if len(f.members) != 1 || f.members[0].Id != bobID {
		t.Errorf("members left in the group = %v, want only Bob", f.members)
	}
	if d.Id() == "" {
		t.Errorf("Id() is empty, want the resource kept in state after a failed delete")
	}
}

// unknownValue is the value that the SDK uses for an unknown attribute.
const unknownValue = "74D93920-ED26-11E3-AC10-0800200C9A66"

func TestResourceGroupMembers_groupIdChange(t *testing.T) {
	fastGroupMembersPoll(t)

	f := newFakeDirectory()
	f.groups = map[string]string{"group@example.com": "G1", "g1": "G1", "other@example.com": "G2"}
	meta := f.meta(t)
	d := schema.TestResourceDataRaw(t, resourceGroupMembers().Schema, groupMembersConfig(
		map[string]interface{}{"email": "alice@example.com"},
	))
	if diags := resourceGroupMembersCreate(operationContext(t, 20*time.Minute), d, meta); len(diags) != 0 {
		t.Fatalf("Create() diagnostics = %#v", diags)
	}
	state := d.State()
	config := func(groupId string) map[string]interface{} {
		raw := groupMembersConfig(map[string]interface{}{"email": "alice@example.com"})
		raw["group_id"] = groupId
		return raw
	}

	t.Run("case only is not a diff", func(t *testing.T) {
		lookups := f.groupGets
		_, diff, err := planGroupMembersChange(state, config("Group@EXAMPLE.com"), meta)
		if err != nil {
			t.Fatal(err)
		}
		if diff != nil && !diff.Empty() {
			t.Errorf("Diff() = %#v, want no diff", diff.Attributes)
		}
		if f.groupGets != lookups {
			t.Errorf("group lookups = %d, want none", f.groupGets-lookups)
		}
	})

	t.Run("another group replaces", func(t *testing.T) {
		_, diff, err := planGroupMembersChange(state, config("other@example.com"), meta)
		if err != nil {
			t.Fatal(err)
		}
		if diff == nil || !diff.RequiresNew() {
			t.Errorf("Diff() = %#v, want a replacement", diff)
		}
	})

	t.Run("unknown value replaces", func(t *testing.T) {
		lookups := f.groupGets
		_, diff, err := planGroupMembersChange(state, config(unknownValue), meta)
		if err != nil {
			t.Fatal(err)
		}
		if diff == nil || !diff.RequiresNew() {
			t.Errorf("Diff() = %#v, want a replacement", diff)
		}
		if f.groupGets != lookups {
			t.Errorf("group lookups = %d, want none for an unknown value", f.groupGets-lookups)
		}
	})

	t.Run("same group by ID updates in place", func(t *testing.T) {
		upd, diff, err := planGroupMembersChange(state, config("G1"), meta)
		if err != nil {
			t.Fatal(err)
		}
		if diff == nil || diff.Empty() || diff.RequiresNew() {
			t.Fatalf("Diff() = %#v, want an in-place change", diff)
		}
		writes := f.inserts + f.updates + f.deletes
		if diags := resourceGroupMembersUpdate(operationContext(t, 20*time.Minute), upd, meta); len(diags) != 0 {
			t.Fatalf("Update() diagnostics = %#v", diags)
		}
		if got := f.inserts + f.updates + f.deletes; got != writes {
			t.Errorf("member writes = %d, want none", got-writes)
		}
		if upd.Id() != "groups/G1" {
			t.Errorf("Id() = %q, want groups/G1", upd.Id())
		}
	})

	setGroupStatus := func(status int) {
		f.mu.Lock()
		f.groupStatus = status
		f.mu.Unlock()
	}

	t.Run("lookup error fails the plan", func(t *testing.T) {
		setGroupStatus(http.StatusBadRequest)
		defer setGroupStatus(0)
		_, _, err := planGroupMembersChange(state, config("G1"), meta)
		if err == nil || !strings.Contains(err.Error(), "names the same group") || !strings.Contains(err.Error(), "400") {
			t.Errorf("Diff() error = %v, want the lookup error", err)
		}
	})

	for _, message := range []string{"Not Authorized to access this resource/api", "Quota exceeded for quota metric"} {
		t.Run("retryable 403 fails the plan: "+message, func(t *testing.T) {
			f.mu.Lock()
			f.groupStatus, f.groupMessage = http.StatusForbidden, message
			f.mu.Unlock()
			defer func() {
				f.mu.Lock()
				f.groupStatus, f.groupMessage = 0, ""
				f.mu.Unlock()
			}()
			_, _, err := planGroupMembersChange(state, config("G1"), meta)
			if err == nil || !strings.Contains(err.Error(), "names the same group") || !strings.Contains(err.Error(), "403") {
				t.Errorf("Diff() error = %v, want the lookup error", err)
			}
		})
	}

	t.Run("group that cannot be read (403) replaces", func(t *testing.T) {
		setGroupStatus(http.StatusForbidden)
		defer setGroupStatus(0)
		_, diff, err := planGroupMembersChange(state, config("G1"), meta)
		if err != nil {
			t.Fatalf("Diff() error = %v, want a replacement", err)
		}
		if diff == nil || !diff.RequiresNew() {
			t.Errorf("Diff() = %#v, want a replacement", diff)
		}
	})
}

func TestResourceGroupMembersCreate_adoptsExistingMembers(t *testing.T) {
	fastGroupMembersPoll(t)

	// Alice is already a member, with another role. Bob is already a member
	// with the configured settings.
	f := newFakeDirectory(
		&directory.Member{Email: "alice@example.com", Id: "A", Role: "MANAGER", Type: "USER", DeliverySettings: "ALL_MAIL"},
		&directory.Member{Email: "bob@example.com", Id: "B", Role: "MEMBER", Type: "USER", DeliverySettings: "ALL_MAIL"},
	)
	d := schema.TestResourceDataRaw(t, resourceGroupMembers().Schema, groupMembersConfig(
		map[string]interface{}{"email": "Alice@Example.com", "role": "OWNER"},
		map[string]interface{}{"email": "bob@example.com"},
		map[string]interface{}{"email": "carol@example.com"},
	))

	diags := resourceGroupMembersCreate(operationContext(t, 20*time.Minute), d, f.meta(t))
	if diags.HasError() {
		t.Fatalf("Create() diagnostics = %#v, want no error", diags)
	}
	w := adoptionWarnings(diags)
	if len(w) != 2 || len(warnings(diags)) != 2 {
		t.Fatalf("warnings = %#v, want one adoption warning for Alice and one for Bob", warnings(diags))
	}
	if !strings.Contains(w["Alice@Example.com"], "role changed from MANAGER to OWNER") ||
		strings.Contains(w["Alice@Example.com"], "lower than") {
		t.Errorf("Alice warning = %q, want her role change", w["Alice@Example.com"])
	}
	if strings.Contains(w["bob@example.com"], "changed") || !strings.Contains(w["bob@example.com"], "Destroying this resource removes") {
		t.Errorf("Bob warning = %q, want no change", w["bob@example.com"])
	}
	// Three inserts (two 409), two gets for the adopted members, and one
	// update for Alice's role only.
	if f.inserts != 3 || f.gets != 2 || f.updates != 1 {
		t.Errorf("inserts, gets, updates = %d, %d, %d; want 3, 2, 1", f.inserts, f.gets, f.updates)
	}
	got := stateMembers(t, d)
	if m := got["Alice@Example.com"]; m == nil || m["id"] != "A" || m["role"] != "OWNER" {
		t.Errorf("Alice in state = %v", m)
	}
	if m := got["bob@example.com"]; m == nil || m["id"] != "B" {
		t.Errorf("Bob in state = %v", m)
	}
}

func TestResourceGroupMembersCreate_failedInsertSavesNoStateAndRetryAdopts(t *testing.T) {
	fastGroupMembersPoll(t)

	// Carol is a declared OWNER and Dave an undeclared member, both from
	// before Terraform.
	f := newFakeDirectory(
		&directory.Member{Email: "carol@example.com", Id: "C", Role: "OWNER", Type: "USER", DeliverySettings: "ALL_MAIL"},
		&directory.Member{Email: "dave@example.com", Id: "D", Role: "MEMBER", Type: "USER", DeliverySettings: "ALL_MAIL"},
	)
	f.insertStatus = map[string]int{"bob@example.com": http.StatusBadRequest}
	meta := f.meta(t)
	config := groupMembersConfig(
		map[string]interface{}{"email": "alice@example.com"},
		map[string]interface{}{"email": "bob@example.com"},
		map[string]interface{}{"email": "carol@example.com", "role": "OWNER"},
	)

	d := schema.TestResourceDataRaw(t, resourceGroupMembers().Schema, config)
	diags := resourceGroupMembersCreate(operationContext(t, 20*time.Minute), d, meta)
	if !diags.HasError() {
		t.Fatalf("Create() diagnostics = %#v, want an error", diags)
	}
	// No ID: Terraform saves no state, so nothing is tainted and destroyed.
	if d.Id() != "" {
		t.Fatalf("Id() = %q, want no state after a failed create", d.Id())
	}

	// The retry adopts every member that the first attempt inserted.
	f.insertStatus = nil
	d = schema.TestResourceDataRaw(t, resourceGroupMembers().Schema, config)
	diags = resourceGroupMembersCreate(operationContext(t, 20*time.Minute), d, meta)
	if diags.HasError() {
		t.Fatalf("retry Create() diagnostics = %#v, want no error", diags)
	}
	if w := adoptionWarnings(diags); len(w) != 2 || w["alice@example.com"] == "" || w["carol@example.com"] == "" {
		t.Errorf("adoption warnings = %v, want Alice and Carol", w)
	}
	if f.deletes != 0 {
		t.Errorf("deletes = %d, want 0", f.deletes)
	}
	for _, email := range []string{"alice@example.com", "bob@example.com", "carol@example.com", "dave@example.com"} {
		if f.find(email) < 0 {
			t.Errorf("%s is not in the group", email)
		}
	}
	if m := stateMembers(t, d)["carol@example.com"]; m == nil || m["id"] != "C" || m["role"] != "OWNER" {
		t.Errorf("Carol in state = %v", m)
	}
}

func TestResourceGroupMembersCreate_resolvesEmptyInsertID(t *testing.T) {
	fastGroupMembersPoll(t)

	f := newFakeDirectory()
	f.emptyInsertID = true
	// The configuration names Alice by an alias; the listing shows her
	// primary address. Only the member ID can match her.
	f.listView = func(_ int, members []*directory.Member) []*directory.Member {
		var view []*directory.Member
		for _, m := range members {
			c := *m
			c.Email = "primary@example.com"
			view = append(view, &c)
		}
		return view
	}
	d := schema.TestResourceDataRaw(t, resourceGroupMembers().Schema, groupMembersConfig(
		map[string]interface{}{"email": "alias@example.com"},
	))

	diags := resourceGroupMembersCreate(operationContext(t, 400*time.Millisecond), d, f.meta(t))
	if len(diags) != 0 {
		t.Fatalf("Create() diagnostics = %#v, want none", diags)
	}
	if f.gets != 1 {
		t.Errorf("gets = %d, want 1 to resolve the member ID", f.gets)
	}
}

func TestResourceGroupMembersDelete_stopsWhenContextCanceled(t *testing.T) {
	fastGroupMembersPoll(t)

	f := newFakeDirectory()
	meta := f.meta(t)
	d := createdGroupMembers(t, f, meta)
	deletes := f.deletes

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	diags := resourceGroupMembersDelete(ctx, d, meta)
	if len(diags) != 1 || !strings.Contains(diags[0].Summary, "stopped") || !strings.Contains(diags[0].Detail, "3 of 3") {
		t.Fatalf("Delete() diagnostics = %#v, want one that tells the delete stopped", diags)
	}
	if f.deletes != deletes {
		t.Errorf("deletes = %d, want none", f.deletes-deletes)
	}
}

// adoptionWarnings returns the detail of each adoption warning by email.
func adoptionWarnings(diags diag.Diagnostics) map[string]string {
	got := map[string]string{}
	for _, d := range warnings(diags) {
		if rest, ok := strings.CutPrefix(d.Summary, "Adopted existing member "); ok {
			got[strings.Fields(rest)[0]] = d.Detail
		}
	}
	return got
}

func TestResourceGroupMembersCreate_adoptionThatLowersTheRoleWarns(t *testing.T) {
	fastGroupMembersPoll(t)

	f := newFakeDirectory(&directory.Member{Email: "alice@example.com", Id: "A", Role: "OWNER", Type: "USER", DeliverySettings: "ALL_MAIL"})
	d := schema.TestResourceDataRaw(t, resourceGroupMembers().Schema, groupMembersConfig(
		map[string]interface{}{"email": "alice@example.com", "delivery_settings": "DIGEST"},
	))

	diags := resourceGroupMembersCreate(operationContext(t, 20*time.Minute), d, f.meta(t))
	if diags.HasError() {
		t.Fatalf("Create() diagnostics = %#v, want no error", diags)
	}
	w := adoptionWarnings(diags)["alice@example.com"]
	for _, want := range []string{"role changed from OWNER to MEMBER", "lower than the existing role", "delivery settings changed from ALL_MAIL to DIGEST"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning = %q, want it to contain %q", w, want)
		}
	}
	// The configuration wins.
	if m := f.members[0]; m.Role != "MEMBER" || m.DeliverySettings != "DIGEST" {
		t.Errorf("member = %s %s, want MEMBER DIGEST", m.Role, m.DeliverySettings)
	}
}

func TestResourceGroupMembersCreate_otherConflictIsAnError(t *testing.T) {
	fastGroupMembersPoll(t)

	// A 409 without "Member already exists" is not an adoption.
	f := newFakeDirectory()
	f.insertStatus = map[string]int{"alice@example.com": http.StatusConflict}
	d := schema.TestResourceDataRaw(t, resourceGroupMembers().Schema, groupMembersConfig(
		map[string]interface{}{"email": "alice@example.com"},
	))

	diags := resourceGroupMembersCreate(operationContext(t, 20*time.Minute), d, f.meta(t))
	if !diags.HasError() {
		t.Fatalf("Create() diagnostics = %#v, want an error", diags)
	}
	if f.gets != 0 || d.Id() != "" {
		t.Errorf("gets, Id() = %d, %q; want 0 and no state", f.gets, d.Id())
	}
}

// A state entry and a configured address can name the same member: an alias
// in the configuration, or a renamed user whose state has the new primary
// address and whose old address is now an alias. Map order in Update is
// random, so the test runs several times.
func TestResourceGroupMembersUpdate_otherAddressOfSameMemberIsNotRemoved(t *testing.T) {
	fastGroupMembersPoll(t)

	tests := map[string]struct{ stateEmail, configEmail string }{
		"alias in the configuration": {stateEmail: "primary@example.com", configEmail: "alias@example.com"},
		"renamed user":               {stateEmail: "new@example.com", configEmail: "old@example.com"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			for i := 0; i < 10; i++ {
				f := newFakeDirectory()
				meta := f.meta(t)
				d := schema.TestResourceDataRaw(t, resourceGroupMembers().Schema, groupMembersConfig(
					map[string]interface{}{"email": test.stateEmail},
					map[string]interface{}{"email": "bob@example.com"},
				))
				if diags := resourceGroupMembersCreate(operationContext(t, 20*time.Minute), d, meta); len(diags) != 0 {
					t.Fatalf("Create() diagnostics = %#v", diags)
				}
				f.aliases = map[string]string{test.configEmail: test.stateEmail}

				d, _ = planGroupMembersUpdate(t, d.State(), groupMembersConfig(
					map[string]interface{}{"email": test.configEmail},
					map[string]interface{}{"email": "bob@example.com"},
				))
				diags := resourceGroupMembersUpdate(operationContext(t, 400*time.Millisecond), d, meta)
				if diags.HasError() || len(warnings(diags)) != 1 || adoptionWarnings(diags)[test.configEmail] == "" {
					t.Fatalf("Update() diagnostics = %#v, want only the adoption warning", diags)
				}
				if f.deletes != 0 || f.find(test.stateEmail) < 0 {
					t.Fatalf("deletes = %d, member in group = %v; want 0 and true", f.deletes, f.find(test.stateEmail) >= 0)
				}
			}
		})
	}
}

func TestResourceGroupMembersUpdate_failureKeepsPriorState(t *testing.T) {
	fastGroupMembersPoll(t)

	f := newFakeDirectory()
	meta := f.meta(t)
	d := schema.TestResourceDataRaw(t, resourceGroupMembers().Schema, groupMembersConfig(
		map[string]interface{}{"email": "alice@example.com"},
		map[string]interface{}{"email": "bob@example.com"},
	))
	if diags := resourceGroupMembersCreate(operationContext(t, 20*time.Minute), d, meta); len(diags) != 0 {
		t.Fatalf("Create() diagnostics = %#v", diags)
	}
	prior := stateMembers(t, d)

	f.insertStatus = map[string]int{"dave@example.com": http.StatusBadRequest}
	d, _ = planGroupMembersUpdate(t, d.State(), groupMembersConfig(
		map[string]interface{}{"email": "alice@example.com"},
		map[string]interface{}{"email": "carol@example.com"},
		map[string]interface{}{"email": "dave@example.com"},
	))
	if diags := resourceGroupMembersUpdate(operationContext(t, 20*time.Minute), d, meta); !diags.HasError() {
		t.Fatalf("Update() diagnostics = %#v, want an error", diags)
	}

	after, err := schema.InternalMap(resourceGroupMembers().Schema).Data(d.State(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := stateMembers(t, after)
	if len(got) != len(prior) {
		t.Fatalf("state members = %v, want the prior state %v", got, prior)
	}
	for email, m := range prior {
		if got[email] == nil || got[email]["id"] != m["id"] {
			t.Errorf("state member %s = %v, want %v", email, got[email], m)
		}
	}
	// Phase 2 (deletes) does not run after a failed insert.
	if f.deletes != 0 {
		t.Errorf("deletes = %d, want 0", f.deletes)
	}
}

func TestResourceGroupMembersUpdate_vanishedMembers(t *testing.T) {
	fastGroupMembersPoll(t)

	f := newFakeDirectory()
	meta := f.meta(t)
	d := schema.TestResourceDataRaw(t, resourceGroupMembers().Schema, groupMembersConfig(
		map[string]interface{}{"email": "alice@example.com"},
		map[string]interface{}{"email": "bob@example.com"},
	))
	if diags := resourceGroupMembersCreate(operationContext(t, 20*time.Minute), d, meta); len(diags) != 0 {
		t.Fatalf("Create() diagnostics = %#v", diags)
	}
	state := d.State()

	// Both members were removed outside Terraform.
	f.members = nil
	inserts := f.inserts

	// Bob is removed from the configuration (delete gets 404); Alice gets a
	// new role (update gets 404, so she is inserted again).
	d, _ = planGroupMembersUpdate(t, state, groupMembersConfig(
		map[string]interface{}{"email": "alice@example.com", "role": "OWNER"},
	))
	diags := resourceGroupMembersUpdate(operationContext(t, 20*time.Minute), d, meta)
	if len(diags) != 0 {
		t.Fatalf("Update() diagnostics = %#v, want none", diags)
	}
	if f.deletes != 1 || f.updates != 1 || f.inserts != inserts+1 {
		t.Errorf("deletes, updates, inserts = %d, %d, %d; want 1, 1, 1", f.deletes, f.updates, f.inserts-inserts)
	}
	if i := f.find("alice@example.com"); i < 0 || f.members[i].Role != "OWNER" {
		t.Errorf("Alice is not in the group as OWNER")
	}
	if m := stateMembers(t, d)["alice@example.com"]; m == nil || m["role"] != "OWNER" {
		t.Errorf("Alice in state = %v", m)
	}
}
