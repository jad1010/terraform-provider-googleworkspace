// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"

	directory "google.golang.org/api/admin/directory/v1"
)

func groupMemberState() *terraform.InstanceState {
	return &terraform.InstanceState{
		ID: "groups/group@example.com/members/ID1",
		Attributes: map[string]string{
			"id":                "groups/group@example.com/members/ID1",
			"group_id":          "group@example.com",
			"email":             "alice@example.com",
			"role":              "MEMBER",
			"type":              "USER",
			"delivery_settings": "ALL_MAIL",
			"member_id":         "ID1",
			"etag":              "etag-1",
			"status":            "ACTIVE",
		},
	}
}

func storedAlice() *directory.Member {
	return &directory.Member{
		Email: "alice@example.com", Id: "ID1", Role: "MEMBER", Type: "USER",
		DeliverySettings: "ALL_MAIL", Etag: "etag-1", Status: "ACTIVE",
	}
}

func TestResourceGroupMemberCreate_consistencyTimeoutKeepsMemberInState(t *testing.T) {
	t.Parallel()

	f := newFakeDirectory()
	// Every get returns a new etag, so the etag never settles.
	f.getView = func(n int, m *directory.Member) (*directory.Member, int) {
		c := *m
		c.Etag = fmt.Sprintf("etag-get-%d", n)
		return &c, http.StatusOK
	}

	d := schema.TestResourceDataRaw(t, resourceGroupMember().Schema, map[string]interface{}{
		"group_id": "group@example.com",
		"email":    "Alice@Example.com",
	})

	// A short deadline gives the wait half of it: one second.
	diags := resourceGroupMemberCreate(operationContext(t, 2*time.Second), d, f.meta(t))
	if diags.HasError() {
		t.Fatalf("Create() diagnostics = %#v, want no error", diags)
	}
	w := warnings(diags)
	if len(w) != 1 || !strings.Contains(w[0].Detail, "timed out while waiting for group_member to be inserted") {
		t.Fatalf("warnings = %#v, want one consistency warning", w)
	}
	if f.inserts != 1 {
		t.Errorf("inserts = %d, want 1", f.inserts)
	}
	if want := "groups/group@example.com/members/ID1"; d.Id() != want {
		t.Errorf("Id() = %q, want %q", d.Id(), want)
	}
	if d.Get("member_id") != "ID1" {
		t.Errorf("member_id = %q, want ID1", d.Get("member_id"))
	}
	// Read ran, and kept the spelling of the email from the configuration.
	if d.Get("status") != "ACTIVE" {
		t.Errorf("status = %q, want ACTIVE from Read", d.Get("status"))
	}
	if d.Get("email") != "Alice@Example.com" {
		t.Errorf("email = %q, want Alice@Example.com", d.Get("email"))
	}
}

func TestResourceGroupMemberCreate_apiErrorDuringWaitFails(t *testing.T) {
	t.Parallel()

	f := newFakeDirectory()
	f.getView = func(int, *directory.Member) (*directory.Member, int) {
		return nil, http.StatusBadRequest
	}

	d := schema.TestResourceDataRaw(t, resourceGroupMember().Schema, map[string]interface{}{
		"group_id": "group@example.com",
		"email":    "alice@example.com",
	})

	diags := resourceGroupMemberCreate(operationContext(t, 5*time.Minute), d, f.meta(t))
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "unexpected error during retries of group_member") {
		t.Fatalf("Create() diagnostics = %#v, want the API error", diags)
	}
	// No state: a tainted resource would delete the member on the next
	// apply. The retry adopts the member (409).
	if d.Id() != "" {
		t.Errorf("Id() = %q, want no state after a failed create", d.Id())
	}
	if f.gets != 1 {
		t.Errorf("gets = %d, want 1", f.gets)
	}
}

func TestResourceGroupMemberUpdate_retriesTransientNotFound(t *testing.T) {
	t.Parallel()

	f := newFakeDirectory(storedAlice())
	// The first get after the update returns 404.
	f.getView = func(n int, m *directory.Member) (*directory.Member, int) {
		if n == 0 {
			return nil, http.StatusNotFound
		}
		return m, http.StatusOK
	}

	r := resourceGroupMember()
	state := groupMemberState()
	diff, err := r.Diff(context.Background(), state, terraform.NewResourceConfigRaw(map[string]interface{}{
		"group_id": "group@example.com",
		"email":    "Alice@Example.com",
		"role":     "OWNER",
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if diff.RequiresNew() {
		t.Fatalf("Diff() requires a new resource: %#v", diff.Attributes)
	}
	if _, ok := diff.Attributes["email"]; ok {
		t.Errorf("Diff() has a change of email case: %#v", diff.Attributes["email"])
	}
	d, err := schema.InternalMap(r.Schema).Data(state, diff)
	if err != nil {
		t.Fatal(err)
	}

	diags := resourceGroupMemberUpdate(operationContext(t, 5*time.Minute), d, f.meta(t))
	if len(diags) != 0 {
		t.Fatalf("Update() diagnostics = %#v, want none", diags)
	}
	if f.updates != 1 {
		t.Errorf("updates = %d, want 1", f.updates)
	}
	if d.Get("role") != "OWNER" {
		t.Errorf("role = %q, want OWNER", d.Get("role"))
	}
	// 404, new etag, two 304s, then Read.
	if f.gets != 5 {
		t.Errorf("gets = %d, want 5", f.gets)
	}
}

func TestResourceGroupMemberUpdate_failureKeepsPriorState(t *testing.T) {
	t.Parallel()

	f := newFakeDirectory() // Alice is not in the group: the update gets 404.
	r := resourceGroupMember()
	state := groupMemberState()
	diff, err := r.Diff(context.Background(), state, terraform.NewResourceConfigRaw(map[string]interface{}{
		"group_id": "group@example.com",
		"email":    "alice@example.com",
		"role":     "OWNER",
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := schema.InternalMap(r.Schema).Data(state, diff)
	if err != nil {
		t.Fatal(err)
	}

	if diags := resourceGroupMemberUpdate(operationContext(t, 5*time.Minute), d, f.meta(t)); !diags.HasError() {
		t.Fatalf("Update() diagnostics = %#v, want an error", diags)
	}
	if role := d.State().Attributes["role"]; role != "MEMBER" {
		t.Errorf("role in state = %q, want the prior MEMBER", role)
	}
}

func TestResourceGroupMemberUpdate_noChangeSendsNoUpdate(t *testing.T) {
	t.Parallel()

	f := newFakeDirectory(storedAlice())

	d, err := schema.InternalMap(resourceGroupMember().Schema).Data(groupMemberState(), nil)
	if err != nil {
		t.Fatal(err)
	}

	diags := resourceGroupMemberUpdate(operationContext(t, 5*time.Minute), d, f.meta(t))
	if len(diags) != 0 {
		t.Fatalf("Update() diagnostics = %#v, want none", diags)
	}
	if f.updates != 0 {
		t.Errorf("updates = %d, want 0", f.updates)
	}
	// Only Read.
	if f.gets != 1 {
		t.Errorf("gets = %d, want 1", f.gets)
	}
}

func groupMemberDiff(t *testing.T, raw map[string]interface{}, meta interface{}) (*terraform.InstanceDiff, error) {
	t.Helper()
	return resourceGroupMember().Diff(context.Background(), groupMemberState(), terraform.NewResourceConfigRaw(raw), meta)
}

func TestResourceGroupMember_groupIdChange(t *testing.T) {
	t.Parallel()

	f := newFakeDirectory(storedAlice())
	f.groups = map[string]string{"group@example.com": "G1", "g1": "G1", "other@example.com": "G2"}
	meta := f.meta(t)
	config := func(groupId string) map[string]interface{} {
		return map[string]interface{}{"group_id": groupId, "email": "alice@example.com"}
	}

	diff, err := groupMemberDiff(t, config("other@example.com"), meta)
	if err != nil {
		t.Fatal(err)
	}
	if diff == nil || !diff.RequiresNew() {
		t.Errorf("another group: Diff() = %#v, want a replacement", diff)
	}

	// State from an import names the group by ID; the configuration by email.
	for _, pair := range [][2]string{{"G1", "group@example.com"}, {"group@example.com", "G1"}} {
		state := groupMemberState()
		state.Attributes["group_id"] = pair[0]
		diff, err := resourceGroupMember().Diff(context.Background(), state, terraform.NewResourceConfigRaw(config(pair[1])), meta)
		if err != nil {
			t.Fatal(err)
		}
		if diff == nil || diff.Empty() || diff.RequiresNew() {
			t.Errorf("%s to %s: Diff() = %#v, want an in-place change", pair[0], pair[1], diff)
			continue
		}
		d, err := schema.InternalMap(resourceGroupMember().Schema).Data(state, diff)
		if err != nil {
			t.Fatal(err)
		}
		if diags := resourceGroupMemberUpdate(operationContext(t, 5*time.Minute), d, meta); len(diags) != 0 {
			t.Fatalf("Update() diagnostics = %#v", diags)
		}
		if f.updates != 0 {
			t.Errorf("updates = %d, want 0", f.updates)
		}
		if want := "groups/" + pair[1] + "/members/ID1"; d.Id() != want {
			t.Errorf("Id() = %q, want %q", d.Id(), want)
		}
	}

	lookups := f.groupGets
	diff, err = groupMemberDiff(t, config("Group@Example.COM"), meta)
	if err != nil {
		t.Fatal(err)
	}
	if diff != nil && !diff.Empty() {
		t.Errorf("case only: Diff() = %#v, want no diff", diff.Attributes)
	}
	if f.groupGets != lookups {
		t.Errorf("case only: group lookups = %d, want none", f.groupGets-lookups)
	}

	f.mu.Lock()
	f.groupStatus = http.StatusForbidden
	f.mu.Unlock()
	diff, err = groupMemberDiff(t, config("G1"), meta)
	if err != nil || diff == nil || !diff.RequiresNew() {
		t.Errorf("403: Diff() = %#v, %v; want a replacement", diff, err)
	}

	f.mu.Lock()
	f.groupStatus = http.StatusInternalServerError
	f.mu.Unlock()
	if _, err := groupMemberDiff(t, config("G1"), meta); err == nil || !strings.Contains(err.Error(), "names the same group") {
		t.Errorf("lookup error: Diff() error = %v, want the lookup error", err)
	}
}

func TestResourceGroupMember_unchangedGroupIdIsNotADiff(t *testing.T) {
	t.Parallel()

	// State as an earlier provider version wrote it, with the same values
	// as the configuration. No API lookup is needed (nil meta).
	diff, err := groupMemberDiff(t, map[string]interface{}{
		"group_id": "group@example.com",
		"email":    "alice@example.com",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if diff != nil && !diff.Empty() {
		t.Errorf("Diff() = %#v, want no diff", diff.Attributes)
	}
}

func TestResourceGroupMemberCreate_adoptsExistingMember(t *testing.T) {
	t.Parallel()

	f := newFakeDirectory(storedAlice())
	d := schema.TestResourceDataRaw(t, resourceGroupMember().Schema, map[string]interface{}{
		"group_id":          "group@example.com",
		"email":             "alice@example.com",
		"role":              "OWNER",
		"delivery_settings": "DIGEST",
	})

	diags := resourceGroupMemberCreate(operationContext(t, 5*time.Minute), d, f.meta(t))
	if diags.HasError() {
		t.Fatalf("Create() diagnostics = %#v, want no error", diags)
	}
	if want := "groups/group@example.com/members/ID1"; d.Id() != want {
		t.Errorf("Id() = %q, want %q", d.Id(), want)
	}
	w := adoptionWarnings(diags)["alice@example.com"]
	if !strings.Contains(w, "role changed from MEMBER to OWNER") || !strings.Contains(w, "delivery settings changed from ALL_MAIL to DIGEST") {
		t.Errorf("adoption warning = %q, want the role and delivery changes", w)
	}
	// One apply converges: the adopted member gets the configured settings.
	if f.inserts != 1 || f.updates != 1 {
		t.Errorf("inserts, updates = %d, %d; want 1, 1", f.inserts, f.updates)
	}
	if d.Get("role") != "OWNER" || d.Get("delivery_settings") != "DIGEST" {
		t.Errorf("role, delivery_settings = %q, %q; want OWNER, DIGEST", d.Get("role"), d.Get("delivery_settings"))
	}
}

func TestResourceGroupMemberCreate_adoptedMemberWithSameSettingsIsNotUpdated(t *testing.T) {
	t.Parallel()

	f := newFakeDirectory(storedAlice())
	d := schema.TestResourceDataRaw(t, resourceGroupMember().Schema, map[string]interface{}{
		"group_id": "group@example.com",
		"email":    "alice@example.com",
	})

	diags := resourceGroupMemberCreate(operationContext(t, 5*time.Minute), d, f.meta(t))
	if diags.HasError() {
		t.Fatalf("Create() diagnostics = %#v, want no error", diags)
	}
	if f.updates != 0 {
		t.Errorf("updates = %d, want 0", f.updates)
	}
	if w, ok := adoptionWarnings(diags)["alice@example.com"]; !ok || strings.Contains(w, "changed") {
		t.Errorf("adoption warning = %q (present %v), want one without changes", w, ok)
	}
}

func TestResourceGroupMemberCreate_readNotFoundKeepsMemberInState(t *testing.T) {
	stubNotFoundBackoff(t, []time.Duration{0, 0, 0, 0, 0})

	f := newFakeDirectory()
	// The inserted member is never readable.
	f.getView = func(int, *directory.Member) (*directory.Member, int) {
		return nil, http.StatusNotFound
	}
	d := schema.TestResourceDataRaw(t, resourceGroupMember().Schema, map[string]interface{}{
		"group_id": "group@example.com",
		"email":    "Alice@Example.com",
	})

	diags := resourceGroupMemberCreate(operationContext(t, 2*time.Second), d, f.meta(t))
	if diags.HasError() {
		t.Fatalf("Create() diagnostics = %#v, want no error", diags)
	}
	w := warnings(diags)
	if len(w) != 2 || !strings.Contains(w[1].Summary, "not yet readable") {
		t.Fatalf("warnings = %#v, want the consistency warning and the not-readable warning", w)
	}
	if want := "groups/group@example.com/members/ID1"; d.Id() != want {
		t.Errorf("Id() = %q, want %q restored after Read removed it", d.Id(), want)
	}
	if d.Get("member_id") != "ID1" || d.Get("email") != "Alice@Example.com" {
		t.Errorf("member_id, email = %q, %q", d.Get("member_id"), d.Get("email"))
	}
}

func TestDataSourceGroupMemberRead_returnsAPIEmail(t *testing.T) {
	t.Parallel()

	f := newFakeDirectory(storedAlice())
	d := schema.TestResourceDataRaw(t, dataSourceGroupMember().Schema, map[string]interface{}{
		"group_id": "group@example.com",
		"email":    "Alice@Example.com",
	})

	if diags := dataSourceGroupMemberRead(context.Background(), d, f.meta(t)); len(diags) != 0 {
		t.Fatalf("Read() diagnostics = %#v", diags)
	}
	if d.Get("email") != "alice@example.com" || d.Get("member_id") != "ID1" {
		t.Errorf("email, member_id = %q, %q; want alice@example.com, ID1", d.Get("email"), d.Get("member_id"))
	}
	if strings.Contains(dataSourceGroupMember().Schema["email"].Description, "regard to case") ||
		strings.Contains(dataSourceGroupMember().Schema["group_id"].Description, "regard to case") {
		t.Errorf("data source descriptions keep the note about case")
	}
}
