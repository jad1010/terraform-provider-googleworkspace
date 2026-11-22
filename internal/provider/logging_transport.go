// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/logging"
)

// getValuesToScrub lists the JSON keys whose values are never written to the
// debug log, at any depth of a request or response body. The key match is not
// case-sensitive. "password" covers Directory API users (password) and Gmail
// send-as aliases (smtpMsa.password). The recovery email and phone of a user
// are account recovery factors. Verification codes are backup codes for
// 2-Step Verification. "passphrase", "psk" and "preSharedKey" cover Chrome
// network policy values, such as a Wi-Fi passphrase.
func getValuesToScrub() []string {
	return []string{
		"accessToken",
		"access_token",
		"refresh_token",
		"id_token",
		"client_secret",
		"private_key",
		"privateKey",
		"password",
		"newPassword",
		"recoveryEmail",
		"recoveryPhone",
		"verificationCode",
		"verificationCodes",
		"passphrase",
		"psk",
		"preSharedKey",
	}
}

// getHeadersToScrub lists the HTTP headers whose values are never written to
// the debug log. The header name match is not case-sensitive.
func getHeadersToScrub() []string {
	return []string{
		"Authorization",
		"Proxy-Authorization",
		"Cookie",
		"Set-Cookie",
		"X-Goog-Api-Key",
		"X-Goog-Iam-Authorization-Token",
	}
}

const scrubbedValue = "********"

// scrubKeyPattern matches a scrubbed key and its colon. It is used for a body
// that is not exactly one JSON value, for example a multipart body or a
// truncated body.
var scrubKeyPattern = func() *regexp.Regexp {
	keys := make([]string, 0, len(getValuesToScrub()))
	for _, key := range getValuesToScrub() {
		keys = append(keys, regexp.QuoteMeta(key))
	}
	return regexp.MustCompile(`"((?i:` + strings.Join(keys, "|") + `))"\s*:\s*`)
}()

type loggingTransport struct {
	name      string
	transport http.RoundTripper
}

func (t *loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if logging.IsDebugOrHigher() {
		t.logRequest(req)
	}

	resp, err := t.transport.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	if logging.IsDebugOrHigher() {
		t.logResponse(resp)
	}

	return resp, nil
}

func NewTransportWithScrubbedLogs(name string, t http.RoundTripper) *loggingTransport {
	return &loggingTransport{name, t}
}

// logRequest logs the scrubbed headers and body of a request. The body is
// read from req.GetBody when it is set, so req.Body stays unread. Otherwise
// req.Body is read and replaced with a copy, so the same body is sent. A
// failure is logged and never stops the request.
func (t *loggingTransport) logRequest(req *http.Request) {
	head, err := httputil.DumpRequestOut(req, false)
	if err != nil {
		log.Printf("[WARN] %s API request omitted from the debug log: %s", t.name, err)
		return
	}

	var body []byte
	switch {
	case req.Body == nil || req.Body == http.NoBody:
	case req.GetBody != nil:
		var rc io.ReadCloser
		if rc, err = req.GetBody(); err == nil {
			body, err = io.ReadAll(rc)
			rc.Close()
		}
	default:
		body, err = io.ReadAll(req.Body)
		req.Body.Close()
		req.Body = restoredBody(body, err)
	}
	if err != nil {
		log.Printf("[WARN] %s API body omitted from the debug log: %s", t.name, err)
		body = nil
	}

	log.Printf("[DEBUG] "+logReqMsg, t.name, scrubHeaders(string(head))+loggedBody(body))
}

// logResponse logs the scrubbed headers and body of a response. resp.Body is
// read and replaced with a copy, so the caller still gets the whole body. A
// failure is logged and never stops the response.
func (t *loggingTransport) logResponse(resp *http.Response) {
	head, err := httputil.DumpResponse(resp, false)
	if err != nil {
		log.Printf("[WARN] %s API response omitted from the debug log: %s", t.name, err)
		return
	}

	var body []byte
	if resp.Body != nil && resp.Body != http.NoBody {
		body, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		resp.Body = restoredBody(body, err)
		if err != nil {
			log.Printf("[WARN] %s API body omitted from the debug log: %s", t.name, err)
			body = nil
		}
	}

	log.Printf("[DEBUG] "+logRespMsg, t.name, scrubHeaders(string(head))+loggedBody(body))
}

// restoredBody returns a body that gives the bytes already read and then the
// read error, if there was one. The reader sees the same result as from the
// original body.
func restoredBody(b []byte, err error) io.ReadCloser {
	if err == nil {
		return io.NopCloser(bytes.NewReader(b))
	}
	return io.NopCloser(io.MultiReader(bytes.NewReader(b), errorReader{err}))
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

// maxLoggedBodyBytes is the number of bytes of a body that are written to the
// debug log. The caller always gets the whole body.
const maxLoggedBodyBytes = 64 << 10

// loggedBody scrubs a body for the debug log and cuts it after
// maxLoggedBodyBytes. A cut-off JSON body is scrubbed by scrubText.
func loggedBody(b []byte) string {
	if len(b) <= maxLoggedBodyBytes {
		return scrubBody(b)
	}
	return scrubBody(b[:maxLoggedBodyBytes]) + fmt.Sprintf("\n[truncated %d bytes]", len(b)-maxLoggedBodyBytes)
}

// scrubBody returns a body for the debug log. A body that is exactly one JSON
// value is scrubbed at any depth and indented. Its object keys are sorted,
// because the value is decoded into a map, but numbers and HTML characters
// are kept as they are. Any other body is scrubbed by scrubText.
func scrubBody(b []byte) string {
	if len(bytes.TrimSpace(b)) == 0 {
		return string(b)
	}

	if pretty, err := scrubJson(b); err == nil {
		return pretty
	}

	return scrubText(string(b))
}

// scrubText replaces the value after each scrubbed key in a body that is not
// exactly one JSON value, and keeps the rest of the body. See jsonValueLen
// for where a value ends.
func scrubText(s string) string {
	var out strings.Builder
	for {
		loc := scrubKeyPattern.FindStringSubmatchIndex(s)
		if loc == nil {
			out.WriteString(s)
			return out.String()
		}
		out.WriteString(s[:loc[0]])
		out.WriteString(`"` + s[loc[2]:loc[3]] + `": "` + scrubbedValue + `"`)
		s = s[loc[1]+jsonValueLen(s[loc[1]:]):]
	}
}

// jsonValueLen returns the length of the JSON value at the start of s. A
// string, array or object ends at its closing quote or bracket. If it is not
// closed, it runs to the end of s, so a cut-off value is never logged. Any
// other value ends at the next comma, closing bracket or line end.
func jsonValueLen(s string) int {
	if s == "" || !strings.ContainsRune(`"[{`, rune(s[0])) {
		if i := strings.IndexAny(s, ",]}\r\n"); i >= 0 {
			return i
		}
		return len(s)
	}

	depth, inString := 0, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case inString && c == '\\':
			i++
		case inString && c == '"':
			inString = false
			if depth == 0 {
				return i + 1
			}
		case inString:
		case c == '"':
			inString = true
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(s)
}

// scrubJson scrubs a body that is exactly one JSON value and returns it
// indented.
func scrubJson(b []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()

	var value interface{}
	if err := dec.Decode(&value); err != nil {
		return "", err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("body is not exactly one JSON value")
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(obfuscateValues(value)); err != nil {
		return "", err
	}
	return out.String(), nil
}

// scrubHeaders replaces the values of the headers in getHeadersToScrub in the
// header block of a dump. The provider logs requests before the OAuth
// transport adds the Authorization header, so this is also a safeguard
// against a change in the transport order.
func scrubHeaders(head string) string {
	lines := strings.Split(head, "\r\n")
	for i, line := range lines {
		name, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		for _, header := range getHeadersToScrub() {
			if strings.EqualFold(strings.TrimSpace(name), header) {
				lines[i] = name + ": " + scrubbedValue
				break
			}
		}
	}
	return strings.Join(lines, "\r\n")
}

// obfuscateValues replaces the values of the keys in getValuesToScrub in
// nested objects and arrays too, not only at the top level.
func obfuscateValues(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		for key, child := range v {
			if isScrubbedKey(key) {
				v[key] = scrubbedValue
			} else {
				v[key] = obfuscateValues(child)
			}
		}
		return v
	case []interface{}:
		for i, child := range v {
			v[i] = obfuscateValues(child)
		}
		return v
	default:
		return v
	}
}

func isScrubbedKey(key string) bool {
	for _, scrub := range getValuesToScrub() {
		if strings.EqualFold(key, scrub) {
			return true
		}
	}
	return false
}

const logReqMsg = `%s API Request Details:
---[ REQUEST ]---------------------------------------
%s
-----------------------------------------------------`

const logRespMsg = `%s API Response Details:
---[ RESPONSE ]--------------------------------------
%s
-----------------------------------------------------`
