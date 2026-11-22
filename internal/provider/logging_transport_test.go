// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

func TestScrubBody_scrubsSecrets(t *testing.T) {
	cases := map[string]string{
		"user password":       `{"primaryEmail":"a@example.com","password":"SuperSecret1","hashFunction":"MD5"}`,
		"access token":        `{"accessToken":"SuperSecret1"}`,
		"nested send-as smtp": `{"sendAsEmail":"b@example.com","smtpMsa":{"host":"smtp.example.com","password":"SuperSecret1"}}`,
		"array of objects":    `{"users":[{"password":"SuperSecret1"},{"password":"SuperSecret1"}]}`,
		"top-level array":     `[{"password":"SuperSecret1"}]`,
		"recovery email":      `{"primaryEmail":"a@example.com","recoveryEmail":"SuperSecret1"}`,
		"recovery phone":      `{"primaryEmail":"a@example.com","recoveryPhone":"SuperSecret1"}`,
		"oauth tokens":        `{"access_token":"SuperSecret1","refresh_token":"SuperSecret1","id_token":"SuperSecret1","client_secret":"SuperSecret1","token_type":"Bearer"}`,
		"indented":            "{\n  \"primaryEmail\": \"a@example.com\",\n  \"recoveryEmail\": \"SuperSecret1\"\n}\n",
		"non-string value":    `{"password":{"value":"SuperSecret1"}}`,
		"other key case":      `{"Password":"SuperSecret1","ACCESSTOKEN":"SuperSecret1"}`,
		"new password":        `{"newPassword":"SuperSecret1"}`,
		"private keys":        `{"private_key":"SuperSecret1","privateKey":"SuperSecret1"}`,
		"verification codes":  `{"items":[{"verificationCode":"SuperSecret1"}],"verificationCodes":["SuperSecret1"]}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			out := scrubBody([]byte(body))
			if strings.Contains(out, "SuperSecret1") {
				t.Fatalf("secret value is in the log output:\n%s", out)
			}
			if !strings.Contains(out, "********") {
				t.Fatalf("expected a scrubbed value in the log output:\n%s", out)
			}
		})
	}
}

// A body that is not exactly one JSON value fails closed: the string value of
// each scrubbed key is replaced, and the rest of the body is kept.
func TestScrubBody_scrubsBodyThatIsNotOneJsonValue(t *testing.T) {
	cases := map[string]string{
		"two values":    `{"name":"keep","password":"x"}{"password":"SuperSecret1"}`,
		"multipart":     "--b\r\nContent-Type: application/json\r\n\r\n{\n \"password\": \"SuperSecret1\",\n \"name\": \"keep\"\n}\r\n--b--",
		"truncated":     `{"name":"keep","password":"Super\"Secret1"`,
		"other case":    `{"name":"keep","Password":"SuperSecret1"`,
		"number":        `{"name":"keep","password":12345678SuperSecret1,"x":1}{}`,
		"array":         "--b\r\n{\n \"name\": \"keep\",\n \"verificationCodes\": [\n  \"SuperSecret1\",\n  {\"a\": \"]SuperSecret1\"}\n ],\n \"id\": 1\n}\r\n--b--",
		"object":        `{"name":"keep","privateKey":{"pem":"SuperSecret1","n":{"x":"SuperSecret1"}}} trailing`,
		"cut off":       `{"name":"keep","password":"abcSuperSecret1`,
		"cut off array": `{"name":"keep","password":["SuperSecret1"`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			out := scrubBody([]byte(body))
			if strings.Contains(out, "Secret1") {
				t.Fatalf("secret value is in the log output:\n%s", out)
			}
			if !strings.Contains(out, `": "********"`) || !strings.Contains(out, "keep") {
				t.Fatalf("expected a scrubbed value and the other values in the log output:\n%s", out)
			}
		})
	}
}

func TestScrubBody_keepsOtherValues(t *testing.T) {
	out := scrubBody([]byte(`{"primaryEmail":"a@example.com","id":123456789012345678,"big":1e999,"sig":"<b>x</b>","smtpMsa":{"host":"smtp.example.com"}}`))
	for _, want := range []string{"a@example.com", "123456789012345678", "1e999", "<b>x</b>", "smtp.example.com"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in the log output:\n%s", want, out)
		}
	}
}

func TestScrubBody_keepsNonJsonBody(t *testing.T) {
	body := "not json\nstill not json"
	if out := scrubBody([]byte(body)); out != body {
		t.Fatalf("expected the body unchanged, got:\n%q", out)
	}
}

func TestScrubHeaders(t *testing.T) {
	head := "POST /token HTTP/1.1\r\nHost: oauth2.googleapis.com\r\nauthorization: Bearer SuperSecret1\r\n" +
		"Proxy-Authorization: Basic SuperSecret1\r\nCookie: SuperSecret1\r\nset-cookie: SuperSecret1\r\n" +
		"X-Goog-Api-Key: SuperSecret1\r\nX-GOOG-IAM-AUTHORIZATION-TOKEN: SuperSecret1\r\n\r\n"

	out := scrubHeaders(head)
	if strings.Contains(out, "SuperSecret1") {
		t.Fatalf("secret value is in the log output:\n%s", out)
	}
	for _, want := range []string{"Host: oauth2.googleapis.com", "authorization: ********", "set-cookie: ********", "X-GOOG-IAM-AUTHORIZATION-TOKEN: ********"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in the log output:\n%s", want, out)
		}
	}
}

const testUserBody = "{\n \"primaryEmail\": \"a@example.com\",\n \"recoveryEmail\": \"SuperSecret1@example.com\",\n \"recoveryPhone\": \"+1555SuperSecret1\",\n \"id\": 123456789012345678\n}\n"

const testRequestBody = `{"primaryEmail":"a@example.com","password":"SuperSecret1","smtpMsa":{"password":"SuperSecret1"}}` + "\n"

// captureDebugLog turns on SDK debug logging and returns the buffer that gets
// the standard logger output for the rest of the test.
func captureDebugLog(t *testing.T) *bytes.Buffer {
	t.Setenv("TF_LOG", "DEBUG")
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

// userHandler records the request body and sends testUserBody as a chunked
// response: the headers are flushed before the body is written.
func userHandler(gotBody *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=SuperSecret1")
		w.(http.Flusher).Flush()
		io.WriteString(w, testUserBody)
	}
}

// doScrubbedRequest sends testRequestBody through the logging transport and
// returns the response body and the debug log.
func doScrubbedRequest(t *testing.T, srv *httptest.Server) (string, string) {
	t.Helper()
	logs := captureDebugLog(t)

	c := srv.Client()
	c.Transport = NewTransportWithScrubbedLogs("Test", c.Transport)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/admin/directory/v1/users", strings.NewReader(testRequestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer SuperSecret1")
	req.Header.Set("X-Goog-Api-Key", "SuperSecret1")

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	return string(body), logs.String()
}

func assertScrubbedRoundTrip(t *testing.T, gotReqBody, gotRespBody, logs string) {
	t.Helper()
	if strings.Contains(logs, "SuperSecret1") {
		t.Fatalf("secret value is in the log output:\n%s", logs)
	}
	for _, want := range []string{"API Request Details", "API Response Details", "a@example.com", "123456789012345678", "********"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("expected %q in the log output:\n%s", want, logs)
		}
	}
	if gotReqBody != testRequestBody {
		t.Fatalf("server got request body %q, want %q", gotReqBody, testRequestBody)
	}
	if gotRespBody != testUserBody {
		t.Fatalf("caller got response body %q, want %q", gotRespBody, testUserBody)
	}
}

func TestLoggingTransport_http1Chunked(t *testing.T) {
	var gotReqBody string
	srv := httptest.NewServer(userHandler(&gotReqBody))
	defer srv.Close()

	gotRespBody, logs := doScrubbedRequest(t, srv)
	if !strings.Contains(logs, "HTTP/1.1 200 OK") || !strings.Contains(logs, "Transfer-Encoding: chunked") {
		t.Fatalf("expected a chunked HTTP/1.1 response in the log output:\n%s", logs)
	}
	assertScrubbedRoundTrip(t, gotReqBody, gotRespBody, logs)
}

func TestLoggingTransport_http2(t *testing.T) {
	var gotReqBody string
	srv := httptest.NewUnstartedServer(userHandler(&gotReqBody))
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{}
	srv.StartTLS()
	defer srv.Close()

	gotRespBody, logs := doScrubbedRequest(t, srv)
	if !strings.Contains(logs, "HTTP/2.0 200 OK") {
		t.Fatalf("expected an HTTP/2 response in the log output:\n%s", logs)
	}
	assertScrubbedRoundTrip(t, gotReqBody, gotRespBody, logs)
}

// A request without GetBody is read for the log and still sent intact.
func TestLoggingTransport_requestWithoutGetBody(t *testing.T) {
	var gotReqBody string
	srv := httptest.NewServer(userHandler(&gotReqBody))
	defer srv.Close()
	logs := captureDebugLog(t)

	req, err := http.NewRequest(http.MethodPost, srv.URL, io.NopCloser(strings.NewReader(testRequestBody)))
	if err != nil {
		t.Fatal(err)
	}
	if req.GetBody != nil {
		t.Fatal("expected a request without GetBody")
	}

	resp, err := NewTransportWithScrubbedLogs("Test", srv.Client().Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	defer resp.Body.Close()
	gotRespBody, _ := io.ReadAll(resp.Body)
	assertScrubbedRoundTrip(t, gotReqBody, string(gotRespBody), logs.String())
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// An error from reading the response body for the log does not fail the
// call. The caller reads the same bytes and the same error.
func TestLoggingTransport_responseBodyReadError(t *testing.T) {
	logs := captureDebugLog(t)
	readErr := errors.New("connection reset")
	next := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        http.Header{},
			ContentLength: -1,
			Body:          io.NopCloser(io.MultiReader(strings.NewReader(`{"password":"Super`), iotest.ErrReader(readErr))),
			Request:       req,
		}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "https://admin.googleapis.com/", nil)
	resp, err := NewTransportWithScrubbedLogs("Test", next).RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if !strings.Contains(logs.String(), "[WARN] Test API body omitted from the debug log: connection reset") {
		t.Fatalf("expected a warning in the log output:\n%s", logs)
	}
	if strings.Contains(logs.String(), "Super") {
		t.Fatalf("partial body is in the log output:\n%s", logs)
	}

	got, err := io.ReadAll(resp.Body)
	if !errors.Is(err, readErr) || string(got) != `{"password":"Super` {
		t.Fatalf("caller got %q, %v; want the partial body and the read error", got, err)
	}
}

func TestLoggingTransport_noLogWithoutDebug(t *testing.T) {
	logs := captureDebugLog(t)
	t.Setenv("TF_LOG", "")

	var gotReqBody string
	srv := httptest.NewServer(userHandler(&gotReqBody))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(testRequestBody))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := NewTransportWithScrubbedLogs("Test", srv.Client().Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	resp.Body.Close()
	if strings.Contains(logs.String(), "API Request Details") {
		t.Fatalf("expected no debug log, got:\n%s", logs)
	}
}

// A body larger than maxLoggedBodyBytes is cut in the debug log, and the
// caller still gets the whole body.
func TestLoggingTransport_truncatesLargeBody(t *testing.T) {
	logs := captureDebugLog(t)
	padding := strings.Repeat("a", maxLoggedBodyBytes)
	body := `{"name":"keep","padding":"` + padding + `","password":"SuperSecret1"}`
	next := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        http.Header{},
			ContentLength: int64(len(body)),
			Body:          io.NopCloser(strings.NewReader(body)),
			Request:       req,
		}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "https://admin.googleapis.com/", nil)
	resp, err := NewTransportWithScrubbedLogs("Test", next).RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil || string(got) != body {
		t.Fatalf("caller got %d bytes, %v; want the whole body of %d bytes", len(got), err, len(body))
	}

	out := logs.String()
	want := fmt.Sprintf("[truncated %d bytes]", len(body)-maxLoggedBodyBytes)
	if !strings.Contains(out, want) || !strings.Contains(out, "keep") {
		t.Fatalf("expected %q and the start of the body in the log output", want)
	}
	if strings.Contains(out, "SuperSecret1") || len(out) > maxLoggedBodyBytes+4096 {
		t.Fatalf("log output has %d bytes or the secret, want the cut body only", len(out))
	}
}
