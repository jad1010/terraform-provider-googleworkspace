package googleworkspace

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

const testUserHTTPID = "123456789012345678901"

type testUserRequest struct {
	method string
	path   string
	body   map[string]interface{}
}

// testUserDirectoryServer is a fake of the Directory API users endpoints. It
// records each request. PUT returns putStatus. GET returns the user with the
// etag "etag-1", or 304 when the request sends that etag in If-None-Match.
type testUserDirectoryServer struct {
	mu        sync.Mutex
	requests  []testUserRequest
	putStatus int
}

func (s *testUserDirectoryServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req := testUserRequest{method: r.Method, path: r.URL.Path}
	if data, _ := io.ReadAll(r.Body); len(data) > 0 {
		_ = json.Unmarshal(data, &req.body)
	}

	s.mu.Lock()
	s.requests = append(s.requests, req)
	putStatus := s.putStatus
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	if r.Method == http.MethodPut && putStatus != http.StatusOK {
		w.WriteHeader(putStatus)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Invalid Password"}}`))
		return
	}

	if r.Method == http.MethodGet && r.Header.Get("If-None-Match") == `"etag-1"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"id":           testUserHTTPID,
		"primaryEmail": "user@example.com",
		"etag":         `"etag-1"`,
		"name":         map[string]interface{}{"familyName": "Scott", "givenName": "Michael"},
	})
}

func (s *testUserDirectoryServer) requestsFor(method string) []testUserRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	var result []testUserRequest
	for _, r := range s.requests {
		if r.method == method {
			result = append(result, r)
		}
	}

	return result
}

// testUserRedirectTransport sends every request to the fake server.
type testUserRedirectTransport struct {
	target *url.URL
}

func (rt testUserRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Scheme = rt.target.Scheme
	r.URL.Host = rt.target.Host
	r.Host = rt.target.Host

	return http.DefaultTransport.RoundTrip(r)
}

func newTestUserDirectoryClient(t *testing.T, putStatus int) (*testUserDirectoryServer, *apiClient) {
	t.Helper()

	server := &testUserDirectoryServer{putStatus: putStatus}
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)

	target, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}

	return server, &apiClient{client: &http.Client{Transport: testUserRedirectTransport{target: target}}}
}

// testUserHTTPApply plans the change from the prior state attributes to the
// configuration with the real resource schema, and applies it with the real
// update function against the fake server. With emptyPlan, it applies an empty
// diff, as the SDK does when the planned state equals the prior state.
func testUserHTTPApply(t *testing.T, client *apiClient, emptyPlan bool, stateAttrs map[string]string, configAttrs map[string]cty.Value) (*terraform.InstanceState, error) {
	t.Helper()

	r := resourceUser()

	cfg := map[string]cty.Value{
		"primary_email": cty.StringVal("user@example.com"),
		"name":          userRawName("Scott", "Michael"),
	}
	for k, v := range configAttrs {
		cfg[k] = v
	}
	rawConfig := userRawConfig(cfg)

	attrs := map[string]string{
		"id":                 testUserHTTPID,
		"primary_email":      "user@example.com",
		"name.#":             "1",
		"name.0.family_name": "Scott",
		"name.0.given_name":  "Michael",
	}
	for k, v := range stateAttrs {
		attrs[k] = v
	}
	state := &terraform.InstanceState{ID: testUserHTTPID, Attributes: attrs, RawConfig: rawConfig}

	config := terraform.NewResourceConfigShimmed(rawConfig, r.CoreConfigSchema())
	if diags := r.Validate(config); diags.HasError() {
		t.Fatalf("unexpected validation error: %+v", diags)
	}

	var instanceDiff *terraform.InstanceDiff
	if !emptyPlan {
		var err error
		instanceDiff, err = r.Diff(context.Background(), state, config, client)
		if err != nil {
			t.Fatalf("Diff: %s", err)
		}
	}
	if instanceDiff == nil {
		instanceDiff = &terraform.InstanceDiff{Attributes: map[string]*terraform.ResourceAttrDiff{}}
	}
	instanceDiff.RawConfig = rawConfig

	newState, diags := r.Apply(context.Background(), state, instanceDiff, client)
	if diags.HasError() {
		return newState, checkDiags(diags)
	}

	return newState, nil
}

func TestResourceUserHTTP_passwordWORotationSendsHashFunction(t *testing.T) {
	t.Parallel()

	server, client := newTestUserDirectoryClient(t, http.StatusOK)

	hashed := "34819d7beeabb9260a5c854bc85b3e44"
	_, err := testUserHTTPApply(t, client, false,
		map[string]string{"password_wo_version": "1", "hash_function": "MD5"},
		map[string]cty.Value{
			"password_wo":         cty.StringVal(hashed),
			"password_wo_version": cty.NumberIntVal(2),
			"hash_function":       cty.StringVal("MD5"),
		})
	if err != nil {
		t.Fatalf("Apply: %s", err)
	}

	puts := server.requestsFor(http.MethodPut)
	if len(puts) != 1 {
		t.Fatalf("expected 1 PUT request, got %d: %+v", len(puts), puts)
	}

	if puts[0].path != "/admin/directory/v1/users/"+testUserHTTPID {
		t.Errorf("unexpected PUT path %q", puts[0].path)
	}
	if got := puts[0].body["password"]; got != hashed {
		t.Errorf("PUT body password = %v, want the new password_wo value", got)
	}
	if got := puts[0].body["hashFunction"]; got != "MD5" {
		t.Errorf("PUT body hashFunction = %v, want MD5", got)
	}
}

func TestResourceUserHTTP_failedUpdateKeepsPriorState(t *testing.T) {
	t.Parallel()

	server, client := newTestUserDirectoryClient(t, http.StatusBadRequest)

	newState, err := testUserHTTPApply(t, client, false,
		map[string]string{"password_wo_version": "1"},
		map[string]cty.Value{
			"password_wo":         cty.StringVal("new-write-only"),
			"password_wo_version": cty.NumberIntVal(2),
		})
	if err == nil {
		t.Fatal("expected an error from the rejected update")
	}

	if len(server.requestsFor(http.MethodPut)) != 1 {
		t.Fatalf("expected 1 PUT request, got %+v", server.requestsFor(http.MethodPut))
	}

	if newState == nil {
		t.Fatal("expected a state after the failed update")
	}
	if got := newState.Attributes["password_wo_version"]; got != "1" {
		t.Errorf("password_wo_version in state = %q, want the prior value 1, so that the next apply sends the password again", got)
	}
}

func TestResourceUserHTTP_noAttributeChangeSendsNothing(t *testing.T) {
	t.Parallel()

	server, client := newTestUserDirectoryClient(t, http.StatusOK)

	// The configuration matches the prior state, and the SDK computes an empty
	// diff. Terraform plans an update like this when only the sensitivity of
	// attributes changed.
	_, err := testUserHTTPApply(t, client, true,
		map[string]string{
			"password_wo_version": "1",
			"recovery_email":      "recovery@example.com",
		},
		map[string]cty.Value{
			"password_wo":         cty.StringVal("write-only"),
			"password_wo_version": cty.NumberIntVal(1),
			"recovery_email":      cty.StringVal("recovery@example.com"),
		})
	if err != nil {
		t.Fatalf("Apply: %s", err)
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.requests) != 0 {
		t.Errorf("expected no API requests, got %+v", server.requests)
	}
}
