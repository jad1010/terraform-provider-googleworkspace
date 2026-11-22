// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	directory "google.golang.org/api/admin/directory/v1"
)

func TestPendingGroupMembers(t *testing.T) {
	t.Parallel()

	listed := []*directory.Member{
		{Email: "alice@example.com", Id: "A", Role: "OWNER"},
		{Email: "bob@example.com", Id: "B", Role: "MEMBER"},
		{Email: "primary@example.com", Id: "P", Role: "MEMBER"},
		{Email: "extra@example.com", Id: "X", Role: "MEMBER"},
		{Email: "", Id: "C0123", Role: "MEMBER", Type: "CUSTOMER"},
	}

	tests := map[string]struct {
		expected []groupMemberExpectation
		want     []string
	}{
		"listed extra and CUSTOMER members are ignored": {
			expected: []groupMemberExpectation{{email: "alice@example.com", role: "OWNER"}},
		},
		"mixed-case email matches": {
			expected: []groupMemberExpectation{{email: "Alice@Example.COM", role: "OWNER"}},
		},
		"role compared without regard to case": {
			expected: []groupMemberExpectation{{email: "alice@example.com", role: "owner"}},
		},
		"member not listed": {
			expected: []groupMemberExpectation{{email: "carol@example.com", role: "MEMBER"}},
			want:     []string{"carol@example.com (not listed)"},
		},
		"role not yet updated": {
			expected: []groupMemberExpectation{{email: "bob@example.com", id: "B", role: "OWNER"}},
			want:     []string{"bob@example.com (listed with role MEMBER, expected OWNER)"},
		},
		"deleted member still listed": {
			expected: []groupMemberExpectation{{email: "bob@example.com", id: "B", absent: true}},
			want:     []string{"bob@example.com (still listed)"},
		},
		"deleted member gone": {
			expected: []groupMemberExpectation{{email: "dave@example.com", id: "D", absent: true}},
		},
		"alias matched by member ID": {
			expected: []groupMemberExpectation{{email: "alias@example.com", id: "P", role: "MEMBER"}},
		},
		"ID takes precedence over email": {
			expected: []groupMemberExpectation{{email: "alice@example.com", id: "NEW", role: "OWNER"}},
			want:     []string{"alice@example.com (not listed)"},
		},
		"expectation without email or ID is skipped": {
			expected: []groupMemberExpectation{{email: "", id: "", role: "MEMBER"}},
		},
		"CUSTOMER member matched by ID": {
			expected: []groupMemberExpectation{{id: "C0123", role: "OWNER"}},
			want:     []string{"member ID C0123 (listed with role MEMBER, expected OWNER)"},
		},
		"member deleted and added again must be listed": {
			expected: []groupMemberExpectation{
				{email: "alias@example.com", id: "P", absent: true},
				{email: "primary@example.com", id: "P", role: "MEMBER"},
			},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got := pendingGroupMembers(test.expected, listed)
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("pendingGroupMembers() = %q, want %q", got, test.want)
			}
		})
	}
}

// listSequence returns a list function that returns the snapshots in order
// and then repeats the last one. It counts the calls.
func listSequence(calls *int, snapshots ...[]*directory.Member) func(context.Context) ([]*directory.Member, error) {
	return func(context.Context) ([]*directory.Member, error) {
		i := *calls
		if i >= len(snapshots) {
			i = len(snapshots) - 1
		}
		*calls++
		return snapshots[i], nil
	}
}

func TestWaitForGroupMembers_convergesOnWrittenMembersOnly(t *testing.T) {
	t.Parallel()

	expected := []groupMemberExpectation{
		{email: "Alice@Example.com", role: "OWNER"},
		{email: "bob@example.com", id: "B", absent: true},
	}
	extra := &directory.Member{Email: "extra@example.com", Id: "X", Role: "MEMBER"}
	calls := 0
	list := listSequence(&calls,
		// Alice not yet listed, Bob still listed.
		[]*directory.Member{extra, {Email: "bob@example.com", Id: "B", Role: "MEMBER"}},
		// Alice listed with her old role.
		[]*directory.Member{extra, {Email: "alice@example.com", Id: "A", Role: "MEMBER"}},
		// Consistent, with a member that the configuration does not declare.
		[]*directory.Member{extra, {Email: "alice@example.com", Id: "A", Role: "OWNER"}},
	)

	if err := waitForGroupMembers(context.Background(), time.Minute, time.Millisecond, expected, groupMembersSource{list: list}); err != nil {
		t.Fatalf("waitForGroupMembers() error = %v", err)
	}
	// Two stale listings, then numConsistent consistent listings.
	if want := 2 + numConsistent; calls != want {
		t.Errorf("list calls = %d, want %d", calls, want)
	}
}

func TestWaitForGroupMembers_inconsistentListingResetsCount(t *testing.T) {
	t.Parallel()

	expected := []groupMemberExpectation{{email: "alice@example.com", role: "OWNER"}}
	ok := []*directory.Member{{Email: "alice@example.com", Role: "OWNER"}}
	calls := 0
	list := listSequence(&calls, ok, nil, ok)

	if err := waitForGroupMembers(context.Background(), time.Minute, time.Millisecond, expected, groupMembersSource{list: list}); err != nil {
		t.Fatalf("waitForGroupMembers() error = %v", err)
	}
	if want := 2 + numConsistent; calls != want {
		t.Errorf("list calls = %d, want %d", calls, want)
	}
}

func TestWaitForGroupMembers_noExpectationsDoesNotList(t *testing.T) {
	t.Parallel()

	calls := 0
	if err := waitForGroupMembers(context.Background(), time.Minute, time.Millisecond, nil, groupMembersSource{list: listSequence(&calls, nil)}); err != nil {
		t.Fatalf("waitForGroupMembers() error = %v", err)
	}
	if calls != 0 {
		t.Errorf("list calls = %d, want 0", calls)
	}
}

func TestWaitForGroupMembers_timeoutNamesPendingMembers(t *testing.T) {
	t.Parallel()

	expected := []groupMemberExpectation{
		{email: "alice@example.com", role: "OWNER"},
		{email: "bob@example.com", id: "B", absent: true},
		{email: "carol@example.com", id: "C", role: "MANAGER"},
	}
	calls := 0
	list := listSequence(&calls, []*directory.Member{
		{Email: "bob@example.com", Id: "B", Role: "MEMBER"},
		{Email: "carol@example.com", Id: "C", Role: "MEMBER"},
	})

	err := waitForGroupMembers(context.Background(), 50*time.Millisecond, time.Millisecond, expected, groupMembersSource{list: list})

	var notConsistent *groupMembersNotConsistentError
	if !errors.As(err, &notConsistent) {
		t.Fatalf("waitForGroupMembers() error = %v, want *groupMembersNotConsistentError", err)
	}
	want := []string{
		"alice@example.com (not listed)",
		"bob@example.com (still listed)",
		"carol@example.com (listed with role MEMBER, expected MANAGER)",
	}
	if !reflect.DeepEqual(notConsistent.pending, want) {
		t.Errorf("pending = %q, want %q", notConsistent.pending, want)
	}
	if !isConsistencyTimeout(context.Background(), err) || !IsNotConsistent(err) {
		t.Errorf("error %v is not reported as a consistency timeout", err)
	}
	if calls < 2 {
		t.Errorf("list calls = %d, want at least 2", calls)
	}
}

func TestWaitForGroupMembers_listErrorFailsAtOnce(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	calls := 0
	list := func(context.Context) ([]*directory.Member, error) {
		calls++
		return nil, boom
	}

	err := waitForGroupMembers(context.Background(), time.Minute, time.Millisecond,
		[]groupMemberExpectation{{email: "alice@example.com"}}, groupMembersSource{list: list})
	if !errors.Is(err, boom) {
		t.Fatalf("waitForGroupMembers() error = %v, want it to wrap %v", err, boom)
	}
	if isConsistencyTimeout(context.Background(), err) {
		t.Errorf("listing error %v is reported as a consistency timeout", err)
	}
	if calls != 1 {
		t.Errorf("list calls = %d, want 1", calls)
	}
}

func TestWaitForGroupMembers_contextExpiryIsATimeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// The listing blocks until the context expires, like an HTTP request that
	// is still running at the deadline.
	list := func(ctx context.Context) ([]*directory.Member, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	err := waitForGroupMembers(ctx, time.Minute, time.Millisecond,
		[]groupMemberExpectation{{email: "alice@example.com"}}, groupMembersSource{list: list})

	var notConsistent *groupMembersNotConsistentError
	if !errors.As(err, &notConsistent) {
		t.Fatalf("waitForGroupMembers() error = %v, want *groupMembersNotConsistentError", err)
	}
	if strings.Contains(err.Error(), "unexpected") {
		t.Errorf("error %q reports an unexpected error", err)
	}
	if want := []string{"alice@example.com (not checked)"}; !reflect.DeepEqual(notConsistent.pending, want) {
		t.Errorf("pending = %q, want %q", notConsistent.pending, want)
	}
}

func TestConsistencyWaitBudget(t *testing.T) {
	t.Parallel()

	if got := consistencyWaitBudget(context.Background(), time.Minute); got != time.Minute {
		t.Errorf("no deadline: budget = %s, want %s", got, time.Minute)
	}

	tests := map[string]struct {
		left     time.Duration
		max      time.Duration
		min, top time.Duration
	}{
		"distant deadline gives max":           {left: 20 * time.Minute, max: 5 * time.Minute, min: 5 * time.Minute, top: 5 * time.Minute},
		"deadline leaves the reserve for Read": {left: consistencyReadReserve + 2*time.Minute, max: 5 * time.Minute, min: 110 * time.Second, top: 2 * time.Minute},
		"short deadline gives half":            {left: consistencyReadReserve, max: 5 * time.Minute, min: 25 * time.Second, top: consistencyReadReserve / 2},
		"very short deadline gives half":       {left: 2 * time.Second, max: 5 * time.Minute, min: 500 * time.Millisecond, top: time.Second},
		"half is still limited by max":         {left: consistencyReadReserve, max: time.Second, min: time.Second, top: time.Second},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), test.left)
			defer cancel()
			if got := consistencyWaitBudget(ctx, test.max); got < test.min || got > test.top {
				t.Errorf("budget = %s, want %s to %s", got, test.min, test.top)
			}
		})
	}

	expired, cancel := context.WithTimeout(context.Background(), -time.Second)
	defer cancel()
	if got := consistencyWaitBudget(expired, time.Minute); got != 0 {
		t.Errorf("expired deadline: budget = %s, want 0", got)
	}
}

func TestIsConsistencyTimeout_deadlineExceededNeedsExpiredContext(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("unexpected error during retries of group_member: %w", context.DeadlineExceeded)
	if isConsistencyTimeout(context.Background(), err) {
		t.Errorf("a request deadline with a live context is reported as a consistency timeout")
	}
	ctx, cancel := context.WithTimeout(context.Background(), -time.Second)
	defer cancel()
	if !isConsistencyTimeout(ctx, err) {
		t.Errorf("a deadline with an expired context is not reported as a consistency timeout")
	}
}

func TestWaitForGroupMembers_backsOffBetweenChecks(t *testing.T) {
	t.Parallel()

	const poll = 10 * time.Millisecond
	var times []time.Time
	list := func(context.Context) ([]*directory.Member, error) {
		times = append(times, time.Now())
		return nil, nil
	}

	_ = waitForGroupMembers(context.Background(), 650*time.Millisecond, poll,
		[]groupMemberExpectation{{email: "alice@example.com"}}, groupMembersSource{list: list})

	// Intervals: 10, 20, 40, 80, then 100 (the cap of ten times poll).
	want := []time.Duration{poll, 2 * poll, 4 * poll, 8 * poll, 10 * poll, 10 * poll}
	if len(times) < len(want)+1 {
		t.Fatalf("checks = %d, want at least %d", len(times), len(want)+1)
	}
	for i, w := range want {
		gap := times[i+1].Sub(times[i])
		if gap < w || gap > w+150*time.Millisecond {
			t.Errorf("interval %d = %s, want %s", i, gap, w)
		}
	}
}

// bigGroup returns more than one listing page of members, with the given
// members first.
func bigGroup(members ...*directory.Member) []*directory.Member {
	for i := 0; i <= groupMembersPageSize; i++ {
		members = append(members, &directory.Member{Email: fmt.Sprintf("user%d@example.com", i), Id: fmt.Sprintf("U%d", i), Role: "MEMBER"})
	}
	return members
}

func TestWaitForGroupMembers_largeGroupChecksExpectedMembersWithGet(t *testing.T) {
	t.Parallel()

	alice := &directory.Member{Email: "alice@example.com", Id: "A", Role: "OWNER"}
	expected := []groupMemberExpectation{{email: "alice@example.com", id: "A", role: "OWNER"}}
	lists := 0
	list := listSequence(&lists,
		bigGroup(),      // Alice not yet listed.
		bigGroup(),      // The confirmation listing is still stale.
		bigGroup(alice), // Consistent.
	)
	var gets []string
	get := func(_ context.Context, key string) (*directory.Member, error) {
		gets = append(gets, key)
		return alice, nil
	}

	if err := waitForGroupMembers(context.Background(), time.Minute, time.Millisecond, expected, groupMembersSource{list: list, get: get}); err != nil {
		t.Fatalf("waitForGroupMembers() error = %v", err)
	}
	// list, get, list (stale), get, list.
	if lists != 3 {
		t.Errorf("list calls = %d, want 3", lists)
	}
	if want := []string{"A", "A"}; !reflect.DeepEqual(gets, want) {
		t.Errorf("gets = %q, want %q", gets, want)
	}
}

func TestWaitForGroupMembers_smallGroupDoesNotUseGet(t *testing.T) {
	t.Parallel()

	expected := []groupMemberExpectation{{email: "alice@example.com", role: "OWNER"}}
	lists := 0
	list := listSequence(&lists, nil, []*directory.Member{{Email: "alice@example.com", Role: "OWNER"}})
	gets := 0
	get := func(context.Context, string) (*directory.Member, error) {
		gets++
		return nil, nil
	}

	if err := waitForGroupMembers(context.Background(), time.Minute, time.Millisecond, expected, groupMembersSource{list: list, get: get}); err != nil {
		t.Fatalf("waitForGroupMembers() error = %v", err)
	}
	if gets != 0 || lists != 1+numConsistent {
		t.Errorf("gets, lists = %d, %d; want 0, %d", gets, lists, 1+numConsistent)
	}
}

func TestWaitForGroupMembers_manyExpectationsDoNotUseGet(t *testing.T) {
	t.Parallel()

	var expected []groupMemberExpectation
	var listedMembers []*directory.Member
	for i := 0; i <= groupMembersGetLimit; i++ {
		email := fmt.Sprintf("new%d@example.com", i)
		expected = append(expected, groupMemberExpectation{email: email})
		listedMembers = append(listedMembers, &directory.Member{Email: email})
	}
	lists := 0
	list := listSequence(&lists, bigGroup(), bigGroup(listedMembers...))
	gets := 0
	get := func(context.Context, string) (*directory.Member, error) {
		gets++
		return nil, nil
	}

	if err := waitForGroupMembers(context.Background(), time.Minute, time.Millisecond, expected, groupMembersSource{list: list, get: get}); err != nil {
		t.Fatalf("waitForGroupMembers() error = %v", err)
	}
	if gets != 0 {
		t.Errorf("gets = %d, want 0 for more than %d expectations", gets, groupMembersGetLimit)
	}
}

// The wait must not end on a check made with gets: Read records a listing,
// which can still be stale.
func TestWaitForGroupMembers_getResultNeedsListingConfirmation(t *testing.T) {
	t.Parallel()

	alice := &directory.Member{Email: "alice@example.com", Id: "A", Role: "OWNER"}
	expected := []groupMemberExpectation{{email: "alice@example.com", id: "A", role: "OWNER"}}
	get := func(context.Context, string) (*directory.Member, error) { return alice, nil }
	want := []string{"alice@example.com (not confirmed by a listing)"}

	t.Run("deadline during the wait after a get", func(t *testing.T) {
		t.Parallel()
		lists := 0
		list := listSequence(&lists, bigGroup())
		// list at 0, get at 30ms, deadline during the next 60ms interval.
		err := waitForGroupMembers(context.Background(), 60*time.Millisecond, 30*time.Millisecond, expected,
			groupMembersSource{list: list, get: get})
		var notConsistent *groupMembersNotConsistentError
		if !errors.As(err, &notConsistent) {
			t.Fatalf("waitForGroupMembers() error = %v, want *groupMembersNotConsistentError", err)
		}
		if !reflect.DeepEqual(notConsistent.pending, want) {
			t.Errorf("pending = %q, want %q", notConsistent.pending, want)
		}
	})

	t.Run("deadline during the confirmation listing", func(t *testing.T) {
		t.Parallel()
		lists := 0
		list := func(ctx context.Context) ([]*directory.Member, error) {
			lists++
			if lists == 1 {
				return bigGroup(), nil
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		err := waitForGroupMembers(context.Background(), 100*time.Millisecond, time.Millisecond, expected,
			groupMembersSource{list: list, get: get})
		var notConsistent *groupMembersNotConsistentError
		if !errors.As(err, &notConsistent) {
			t.Fatalf("waitForGroupMembers() error = %v, want *groupMembersNotConsistentError", err)
		}
		if !reflect.DeepEqual(notConsistent.pending, want) {
			t.Errorf("pending = %q, want %q", notConsistent.pending, want)
		}
	})
}
