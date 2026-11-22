// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"golang.org/x/oauth2"
	googleoauth "golang.org/x/oauth2/google"

	"google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
)

func TestConfigLoadAndValidate_credsInvalidJSON(t *testing.T) {
	config := &apiClient{
		Credentials:           "{this is not json}",
		ImpersonatedUserEmail: "my-fake-email@example.com",
	}

	diags := config.loadAndValidate(context.Background())
	if !diags.HasError() {
		t.Fatalf("expected error, but got nil")
	}
}

func TestConfigLoadAndValidate_credsJSON(t *testing.T) {
	contents, err := ioutil.ReadFile(testFakeCredentialsPath)
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	config := &apiClient{
		Credentials:           string(contents),
		ImpersonatedUserEmail: "my-fake-email@example.com",
	}

	diags := config.loadAndValidate(context.Background())
	err = checkDiags(diags)
	if err != nil {
		t.Fatalf("%s", err.Error())
	}
}

func TestConfigLoadAndValidate_credsFromFile(t *testing.T) {
	config := &apiClient{
		Credentials:           testFakeCredentialsPath,
		ImpersonatedUserEmail: "my-fake-email@example.com",
	}

	diags := config.loadAndValidate(context.Background())
	err := checkDiags(diags)
	if err != nil {
		t.Fatalf("%s", err.Error())
	}
}

func TestAccConfigLoadAndValidate_credsFromEnv(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip(fmt.Sprintf("Network access not allowed; use TF_ACC=1 to enable"))
	}

	testAccPreCheck(t)

	creds := getTestCredsFromEnv()
	config := &apiClient{
		Credentials:           creds,
		Customer:              os.Getenv("GOOGLEWORKSPACE_CUSTOMER_ID"),
		ImpersonatedUserEmail: os.Getenv("GOOGLEWORKSPACE_IMPERSONATED_USER_EMAIL"),
	}

	diags := config.loadAndValidate(context.Background())
	err := checkDiags(diags)
	if err != nil {
		t.Fatalf("%s", err.Error())
	}

	diags = checkValidCreds(config)
	err = checkDiags(diags)
	if err != nil {
		t.Fatalf("%s", err.Error())
	}
}

func TestConfigLoadAndValidate_credsNoImpersonation(t *testing.T) {
	config := &apiClient{
		Credentials: testFakeCredentialsPath,
	}

	diags := config.loadAndValidate(context.Background())
	err := checkDiags(diags)
	if err != nil {
		t.Fatalf("%s", err.Error())
	}
}

func TestConfigOauthScopes_custom(t *testing.T) {
	config := &apiClient{
		Credentials:           testFakeCredentialsPath,
		ClientScopes:          []string{"https://www.googleapis.com/auth/admin/directory"},
		ImpersonatedUserEmail: "my-fake-email@example.com",
	}

	diags := config.loadAndValidate(context.Background())
	err := checkDiags(diags)
	if err != nil {
		t.Fatalf("%s", err.Error())
	}

	if len(config.ClientScopes) != 1 {
		t.Fatalf("expected 1 scope, got %d scopes: %v", len(config.ClientScopes), config.ClientScopes)
	}
	if config.ClientScopes[0] != "https://www.googleapis.com/auth/admin/directory" {
		t.Fatalf("expected scope to be %q, got %q", "https://www.googleapis.com/auth/admin/directory", config.ClientScopes[0])
	}
}

func TestConfigLoadAndValidate_accessTokenInvalid(t *testing.T) {
	config := &apiClient{
		AccessToken:           "abcdefghijklmnopqrstuvwxyz",
		Customer:              os.Getenv("GOOGLEWORKSPACE_CUSTOMER_ID"),
		ImpersonatedUserEmail: os.Getenv("GOOGLEWORKSPACE_IMPERSONATED_USER_EMAIL"),
		ClientScopes:          []string{"https://www.googleapis.com/auth/admin.directory.domain"},
	}

	config.loadAndValidate(context.Background())
	diags := checkValidCreds(config)
	err := checkDiags(diags)
	if err == nil {
		t.Fatalf("expected error, but got nil")
	}
}

func TestConfigLoadAndValidate_accessToken(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip(fmt.Sprintf("Network access not allowed; use TF_ACC=1 to enable"))
	}

	testAccPreCheck(t)

	creds := getTestCredsFromEnv()
	gcpConfig := &apiClient{
		Credentials:  creds,
		ClientScopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
	}

	diags := gcpConfig.loadAndValidate(context.Background())
	err := checkDiags(diags)
	if err != nil {
		t.Fatalf("%s", err.Error())
	}

	iamCredsService, err := iamcredentials.NewService(context.Background(), option.WithHTTPClient(gcpConfig.client))
	if err != nil {
		t.Fatalf("%s", err.Error())
	}
	serviceAccount := fmt.Sprintf("projects/-/serviceAccounts/%s", os.Getenv("GOOGLEWORKSPACE_IMPERSONATED_SERVICE_ACCOUNT"))
	tokenRequest := &iamcredentials.GenerateAccessTokenRequest{
		Lifetime: "300s",
		Scope:    []string{"https://www.googleapis.com/auth/cloud-platform"},
	}
	at, err := iamCredsService.Projects.ServiceAccounts.GenerateAccessToken(serviceAccount, tokenRequest).Do()
	if err != nil {
		t.Fatalf("%s", err.Error())
	}

	config := &apiClient{
		AccessToken:           at.AccessToken,
		Customer:              os.Getenv("GOOGLEWORKSPACE_CUSTOMER_ID"),
		ImpersonatedUserEmail: os.Getenv("GOOGLEWORKSPACE_IMPERSONATED_USER_EMAIL"),
		ServiceAccount:        os.Getenv("GOOGLEWORKSPACE_IMPERSONATED_SERVICE_ACCOUNT"),
	}

	diags = config.loadAndValidate(context.Background())
	err = checkDiags(diags)
	if err != nil {
		t.Fatalf("%s", err.Error())
	}

	diags = checkValidCreds(config)
	err = checkDiags(diags)
	if err != nil {
		t.Fatalf("%s", err.Error())
	}
}

// TestConfigLoadAndValidate_accessTokenOnly covers a scenario where:
// 1. A service account is given an Admin Role in Google Workspace directly (no impersonation used in this test)
// 2. That role gives it Admin API privileges to query the groups endpoint of the Admin API - `Groups Admin role`
// The provider will then only need to be configured with the customer ID and an access token for that service account
func TestConfigLoadAndValidate_accessTokenOnly(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip(fmt.Sprintf("Network access not allowed; use TF_ACC=1 to enable"))
	}

	testAccPreCheck(t)

	// Get access token for the service account
	// --- Use service account credentials to request the access token
	// --- Request the `/auth/admin.directory.group` scope as it matches privileges in the Groups Admin role
	credsFile := getTestCredsFromEnv()

	contents, _, err := pathOrContents(credsFile)
	if err != nil {
		t.Fatalf("could not get credentials: %s", err.Error())
	}

	credParams := googleoauth.CredentialsParams{
		Scopes: []string{"https://www.googleapis.com/auth/admin.directory.group"},
	}

	creds, err := googleoauth.CredentialsFromJSONWithParams(context.Background(), []byte(contents), credParams)
	if err != nil {
		t.Fatalf("could not get oauth2 credentials: %s", err.Error())
	}

	at, err := creds.TokenSource.Token()
	if err != nil {
		t.Fatalf("could not get token from oauth2 credentials: %s", err.Error())
	}

	// Configure the provider with the scoped access token from above + the customer ID
	config := &apiClient{
		AccessToken: at.AccessToken,
		Customer:    os.Getenv("GOOGLEWORKSPACE_CUSTOMER_ID"),
	}

	diags := config.loadAndValidate(context.Background())
	err = checkDiags(diags)
	if err != nil {
		t.Fatalf("%s", err.Error())
	}

	diags = checkValidCredsGroupAdmin(config)
	err = checkDiags(diags)
	if err != nil {
		t.Fatalf("%s", err.Error())
	}
}

func checkValidCreds(config *apiClient) diag.Diagnostics {
	var diags diag.Diagnostics

	directoryService, diags := config.NewDirectoryService()
	if diags.HasError() {
		return diags
	}

	_, err := directoryService.Customers.Get(config.Customer).Do()
	if err != nil {
		return diag.FromErr(err)
	}

	return diags
}

// checkValidCredsGroupAdmin makes an arbitary API call to check the auth is set correctly.
// It makes a groups-related API call, to be used when testing auth relted to service accounts
// given the Group Admin role directly (no impersonisation done during the auth)
func checkValidCredsGroupAdmin(config *apiClient) diag.Diagnostics {
	var diags diag.Diagnostics

	directoryService, diags := config.NewDirectoryService()
	if diags.HasError() {
		return diags
	}
	groupsService := directoryService.Groups
	_, err := groupsService.List().Customer(config.Customer).Do()
	if err != nil {
		return diag.FromErr(err)
	}

	return diags
}

// TestNewGmailService_accessTokenServiceAccount verifies that the Gmail
// service, which impersonates the mailbox owner, keeps the access_token and
// service_account authentication of the provider. It needs no network access.
func TestNewGmailService_accessTokenServiceAccount(t *testing.T) {
	// Make the default credentials fail, so that a client that drops the
	// access token cannot get a token: no key file, no gcloud credentials, and
	// a metadata server that fails the test when it is called.
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("metadata server called: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected metadata request", http.StatusInternalServerError)
	}))
	defer metadata.Close()
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(metadata.URL, "http://"))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLOUDSDK_CONFIG", t.TempDir())

	var got impersonate.CredentialsConfig
	impersonateCalls := 0
	previous := impersonateTokenSource
	t.Cleanup(func() { impersonateTokenSource = previous })
	impersonateTokenSource = func(_ context.Context, config impersonate.CredentialsConfig, _ ...option.ClientOption) (oauth2.TokenSource, error) {
		impersonateCalls++
		got = config
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "impersonated-token"}), nil
	}

	config := &apiClient{
		AccessToken:    "fake-access-token",
		ServiceAccount: "sa@example-project.iam.gserviceaccount.com",
		Customer:       "C0123abcd",
	}

	svc, diags := config.NewGmailService(context.Background(), "user@example.com")
	if err := checkDiags(diags); err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}
	if svc == nil {
		t.Fatal("expected a Gmail service")
	}
	if config.ImpersonatedUserEmail != "" {
		t.Fatalf("provider config was modified: ImpersonatedUserEmail = %q", config.ImpersonatedUserEmail)
	}
	if impersonateCalls != 1 {
		t.Fatalf("access_token impersonation used %d times, want 1", impersonateCalls)
	}
	if got.TargetPrincipal != config.ServiceAccount || got.Subject != "user@example.com" {
		t.Fatalf("impersonation config = %+v, want service account %q and subject %q", got, config.ServiceAccount, "user@example.com")
	}

	// The Gmail client sends the token of the impersonation.
	gmailAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer impersonated-token" {
			t.Errorf("Authorization = %q, want the impersonated token", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer gmailAPI.Close()
	svc.BasePath = gmailAPI.URL + "/"
	if _, err := svc.Users.Settings.SendAs.List("user@example.com").Do(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
}

// TestNewGmailService_accessTokenWithoutServiceAccount verifies that the error
// for access_token without service_account says what to change.
func TestNewGmailService_accessTokenWithoutServiceAccount(t *testing.T) {
	config := &apiClient{
		AccessToken: "fake-access-token",
		Credentials: "{}",
		Customer:    "C0123abcd",
	}

	_, diags := config.NewGmailService(context.Background(), "user@example.com")
	if !diags.HasError() {
		t.Fatal("expected an error")
	}
	detail := diags[0].Detail
	for _, want := range []string{"user@example.com", "Gmail resources", "service_account", "domain-wide delegation", "Service Account Token Creator", "remove access_token"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("expected %q in the error detail: %s", want, detail)
		}
	}
}

// TestNewGmailService_cachesClientPerUser verifies that the Gmail client of a
// user is created once and shared by copies of the provider config.
func TestNewGmailService_cachesClientPerUser(t *testing.T) {
	var mu sync.Mutex
	subjects := map[string]int{}
	var ctxErrs []error
	previous := impersonateTokenSource
	t.Cleanup(func() { impersonateTokenSource = previous })
	impersonateTokenSource = func(ctx context.Context, config impersonate.CredentialsConfig, _ ...option.ClientOption) (oauth2.TokenSource, error) {
		mu.Lock()
		subjects[config.Subject]++
		ctxErrs = append(ctxErrs, ctx.Err())
		mu.Unlock()
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "impersonated-token"}), nil
	}

	config := &apiClient{
		gmailClients:   &gmailClientCache{},
		AccessToken:    "fake-access-token",
		ServiceAccount: "sa@example-project.iam.gserviceaccount.com",
		Customer:       "C0123abcd",
	}
	configCopy := *config

	// The client must keep working after the context of the first call ends.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var wg sync.WaitGroup
	for _, c := range []*apiClient{config, &configCopy} {
		for _, user := range []string{"user@example.com", "User@Example.com", "other@example.com"} {
			wg.Add(1)
			go func(c *apiClient, user string) {
				defer wg.Done()
				if _, diags := c.NewGmailService(ctx, user); diags.HasError() {
					t.Errorf("unexpected error: %v", diags)
				}
			}(c, user)
		}
	}
	wg.Wait()

	if len(subjects) != 2 || subjects["other@example.com"] != 1 || subjects["user@example.com"]+subjects["User@Example.com"] != 1 {
		t.Fatalf("impersonated subjects = %v, want one client for each user", subjects)
	}
	for _, err := range ctxErrs {
		if err != nil {
			t.Fatalf("cached client was created with a canceled context: %v", err)
		}
	}
}

// TestNewGmailService_createsClientsOfUsersConcurrently verifies that the
// creation of one user's client does not block the creation for another user.
func TestNewGmailService_createsClientsOfUsersConcurrently(t *testing.T) {
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	var mu sync.Mutex
	subjects := map[string]int{}
	previous := impersonateTokenSource
	t.Cleanup(func() { impersonateTokenSource = previous })
	impersonateTokenSource = func(_ context.Context, config impersonate.CredentialsConfig, _ ...option.ClientOption) (oauth2.TokenSource, error) {
		mu.Lock()
		subjects[config.Subject]++
		mu.Unlock()
		if config.Subject == "slow@example.com" {
			close(slowStarted)
			<-releaseSlow
		}
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "impersonated-token"}), nil
	}

	config := &apiClient{
		gmailClients:   &gmailClientCache{},
		AccessToken:    "fake-access-token",
		ServiceAccount: "sa@example-project.iam.gserviceaccount.com",
		Customer:       "C0123abcd",
	}
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, diags := config.NewGmailService(ctx, "slow@example.com"); diags.HasError() {
				t.Errorf("unexpected error: %v", diags)
			}
		}()
	}
	<-slowStarted

	fastDone := make(chan diag.Diagnostics, 1)
	go func() {
		_, diags := config.NewGmailService(ctx, "fast@example.com")
		fastDone <- diags
	}()
	select {
	case diags := <-fastDone:
		if diags.HasError() {
			t.Errorf("unexpected error: %v", diags)
		}
	case <-time.After(10 * time.Second):
		t.Error("client creation for another user waited for the slow user")
	}
	close(releaseSlow)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if subjects["slow@example.com"] != 1 || subjects["fast@example.com"] != 1 {
		t.Fatalf("impersonated subjects = %v, want one client for each user", subjects)
	}
}
