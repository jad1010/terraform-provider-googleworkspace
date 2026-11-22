// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// TestUserDelegate_usesUserIdAsGmailUserId verifies that create, read and
// delete address the mailbox of user_id and never "me".
func TestUserDelegate_usesUserIdAsGmailUserId(t *testing.T) {
	const userId = "user@example.com"
	const delegateEmail = "delegate@example.com"

	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if strings.Contains(r.URL.Path, "/users/me/") {
			t.Errorf("%s %s uses the user ID \"me\", want %q", r.Method, r.URL.Path, userId)
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(gmail.Delegate{DelegateEmail: delegateEmail, VerificationStatus: "accepted"})
	}))
	defer server.Close()

	previous := newGmailUserDelegateService
	t.Cleanup(func() { newGmailUserDelegateService = previous })
	newGmailUserDelegateService = func(ctx context.Context, _ *apiClient, user string) (*gmail.Service, diag.Diagnostics) {
		if user != userId {
			t.Errorf("Gmail service impersonates %q, want %q", user, userId)
		}
		svc, err := gmail.NewService(ctx, option.WithEndpoint(server.URL+"/"), option.WithHTTPClient(server.Client()))
		if err != nil {
			return nil, diag.FromErr(err)
		}
		return svc, nil
	}

	ctx := context.Background()
	d := schema.TestResourceDataRaw(t, resourceUserDelegate().Schema, map[string]interface{}{
		"user_id":        userId,
		"delegate_email": delegateEmail,
	})
	steps := []struct {
		name string
		op   func(context.Context, *schema.ResourceData, interface{}) diag.Diagnostics
	}{
		{"create", resourceUserDelegateCreate},
		{"read", resourceUserDelegateRead},
		{"delete", resourceUserDelegateDelete},
	}
	for _, step := range steps {
		if diags := step.op(ctx, d, &apiClient{}); diags.HasError() {
			t.Fatalf("%s: unexpected error: %v", step.name, diags)
		}
	}

	collection := "/gmail/v1/users/" + userId + "/settings/delegates"
	delegate := collection + "/" + delegateEmail
	want := []string{
		"POST " + collection, "GET " + delegate, // create, then read
		"GET " + delegate,    // read
		"DELETE " + delegate, // delete
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("Gmail requests:\n%s\nwant:\n%s", strings.Join(requests, "\n"), strings.Join(want, "\n"))
	}
}
