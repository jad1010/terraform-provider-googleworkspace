// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestConfigLoadAndValidate_universeDomainEnvUnsupported(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_UNIVERSE_DOMAIN", "example-universe.com")

	config := &apiClient{
		AccessToken: "fake-token",
	}

	diags := config.loadAndValidate(context.Background())
	if !diags.HasError() {
		t.Fatalf("expected an error for an unsupported universe domain, but got none")
	}
	if !strings.Contains(diags[0].Summary, "example-universe.com") {
		t.Fatalf("expected the summary to name the universe domain, got %q", diags[0].Summary)
	}
	if !strings.Contains(diags[0].Detail, "GOOGLE_CLOUD_UNIVERSE_DOMAIN") {
		t.Fatalf("expected the detail to name the environment variable, got %q", diags[0].Detail)
	}
}

func TestConfigLoadAndValidate_universeDomainEnvSupported(t *testing.T) {
	for _, universeDomain := range []string{"", "googleapis.com"} {
		t.Run(fmt.Sprintf("value %q", universeDomain), func(t *testing.T) {
			t.Setenv("GOOGLE_CLOUD_UNIVERSE_DOMAIN", universeDomain)

			config := &apiClient{
				AccessToken: "fake-token",
			}

			diags := config.loadAndValidate(context.Background())
			if err := checkDiags(diags); err != nil {
				t.Fatalf("%s", err.Error())
			}
		})
	}
}

// fakeCredentialsWithUniverse returns the fake service account key with
// universe_domain set to universeDomain.
func fakeCredentialsWithUniverse(t *testing.T, universeDomain string) string {
	t.Helper()

	contents, err := ioutil.ReadFile(testFakeCredentialsPath)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var creds map[string]interface{}
	if err := json.Unmarshal(contents, &creds); err != nil {
		t.Fatalf("error: %v", err)
	}
	creds["universe_domain"] = universeDomain
	contents, err = json.Marshal(creds)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	return string(contents)
}

// fakeExternalAccountWithUniverse returns a workload identity federation
// configuration with universe_domain set to universeDomain. Loading it makes
// no network call.
func fakeExternalAccountWithUniverse(universeDomain string) string {
	return fmt.Sprintf(`{
  "type": "external_account",
  "audience": "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/pool/providers/provider",
  "subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
  "token_url": "https://sts.googleapis.com/v1/token",
  "credential_source": {"file": "/nonexistent/token"},
  "universe_domain": %q
}`, universeDomain)
}

func TestConfigLoadAndValidate_credsUniverseDomainUnsupported(t *testing.T) {
	cases := map[string]struct {
		// credentials is the value of the credentials argument. If it is
		// empty, adc is written to a file that GOOGLE_APPLICATION_CREDENTIALS
		// names.
		credentials string
		adc         string
		wantSource  string
	}{
		"credentials argument, service account key": {
			credentials: fakeCredentialsWithUniverse(t, "example-universe.com"),
			wantSource:  "credentials argument",
		},
		"credentials argument, external account": {
			credentials: fakeExternalAccountWithUniverse("example-universe.com"),
			wantSource:  "credentials argument",
		},
		"GOOGLE_APPLICATION_CREDENTIALS, service account key": {
			adc:        fakeCredentialsWithUniverse(t, "example-universe.com"),
			wantSource: "GOOGLE_APPLICATION_CREDENTIALS",
		},
		"GOOGLE_APPLICATION_CREDENTIALS, external account": {
			adc:        fakeExternalAccountWithUniverse("example-universe.com"),
			wantSource: "GOOGLE_APPLICATION_CREDENTIALS",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("GOOGLE_CLOUD_UNIVERSE_DOMAIN", "")
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
			if tc.adc != "" {
				path := t.TempDir() + "/adc.json"
				if err := ioutil.WriteFile(path, []byte(tc.adc), 0o600); err != nil {
					t.Fatalf("error: %v", err)
				}
				t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
			}

			config := &apiClient{
				Credentials:           tc.credentials,
				ImpersonatedUserEmail: "my-fake-email@example.com",
			}

			diags := config.loadAndValidate(context.Background())
			if !diags.HasError() {
				t.Fatalf("expected an error for an unsupported universe domain, but got none")
			}
			detail := diags[0].Detail
			if !strings.Contains(detail, "universe_domain") || !strings.Contains(detail, tc.wantSource) {
				t.Fatalf("expected the detail to name the universe_domain field and %q, got %q", tc.wantSource, detail)
			}
		})
	}
}

func TestConfigLoadAndValidate_userProject(t *testing.T) {
	cases := map[string]struct {
		userProject string
		env         string
		want        string
	}{
		"set":   {userProject: "my-quota-project", want: "my-quota-project"},
		"unset": {userProject: "", want: ""},
		// The client library reads GOOGLE_CLOUD_QUOTA_PROJECT itself, so an
		// empty user_project does not stop the header.
		"empty, environment variable set": {userProject: "", env: "env-quota-project", want: "env-quota-project"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("GOOGLE_CLOUD_UNIVERSE_DOMAIN", "")
			t.Setenv("GOOGLE_CLOUD_QUOTA_PROJECT", tc.env)

			var got string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("X-Goog-User-Project")
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			config := &apiClient{
				AccessToken: "fake-token",
				UserProject: tc.userProject,
			}

			diags := config.loadAndValidate(context.Background())
			if err := checkDiags(diags); err != nil {
				t.Fatalf("%s", err.Error())
			}

			resp, err := config.client.Get(server.URL)
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			resp.Body.Close()

			if got != tc.want {
				t.Fatalf("expected X-Goog-User-Project %q, got %q", tc.want, got)
			}
		})
	}
}

func TestProviderUserProject_envDefault(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_QUOTA_PROJECT", "env-quota-project")

	v, err := New("dev")().Schema["user_project"].DefaultValue()
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if v != "env-quota-project" {
		t.Fatalf("expected user_project default %q, got %v", "env-quota-project", v)
	}
}

// redirectDefaultTransport sends every connection that is made through
// http.DefaultTransport to server, whatever the host name. The Google API
// transports and the default OAuth 2.0 client use http.DefaultTransport.
// It replaces a process-wide value, so a test that calls it must not call
// t.Parallel.
func redirectDefaultTransport(t *testing.T, server *httptest.Server) {
	t.Helper()

	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	serverAddr := server.Listener.Addr().String()
	http.DefaultTransport = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, serverAddr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test server certificate only
	}
}

// pinPlainEndpoints keeps the client library on the default (not mTLS)
// endpoints, whatever the environment of the test run.
func pinPlainEndpoints(t *testing.T) {
	t.Helper()

	t.Setenv("GOOGLE_CLOUD_UNIVERSE_DOMAIN", "")
	t.Setenv("GOOGLE_API_USE_CLIENT_CERTIFICATE", "false")
	t.Setenv("GOOGLE_API_USE_MTLS_ENDPOINT", "never")
}

// TestConfigLoadAndValidate_userProjectImpersonation checks that the quota
// project is sent on the IAM Credentials signJwt request of domain-wide
// delegation, on the OAuth 2.0 token exchange, and on the API requests that
// use the delegated token.
// This test replaces http.DefaultTransport, a process-wide value. It must not
// call t.Parallel. t.Setenv below also prevents that.
func TestConfigLoadAndValidate_userProjectImpersonation(t *testing.T) {
	pinPlainEndpoints(t)
	t.Setenv("GOOGLE_CLOUD_QUOTA_PROJECT", "")

	var mu sync.Mutex
	got := map[string]string{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.Host+r.URL.Path] = r.Header.Get("X-Goog-User-Project")
		mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, ":signJwt"):
			w.Write([]byte(`{"keyId":"fake-key","signedJwt":"fake.signed.jwt"}`))
		case r.URL.Path == "/token":
			w.Write([]byte(`{"access_token":"fake-delegated-token","token_type":"Bearer","expires_in":3600}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	redirectDefaultTransport(t, server)

	serviceAccount := "workspace-admin@my-project.iam.gserviceaccount.com"
	config := &apiClient{
		AccessToken:           "fake-caller-token",
		ServiceAccount:        serviceAccount,
		ImpersonatedUserEmail: "admin@example.com",
		UserProject:           "my-quota-project",
	}

	diags := config.loadAndValidate(context.Background())
	if err := checkDiags(diags); err != nil {
		t.Fatalf("%s", err.Error())
	}

	resp, err := config.client.Get("https://admin.googleapis.com/admin/directory/v1/users")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	for _, key := range []string{
		"iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/" + serviceAccount + ":signJwt",
		"oauth2.googleapis.com/token",
		"admin.googleapis.com/admin/directory/v1/users",
	} {
		value, ok := got[key]
		if !ok {
			t.Fatalf("expected a request to %s, got requests %v", key, got)
		}
		if value != "my-quota-project" {
			t.Fatalf("expected X-Goog-User-Project %q on %s, got %q", "my-quota-project", key, value)
		}
	}
}

// TestConfigLoadAndValidate_credsQuotaProjectIDNotSent checks that the
// provider does not send the quota_project_id field of a credentials file.
// This test replaces http.DefaultTransport, a process-wide value. It must not
// call t.Parallel.
func TestConfigLoadAndValidate_credsQuotaProjectIDNotSent(t *testing.T) {
	pinPlainEndpoints(t)
	t.Setenv("GOOGLE_CLOUD_QUOTA_PROJECT", "")

	var mu sync.Mutex
	got := map[string]string{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.Host+r.URL.Path] = r.Header.Get("X-Goog-User-Project")
		mu.Unlock()

		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"fake-user-token","token_type":"Bearer","expires_in":3600}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	redirectDefaultTransport(t, server)

	config := &apiClient{
		Credentials: `{
  "type": "authorized_user",
  "client_id": "fake-client-id",
  "client_secret": "fake-client-secret",
  "refresh_token": "fake-refresh-token",
  "quota_project_id": "file-quota-project"
}`,
	}

	diags := config.loadAndValidate(context.Background())
	if err := checkDiags(diags); err != nil {
		t.Fatalf("%s", err.Error())
	}

	resp, err := config.client.Get("https://admin.googleapis.com/admin/directory/v1/users")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	value, ok := got["admin.googleapis.com/admin/directory/v1/users"]
	if !ok {
		t.Fatalf("expected an API request, got requests %v", got)
	}
	if value != "" {
		t.Fatalf("expected no X-Goog-User-Project header, got %q", value)
	}
}
