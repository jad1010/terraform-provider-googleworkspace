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

// TestGmailSendAsAlias_usesPrimaryEmailAsUserId verifies that create, read,
// update and delete address the mailbox of primary_email and never "me".
// With "me", Gmail acts on the mailbox of the credentials instead.
func TestGmailSendAsAlias_usesPrimaryEmailAsUserId(t *testing.T) {
	const primaryEmail = "user@example.com"
	const sendAsEmail = "alias@example.com"

	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if strings.Contains(r.URL.Path, "/users/me/") {
			t.Errorf("%s %s uses the user ID \"me\", want %q", r.Method, r.URL.Path, primaryEmail)
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(gmail.SendAs{SendAsEmail: sendAsEmail, TreatAsAlias: true})
	}))
	defer server.Close()

	previous := newGmailSendAsAliasService
	t.Cleanup(func() { newGmailSendAsAliasService = previous })
	newGmailSendAsAliasService = func(ctx context.Context, _ *apiClient, userId string) (*gmail.Service, diag.Diagnostics) {
		if userId != primaryEmail {
			t.Errorf("Gmail service impersonates %q, want %q", userId, primaryEmail)
		}
		svc, err := gmail.NewService(ctx, option.WithEndpoint(server.URL+"/"), option.WithHTTPClient(server.Client()))
		if err != nil {
			return nil, diag.FromErr(err)
		}
		return svc, nil
	}

	ctx := context.Background()
	d := schema.TestResourceDataRaw(t, resourceGmailSendAsAlias().Schema, map[string]interface{}{
		"primary_email": primaryEmail,
		"send_as_email": sendAsEmail,
		"display_name":  "Alias",
	})
	steps := []struct {
		name string
		op   func(context.Context, *schema.ResourceData, interface{}) diag.Diagnostics
	}{
		{"create", resourceGmailSendAsAliasCreate},
		{"read", resourceGmailSendAsAliasRead},
		{"update", resourceGmailSendAsAliasUpdate},
		{"delete", resourceGmailSendAsAliasDelete},
	}
	for _, step := range steps {
		if diags := step.op(ctx, d, &apiClient{}); diags.HasError() {
			t.Fatalf("%s: unexpected error: %v", step.name, diags)
		}
	}

	collection := "/gmail/v1/users/" + primaryEmail + "/settings/sendAs"
	alias := collection + "/" + sendAsEmail
	want := []string{
		"POST " + collection, "GET " + alias, // create, then read
		"GET " + alias,                 // read
		"PUT " + alias, "GET " + alias, // update, then read
		"DELETE " + alias, // delete
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("Gmail requests:\n%s\nwant:\n%s", strings.Join(requests, "\n"), strings.Join(want, "\n"))
	}
}
