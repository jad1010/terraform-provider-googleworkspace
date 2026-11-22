// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

// These tests run the resource and data source functions through the
// generated licensing client and the provider's retry transport against a
// local server. They do not need credentials.

const (
	testLicenseProductId = "Google-Apps"
	testLicenseSkuId     = "1010020027"
	testLicenseNewSkuId  = "1010020028"
	testLicenseUserId    = "alice@example.com"
	testLicenseCustomer  = "C01234567"
)

// testLicenseRequest is a request that the fake License Manager API received.
type testLicenseRequest struct {
	method string
	path   string
	query  url.Values
	body   map[string]interface{}
}

// testLicenseServer is a fake License Manager API. The handler gets each
// request and returns a status code and a JSON body.
type testLicenseServer struct {
	t        *testing.T
	mu       sync.Mutex
	requests []testLicenseRequest
	handler  func(r testLicenseRequest) (int, interface{})
}

func (s *testLicenseServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := map[string]interface{}{}
	if b, err := io.ReadAll(r.Body); err == nil && len(b) > 0 {
		if err := json.Unmarshal(b, &body); err != nil {
			s.t.Errorf("request body is not JSON: %s", b)
		}
	}
	req := testLicenseRequest{method: r.Method, path: r.URL.Path, query: r.URL.Query(), body: body}

	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()

	code, resp := s.handler(req)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(resp)
}

func (s *testLicenseServer) requestsFor(method, path string) []testLicenseRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	var result []testLicenseRequest
	for _, r := range s.requests {
		if r.method == method && r.path == path {
			result = append(result, r)
		}
	}
	return result
}

// testRedirectTransport sends each request to the test server instead of
// licensing.googleapis.com.
type testRedirectTransport struct {
	target *url.URL
}

func (rt testRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Scheme = rt.target.Scheme
	r.URL.Host = rt.target.Host
	r.Host = rt.target.Host
	return http.DefaultTransport.RoundTrip(r)
}

// newTestLicenseClient starts the fake API and returns a provider client that
// uses the default retry transport.
func newTestLicenseClient(t *testing.T, handler func(r testLicenseRequest) (int, interface{})) (*testLicenseServer, *apiClient) {
	server := &testLicenseServer{t: t, handler: handler}
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)

	target, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("unable to parse test server URL: %v", err)
	}

	client := &apiClient{
		client:                 &http.Client{Transport: NewTransportWithDefaultRetries(testRedirectTransport{target: target})},
		Customer:               testLicenseCustomer,
		licensingCustomer:      &licensingCustomerCache{},
		licenseAssignmentLists: &licenseAssignmentListCache{},
	}
	return server, client
}

func testLicenseAssignmentBody(skuId string) map[string]interface{} {
	return map[string]interface{}{
		"kind":        "licensing#licenseAssignment",
		"etags":       "\"etag-" + skuId + "\"",
		"productId":   testLicenseProductId,
		"productName": "Google Workspace",
		"skuId":       skuId,
		"skuName":     "SKU " + skuId,
		"userId":      testLicenseUserId,
		"selfLink":    "https://licensing.googleapis.com/self/" + skuId,
	}
}

func testLicenseError(code int, message string) map[string]interface{} {
	return map[string]interface{}{"error": map[string]interface{}{"code": code, "message": message}}
}

func testLicenseInsertPath(skuId string) string {
	return fmt.Sprintf("/apps/licensing/v1/product/%s/sku/%s/user", testLicenseProductId, skuId)
}

func testLicenseUserPath(skuId string) string {
	return fmt.Sprintf("/apps/licensing/v1/product/%s/sku/%s/user/%s", testLicenseProductId, skuId, testLicenseUserId)
}

// testLicenseContext bounds each test, so that a regression that retries a
// 412 fails the test instead of blocking it.
func testLicenseContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestLicenseAssignmentHTTP_createAndUpdate(t *testing.T) {
	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.method == http.MethodPost && r.path == testLicenseInsertPath(testLicenseSkuId):
			return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
		case r.method == http.MethodPatch && r.path == testLicenseUserPath(testLicenseSkuId):
			return http.StatusOK, testLicenseAssignmentBody(testLicenseNewSkuId)
		case r.method == http.MethodGet && r.path == testLicenseUserPath(testLicenseSkuId):
			return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
		case r.method == http.MethodGet && r.path == testLicenseUserPath(testLicenseNewSkuId):
			return http.StatusOK, testLicenseAssignmentBody(testLicenseNewSkuId)
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})
	ctx := testLicenseContext(t)

	// Create
	d := schema.TestResourceDataRaw(t, resourceLicenseAssignment().Schema, map[string]interface{}{
		"product_id": testLicenseProductId,
		"sku_id":     testLicenseSkuId,
		"user_id":    testLicenseUserId,
	})
	if diags := resourceLicenseAssignmentCreate(ctx, d, client); diags.HasError() {
		t.Fatalf("create returned errors: %v", diags)
	}

	inserts := server.requestsFor(http.MethodPost, testLicenseInsertPath(testLicenseSkuId))
	if len(inserts) != 1 {
		t.Fatalf("got %d insert requests, want 1", len(inserts))
	}
	if got := inserts[0].body["userId"]; got != testLicenseUserId {
		t.Errorf("insert body userId = %v, want %s", got, testLicenseUserId)
	}
	wantId := licenseAssignmentId(testLicenseProductId, testLicenseSkuId, testLicenseUserId)
	if d.Id() != wantId {
		t.Errorf("ID after create = %q, want %q", d.Id(), wantId)
	}
	if got := d.Get("sku_name").(string); got != "SKU "+testLicenseSkuId {
		t.Errorf("sku_name after create = %q, want %q", got, "SKU "+testLicenseSkuId)
	}

	// Update: change sku_id on a resource that has the ID of the old SKU.
	d = schema.TestResourceDataRaw(t, resourceLicenseAssignment().Schema, map[string]interface{}{
		"product_id": testLicenseProductId,
		"sku_id":     testLicenseNewSkuId,
		"user_id":    testLicenseUserId,
	})
	d.SetId(wantId)
	if diags := resourceLicenseAssignmentUpdate(ctx, d, client); diags.HasError() {
		t.Fatalf("update returned errors: %v", diags)
	}

	// The old SKU is in the path and the new SKU is in the body.
	patches := server.requestsFor(http.MethodPatch, testLicenseUserPath(testLicenseSkuId))
	if len(patches) != 1 {
		t.Fatalf("got %d patch requests to the old SKU path, want 1", len(patches))
	}
	if got := patches[0].body["skuId"]; got != testLicenseNewSkuId {
		t.Errorf("patch body skuId = %v, want %s", got, testLicenseNewSkuId)
	}
	wantId = licenseAssignmentId(testLicenseProductId, testLicenseNewSkuId, testLicenseUserId)
	if d.Id() != wantId {
		t.Errorf("ID after update = %q, want %q", d.Id(), wantId)
	}
	if got := d.Get("sku_name").(string); got != "SKU "+testLicenseNewSkuId {
		t.Errorf("sku_name after update = %q, want %q", got, "SKU "+testLicenseNewSkuId)
	}
}

func TestLicenseAssignmentHTTP_insertPreconditionFailedNotRetried(t *testing.T) {
	const message = "There aren't enough available licenses for the specified product-SKU pair"
	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		return http.StatusPreconditionFailed, testLicenseError(412, message)
	})

	d := schema.TestResourceDataRaw(t, resourceLicenseAssignment().Schema, map[string]interface{}{
		"product_id": testLicenseProductId,
		"sku_id":     testLicenseSkuId,
		"user_id":    testLicenseUserId,
	})
	diags := resourceLicenseAssignmentCreate(testLicenseContext(t), d, client)
	if !diags.HasError() {
		t.Fatalf("create returned no error, want a 412 error")
	}

	if n := len(server.requestsFor(http.MethodPost, testLicenseInsertPath(testLicenseSkuId))); n != 1 {
		t.Errorf("got %d insert attempts, want exactly 1", n)
	}
	if d.Id() != "" {
		t.Errorf("ID after a failed create = %q, want empty", d.Id())
	}
	summary := diags[0].Summary
	for _, want := range []string{message, "already has a license", "no free seats",
		licenseAssignmentId(testLicenseProductId, testLicenseSkuId, testLicenseUserId)} {
		if !strings.Contains(summary, want) {
			t.Errorf("create error %q does not contain %q", summary, want)
		}
	}
}

func TestLicenseAssignmentHTTP_patchPreconditionFailedNotRetried(t *testing.T) {
	const message = "There aren't enough available licenses for the specified product-SKU pair"
	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		return http.StatusPreconditionFailed, testLicenseError(412, message)
	})

	oldId := licenseAssignmentId(testLicenseProductId, testLicenseSkuId, testLicenseUserId)
	d := schema.TestResourceDataRaw(t, resourceLicenseAssignment().Schema, map[string]interface{}{
		"product_id": testLicenseProductId,
		"sku_id":     testLicenseNewSkuId,
		"user_id":    testLicenseUserId,
	})
	d.SetId(oldId)
	diags := resourceLicenseAssignmentUpdate(testLicenseContext(t), d, client)
	if !diags.HasError() {
		t.Fatalf("update returned no error, want a 412 error")
	}

	if n := len(server.requestsFor(http.MethodPatch, testLicenseUserPath(testLicenseSkuId))); n != 1 {
		t.Errorf("got %d patch attempts, want exactly 1", n)
	}
	if d.Id() != oldId {
		t.Errorf("ID after a failed update = %q, want %q", d.Id(), oldId)
	}
	summary := diags[0].Summary
	for _, want := range []string{message, "no free seats"} {
		if !strings.Contains(summary, want) {
			t.Errorf("update error %q does not contain %q", summary, want)
		}
	}
}

func TestLicenseAssignmentHTTP_readAfterCreateWaitsForNotFound(t *testing.T) {
	var mu sync.Mutex
	gets := 0
	_, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		if r.method == http.MethodPost {
			return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
		}
		mu.Lock()
		defer mu.Unlock()
		gets++
		// The new license assignment is not visible for the first get.
		if gets == 1 {
			return http.StatusNotFound, testLicenseError(404, "Not Found")
		}
		return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
	})

	d := schema.TestResourceDataRaw(t, resourceLicenseAssignment().Schema, map[string]interface{}{
		"product_id": testLicenseProductId,
		"sku_id":     testLicenseSkuId,
		"user_id":    testLicenseUserId,
	})
	if diags := resourceLicenseAssignmentCreate(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("create returned errors: %v", diags)
	}
	if want := licenseAssignmentId(testLicenseProductId, testLicenseSkuId, testLicenseUserId); d.Id() != want {
		t.Errorf("ID after create = %q, want %q", d.Id(), want)
	}
}

func TestLicenseAssignmentHTTP_readAfterCreateKeepsIdOnError(t *testing.T) {
	_, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		if r.method == http.MethodPost {
			return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
		}
		return http.StatusBadRequest, testLicenseError(400, "Bad Request")
	})

	d := schema.TestResourceDataRaw(t, resourceLicenseAssignment().Schema, map[string]interface{}{
		"product_id": testLicenseProductId,
		"sku_id":     testLicenseSkuId,
		"user_id":    testLicenseUserId,
	})
	diags := resourceLicenseAssignmentCreate(testLicenseContext(t), d, client)
	if !diags.HasError() {
		t.Fatalf("create returned no error, want a read error")
	}
	if want := licenseAssignmentId(testLicenseProductId, testLicenseSkuId, testLicenseUserId); d.Id() != want {
		t.Errorf("ID after a failed read = %q, want %q", d.Id(), want)
	}
}

func TestLicenseAssignmentsHTTP_dataSourcePages(t *testing.T) {
	listPath := fmt.Sprintf("/apps/licensing/v1/product/%s/sku/%s/users", testLicenseProductId, testLicenseSkuId)
	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		if r.method != http.MethodGet || r.path != listPath {
			return http.StatusNotFound, testLicenseError(404, "Not Found")
		}
		if r.query.Get("pageToken") == "" {
			return http.StatusOK, map[string]interface{}{
				"items":         []interface{}{testLicenseAssignmentBody(testLicenseSkuId)},
				"nextPageToken": "page-2",
			}
		}
		second := testLicenseAssignmentBody(testLicenseSkuId)
		second["userId"] = "bob@example.com"
		return http.StatusOK, map[string]interface{}{"items": []interface{}{second}}
	})

	d := schema.TestResourceDataRaw(t, dataSourceLicenseAssignments().Schema, map[string]interface{}{
		"product_id": testLicenseProductId,
		"sku_id":     testLicenseSkuId,
	})
	if diags := dataSourceLicenseAssignmentsRead(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("read returned errors: %v", diags)
	}

	requests := server.requestsFor(http.MethodGet, listPath)
	if len(requests) != 2 {
		t.Fatalf("got %d list requests, want 2", len(requests))
	}
	for _, r := range requests {
		if got := r.query.Get("maxResults"); got != "1000" {
			t.Errorf("maxResults = %q, want 1000", got)
		}
		if got := r.query.Get("customerId"); got != testLicenseCustomer {
			t.Errorf("customerId = %q, want %s", got, testLicenseCustomer)
		}
	}
	if got := d.Get("license_assignments.#").(int); got != 2 {
		t.Errorf("got %d license assignments, want 2", got)
	}
	if got := d.Get("license_assignments.1.user_id").(string); got != "bob@example.com" {
		t.Errorf("second user_id = %q, want bob@example.com", got)
	}
}

func testLicenseProductListPath() string {
	return fmt.Sprintf("/apps/licensing/v1/product/%s/users", testLicenseProductId)
}

func testDirectoryUserPath(email string) string {
	return "/admin/directory/v1/users/" + email
}

// testLicenseDirectoryUserId is the Directory user ID of testLicenseUserId.
const testLicenseDirectoryUserId = "111"

func testLicenseRefreshData(t *testing.T) *schema.ResourceData {
	d := schema.TestResourceDataRaw(t, resourceLicenseAssignment().Schema, map[string]interface{}{
		"product_id": testLicenseProductId,
		"sku_id":     testLicenseSkuId,
		"user_id":    testLicenseUserId,
	})
	d.SetId(licenseAssignmentId(testLicenseProductId, testLicenseSkuId, testLicenseUserId))
	d.Set("directory_user_id", testLicenseDirectoryUserId)
	return d
}

// A SKU changed outside Terraform: the refresh finds the user's current SKU.
func TestLicenseAssignmentHTTP_readFindsOtherSku(t *testing.T) {
	stubNotFoundBackoff(t, nil)

	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		if r.method == http.MethodGet && (r.path == testDirectoryUserPath(testLicenseUserId) || r.path == testDirectoryUserPath(testLicenseDirectoryUserId)) {
			return http.StatusOK, map[string]interface{}{"id": "111", "primaryEmail": testLicenseUserId}
		}
		if r.method == http.MethodGet && r.path == testLicenseProductListPath() {
			switch r.query.Get("pageToken") {
			case "":
				other := testLicenseAssignmentBody(testLicenseSkuId)
				other["userId"] = "bob@example.com"
				return http.StatusOK, map[string]interface{}{"items": []interface{}{other}, "nextPageToken": "page-2"}
			case "page-2":
				current := testLicenseAssignmentBody(testLicenseNewSkuId)
				current["userId"] = "Alice@Example.com"
				return http.StatusOK, map[string]interface{}{"items": []interface{}{current}, "nextPageToken": "page-3"}
			}
			return http.StatusOK, map[string]interface{}{"items": []interface{}{}}
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})

	d := testLicenseRefreshData(t)
	if diags := resourceLicenseAssignmentRead(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("read returned errors: %v", diags)
	}

	if want := licenseAssignmentId(testLicenseProductId, testLicenseNewSkuId, testLicenseUserId); d.Id() != want {
		t.Errorf("ID after read = %q, want %q", d.Id(), want)
	}
	if got := d.Get("sku_id").(string); got != testLicenseNewSkuId {
		t.Errorf("sku_id after read = %q, want %q", got, testLicenseNewSkuId)
	}
	if got := d.Get("user_id").(string); got != testLicenseUserId {
		t.Errorf("user_id after read = %q, want %q", got, testLicenseUserId)
	}
	if got := d.Get("sku_name").(string); got != "SKU "+testLicenseNewSkuId {
		t.Errorf("sku_name after read = %q, want %q", got, "SKU "+testLicenseNewSkuId)
	}

	// The listing is read in full once and shared (3 pages).
	requests := server.requestsFor(http.MethodGet, testLicenseProductListPath())
	if len(requests) != 3 {
		t.Errorf("got %d list requests, want 3", len(requests))
	}
	for _, r := range requests {
		if got := r.query.Get("customerId"); got != testLicenseCustomer {
			t.Errorf("customerId = %q, want %s", got, testLicenseCustomer)
		}
	}
}

func TestLicenseAssignmentHTTP_readRemovesWhenNoLicenseForProduct(t *testing.T) {
	stubNotFoundBackoff(t, nil)

	_, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		if r.method == http.MethodGet && (r.path == testDirectoryUserPath(testLicenseUserId) || r.path == testDirectoryUserPath(testLicenseDirectoryUserId)) {
			return http.StatusOK, map[string]interface{}{"id": "111", "primaryEmail": testLicenseUserId}
		}
		if r.method == http.MethodGet && r.path == testLicenseProductListPath() {
			other := testLicenseAssignmentBody(testLicenseSkuId)
			other["userId"] = "bob@example.com"
			return http.StatusOK, map[string]interface{}{"items": []interface{}{other}}
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})

	d := testLicenseRefreshData(t)
	if diags := resourceLicenseAssignmentRead(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("read returned errors: %v", diags)
	}
	if d.Id() != "" {
		t.Errorf("ID after read = %q, want empty", d.Id())
	}
}

// customer_id "my_customer" is resolved once to the unique customer ID for
// the licensing list calls.
func TestLicenseAssignmentsHTTP_myCustomerResolvedOnce(t *testing.T) {
	const resolvedCustomer = "C0resolved"
	customerPath := "/admin/directory/v1/customers/my_customer"
	listPath := fmt.Sprintf("/apps/licensing/v1/product/%s/users", testLicenseProductId)

	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.method == http.MethodGet && r.path == customerPath:
			return http.StatusOK, map[string]interface{}{"id": resolvedCustomer}
		case r.method == http.MethodGet && r.path == listPath:
			return http.StatusOK, map[string]interface{}{"items": []interface{}{testLicenseAssignmentBody(testLicenseSkuId)}}
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})
	client.Customer = "my_customer"

	for i := 0; i < 2; i++ {
		d := schema.TestResourceDataRaw(t, dataSourceLicenseAssignments().Schema, map[string]interface{}{
			"product_id": testLicenseProductId,
		})
		if diags := dataSourceLicenseAssignmentsRead(testLicenseContext(t), d, client); diags.HasError() {
			t.Fatalf("read %d returned errors: %v", i, diags)
		}
	}

	if n := len(server.requestsFor(http.MethodGet, customerPath)); n != 1 {
		t.Errorf("got %d customers.get requests, want 1", n)
	}
	requests := server.requestsFor(http.MethodGet, listPath)
	if len(requests) != 2 {
		t.Fatalf("got %d list requests, want 2", len(requests))
	}
	for _, r := range requests {
		if got := r.query.Get("customerId"); got != resolvedCustomer {
			t.Errorf("customerId = %q, want %s", got, resolvedCustomer)
		}
	}
}

func TestLicenseAssignmentsHTTP_customerIdNotResolved(t *testing.T) {
	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		return http.StatusOK, map[string]interface{}{"items": []interface{}{}}
	})

	d := schema.TestResourceDataRaw(t, dataSourceLicenseAssignments().Schema, map[string]interface{}{
		"product_id": testLicenseProductId,
	})
	if diags := dataSourceLicenseAssignmentsRead(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("read returned errors: %v", diags)
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	for _, r := range server.requests {
		if strings.HasPrefix(r.path, "/admin/directory/") {
			t.Errorf("unexpected Directory API request %s %s", r.method, r.path)
		}
	}
}

// A change to user_id is in place only if both addresses are the same user
// and the new address has the license.
func TestLicenseAssignmentHTTP_userIdChangeDiff(t *testing.T) {
	const oldEmail = "alice@example.com"
	const newEmail = "alice.smith@example.com"

	cases := map[string]struct {
		newUserId       string
		directoryIds    map[string]string
		newHasLicense   bool
		directoryError  int
		noStoredUserId  bool
		wantRequiresNew bool
		wantNoDiff      bool
		wantError       string
	}{
		"rename of the same user with the license": {
			newUserId:       newEmail,
			directoryIds:    map[string]string{oldEmail: "111", newEmail: "111"},
			newHasLicense:   true,
			wantRequiresNew: false,
		},
		"different user": {
			newUserId:       newEmail,
			directoryIds:    map[string]string{oldEmail: "111", newEmail: "222"},
			newHasLicense:   true,
			wantRequiresNew: true,
		},
		"same user without the license": {
			newUserId:       newEmail,
			directoryIds:    map[string]string{oldEmail: "111", newEmail: "111"},
			newHasLicense:   false,
			wantRequiresNew: true,
		},
		"new user does not exist": {
			newUserId:       newEmail,
			directoryIds:    map[string]string{oldEmail: "111"},
			newHasLicense:   true,
			wantRequiresNew: true,
		},
		"directory 403 fails the plan": {
			newUserId:      newEmail,
			directoryError: http.StatusForbidden,
			wantError:      directoryUserReadonlyScope,
		},
		"directory 500 fails the plan": {
			newUserId:      newEmail,
			directoryError: http.StatusInternalServerError,
			wantError:      directoryUserReadonlyScope,
		},
		"directory_user_id not set": {
			newUserId:       newEmail,
			directoryIds:    map[string]string{oldEmail: "111", newEmail: "111"},
			newHasLicense:   true,
			noStoredUserId:  true,
			wantRequiresNew: true,
		},
		"alias of the same user fails the plan": {
			newUserId:    "alice.alias@example.com",
			directoryIds: map[string]string{"alice.alias@example.com": "111"},
			wantError:    "user_id must be the user's primary email address (" + newEmail + ")",
		},
		"case change only": {
			newUserId:  "Alice@Example.com",
			wantNoDiff: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
				if r.method != http.MethodGet {
					return http.StatusBadRequest, testLicenseError(400, "unexpected method")
				}
				if tc.directoryError != 0 && strings.HasPrefix(r.path, "/admin/directory/") {
					return tc.directoryError, testLicenseError(tc.directoryError, "Request had insufficient authentication scopes.")
				}
				for email, id := range tc.directoryIds {
					if r.path == testDirectoryUserPath(email) {
						return http.StatusOK, map[string]interface{}{"id": id, "primaryEmail": newEmail}
					}
				}
				if tc.newHasLicense && r.path == fmt.Sprintf("/apps/licensing/v1/product/%s/sku/%s/user/%s", testLicenseProductId, testLicenseSkuId, tc.newUserId) {
					return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
				}
				return http.StatusNotFound, testLicenseError(404, "Not Found")
			})

			state := &terraform.InstanceState{
				ID: licenseAssignmentId(testLicenseProductId, testLicenseSkuId, oldEmail),
				Attributes: map[string]string{
					"id":         licenseAssignmentId(testLicenseProductId, testLicenseSkuId, oldEmail),
					"product_id": testLicenseProductId,
					"sku_id":     testLicenseSkuId,
					"user_id":    oldEmail,
				},
			}
			if !tc.noStoredUserId {
				state.Attributes["directory_user_id"] = testLicenseDirectoryUserId
			}
			config := terraform.NewResourceConfigRaw(map[string]interface{}{
				"product_id": testLicenseProductId,
				"sku_id":     testLicenseSkuId,
				"user_id":    tc.newUserId,
			})

			// A 500 is retried until the context ends, so that case gets a short context.
			ctx, cancel := context.WithTimeout(testLicenseContext(t), 3*time.Second)
			defer cancel()
			diff, err := resourceLicenseAssignment().Diff(ctx, state, config, client)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("diff error = %v, want an error that contains %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("diff returned error: %v", err)
			}

			if tc.wantNoDiff {
				if diff != nil && len(diff.Attributes) > 0 {
					t.Errorf("got diff %#v, want no diff", diff.Attributes)
				}
				return
			}
			if diff == nil || diff.Attributes["user_id"] == nil {
				t.Fatalf("got no user_id diff")
			}
			if got := diff.RequiresNew(); got != tc.wantRequiresNew {
				t.Errorf("RequiresNew() = %t, want %t", got, tc.wantRequiresNew)
			}
		})
	}
}

// A plan that changes user_id in place, then the update: the update makes no
// write request and updates the ID.
func TestLicenseAssignmentHTTP_updateUserIdInPlace(t *testing.T) {
	const newEmail = "alice.smith@example.com"
	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.method == http.MethodGet && (r.path == testDirectoryUserPath(testLicenseUserId) || r.path == testDirectoryUserPath(newEmail) || r.path == testDirectoryUserPath(testLicenseDirectoryUserId)):
			return http.StatusOK, map[string]interface{}{"id": "111", "primaryEmail": newEmail}
		case r.method == http.MethodGet && r.path == fmt.Sprintf("/apps/licensing/v1/product/%s/sku/%s/user/%s", testLicenseProductId, testLicenseSkuId, newEmail):
			return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})
	ctx := testLicenseContext(t)

	r := resourceLicenseAssignment()
	oldId := licenseAssignmentId(testLicenseProductId, testLicenseSkuId, testLicenseUserId)
	state := &terraform.InstanceState{
		ID: oldId,
		Attributes: map[string]string{
			"id":         oldId,
			"product_id": testLicenseProductId,
			"sku_id":     testLicenseSkuId,
			"user_id":    testLicenseUserId,

			"directory_user_id": testLicenseDirectoryUserId,
		},
	}
	config := terraform.NewResourceConfigRaw(map[string]interface{}{
		"product_id": testLicenseProductId,
		"sku_id":     testLicenseSkuId,
		"user_id":    newEmail,
	})

	diff, err := r.Diff(ctx, state, config, client)
	if err != nil {
		t.Fatalf("diff returned error: %v", err)
	}
	if diff == nil || diff.RequiresNew() {
		t.Fatalf("got diff %#v, want an in-place user_id change", diff)
	}

	d, err := schema.InternalMap(r.Schema).Data(state, diff)
	if err != nil {
		t.Fatalf("unable to build resource data: %v", err)
	}
	if diags := resourceLicenseAssignmentUpdate(ctx, d, client); diags.HasError() {
		t.Fatalf("update returned errors: %v", diags)
	}

	if want := licenseAssignmentId(testLicenseProductId, testLicenseSkuId, newEmail); d.Id() != want {
		t.Errorf("ID after update = %q, want %q", d.Id(), want)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	for _, r := range server.requests {
		if r.method != http.MethodGet {
			t.Errorf("unexpected write request %s %s", r.method, r.path)
		}
	}
}

func testLicenseState(userId string) *terraform.InstanceState {
	id := licenseAssignmentId(testLicenseProductId, testLicenseSkuId, userId)
	return &terraform.InstanceState{
		ID: id,
		Attributes: map[string]string{
			"id":         id,
			"product_id": testLicenseProductId,
			"sku_id":     testLicenseSkuId,
			"user_id":    userId,

			"directory_user_id": testLicenseDirectoryUserId,
		},
	}
}

func testLicenseConfig(userId string) *terraform.ResourceConfig {
	return terraform.NewResourceConfigRaw(map[string]interface{}{
		"product_id": testLicenseProductId,
		"sku_id":     testLicenseSkuId,
		"user_id":    userId,
	})
}

func TestLicenseAssignmentHTTP_readRemovesWhenUserMissing(t *testing.T) {
	stubNotFoundBackoff(t, nil)

	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})

	d := testLicenseRefreshData(t)
	if diags := resourceLicenseAssignmentRead(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("read returned errors: %v", diags)
	}
	if d.Id() != "" {
		t.Errorf("ID after read = %q, want empty", d.Id())
	}
	if n := len(server.requestsFor(http.MethodGet, testLicenseProductListPath())); n != 0 {
		t.Errorf("got %d list requests, want 0 for a user that does not exist", n)
	}
}

func TestLicenseAssignmentHTTP_readListError(t *testing.T) {
	stubNotFoundBackoff(t, nil)

	_, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.path == testDirectoryUserPath(testLicenseUserId), r.path == testDirectoryUserPath(testLicenseDirectoryUserId):
			return http.StatusOK, map[string]interface{}{"id": "111", "primaryEmail": testLicenseUserId}
		case r.path == testLicenseProductListPath():
			return http.StatusBadRequest, testLicenseError(400, "Invalid customer")
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})

	d := testLicenseRefreshData(t)
	diags := resourceLicenseAssignmentRead(testLicenseContext(t), d, client)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "Invalid customer") {
		t.Fatalf("read diags = %v, want the list error", diags)
	}
	if want := licenseAssignmentId(testLicenseProductId, testLicenseSkuId, testLicenseUserId); d.Id() != want {
		t.Errorf("ID after a failed read = %q, want %q", d.Id(), want)
	}
}

// A user renamed outside Terraform: the License Manager API does not find the
// old address. The refresh finds the license with the new primary address
// and keeps the old user_id, and the plan then changes user_id in place.
func TestLicenseAssignmentHTTP_readThenDiffRenamedUser(t *testing.T) {
	stubNotFoundBackoff(t, nil)
	const newEmail = "alice.smith@example.com"

	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.method != http.MethodGet:
			return http.StatusBadRequest, testLicenseError(400, "unexpected method")
		case r.path == testDirectoryUserPath(testLicenseUserId), r.path == testDirectoryUserPath(newEmail), r.path == testDirectoryUserPath(testLicenseDirectoryUserId):
			return http.StatusOK, map[string]interface{}{"id": "111", "primaryEmail": newEmail}
		case r.path == testLicenseUserPath(testLicenseSkuId):
			return http.StatusBadRequest, testLicenseError(400, "User email not valid.")
		case r.path == fmt.Sprintf("/apps/licensing/v1/product/%s/sku/%s/user/%s", testLicenseProductId, testLicenseSkuId, newEmail):
			return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})
	ctx := testLicenseContext(t)

	r := resourceLicenseAssignment()
	state, diags := r.RefreshWithoutUpgrade(ctx, testLicenseState(testLicenseUserId), client)
	if diags.HasError() {
		t.Fatalf("refresh returned errors: %v", diags)
	}
	if state == nil || state.ID != licenseAssignmentId(testLicenseProductId, testLicenseSkuId, testLicenseUserId) {
		t.Fatalf("state after refresh = %#v, want the ID with the old user_id", state)
	}
	if got := state.Attributes["user_id"]; got != testLicenseUserId {
		t.Errorf("user_id after refresh = %q, want %q", got, testLicenseUserId)
	}
	if n := len(server.requestsFor(http.MethodGet, testLicenseProductListPath())); n != 0 {
		t.Errorf("got %d list requests, want 0", n)
	}

	diff, err := r.Diff(ctx, state, testLicenseConfig(newEmail), client)
	if err != nil {
		t.Fatalf("diff returned error: %v", err)
	}
	if diff == nil || diff.Attributes["user_id"] == nil {
		t.Fatalf("got no user_id diff")
	}
	if diff.RequiresNew() {
		t.Errorf("diff requires a new resource, want an in-place user_id change")
	}
}

// A renamed user who also has a different SKU: the refresh lists the product
// and matches the new primary address.
func TestLicenseAssignmentHTTP_readRenamedUserOtherSku(t *testing.T) {
	stubNotFoundBackoff(t, nil)
	const newEmail = "alice.smith@example.com"

	_, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.path == testDirectoryUserPath(testLicenseUserId), r.path == testDirectoryUserPath(testLicenseDirectoryUserId):
			return http.StatusOK, map[string]interface{}{"id": "111", "primaryEmail": newEmail}
		case r.path == testLicenseProductListPath():
			current := testLicenseAssignmentBody(testLicenseNewSkuId)
			current["userId"] = newEmail
			return http.StatusOK, map[string]interface{}{"items": []interface{}{current}}
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})

	d := testLicenseRefreshData(t)
	if diags := resourceLicenseAssignmentRead(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("read returned errors: %v", diags)
	}
	if want := licenseAssignmentId(testLicenseProductId, testLicenseNewSkuId, testLicenseUserId); d.Id() != want {
		t.Errorf("ID after read = %q, want %q", d.Id(), want)
	}
}

func TestLicenseAssignmentHTTP_userIdUnknownForcesNew(t *testing.T) {
	_, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		t.Errorf("unexpected request %s %s", r.method, r.path)
		return http.StatusInternalServerError, nil
	})

	// The SDK reads this value as an unknown value (hcl2shim.UnknownVariableValue).
	const unknown = "74D93920-ED26-11E3-AC10-0800200C9A66"
	diff, err := resourceLicenseAssignment().Diff(testLicenseContext(t), testLicenseState(testLicenseUserId), testLicenseConfig(unknown), client)
	if err != nil {
		t.Fatalf("diff returned error: %v", err)
	}
	if diff == nil || !diff.RequiresNew() {
		t.Errorf("got diff %#v, want a replacement", diff)
	}
}

// An in-place change to user_id when the license is gone: one read with no
// wait, and a clear error.
func TestLicenseAssignmentHTTP_updateUserIdLicenseGone(t *testing.T) {
	const newEmail = "alice.smith@example.com"
	_, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		if r.path == testDirectoryUserPath(newEmail) {
			return http.StatusOK, map[string]interface{}{"id": "111", "primaryEmail": newEmail}
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})

	r := resourceLicenseAssignment()
	state := testLicenseState(testLicenseUserId)
	d, err := schema.InternalMap(r.Schema).Data(state, &terraform.InstanceDiff{
		Attributes: map[string]*terraform.ResourceAttrDiff{
			"user_id": {Old: testLicenseUserId, New: newEmail},
		},
	})
	if err != nil {
		t.Fatalf("unable to build resource data: %v", err)
	}

	start := time.Now()
	diags := resourceLicenseAssignmentUpdate(testLicenseContext(t), d, client)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("update took %s, want no wait", elapsed)
	}
	if !diags.HasError() || !strings.Contains(diags[len(diags)-1].Summary, "no longer exists for user "+newEmail) {
		t.Fatalf("update diags = %v, want a 'no longer exists' error", diags)
	}
	if d.Id() != state.ID {
		t.Errorf("ID after a failed update = %q, want %q", d.Id(), state.ID)
	}
}

func TestLicensingCustomerId_customersGetFailure(t *testing.T) {
	_, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		return http.StatusForbidden, testLicenseError(403, "Request had insufficient authentication scopes.")
	})
	client.Customer = "my_customer"

	_, diags := client.LicensingCustomerId(testLicenseContext(t))
	if !diags.HasError() {
		t.Fatalf("got no error, want a customers.get error")
	}
	for _, want := range []string{"insufficient authentication scopes", directoryCustomerReadonlyScope, "C01234567"} {
		if !strings.Contains(diags[0].Summary, want) {
			t.Errorf("error %q does not contain %q", diags[0].Summary, want)
		}
	}
}

func TestLicensingCustomerId_concurrent(t *testing.T) {
	const resolvedCustomer = "C0resolved"
	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		return http.StatusOK, map[string]interface{}{"id": resolvedCustomer}
	})
	client.Customer = "my_customer"
	ctx := testLicenseContext(t)

	var wg sync.WaitGroup
	errs := make(chan string, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, diags := client.LicensingCustomerId(ctx)
			if diags.HasError() || id != resolvedCustomer {
				errs <- fmt.Sprintf("got %q, %v", id, diags)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	if n := len(server.requestsFor(http.MethodGet, "/admin/directory/v1/customers/my_customer")); n != 1 {
		t.Errorf("got %d customers.get requests, want 1", n)
	}
}

// A client without a cache (not made by configure) still works.
func TestLicensingCustomerId_noCache(t *testing.T) {
	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		return http.StatusOK, map[string]interface{}{"id": "C0resolved"}
	})
	client.Customer = "my_customer"
	client.licensingCustomer = nil

	for i := 0; i < 2; i++ {
		if id, diags := client.LicensingCustomerId(testLicenseContext(t)); diags.HasError() || id != "C0resolved" {
			t.Fatalf("got %q, %v", id, diags)
		}
	}
	if n := len(server.requestsFor(http.MethodGet, "/admin/directory/v1/customers/my_customer")); n != 2 {
		t.Errorf("got %d customers.get requests, want 2 without a cache", n)
	}
}

// A delete after a rename outside Terraform uses the current primary address,
// so that the license is really removed.
func TestLicenseAssignmentHTTP_deleteRenamedUser(t *testing.T) {
	const newEmail = "alice.smith@example.com"
	newUserPath := fmt.Sprintf("/apps/licensing/v1/product/%s/sku/%s/user/%s", testLicenseProductId, testLicenseSkuId, newEmail)
	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.method == http.MethodGet && (r.path == testDirectoryUserPath(testLicenseUserId) || r.path == testDirectoryUserPath(testLicenseDirectoryUserId)):
			return http.StatusOK, map[string]interface{}{"id": "111", "primaryEmail": newEmail}
		case r.method == http.MethodDelete && r.path == newUserPath:
			return http.StatusOK, map[string]interface{}{}
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})

	d := testLicenseRefreshData(t)
	if diags := resourceLicenseAssignmentDelete(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("delete returned errors: %v", diags)
	}
	if n := len(server.requestsFor(http.MethodDelete, newUserPath)); n != 1 {
		t.Errorf("got %d deletes with the new primary address, want 1", n)
	}
}

func testLicenseUserPathFor(skuId, email string) string {
	return fmt.Sprintf("/apps/licensing/v1/product/%s/sku/%s/user/%s", testLicenseProductId, skuId, email)
}

// testLicenseNoRequestsTo fails if the server got any request for path.
func testLicenseNoRequestsTo(t *testing.T, server *testLicenseServer, path string) {
	t.Helper()
	server.mu.Lock()
	defer server.mu.Unlock()
	for _, r := range server.requests {
		if r.path == path {
			t.Errorf("unexpected request %s %s", r.method, r.path)
		}
	}
}

func TestLicenseAssignmentHTTP_deleteLookupErrorKeepsState(t *testing.T) {
	_, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		if strings.HasPrefix(r.path, "/admin/directory/") {
			return http.StatusForbidden, testLicenseError(403, "Request had insufficient authentication scopes.")
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})

	d := testLicenseRefreshData(t)
	diags := resourceLicenseAssignmentDelete(testLicenseContext(t), d, client)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, directoryUserReadonlyScope) {
		t.Fatalf("delete diags = %v, want an error with the scope", diags)
	}
	if want := licenseAssignmentId(testLicenseProductId, testLicenseSkuId, testLicenseUserId); d.Id() != want {
		t.Errorf("ID after a failed delete = %q, want %q", d.Id(), want)
	}
}

// The user of the license assignment was deleted, and the address is now an
// alias of a different user, who has a license too.
func testLicenseAliasReuseServer(t *testing.T) (*testLicenseServer, *apiClient) {
	const otherEmail = "bob@example.com"
	return newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.path == testDirectoryUserPath(testLicenseUserId):
			return http.StatusOK, map[string]interface{}{"id": "222", "primaryEmail": otherEmail}
		case r.path == testLicenseUserPathFor(testLicenseSkuId, otherEmail):
			return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})
}

func TestLicenseAssignmentHTTP_readAliasReuseDrops(t *testing.T) {
	stubNotFoundBackoff(t, nil)
	server, client := testLicenseAliasReuseServer(t)

	d := testLicenseRefreshData(t)
	if diags := resourceLicenseAssignmentRead(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("read returned errors: %v", diags)
	}
	if d.Id() != "" {
		t.Errorf("ID after read = %q, want empty", d.Id())
	}
	testLicenseNoRequestsTo(t, server, testLicenseUserPathFor(testLicenseSkuId, "bob@example.com"))
	testLicenseNoRequestsTo(t, server, testLicenseProductListPath())
}

func TestLicenseAssignmentHTTP_deleteAliasReuseKeepsOtherLicense(t *testing.T) {
	server, client := testLicenseAliasReuseServer(t)

	d := testLicenseRefreshData(t)
	if diags := resourceLicenseAssignmentDelete(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("delete returned errors: %v", diags)
	}
	if d.Id() != "" {
		t.Errorf("ID after delete = %q, want empty", d.Id())
	}
	testLicenseNoRequestsTo(t, server, testLicenseUserPathFor(testLicenseSkuId, "bob@example.com"))
}

func TestLicenseAssignmentHTTP_patchAliasReuseFails(t *testing.T) {
	server, client := testLicenseAliasReuseServer(t)

	oldId := licenseAssignmentId(testLicenseProductId, testLicenseSkuId, testLicenseUserId)
	d := schema.TestResourceDataRaw(t, resourceLicenseAssignment().Schema, map[string]interface{}{
		"product_id": testLicenseProductId,
		"sku_id":     testLicenseNewSkuId,
		"user_id":    testLicenseUserId,
	})
	d.SetId(oldId)
	d.Set("directory_user_id", testLicenseDirectoryUserId)

	diags := resourceLicenseAssignmentUpdate(testLicenseContext(t), d, client)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "does not exist any more") {
		t.Fatalf("update diags = %v, want a user-gone error", diags)
	}
	if d.Id() != oldId {
		t.Errorf("ID after a failed update = %q, want %q", d.Id(), oldId)
	}
	testLicenseNoRequestsTo(t, server, testLicenseUserPathFor(testLicenseSkuId, "bob@example.com"))
}

// A SKU change after the user was renamed outside Terraform.
func TestLicenseAssignmentHTTP_patchRenamedUser(t *testing.T) {
	const newEmail = "alice.smith@example.com"
	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.method == http.MethodGet && (r.path == testDirectoryUserPath(testLicenseUserId) || r.path == testDirectoryUserPath(testLicenseDirectoryUserId)):
			return http.StatusOK, map[string]interface{}{"id": testLicenseDirectoryUserId, "primaryEmail": newEmail}
		case r.method == http.MethodPatch && r.path == testLicenseUserPathFor(testLicenseSkuId, newEmail):
			return http.StatusOK, testLicenseAssignmentBody(testLicenseNewSkuId)
		case r.method == http.MethodGet && r.path == testLicenseUserPathFor(testLicenseNewSkuId, newEmail):
			return http.StatusOK, testLicenseAssignmentBody(testLicenseNewSkuId)
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})

	d := schema.TestResourceDataRaw(t, resourceLicenseAssignment().Schema, map[string]interface{}{
		"product_id": testLicenseProductId,
		"sku_id":     testLicenseNewSkuId,
		"user_id":    testLicenseUserId,
	})
	d.SetId(licenseAssignmentId(testLicenseProductId, testLicenseSkuId, testLicenseUserId))
	d.Set("directory_user_id", testLicenseDirectoryUserId)

	if diags := resourceLicenseAssignmentUpdate(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("update returned errors: %v", diags)
	}

	patches := server.requestsFor(http.MethodPatch, testLicenseUserPathFor(testLicenseSkuId, newEmail))
	if len(patches) != 1 {
		t.Fatalf("got %d patch requests with the new primary address, want 1", len(patches))
	}
	if got := patches[0].body["skuId"]; got != testLicenseNewSkuId {
		t.Errorf("patch body skuId = %v, want %s", got, testLicenseNewSkuId)
	}
	if want := licenseAssignmentId(testLicenseProductId, testLicenseNewSkuId, testLicenseUserId); d.Id() != want {
		t.Errorf("ID after update = %q, want %q", d.Id(), want)
	}
	if got := d.Get("directory_user_id").(string); got != testLicenseDirectoryUserId {
		t.Errorf("directory_user_id after update = %q, want %q", got, testLicenseDirectoryUserId)
	}
}

// State from before directory_user_id: a refresh sets it when the License
// Manager API finds user_id directly.
func TestLicenseAssignmentHTTP_readUpgradeSetsDirectoryUserId(t *testing.T) {
	_, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.path == testLicenseUserPath(testLicenseSkuId):
			return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
		case r.path == testDirectoryUserPath(testLicenseUserId), r.path == testDirectoryUserPath(testLicenseDirectoryUserId):
			return http.StatusOK, map[string]interface{}{"id": testLicenseDirectoryUserId, "primaryEmail": testLicenseUserId}
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})

	d := testLicenseRefreshData(t)
	d.Set("directory_user_id", "")
	if diags := resourceLicenseAssignmentRead(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("read returned errors: %v", diags)
	}
	if got := d.Get("directory_user_id").(string); got != testLicenseDirectoryUserId {
		t.Errorf("directory_user_id after read = %q, want %q", got, testLicenseDirectoryUserId)
	}
}

// State from before directory_user_id: an address that is found only through
// an alias cannot be confirmed, so the refresh removes the resource.
func TestLicenseAssignmentHTTP_readUpgradeAliasDrops(t *testing.T) {
	stubNotFoundBackoff(t, nil)
	const newEmail = "alice.smith@example.com"
	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.path == testDirectoryUserPath(testLicenseUserId), r.path == testDirectoryUserPath(testLicenseDirectoryUserId):
			return http.StatusOK, map[string]interface{}{"id": testLicenseDirectoryUserId, "primaryEmail": newEmail}
		case r.path == testLicenseUserPathFor(testLicenseSkuId, newEmail):
			return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})

	d := testLicenseRefreshData(t)
	d.Set("directory_user_id", "")
	if diags := resourceLicenseAssignmentRead(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("read returned errors: %v", diags)
	}
	if d.Id() != "" {
		t.Errorf("ID after read = %q, want empty", d.Id())
	}
	testLicenseNoRequestsTo(t, server, testLicenseUserPathFor(testLicenseSkuId, newEmail))
}

// Without the Directory user scope, a create still works and leaves
// directory_user_id empty.
func TestLicenseAssignmentHTTP_createDirectoryForbiddenLeavesIdEmpty(t *testing.T) {
	_, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.method == http.MethodPost && r.path == testLicenseInsertPath(testLicenseSkuId):
			return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
		case r.method == http.MethodGet && r.path == testLicenseUserPath(testLicenseSkuId):
			return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
		case strings.HasPrefix(r.path, "/admin/directory/"):
			return http.StatusForbidden, testLicenseError(403, "Request had insufficient authentication scopes.")
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})

	d := schema.TestResourceDataRaw(t, resourceLicenseAssignment().Schema, map[string]interface{}{
		"product_id": testLicenseProductId,
		"sku_id":     testLicenseSkuId,
		"user_id":    testLicenseUserId,
	})
	if diags := resourceLicenseAssignmentCreate(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("create returned errors: %v", diags)
	}
	if want := licenseAssignmentId(testLicenseProductId, testLicenseSkuId, testLicenseUserId); d.Id() != want {
		t.Errorf("ID after create = %q, want %q", d.Id(), want)
	}
	if got := d.Get("directory_user_id").(string); got != "" {
		t.Errorf("directory_user_id after create = %q, want empty", got)
	}
}

// The user of the license assignment was deleted, and a new user (B) now has
// the same primary address and the same SKU. The License Manager API finds
// B's license with the stored address.
func testLicensePrimaryReuseServer(t *testing.T) (*testLicenseServer, *apiClient) {
	return newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.path == testDirectoryUserPath(testLicenseUserId):
			return http.StatusOK, map[string]interface{}{"id": "222", "primaryEmail": testLicenseUserId}
		case r.path == testLicenseUserPath(testLicenseSkuId):
			return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})
}

func TestLicenseAssignmentHTTP_deletePrimaryReuseKeepsOtherLicense(t *testing.T) {
	server, client := testLicensePrimaryReuseServer(t)

	d := testLicenseRefreshData(t)
	if diags := resourceLicenseAssignmentDelete(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("delete returned errors: %v", diags)
	}
	if d.Id() != "" {
		t.Errorf("ID after delete = %q, want empty", d.Id())
	}
	if n := len(server.requestsFor(http.MethodDelete, testLicenseUserPath(testLicenseSkuId))); n != 0 {
		t.Errorf("got %d delete requests for the other user's license, want 0", n)
	}
}

func TestLicenseAssignmentHTTP_patchPrimaryReuseFails(t *testing.T) {
	server, client := testLicensePrimaryReuseServer(t)

	oldId := licenseAssignmentId(testLicenseProductId, testLicenseSkuId, testLicenseUserId)
	d := schema.TestResourceDataRaw(t, resourceLicenseAssignment().Schema, map[string]interface{}{
		"product_id": testLicenseProductId,
		"sku_id":     testLicenseNewSkuId,
		"user_id":    testLicenseUserId,
	})
	d.SetId(oldId)
	d.Set("directory_user_id", testLicenseDirectoryUserId)

	diags := resourceLicenseAssignmentUpdate(testLicenseContext(t), d, client)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "does not exist any more") {
		t.Fatalf("update diags = %v, want a user-gone error", diags)
	}
	if d.Id() != oldId {
		t.Errorf("ID after a failed update = %q, want %q", d.Id(), oldId)
	}
	if n := len(server.requestsFor(http.MethodPatch, testLicenseUserPath(testLicenseSkuId))); n != 0 {
		t.Errorf("got %d patch requests for the other user's license, want 0", n)
	}
}

func TestLicenseAssignmentHTTP_readPrimaryReuseDrops(t *testing.T) {
	_, client := testLicensePrimaryReuseServer(t)

	d := testLicenseRefreshData(t)
	if diags := resourceLicenseAssignmentRead(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("read returned errors: %v", diags)
	}
	if d.Id() != "" {
		t.Errorf("ID after read = %q, want empty", d.Id())
	}
}

// A refresh that cannot confirm the user keeps the state.
func TestLicenseAssignmentHTTP_readOwnerLookupErrorKeepsState(t *testing.T) {
	_, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.path == testLicenseUserPath(testLicenseSkuId):
			return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
		case strings.HasPrefix(r.path, "/admin/directory/"):
			return http.StatusForbidden, testLicenseError(403, "Request had insufficient authentication scopes.")
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})

	d := testLicenseRefreshData(t)
	if diags := resourceLicenseAssignmentRead(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("read returned errors: %v", diags)
	}
	if want := licenseAssignmentId(testLicenseProductId, testLicenseSkuId, testLicenseUserId); d.Id() != want {
		t.Errorf("ID after read = %q, want %q", d.Id(), want)
	}
	if got := d.Get("directory_user_id").(string); got != testLicenseDirectoryUserId {
		t.Errorf("directory_user_id after read = %q, want %q", got, testLicenseDirectoryUserId)
	}
}

// testLicenseNoDirectoryServer is a License Manager API where the Directory
// API refuses every request (the Directory user scope is missing). The
// license of the user is not found with sku testLicenseSkuId; listItems are
// the license assignments of the product.
func testLicenseNoDirectoryServer(t *testing.T, listItems []interface{}) (*testLicenseServer, *apiClient) {
	return newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case strings.HasPrefix(r.path, "/admin/directory/"):
			return http.StatusForbidden, testLicenseError(403, "Request had insufficient authentication scopes.")
		case r.method == http.MethodGet && r.path == testLicenseProductListPath():
			return http.StatusOK, map[string]interface{}{"items": listItems}
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})
}

func testLicenseRefreshDataWithoutDirectoryUserId(t *testing.T) *schema.ResourceData {
	d := testLicenseRefreshData(t)
	d.Set("directory_user_id", "")
	return d
}

func TestLicenseAssignmentHTTP_readNoIdDirectoryForbiddenDrops(t *testing.T) {
	stubNotFoundBackoff(t, nil)
	other := testLicenseAssignmentBody(testLicenseSkuId)
	other["userId"] = "bob@example.com"
	_, client := testLicenseNoDirectoryServer(t, []interface{}{other})

	d := testLicenseRefreshDataWithoutDirectoryUserId(t)
	if diags := resourceLicenseAssignmentRead(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("read returned errors: %v", diags)
	}
	if d.Id() != "" {
		t.Errorf("ID after read = %q, want empty", d.Id())
	}
}

func TestLicenseAssignmentHTTP_readNoIdDirectoryForbiddenAdoptsSku(t *testing.T) {
	stubNotFoundBackoff(t, nil)
	current := testLicenseAssignmentBody(testLicenseNewSkuId)
	current["userId"] = "Alice@Example.com"
	server, client := testLicenseNoDirectoryServer(t, []interface{}{current})

	d := testLicenseRefreshDataWithoutDirectoryUserId(t)
	if diags := resourceLicenseAssignmentRead(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("read returned errors: %v", diags)
	}
	if want := licenseAssignmentId(testLicenseProductId, testLicenseNewSkuId, testLicenseUserId); d.Id() != want {
		t.Errorf("ID after read = %q, want %q", d.Id(), want)
	}
	if got := d.Get("directory_user_id").(string); got != "" {
		t.Errorf("directory_user_id after read = %q, want empty", got)
	}
	if n := len(server.requestsFor(http.MethodGet, testLicenseProductListPath())); n != 1 {
		t.Errorf("got %d list requests, want 1", n)
	}
}

func TestLicenseAssignmentHTTP_deleteNoIdDirectoryForbidden(t *testing.T) {
	_, client := testLicenseNoDirectoryServer(t, nil)

	d := testLicenseRefreshDataWithoutDirectoryUserId(t)
	if diags := resourceLicenseAssignmentDelete(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("delete returned errors: %v", diags)
	}
	if d.Id() != "" {
		t.Errorf("ID after delete = %q, want empty", d.Id())
	}
}

func stubLicenseDirectoryLookupTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	orig := licenseDirectoryLookupTimeout
	licenseDirectoryLookupTimeout = timeout
	t.Cleanup(func() { licenseDirectoryLookupTimeout = orig })
}

// testLicenseNotAuthorized is the 403 that the retry transport retries until
// the context ends.
func testLicenseNotAuthorized() (int, interface{}) {
	return http.StatusForbidden, testLicenseError(403, "Not Authorized to access this resource/api")
}

// A principal whose admin role cannot read users: the optional lookup after a
// create stops at licenseDirectoryLookupTimeout, and the create succeeds.
func TestLicenseAssignmentHTTP_createDirectoryNotAuthorizedDoesNotWait(t *testing.T) {
	stubLicenseDirectoryLookupTimeout(t, 300*time.Millisecond)
	_, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.method == http.MethodPost && r.path == testLicenseInsertPath(testLicenseSkuId):
			return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
		case r.method == http.MethodGet && r.path == testLicenseUserPath(testLicenseSkuId):
			return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
		case strings.HasPrefix(r.path, "/admin/directory/"):
			return testLicenseNotAuthorized()
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})

	d := schema.TestResourceDataRaw(t, resourceLicenseAssignment().Schema, map[string]interface{}{
		"product_id": testLicenseProductId,
		"sku_id":     testLicenseSkuId,
		"user_id":    testLicenseUserId,
	})
	start := time.Now()
	if diags := resourceLicenseAssignmentCreate(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("create returned errors: %v", diags)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("create took %s, want the lookup to stop at its timeout", elapsed)
	}
	if got := d.Get("directory_user_id").(string); got != "" {
		t.Errorf("directory_user_id after create = %q, want empty", got)
	}
}

// With directory_user_id set, the same 403 makes a delete fail at the lookup
// timeout, with the scope message, and keep the state.
func TestLicenseAssignmentHTTP_deleteDirectoryNotAuthorizedFailsFast(t *testing.T) {
	stubLicenseDirectoryLookupTimeout(t, 300*time.Millisecond)
	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		if strings.HasPrefix(r.path, "/admin/directory/") {
			return testLicenseNotAuthorized()
		}
		return http.StatusOK, map[string]interface{}{}
	})

	d := testLicenseRefreshData(t)
	start := time.Now()
	diags := resourceLicenseAssignmentDelete(testLicenseContext(t), d, client)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("delete took %s, want the lookup to stop at its timeout", elapsed)
	}
	if !diags.HasError() || !strings.Contains(diags[0].Summary, directoryUserReadonlyScope) {
		t.Fatalf("delete diags = %v, want an error with the scope", diags)
	}
	if d.Id() == "" {
		t.Errorf("ID after a failed delete is empty, want the state kept")
	}
	if n := len(server.requestsFor(http.MethodDelete, testLicenseUserPath(testLicenseSkuId))); n != 0 {
		t.Errorf("got %d delete requests, want 0", n)
	}
}

// testLicenseRenameAliasRemovedServer is a user that was renamed, and whose
// old address is not an alias any more: only the lookup by the stored ID
// finds the user.
func testLicenseRenameAliasRemovedServer(t *testing.T, newEmail string) (*testLicenseServer, *apiClient) {
	return newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.method == http.MethodGet && r.path == testDirectoryUserPath(testLicenseDirectoryUserId):
			return http.StatusOK, map[string]interface{}{"id": testLicenseDirectoryUserId, "primaryEmail": newEmail}
		case r.method == http.MethodGet && r.path == testLicenseUserPathFor(testLicenseSkuId, newEmail):
			return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
		case r.method == http.MethodDelete && r.path == testLicenseUserPathFor(testLicenseSkuId, newEmail):
			return http.StatusOK, map[string]interface{}{}
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})
}

// The refresh finds the renamed user by the stored ID, reads the license once
// with the new address (no request with the old address and no 404 wait),
// and keeps user_id.
func TestLicenseAssignmentHTTP_readRenameAliasRemoved(t *testing.T) {
	const newEmail = "alice.smith@example.com"
	server, client := testLicenseRenameAliasRemovedServer(t, newEmail)

	d := testLicenseRefreshData(t)
	start := time.Now()
	diags := resourceLicenseAssignmentRead(testLicenseContext(t), d, client)
	if diags.HasError() {
		t.Fatalf("read returned errors: %v", diags)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("read took %s, want no 404 wait", elapsed)
	}
	if len(diags) != 1 || diags[0].Severity != diag.Warning || !strings.Contains(diags[0].Detail, "Set user_id to "+newEmail) {
		t.Errorf("read diags = %v, want one warning that names %s", diags, newEmail)
	}
	if want := licenseAssignmentId(testLicenseProductId, testLicenseSkuId, testLicenseUserId); d.Id() != want {
		t.Errorf("ID after read = %q, want %q", d.Id(), want)
	}
	if n := len(server.requestsFor(http.MethodGet, testLicenseUserPath(testLicenseSkuId))); n != 0 {
		t.Errorf("got %d license requests with the old address, want 0", n)
	}
	if n := len(server.requestsFor(http.MethodGet, testLicenseUserPathFor(testLicenseSkuId, newEmail))); n != 1 {
		t.Errorf("got %d license requests with the new address, want 1", n)
	}
}

func TestLicenseAssignmentHTTP_deleteRenameAliasRemoved(t *testing.T) {
	const newEmail = "alice.smith@example.com"
	server, client := testLicenseRenameAliasRemovedServer(t, newEmail)

	d := testLicenseRefreshData(t)
	if diags := resourceLicenseAssignmentDelete(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("delete returned errors: %v", diags)
	}
	if n := len(server.requestsFor(http.MethodDelete, testLicenseUserPathFor(testLicenseSkuId, newEmail))); n != 1 {
		t.Errorf("got %d deletes with the new address, want 1", n)
	}
}

// Two license assignments of the same product with SKU drift: the refreshes
// share one listing of the product.
func TestLicenseAssignmentHTTP_refreshesShareProductListing(t *testing.T) {
	const bobEmail = "bob@example.com"
	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.path == testDirectoryUserPath("111"):
			return http.StatusOK, map[string]interface{}{"id": "111", "primaryEmail": testLicenseUserId}
		case r.path == testDirectoryUserPath("222"):
			return http.StatusOK, map[string]interface{}{"id": "222", "primaryEmail": bobEmail}
		case r.path == testLicenseProductListPath():
			alice := testLicenseAssignmentBody(testLicenseNewSkuId)
			bob := testLicenseAssignmentBody(testLicenseNewSkuId)
			bob["userId"] = bobEmail
			return http.StatusOK, map[string]interface{}{"items": []interface{}{alice, bob}}
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})
	ctx := testLicenseContext(t)

	for _, user := range []struct{ email, id string }{{testLicenseUserId, "111"}, {bobEmail, "222"}} {
		d := schema.TestResourceDataRaw(t, resourceLicenseAssignment().Schema, map[string]interface{}{
			"product_id": testLicenseProductId,
			"sku_id":     testLicenseSkuId,
			"user_id":    user.email,
		})
		d.SetId(licenseAssignmentId(testLicenseProductId, testLicenseSkuId, user.email))
		d.Set("directory_user_id", user.id)

		if diags := resourceLicenseAssignmentRead(ctx, d, client); diags.HasError() {
			t.Fatalf("read of %s returned errors: %v", user.email, diags)
		}
		if want := licenseAssignmentId(testLicenseProductId, testLicenseNewSkuId, user.email); d.Id() != want {
			t.Errorf("ID after read = %q, want %q", d.Id(), want)
		}
	}

	if n := len(server.requestsFor(http.MethodGet, testLicenseProductListPath())); n != 1 {
		t.Errorf("got %d list requests, want 1 shared listing", n)
	}
}

// With directory_user_id set, a delete with a SKU that changed outside
// Terraform deletes the user's current SKU.
func TestLicenseAssignmentHTTP_deleteStaleSku(t *testing.T) {
	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.path == testDirectoryUserPath(testLicenseDirectoryUserId):
			return http.StatusOK, map[string]interface{}{"id": testLicenseDirectoryUserId, "primaryEmail": testLicenseUserId}
		case r.method == http.MethodGet && r.path == testLicenseProductListPath():
			return http.StatusOK, map[string]interface{}{"items": []interface{}{testLicenseAssignmentBody(testLicenseNewSkuId)}}
		case r.method == http.MethodDelete && r.path == testLicenseUserPath(testLicenseNewSkuId):
			return http.StatusOK, map[string]interface{}{}
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})

	d := testLicenseRefreshData(t)
	if diags := resourceLicenseAssignmentDelete(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("delete returned errors: %v", diags)
	}
	if n := len(server.requestsFor(http.MethodDelete, testLicenseUserPath(testLicenseNewSkuId))); n != 1 {
		t.Errorf("got %d deletes of the current SKU, want 1", n)
	}
}

// A failed SKU change keeps the old sku_id in state.
func TestLicenseAssignmentHTTP_failedPatchKeepsState(t *testing.T) {
	_, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.path == testDirectoryUserPath(testLicenseDirectoryUserId):
			return http.StatusOK, map[string]interface{}{"id": testLicenseDirectoryUserId, "primaryEmail": testLicenseUserId}
		case r.method == http.MethodPatch:
			return http.StatusPreconditionFailed, testLicenseError(412, "There aren't enough available licenses for the specified product-SKU pair")
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})
	ctx := testLicenseContext(t)

	r := resourceLicenseAssignment()
	state := testLicenseState(testLicenseUserId)
	config := terraform.NewResourceConfigRaw(map[string]interface{}{
		"product_id": testLicenseProductId,
		"sku_id":     testLicenseNewSkuId,
		"user_id":    testLicenseUserId,
	})
	diff, err := r.Diff(ctx, state, config, client)
	if err != nil {
		t.Fatalf("diff returned error: %v", err)
	}

	newState, diags := r.Apply(ctx, state, diff, client)
	if !diags.HasError() {
		t.Fatalf("apply returned no error, want the 412 error")
	}
	if newState == nil || newState.ID != state.ID {
		t.Fatalf("state after a failed apply = %#v, want ID %q", newState, state.ID)
	}
	if got := newState.Attributes["sku_id"]; got != testLicenseSkuId {
		t.Errorf("sku_id after a failed apply = %q, want %q", got, testLicenseSkuId)
	}
}

// The Directory API returns a different user than the stored ID: the delete
// fails, keeps the state and does not touch any license.
func TestLicenseAssignmentHTTP_deleteOwnerIdMismatch(t *testing.T) {
	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.path == testDirectoryUserPath(testLicenseDirectoryUserId):
			return http.StatusOK, map[string]interface{}{"id": "999", "primaryEmail": "other@example.com"}
		case r.method == http.MethodDelete:
			return http.StatusOK, map[string]interface{}{}
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})

	d := testLicenseRefreshData(t)
	diags := resourceLicenseAssignmentDelete(testLicenseContext(t), d, client)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "returned user 999") {
		t.Fatalf("delete diags = %v, want an owner mismatch error", diags)
	}
	if d.Id() == "" {
		t.Errorf("ID after a failed delete is empty, want the state kept")
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	for _, r := range server.requests {
		if r.method == http.MethodDelete {
			t.Errorf("unexpected delete request %s", r.path)
		}
	}
}

// After a refused optional lookup, the next optional lookups of the same
// provider configuration are skipped, also for concurrent refreshes.
func TestLicenseAssignmentHTTP_refusedDirectoryLookupSkipsOthers(t *testing.T) {
	server, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		if strings.HasPrefix(r.path, "/admin/directory/") {
			return http.StatusForbidden, testLicenseError(403, "Request had insufficient authentication scopes.")
		}
		return http.StatusOK, testLicenseAssignmentBody(testLicenseSkuId)
	})
	client.directoryLookups = &directoryLookupState{}
	ctx := testLicenseContext(t)

	var wg sync.WaitGroup
	errs := make(chan string, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := testLicenseRefreshDataWithoutDirectoryUserId(t)
			if diags := resourceLicenseAssignmentRead(ctx, d, client); diags.HasError() {
				errs <- fmt.Sprintf("read returned errors: %v", diags)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	n := 0
	for _, r := range server.requests {
		if strings.HasPrefix(r.path, "/admin/directory/") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("got %d Directory API requests, want 1 refused lookup", n)
	}
}

// With directory_user_id set, a delete whose stored SKU is not found and whose
// product listing fails treats the license as gone.
func TestLicenseAssignmentHTTP_deleteStaleSkuListError(t *testing.T) {
	_, client := newTestLicenseClient(t, func(r testLicenseRequest) (int, interface{}) {
		switch {
		case r.path == testDirectoryUserPath(testLicenseDirectoryUserId):
			return http.StatusOK, map[string]interface{}{"id": testLicenseDirectoryUserId, "primaryEmail": testLicenseUserId}
		case r.path == testLicenseProductListPath():
			return http.StatusBadRequest, testLicenseError(400, "Invalid customer")
		}
		return http.StatusNotFound, testLicenseError(404, "Not Found")
	})

	d := testLicenseRefreshData(t)
	if diags := resourceLicenseAssignmentDelete(testLicenseContext(t), d, client); diags.HasError() {
		t.Fatalf("delete returned errors: %v", diags)
	}
	if d.Id() != "" {
		t.Errorf("ID after delete = %q, want empty", d.Id())
	}
}
