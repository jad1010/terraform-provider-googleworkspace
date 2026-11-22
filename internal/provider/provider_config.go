// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"

	"golang.org/x/oauth2"
	googleoauth "golang.org/x/oauth2/google"

	datatransfer "google.golang.org/api/admin/datatransfer/v1"
	directory "google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/chromepolicy/v1"
	"google.golang.org/api/cloudidentity/v1"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/groupssettings/v1"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/licensing/v1"
	"google.golang.org/api/option"
	"google.golang.org/api/transport"
)

// impersonateTokenSource creates the token source that impersonates a user
// through service_account with the access_token authentication. Unit tests
// replace it to check the impersonation without network access.
var impersonateTokenSource = impersonate.CredentialsTokenSource

type apiClient struct {
	client *http.Client
	// gmailClients caches the HTTP clients that impersonate Gmail users. It is
	// a pointer, so copies of the apiClient share the cache.
	gmailClients *gmailClientCache

	AccessToken           string
	ClientScopes          []string
	Credentials           string
	Customer              string
	ImpersonatedUserEmail string
	ServiceAccount        string
	UserAgent             string

	// licensingCustomer caches the unique customer ID for the License Manager
	// API (see LicensingCustomerId). It is a pointer, so that copies of the
	// client share one cache. It is nil in a client that is not made by
	// configure, which then does not cache.
	licensingCustomer *licensingCustomerCache

	// licenseAssignmentLists caches the license assignments of a product for
	// the SKU lookups of a refresh (see licenseAssignmentsOfProduct). It is a
	// pointer for the same reason, and nil means no cache.
	licenseAssignmentLists *licenseAssignmentListCache

	// directoryLookups remembers whether the Directory API refuses the user
	// lookups of the license resources (see optionalLicenseDirectoryUser). It
	// is a pointer for the same reason, and nil means no memory.
	directoryLookups *directoryLookupState
}

type directoryLookupState struct {
	mu    sync.Mutex
	state int
}

type licenseAssignmentListCache struct {
	mu    sync.Mutex
	lists map[string][]*licensing.LicenseAssignment
}

type licensingCustomerCache struct {
	mu sync.Mutex
	id string
}

func (c *apiClient) loadAndValidate(ctx context.Context) diag.Diagnostics {
	var diags diag.Diagnostics

	if len(c.ClientScopes) == 0 {
		c.ClientScopes = DefaultClientScopes
	}

	if c.AccessToken != "" {
		contents, _, err := pathOrContents(c.AccessToken)
		if err != nil {
			return diag.FromErr(err)
		}
		token := &oauth2.Token{AccessToken: contents}

		log.Printf("[INFO] Authenticating using configured Google JSON 'access_token'...")
		log.Printf("[INFO]   -- Scopes: %s", c.ClientScopes)

		if c.ImpersonatedUserEmail != "" {
			if c.ServiceAccount == "" {
				diags = append(diags, diag.Diagnostic{
					Severity: diag.Error,
					Summary:  "service_account is required to impersonate a user with the access_token authentication.",
					Detail: fmt.Sprintf("The provider must impersonate %q, and access_token is set. "+
						"Gmail resources (googleworkspace_gmail_send_as_alias, googleworkspace_user_delegate) always impersonate the mailbox owner. "+
						"Set service_account to a service account that has domain-wide delegation for the oauth_scopes, "+
						"and grant the principal of access_token the Service Account Token Creator role "+
						"(roles/iam.serviceAccountTokenCreator) on that service account. "+
						"Or remove access_token, so that credentials or the application default credentials are used.",
						c.ImpersonatedUserEmail),
				})

				return diags
			}

			tokenSource, err := impersonateTokenSource(ctx, impersonate.CredentialsConfig{
				TargetPrincipal: c.ServiceAccount,
				Scopes:          c.ClientScopes,
				Subject:         c.ImpersonatedUserEmail,
			}, option.WithTokenSource(oauth2.StaticTokenSource(token)))
			if err != nil {
				return diag.FromErr(err)
			}

			creds := googleoauth.Credentials{
				TokenSource: tokenSource,
			}
			diags = c.SetupClient(ctx, &creds)
			return diags
		}

		creds := googleoauth.Credentials{
			TokenSource: oauth2.StaticTokenSource(token),
		}
		diags = c.SetupClient(ctx, &creds)
		return diags
	}

	if c.Credentials != "" {
		contents, _, err := pathOrContents(c.Credentials)
		if err != nil {
			return diag.FromErr(err)
		}

		credParams := googleoauth.CredentialsParams{
			Scopes:  c.ClientScopes,
			Subject: c.ImpersonatedUserEmail,
		}

		creds, err := googleoauth.CredentialsFromJSONWithParams(ctx, []byte(contents), credParams)
		if err != nil {
			return diag.FromErr(err)
		}

		diags = c.SetupClient(ctx, creds)
	} else {
		credParams := googleoauth.CredentialsParams{
			Scopes:  c.ClientScopes,
			Subject: c.ImpersonatedUserEmail,
		}

		creds, err := googleoauth.FindDefaultCredentialsWithParams(ctx, credParams)
		if err != nil {
			return diag.FromErr(err)
		}

		diags = c.SetupClient(ctx, creds)
	}

	return diags
}

func (c *apiClient) SetupClient(ctx context.Context, creds *googleoauth.Credentials) diag.Diagnostics {
	var diags diag.Diagnostics

	cleanCtx := context.WithValue(ctx, oauth2.HTTPClient, cleanhttp.DefaultClient())

	// 1. MTLS TRANSPORT/CLIENT - sets up proper auth headers
	client, _, err := transport.NewHTTPClient(cleanCtx, option.WithTokenSource(creds.TokenSource))
	if err != nil {
		return diag.FromErr(err)
	}

	// 2. Logging Transport - ensure we log HTTP requests to admin APIs.
	scrubbedLoggingTransport := NewTransportWithScrubbedLogs("Google Workspace", client.Transport)

	// 3. Retry Transport - retries common temporary errors
	// Keep order for wrapping logging so we log each retried request as well.
	// This value should be used if needed to create shallow copies with additional retry predicates.
	// See ClientWithAdditionalRetries
	retryTransport := NewTransportWithDefaultRetries(scrubbedLoggingTransport)

	// Set final transport value.
	client.Transport = retryTransport

	c.client = client
	return diags
}

func (c *apiClient) NewChromePolicyService() (*chromepolicy.Service, diag.Diagnostics) {
	var diags diag.Diagnostics

	log.Printf("[INFO] Instantiating Google Admin Chrome Policy service")

	chromePolicyService, err := chromepolicy.NewService(context.Background(), option.WithHTTPClient(c.client))
	if err != nil {
		return nil, diag.FromErr(err)
	}

	if chromePolicyService == nil {
		diags = append(diags, diag.Diagnostic{
			Severity: diag.Error,
			Summary:  "Directory Service could not be created.",
		})

		return nil, diags
	}

	return chromePolicyService, diags
}

func (c *apiClient) NewDirectoryService() (*directory.Service, diag.Diagnostics) {
	var diags diag.Diagnostics

	log.Printf("[INFO] Instantiating Google Admin Directory service")

	directoryService, err := directory.NewService(context.Background(), option.WithHTTPClient(c.client))
	if err != nil {
		return nil, diag.FromErr(err)
	}

	if directoryService == nil {
		diags = append(diags, diag.Diagnostic{
			Severity: diag.Error,
			Summary:  "Directory Service could not be created.",
		})

		return nil, diags
	}

	return directoryService, diags
}

func (c *apiClient) NewGmailService(ctx context.Context, userId string) (*gmail.Service, diag.Diagnostics) {
	var diags diag.Diagnostics

	log.Printf("[INFO] Instantiating Google Admin Gmail service")

	// the send-as-alias resource requires the oauth token impersonate the user
	// the alias is being created for.
	httpClient, diags := c.gmailClients.get(userId, func() (*http.Client, diag.Diagnostics) {
		log.Printf("[INFO] Creating Google Admin Gmail client that impersonates %q", userId)
		// Copy every field (access token, service account, credentials, ...) so
		// the new client authenticates the same way as the provider; only the
		// impersonated user and the HTTP client differ.
		newClient := *c
		newClient.client = nil
		newClient.ImpersonatedUserEmail = userId
		// The client is cached and used after this call, so it must not stop
		// when ctx is canceled.
		diags := newClient.loadAndValidate(context.WithoutCancel(ctx))
		return newClient.client, diags
	})
	if diags.HasError() {
		return nil, diags
	}

	gmailService, err := gmail.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, diag.FromErr(err)
	}

	if gmailService == nil {
		diags = append(diags, diag.Diagnostic{
			Severity: diag.Error,
			Summary:  "Gmail Service could not be created.",
		})

		return nil, diags
	}

	return gmailService, diags
}

// gmailClientCache keeps one HTTP client for each impersonated Gmail user, so
// each resource operation does not create a new client and token source.
type gmailClientCache struct {
	mu      sync.Mutex
	clients map[string]*gmailClientEntry
}

// gmailClientEntry is created once. Its own sync.Once lets the clients of
// different users be created at the same time.
type gmailClientEntry struct {
	once   sync.Once
	client *http.Client
	diags  diag.Diagnostics
}

// get returns the cached client for userId, or calls create and caches the
// new client. Email addresses are not case-sensitive. A client that failed is
// not cached, so the next call tries again. A nil cache caches nothing.
func (g *gmailClientCache) get(userId string, create func() (*http.Client, diag.Diagnostics)) (*http.Client, diag.Diagnostics) {
	if g == nil {
		return create()
	}

	key := strings.ToLower(userId)
	g.mu.Lock()
	entry, ok := g.clients[key]
	if !ok {
		if g.clients == nil {
			g.clients = make(map[string]*gmailClientEntry)
		}
		entry = &gmailClientEntry{}
		g.clients[key] = entry
	}
	g.mu.Unlock()

	entry.once.Do(func() { entry.client, entry.diags = create() })
	if entry.diags.HasError() {
		g.mu.Lock()
		if g.clients[key] == entry {
			delete(g.clients, key)
		}
		g.mu.Unlock()
		return nil, entry.diags
	}
	return entry.client, entry.diags
}

func (c *apiClient) NewGroupsSettingsService() (*groupssettings.Service, diag.Diagnostics) {
	var diags diag.Diagnostics

	log.Printf("[INFO] Instantiating Google Admin Groups Settings service")

	groupsSettingsService, err := groupssettings.NewService(context.Background(), option.WithHTTPClient(c.client))
	if err != nil {
		return nil, diag.FromErr(err)
	}

	if groupsSettingsService == nil {
		diags = append(diags, diag.Diagnostic{
			Severity: diag.Error,
			Summary:  "Groups Settings Service could not be created.",
		})

		return nil, diags
	}

	return groupsSettingsService, diags
}

func (c *apiClient) NewDataTransferService() (*datatransfer.Service, diag.Diagnostics) {
	var diags diag.Diagnostics

	log.Printf("[INFO] Instantiating Google Admin Datatransfer service")

	dataTransferService, err := datatransfer.NewService(context.Background(), option.WithHTTPClient(c.client))
	if err != nil {
		return nil, diag.FromErr(err)
	}

	if dataTransferService == nil {
		diags = append(diags, diag.Diagnostic{
			Severity: diag.Error,
			Summary:  "Datatransfer Service could not be created.",
		})

		return nil, diags
	}

	return dataTransferService, diags
}

func (c *apiClient) NewCloudIdentityService() (*cloudidentity.Service, diag.Diagnostics) {
	var diags diag.Diagnostics

	log.Printf("[INFO] Instantiating Google Cloud Identity service")

	cloudIdentityService, err := cloudidentity.NewService(context.Background(), option.WithHTTPClient(c.client))
	if err != nil {
		return nil, diag.FromErr(err)
	}

	if cloudIdentityService == nil {
		diags = append(diags, diag.Diagnostic{
			Severity: diag.Error,
			Summary:  "Cloud Identity Service could not be created.",
		})

		return nil, diags
	}

	return cloudIdentityService, diags
}

func (c *apiClient) NewLicensingService() (*licensing.Service, diag.Diagnostics) {
	var diags diag.Diagnostics

	log.Printf("[INFO] Instantiating Google Enterprise License Manager service")

	licensingService, err := licensing.NewService(context.Background(), option.WithHTTPClient(c.client))
	if err != nil {
		return nil, diag.FromErr(err)
	}

	if licensingService == nil {
		diags = append(diags, diag.Diagnostic{
			Severity: diag.Error,
			Summary:  "Licensing Service could not be created.",
		})

		return nil, diags
	}

	return licensingService, diags
}
