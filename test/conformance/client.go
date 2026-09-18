// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The one chokepoint every request of the suite goes through, and the one
// place every assertion fails through. The structures below are the suite's
// own, never the server's: a rename inside the server's packages that
// changes the wire fails a case here instead of compiling.

// request is one call of the surface.
type request struct {
	method, path string
	// body is the request body. A JSON route takes a string; the object
	// routes of spec 005 take bytes, which is why this is a reader and the
	// helpers below pass a string through.
	body io.Reader
	// contentType is sent when it is set, and application/json is sent for
	// a JSON body when it is not.
	contentType string
	// subject names the principal whose bearer is sent. Empty sends none,
	// which is what the anonymous cases and the three public link routes
	// need.
	subject string
	header  map[string]string
}

// response is one HTTP answer with its body decoded when it is JSON.
type response struct {
	status int
	header http.Header
	body   []byte
	json   map[string]any
}

// call sends a request as the subject with a JSON body when one is given.
func (s *session) call(t testing.TB, subject, method, path, body string) response {
	t.Helper()
	r := request{method: method, path: path, subject: subject}
	if body != "" {
		r.body = strings.NewReader(body)
		r.contentType = "application/json"
	}
	return s.do(t, r)
}

// with sends a request as the subject carrying extra headers, which is how
// the conditional requests of spec 013 are driven.
func (s *session) with(t testing.TB, subject, method, path, body string, header map[string]string) response {
	t.Helper()
	r := request{method: method, path: path, subject: subject, header: header}
	if body != "" {
		r.body = strings.NewReader(body)
		r.contentType = "application/json"
	}
	return s.do(t, r)
}

// do sends one request and reads the whole answer. A path that is already a
// URL is sent as it is, which is how a presigned URL and a stub's control
// API go through the same client.
func (s *session) do(t testing.TB, r request) response {
	t.Helper()
	url := r.path
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = s.options.URL + r.path
	}
	req, err := http.NewRequestWithContext(s.context(), r.method, url, r.body)
	failIf(t, err != nil, "build %s %s: %v", r.method, r.path, err)
	if r.subject != "" {
		req.Header.Set("Authorization", "Bearer "+s.bearer(t, r.subject))
	}
	if r.contentType != "" {
		req.Header.Set("Content-Type", r.contentType)
	}
	for k, v := range r.header {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	failIf(t, err != nil, "%s %s: %v", r.method, r.path, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	failIf(t, err != nil, "read %s %s: %v", r.method, r.path, err)
	out := response{status: resp.StatusCode, header: resp.Header, body: raw}
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		_ = json.Unmarshal(raw, &out.json)
	}
	return out
}

// bearer answers the subject's token, minted once per run: a target whose
// Token mints a fresh one per call is asked once, so a case reads the same
// principal the case before it did.
func (s *session) bearer(t testing.TB, subject string) string {
	t.Helper()
	s.mu.Lock()
	token, held := s.tokens[subject]
	s.mu.Unlock()
	if held {
		return token
	}
	token, err := s.options.Token(s.context(), subject)
	failIf(t, err != nil, "mint a token for %q: %v", subject, err)
	failIf(t, token == "", "the target minted an empty token for %q", subject)
	s.mu.Lock()
	s.tokens[subject] = token
	s.mu.Unlock()
	return token
}

// code is the error code of an envelope body, empty when the body is not
// one.
func (r response) code() string {
	e, _ := r.json["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// message is the fixed sentence of an envelope body.
func (r response) message() string {
	e, _ := r.json["error"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}

// details is the details object of an envelope body.
func (r response) details() map[string]any {
	e, _ := r.json["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	return d
}

// expectStatus asserts a status and that the request id spec 013 requires
// came back.
func expectStatus(t testing.TB, r response, status int) response {
	t.Helper()
	failIf(t, r.status != status, "status %d, want %d: %s", r.status, status, r.body)
	failIf(t, r.header.Get(HeaderRequestID) == "", "the answer carries no %s", HeaderRequestID)
	return r
}

// expectError asserts a refusal: the code under the status spec 013's table
// fixes, with that table's sentence and nothing interpolated into it, and
// the request id in the details. It answers the details, so a case can read
// the fields a field naming code carries.
func expectError(t testing.TB, r response, code string) map[string]any {
	t.Helper()
	status, sentence := table(t, code)
	failIf(t, r.status != status || r.code() != code || r.message() != sentence,
		"want %d %s %q, got %d %s %q: %s",
		status, code, sentence, r.status, r.code(), r.message(), r.body)
	details := r.details()
	id, _ := details["request_id"].(string)
	failIf(t, id == "", "the refusal carries no details.request_id: %s", r.body)
	failIf(t, r.header.Get(HeaderRequestID) == "", "the refusal carries no %s", HeaderRequestID)
	return details
}

// expectFields asserts a field naming refusal carries the field paths at
// fault.
func expectFields(t testing.TB, details map[string]any) []string {
	t.Helper()
	raw, ok := details["fields"].([]any)
	failIf(t, !ok || len(raw) == 0, "the refusal names no details.fields: %v", details)
	fields := make([]string, 0, len(raw))
	for _, f := range raw {
		s, _ := f.(string)
		fields = append(fields, s)
	}
	return fields
}

// failIf fails the case with the message when the condition holds: the one
// place every assertion of the suite fails through.
func failIf(t testing.TB, failed bool, format string, args ...any) {
	t.Helper()
	if failed {
		t.Fatalf(format, args...)
	}
}

// The readers a case takes a value out of a decoded body with. Each answers
// the zero value for a field that is absent or of another type, so a case
// asserts the value it wanted rather than panicking on a shape it did not
// get.

// str is a string field of an object.
func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// num is a number field of an object, as an int64: every byte count of spec
// 013 is a JSON number of int64 range.
func num(m map[string]any, key string) int64 {
	v, _ := m[key].(float64)
	return int64(v)
}

// obj is an object field of an object.
func obj(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

// list is an array field of an object as objects, which is what every page
// of spec 013's envelope holds.
func list(m map[string]any, key string) []map[string]any {
	raw, _ := m[key].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		if o, ok := e.(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}

// name is a name for something this run creates: the suite's prefix, the
// run's own value, and the label the case gave. Nothing a run creates is
// named anything else, which is what makes two runs against one
// installation safe.
func (s *session) name(label string) string { return Prefix + s.run + "-" + label }

// drawRunID answers the value that makes a run's names its own. It is drawn
// rather than derived from the clock, so two runs started in the same
// millisecond still differ.
func drawRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// fields is a JSON request body as a case writes it: the field names spec
// 013 fixes against the values this case means. A map rather than a pair
// list, so a key with no value cannot be written at all.
type fields = map[string]any

// body renders a request body from the fields a case named.
func body(f fields) string {
	raw, err := json.Marshal(f)
	if err != nil {
		panic("conformance: a body that cannot be rendered: " + err.Error())
	}
	return string(raw)
}
