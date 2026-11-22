// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"golang.org/x/oauth2"
	googleoauth "golang.org/x/oauth2/google"
	"golang.org/x/oauth2/jwt"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
)

const testServiceAccountEmail = "terraform@example-project.iam.gserviceaccount.com"

var (
	testPrivateKeyOnce sync.Once
	testPrivateKeyPEM  []byte
)

// testPrivateKey returns a generated RSA key in PEM format.
func testPrivateKey(t *testing.T) []byte {
	t.Helper()

	testPrivateKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generating key: %s", err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatalf("encoding key: %s", err)
		}
		testPrivateKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	})
	return testPrivateKeyPEM
}

// newFakeTokenEndpoint returns an OAuth 2.0 token endpoint that rejects each
// token request with the response body, and an IAM signJwt endpoint for the
// impersonate package.
func newFakeTokenEndpoint(t *testing.T, body string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, ":signJwt") {
			_, _ = w.Write([]byte(`{"keyId":"k","signedJwt":"header.payload.signature"}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// gmailRedirectTransport sends each request to the server, whatever its host.
type gmailRedirectTransport struct{ server *url.URL }

func (rt gmailRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = rt.server.Scheme
	req.URL.Host = rt.server.Host
	return http.DefaultTransport.RoundTrip(req)
}

// newFailingGmailEndpoint returns a Gmail endpoint that fails the test: the
// token exchange fails before a Gmail request.
func newFailingGmailEndpoint(t *testing.T) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Gmail request %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestGmailUserSettingsRead_tokenErrors runs a refresh with the real token
// sources of the provider against a token endpoint that rejects the subject.
func TestGmailUserSettingsRead_tokenErrors(t *testing.T) {
	userNotFound := `{"error":"invalid_grant","error_description":"Invalid email or User ID"}`
	responses := map[string]struct {
		body        string
		wantRemoved bool
	}{
		"user does not exist": {userNotFound, true},
		"other invalid_grant": {`{"error":"invalid_grant","error_description":"Invalid JWT: Token must be a short-lived token"}`, false},
		"similar description": {`{"error":"invalid_grant","error_description":"Invalid email or User ID or something else"}`, false},
		"unauthorized client": {`{"error":"unauthorized_client","error_description":"Client is unauthorized to retrieve access tokens using this method"}`, false},
		"not JSON":            {`Bad Request`, false},
	}

	scopes := []string{"https://www.googleapis.com/auth/gmail.settings.basic"}

	// Each token source returns the HTTP client of a Gmail service that
	// impersonates the user.
	sources := map[string]func(t *testing.T, ctx context.Context, tokenURL string) *http.Client{
		"service account key with jwt": func(t *testing.T, ctx context.Context, tokenURL string) *http.Client {
			config := &jwt.Config{
				Email:        testServiceAccountEmail,
				PrivateKey:   testPrivateKey(t),
				PrivateKeyID: "k",
				Subject:      testGmailUserEmail,
				Scopes:       scopes,
				TokenURL:     tokenURL,
			}
			return oauth2.NewClient(ctx, config.TokenSource(ctx))
		},
		"service account key through SetupClient": func(t *testing.T, ctx context.Context, tokenURL string) *http.Client {
			key, err := json.Marshal(map[string]string{
				"type":           "service_account",
				"client_email":   testServiceAccountEmail,
				"client_id":      "1",
				"private_key":    string(testPrivateKey(t)),
				"private_key_id": "k",
				"token_uri":      tokenURL,
			})
			if err != nil {
				t.Fatalf("encoding key: %s", err)
			}
			creds, err := googleoauth.CredentialsFromJSONWithParams(ctx, key, googleoauth.CredentialsParams{
				Scopes:  scopes,
				Subject: testGmailUserEmail,
			})
			if err != nil {
				t.Fatalf("loading credentials: %s", err)
			}
			client := &apiClient{}
			if diags := client.SetupClient(ctx, creds); diags.HasError() {
				t.Fatalf("setting up client: %v", diags)
			}
			return client.client
		},
		"access token with impersonate through SetupClient": func(t *testing.T, ctx context.Context, tokenURL string) *http.Client {
			server, err := url.Parse(tokenURL)
			if err != nil {
				t.Fatalf("parsing URL: %s", err)
			}
			tokenSource, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
				TargetPrincipal: testServiceAccountEmail,
				Scopes:          scopes,
				Subject:         testGmailUserEmail,
			}, option.WithHTTPClient(&http.Client{Transport: gmailRedirectTransport{server: server}}))
			if err != nil {
				t.Fatalf("creating token source: %s", err)
			}
			client := &apiClient{}
			if diags := client.SetupClient(ctx, &googleoauth.Credentials{TokenSource: tokenSource}); diags.HasError() {
				t.Fatalf("setting up client: %v", diags)
			}
			return client.client
		},
	}

	for sourceName, source := range sources {
		for responseName, response := range responses {
			t.Run(sourceName+"/"+responseName, func(t *testing.T) {
				newFakeGmail(t)
				r := resourceGmailUserSettings()
				state := gmailUserSettingsApply(t, r, nil, map[string]interface{}{
					"user_email": testGmailUserEmail,
					"pop":        []interface{}{map[string]interface{}{"access_window": "disabled"}},
				})

				tokenEndpoint := newFakeTokenEndpoint(t, response.body)
				gmailEndpoint := newFailingGmailEndpoint(t)
				newGmailUserSettingsService = func(ctx context.Context, _ *apiClient, _ string) (*gmail.Service, diag.Diagnostics) {
					client := source(t, ctx, tokenEndpoint.URL+"/token")
					svc, err := gmail.NewService(ctx, option.WithEndpoint(gmailEndpoint.URL+"/"), option.WithHTTPClient(client))
					return svc, diag.FromErr(err)
				}

				newState, diags := r.RefreshWithoutUpgrade(context.Background(), state, &apiClient{})
				if response.wantRemoved {
					if diags.HasError() {
						t.Fatalf("unexpected refresh error: %v", diags)
					}
					if len(diags) != 1 || diags[0].Severity != diag.Warning {
						t.Fatalf("expected one warning, got %v", diags)
					}
					if newState != nil {
						t.Fatalf("expected the resource to be removed from state, got %v", newState)
					}
					return
				}
				if !diags.HasError() {
					t.Fatalf("expected a refresh error, got state %v", newState)
				}
				if newState == nil || newState.ID != testGmailUserEmail {
					t.Fatalf("expected the resource to stay in state, got %v", newState)
				}
			})
		}
	}
}
