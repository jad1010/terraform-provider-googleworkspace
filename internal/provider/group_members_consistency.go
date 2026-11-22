// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/errwrap"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"

	directory "google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/googleapi"
)

const (
	// groupMembersConsistencyTimeout is the longest time Create and Update of
	// googleworkspace_group_members wait for the member listing to show their
	// writes. It is independent of the operation timeout.
	groupMembersConsistencyTimeout = 5 * time.Minute

	// consistencyReadReserve is the part of the operation deadline that a
	// consistency wait leaves for the Read that follows it.
	consistencyReadReserve = time.Minute

	// groupMembersPageSize is the number of members in one listing page.
	groupMembersPageSize = 200

	// groupMembersGetLimit is the largest number of expectations that the
	// wait checks with one get each, instead of a listing of a group that
	// has more than one page of members.
	groupMembersGetLimit = 20
)

// groupMembersPollInterval is the time before the second check during a
// consistency wait. The time doubles after each check, up to ten times this
// value. Tests set it to a small value.
var groupMembersPollInterval = 3 * time.Second

// groupMemberExpectation is one result of a write that the member listing
// must show before the write is consistent.
type groupMemberExpectation struct {
	// email is the address the configuration declares. It identifies the
	// member when id is empty, and it names the member in messages.
	email string
	// id is the member ID that a write returned or that state holds. When it
	// is set, the listing is matched by ID, so a member declared by an alias
	// matches the entry that the API lists under the primary address.
	id string
	// role is the role that the listing must show. Empty means any role.
	role string
	// absent is true when the member must not be listed (it was deleted).
	absent bool
}

func (e groupMemberExpectation) name() string {
	if e.email != "" {
		return e.email
	}
	return "member ID " + e.id
}

// groupMembersNotConsistentError tells that the consistency wait stopped
// before the listing showed every write. The writes themselves succeeded.
type groupMembersNotConsistentError struct {
	pending []string
}

func (e *groupMembersNotConsistentError) Error() string {
	return fmt.Sprintf("timed out while waiting for group members to be consistent: %s", strings.Join(e.pending, ", "))
}

// listGroupMembers returns all direct (or, with includeDerived, all) members
// of a group. It retries a 404, which the API can return for a new group.
func listGroupMembers(ctx context.Context, membersService *directory.MembersService, groupId string, includeDerived bool) ([]*directory.Member, error) {
	var result []*directory.Member
	err := retryNotFound(ctx, func() error {
		result = nil
		return membersService.List(groupId).MaxResults(groupMembersPageSize).IncludeDerivedMembership(includeDerived).Pages(ctx, func(resp *directory.Members) error {
			result = append(result, resp.Members...)
			return nil
		})
	})
	return result, err
}

// groupMembersSource reads the members of one group for a consistency wait.
type groupMembersSource struct {
	// list returns all members of the group.
	list func(ctx context.Context) ([]*directory.Member, error)
	// get returns the member with an ID or email, or nil if the API returns
	// 404. If get is nil, the wait uses only list.
	get func(ctx context.Context, memberKey string) (*directory.Member, error)
}

// newGroupMembersSource returns a groupMembersSource for the direct members
// of a group.
func newGroupMembersSource(membersService *directory.MembersService, groupId string) groupMembersSource {
	return groupMembersSource{
		list: func(ctx context.Context) ([]*directory.Member, error) {
			return listGroupMembers(ctx, membersService, groupId, false)
		},
		get: func(ctx context.Context, memberKey string) (*directory.Member, error) {
			m, err := membersService.Get(groupId, memberKey).Context(ctx).Do()
			if isNotFound(err) {
				return nil, nil
			}
			return m, err
		},
	}
}

// getExpectedMembers gets each member that an expectation names. The result
// has the same use as a listing for pendingGroupMembers.
func getExpectedMembers(ctx context.Context, get func(context.Context, string) (*directory.Member, error), expected []groupMemberExpectation) ([]*directory.Member, error) {
	var found []*directory.Member
	for _, e := range expected {
		key := e.id
		if key == "" {
			key = e.email
		}
		if key == "" {
			continue
		}
		m, err := get(ctx, key)
		if err != nil {
			return nil, err
		}
		if m != nil {
			found = append(found, m)
		}
	}
	return found, nil
}

// insertGroupMember inserts a member and returns the member with its ID.
//
// If the member already exists (409 "Member already exists"), for example
// because an earlier apply inserted it and then failed, or because another
// declared address (an alias) names it, the function adopts the existing
// member: it gets the member, and updates its role and delivery settings when
// they differ from the request. The configuration wins, also when it lowers
// the role. The returned warning names the adopted member and the changes;
// destroying the resource then removes the member. If the API returns no
// member ID, the function gets the member by email.
func insertGroupMember(ctx context.Context, membersService *directory.MembersService, groupId string, memberObj *directory.Member) (*directory.Member, diag.Diagnostics, error) {
	member, err := membersService.Insert(groupId, memberObj).Context(ctx).Do()
	existed := isMemberAlreadyExists(err)
	if existed {
		log.Printf("[WARN] Member %s already exists in group %s. Terraform adopts it, and destroying the resource removes it.", memberObj.Email, groupId)
		member, err = &directory.Member{}, nil
	}
	if err != nil {
		return nil, nil, err
	}

	if member.Id == "" {
		err = retryNotFound(ctx, func() (e error) {
			member, e = membersService.Get(groupId, memberObj.Email).Context(ctx).Do()
			return
		})
		if err != nil {
			return nil, nil, fmt.Errorf("getting member %s of group %s after the insert: %w", memberObj.Email, groupId, err)
		}
	}
	if !existed {
		return member, nil, nil
	}

	var changes []string
	if !strings.EqualFold(member.Role, memberObj.Role) {
		change := fmt.Sprintf("its role changed from %s to %s", member.Role, memberObj.Role)
		if memberRoleRank[strings.ToUpper(member.Role)] > memberRoleRank[strings.ToUpper(memberObj.Role)] {
			change += " (the configured role is lower than the existing role)"
		}
		changes = append(changes, change)
	}
	if memberObj.DeliverySettings != "" && !strings.EqualFold(member.DeliverySettings, memberObj.DeliverySettings) {
		changes = append(changes, fmt.Sprintf("its delivery settings changed from %s to %s", member.DeliverySettings, memberObj.DeliverySettings))
	}
	detail := fmt.Sprintf("Member %s (ID %s) already existed in group %s, and Terraform now manages it.", memberObj.Email, member.Id, groupId)
	if len(changes) > 0 {
		log.Printf("[DEBUG] Updating adopted member %s of group %s: %s", memberObj.Email, groupId, strings.Join(changes, ", "))
		member, err = membersService.Update(groupId, member.Id, memberObj).Context(ctx).Do()
		if err != nil {
			return nil, nil, fmt.Errorf("updating adopted member %s of group %s: %w", memberObj.Email, groupId, err)
		}
		detail += " To match the configuration, " + strings.Join(changes, " and ") + "."
	}
	detail += " Destroying this resource removes the member from the group."
	return member, diag.Diagnostics{{
		Severity: diag.Warning,
		Summary:  fmt.Sprintf("Adopted existing member %s of group %s", memberObj.Email, groupId),
		Detail:   detail,
	}}, nil
}

// isRetryableForbidden reports whether err is a 403 that the retry predicates
// treat as transient: a quota error, or the "Not Authorized to access this
// resource/api" that the API can return right after a write. Such a 403 does
// not tell that the group is out of reach, so it must not plan a replacement.
func isRetryableForbidden(err error) bool {
	gerr, ok := errwrap.GetType(err, &googleapi.Error{}).(*googleapi.Error)
	if !ok || gerr == nil {
		return false
	}
	common, _ := isCommonRetryableErrorCode(gerr)
	quota, _ := isRateLimitExceeded(gerr)
	return common || quota
}

// memberRoleRank orders the member roles from the lowest to the highest.
var memberRoleRank = map[string]int{"MEMBER": 1, "MANAGER": 2, "OWNER": 3}

// isMemberAlreadyExists reports whether err is the 409 that an insert of a
// member that is already in the group returns. Another 409 is an error.
func isMemberAlreadyExists(err error) bool {
	if !isApiErrorWithCode(err, http.StatusConflict) {
		return false
	}
	gerr, ok := errwrap.GetType(err, &googleapi.Error{}).(*googleapi.Error)
	if !ok || gerr == nil {
		return false
	}
	if strings.Contains(strings.ToLower(gerr.Message), "already exists") {
		return true
	}
	for _, item := range gerr.Errors {
		if item.Reason == "duplicate" || strings.Contains(strings.ToLower(item.Message), "already exists") {
			return true
		}
	}
	return false
}

// pendingGroupMembers returns a description of each expectation that the
// listing does not satisfy. Listed members that no expectation names are
// ignored: Read records them as drift.
func pendingGroupMembers(expected []groupMemberExpectation, listed []*directory.Member) []string {
	byID := make(map[string]*directory.Member, len(listed))
	byEmail := make(map[string]*directory.Member, len(listed))
	for _, m := range listed {
		if m.Id != "" {
			byID[m.Id] = m
		}
		// A CUSTOMER member has no email. It can only be found by ID.
		if m.Email != "" {
			byEmail[strings.ToLower(m.Email)] = m
		}
	}

	// A member that one write removes and another write adds again (for
	// example, a switch from an alias to the primary address) must be listed.
	present := map[string]bool{}
	for _, e := range expected {
		if e.absent {
			continue
		}
		if e.id != "" {
			present[e.id] = true
		}
		if e.email != "" {
			present[strings.ToLower(e.email)] = true
		}
	}

	var pending []string
	for _, e := range expected {
		var m *directory.Member
		switch {
		case e.id != "":
			m = byID[e.id]
		case e.email != "":
			m = byEmail[strings.ToLower(e.email)]
		default:
			continue
		}

		switch {
		case e.absent:
			readded := (e.id != "" && present[e.id]) || (e.email != "" && present[strings.ToLower(e.email)])
			if m != nil && !readded {
				pending = append(pending, fmt.Sprintf("%s (still listed)", e.name()))
			}
		case m == nil:
			pending = append(pending, fmt.Sprintf("%s (not listed)", e.name()))
		case e.role != "" && !strings.EqualFold(m.Role, e.role):
			pending = append(pending, fmt.Sprintf("%s (listed with role %s, expected %s)", e.name(), m.Role, e.role))
		}
	}
	return pending
}

// waitForGroupMembers reads the group until numConsistent checks in a row
// satisfy every expectation.
//
// Member inserts, updates and deletes are eventually consistent: a listing
// made directly after them can omit a new member, show an old role, or still
// show a deleted member. The wait checks only what the operation wrote, so
// members that the configuration does not declare do not stop it.
//
// The first check lists the group. If the group has more than one page of
// members and there are at most groupMembersGetLimit expectations, the next
// checks get each expected member. The last check before the wait ends is
// always a listing, because Read records the listing. The time between
// checks starts at pollInterval and doubles up to ten times pollInterval.
//
// If timeout or ctx expires first, the function returns a
// *groupMembersNotConsistentError. If a read fails, it returns that error.
func waitForGroupMembers(ctx context.Context, timeout, pollInterval time.Duration, expected []groupMemberExpectation, src groupMembersSource) error {
	if len(expected) == 0 {
		return nil
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pending := make([]string, 0, len(expected))
	for _, e := range expected {
		pending = append(pending, fmt.Sprintf("%s (not checked)", e.name()))
	}

	// unconfirmed is the result when the wait ends before a listing showed
	// what the gets showed.
	unconfirmed := func() error {
		if len(pending) == 0 {
			for _, e := range expected {
				pending = append(pending, fmt.Sprintf("%s (not confirmed by a listing)", e.name()))
			}
		}
		return &groupMembersNotConsistentError{pending: pending}
	}

	consistent := 0
	useGet := false
	interval := pollInterval
	for {
		full := !useGet || consistent == numConsistent-1
		var listed []*directory.Member
		var err error
		if full {
			listed, err = src.list(waitCtx)
		} else {
			listed, err = getExpectedMembers(waitCtx, src.get, expected)
		}
		if err != nil {
			if waitCtx.Err() != nil {
				return unconfirmed()
			}
			return fmt.Errorf("reading group members: %w", err)
		}
		if full && src.get != nil {
			useGet = len(expected) <= groupMembersGetLimit && len(listed) > groupMembersPageSize
		}

		pending = pendingGroupMembers(expected, listed)
		if len(pending) > 0 {
			consistent = 0
		} else if consistent++; consistent >= numConsistent {
			return nil
		}

		timer := time.NewTimer(interval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			if full && len(pending) == 0 {
				// The last check was a listing, and it satisfied every
				// expectation. Read records the same view.
				return nil
			}
			return unconfirmed()
		case <-timer.C:
		}
		if interval *= 2; interval > 10*pollInterval {
			interval = 10 * pollInterval
		}
	}
}

// consistencyWaitBudget returns how long a consistency wait can run: at most
// max, and never into the last consistencyReadReserve before the ctx deadline.
// When less than two reserves are left, the wait gets half of the time left.
func consistencyWaitBudget(ctx context.Context, max time.Duration) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return max
	}
	left := time.Until(deadline)
	switch {
	case left <= 0:
		return 0
	case left < 2*consistencyReadReserve:
		left /= 2
	default:
		left -= consistencyReadReserve
	}
	if left < max {
		return left
	}
	return max
}

// isConsistencyTimeout reports whether err tells that a consistency wait ran
// out of time, as opposed to an API error. A context.DeadlineExceeded counts
// only when ctx expired; otherwise it comes from a request and is an error.
func isConsistencyTimeout(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	var notConsistent *groupMembersNotConsistentError
	var timeout *resource.TimeoutError
	return errors.As(err, &notConsistent) ||
		errors.As(err, &timeout) ||
		(errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil) ||
		IsNotConsistent(err)
}

// caseInsensitiveNote ends the description of an attribute that ignores a
// change of case. Data sources remove it, because they have no diff.
const caseInsensitiveNote = " The value is compared without regard to case."

// groupIDNote ends the description of a group_id that uses
// forceNewIfOtherGroup. Data sources remove it.
const groupIDNote = " A change replaces the resource only when the new value names another group." + caseInsensitiveNote

// forceNewIfOtherGroup is a CustomizeDiff function for a resource with a
// group_id. A change of group_id replaces the resource only when the new
// value names another group. group_id can be the group's email, an alias or
// its ID, so two different values can name the same group; then the change
// updates state in place. An unknown value, a group that does not exist
// (404) or a group that the credentials cannot read (403) replaces the
// resource; a 403 is logged as a warning, because CustomizeDiff cannot
// return a warning diagnostic. Another lookup error fails the plan.
func forceNewIfOtherGroup(ctx context.Context, d *schema.ResourceDiff, meta interface{}) error {
	if d.Id() == "" || !d.HasChange("group_id") {
		return nil
	}
	if !d.NewValueKnown("group_id") {
		return d.ForceNew("group_id")
	}

	// HasChange can see the configured value even when emailDiffSuppress
	// removed a change of case from the diff.
	o, n := d.GetChange("group_id")
	if strings.EqualFold(o.(string), n.(string)) {
		return nil
	}
	oldID, err := groupUniqueID(ctx, meta, o.(string))
	if err != nil {
		return err
	}
	newID, err := groupUniqueID(ctx, meta, n.(string))
	if err != nil {
		return err
	}
	if oldID == "" || newID == "" || oldID != newID {
		return d.ForceNew("group_id")
	}
	log.Printf("[DEBUG] group_id %q and %q name the same group %s", o, n, oldID)
	return nil
}

// groupUniqueID returns the unique ID of the group that key names, or "" if
// the group does not exist.
func groupUniqueID(ctx context.Context, meta interface{}, key string) (string, error) {
	client, ok := meta.(*apiClient)
	if !ok || client == nil {
		return "", fmt.Errorf("checking whether group_id %q names the same group: the provider is not configured", key)
	}
	directoryService, diags := client.NewDirectoryService()
	if diags.HasError() {
		return "", fmt.Errorf("checking whether group_id %q names the same group: cannot create the Directory service", key)
	}
	group, err := directoryService.Groups.Get(key).Fields("id").Context(ctx).Do()
	if isNotFound(err) {
		return "", nil
	}
	if isApiErrorWithCode(err, http.StatusForbidden) && !isRetryableForbidden(err) {
		log.Printf("[WARN] Cannot read group %q to check whether group_id names the same group (%s); planning a replacement", key, err)
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("checking whether group_id %q names the same group: %w", key, err)
	}
	return group.Id, nil
}

// consistencyWarning is the diagnostic for a write that succeeded but that
// the API did not show as consistent in time.
func consistencyWarning(resourceType string, err error) diag.Diagnostic {
	return diag.Diagnostic{
		Severity: diag.Warning,
		Summary:  fmt.Sprintf("%s changes are not yet visible in the Directory API", resourceType),
		Detail: fmt.Sprintf("The changes were written, but the API did not show all of them before the wait ended: %s. "+
			"Terraform records what the API returns now. The next refresh reads the membership again.", err),
	}
}
