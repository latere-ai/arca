// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/arca/internal/auth"
)

// TestErrorTableMatchesSpec013 reads spec 013's error table out of the spec
// and holds this package equal to it: a code the table names and the package
// lacks is an answer no handler can write, a code the package holds and the
// table lacks is an answer nobody decided on, and a status or a sentence
// that differs is a contract the document and the server disagree about.
func TestErrorTableMatchesSpec013(t *testing.T) {
	want := errorsOfSpec013(t)
	if len(want) != 30 {
		t.Fatalf("spec 013's table names %d codes, want thirty", len(want))
	}
	if got, expected := Codes(), slices.Sorted(maps.Keys(want)); !slices.Equal(got, expected) {
		t.Errorf("the package holds the codes\n %v\nthe spec names\n %v", got, expected)
	}
	for code, row := range want {
		if _, ok := errorTable[code]; !ok {
			continue
		}
		if got := Status(code); got != row.status {
			t.Errorf("%s answers %d; spec 013's row says %d", code, got, row.status)
		}
		if got := Sentence(code); got != row.sentence {
			t.Errorf("%s says\n %q\nspec 013's row says\n %q", code, got, row.sentence)
		}
	}
}

// TestEveryCodeHasOneStatusAndOneSentence: the rule spec 013 holds the table
// to. No code appears with two statuses, no sentence is empty, and no
// sentence interpolates, because a caller shows it to a person and branches
// on the code.
func TestEveryCodeHasOneStatusAndOneSentence(t *testing.T) {
	for _, code := range Codes() {
		sentence := Sentence(code)
		switch {
		case sentence == "":
			t.Errorf("%s carries no sentence", code)
		case strings.ContainsAny(sentence, "%{"):
			t.Errorf("%s interpolates: %q", code, sentence)
		case !strings.HasSuffix(sentence, "."):
			t.Errorf("%s is not a sentence: %q", code, sentence)
		}
		if status := Status(code); status < 400 || status > 599 {
			t.Errorf("%s answers %d, which is not a refusal", code, status)
		}
	}
}

// TestAnUnknownCodePanics: the table is the contract, so a handler naming a
// code nobody wrote a row for is a programming error caught in a test rather
// than a response with no sentence.
func TestAnUnknownCodePanics(t *testing.T) {
	for _, call := range []func(){
		func() { Sentence("no_such_code") },
		func() { Status("no_such_code") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("a code outside the table answered without panicking")
				}
			}()
			call()
		}()
	}
}

// TestTheEnvelopeIsTheFamilys: one code, one fixed sentence, the developer
// detail apart from it, and the request id on every refusal.
func TestTheEnvelopeIsTheFamilys(t *testing.T) {
	w, r := recorded(t, Refuse(CodePreconditionFailed,
		"If-Match named 9f2c; the object's checksum is 4d81").About("If-Match"))

	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("the status is %d, want %d", w.Code, http.StatusPreconditionFailed)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("the content type is %q", got)
	}
	body := decode(t, w)
	if body.Error.Code != CodePreconditionFailed {
		t.Errorf("the code is %q", body.Error.Code)
	}
	if body.Error.Message != Sentence(CodePreconditionFailed) {
		t.Errorf("the message is %q, not the row's sentence", body.Error.Message)
	}
	if strings.Contains(body.Error.Message, "9f2c") {
		t.Error("the user sentence names a value; everything that varies goes in details")
	}
	if body.Error.Details["request_id"] != RequestID(r.Context()) {
		t.Errorf("the request id is %v", body.Error.Details["request_id"])
	}
	if got, _ := body.Error.Details["detail"].(string); !strings.Contains(got, "9f2c") {
		t.Errorf("the developer detail is %q", got)
	}
	fields, _ := body.Error.Details["fields"].([]any)
	if len(fields) != 1 || fields[0] != "If-Match" {
		t.Errorf("the fields are %v", fields)
	}
}

// TestAnErrorWithNoRowSaysNothingOfItself: an error nobody wrote a row for
// is internal, and its own text is dropped. Nothing above 499 names a store,
// a query, a key or an internal type, even in the developer detail.
func TestAnErrorWithNoRowSaysNothingOfItself(t *testing.T) {
	w, _ := recorded(t, errors.New("pq: relation \"objects\" does not exist"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("the status is %d", w.Code)
	}
	body := decode(t, w)
	if body.Error.Code != CodeInternal {
		t.Errorf("the code is %q", body.Error.Code)
	}
	if strings.Contains(w.Body.String(), "objects") || strings.Contains(w.Body.String(), "pq:") {
		t.Errorf("a 500 named the store: %s", w.Body)
	}
	if _, ok := body.Error.Details["detail"]; ok {
		t.Error("a 500 carried a developer detail written by something that is not a refusal")
	}
	if body.Error.Details["request_id"] == nil {
		t.Error("a 500 carries no request id")
	}
}

// TestARefusalOfAnUnknownCodeIsInternal: a refusal built with a code outside
// the table is a bug, and the answer is the one code that says nothing
// rather than a body with no sentence.
func TestARefusalOfAnUnknownCodeIsInternal(t *testing.T) {
	w, _ := recorded(t, &Refusal{Code: "quota_read", Detail: "a code spec 019 retired"})
	if w.Code != http.StatusInternalServerError {
		t.Errorf("the status is %d", w.Code)
	}
	if got := decode(t, w).Error.Code; got != CodeInternal {
		t.Errorf("the code is %q", got)
	}
}

// TestFromAuthRendersTheRefusalsOfSpec006: the four codes of spec 006 are
// rows of this table under the same names, and the row of the reason table
// opens the developer detail.
func TestFromAuthRendersTheRefusalsOfSpec006(t *testing.T) {
	cases := []struct {
		name string
		code auth.Code
		want string
	}{
		{"a bearer no issuer signed", auth.CodeUnauthenticated, CodeUnauthenticated},
		{"a deny on the caller's own action", auth.CodeForbidden, CodeForbidden},
		{"a deny at lookup", auth.CodeNotFound, CodeNotFound},
		{"an authorizer that answered nothing", auth.CodeAuthorizerUnavailable, CodeAuthorizerUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FromAuth(&auth.Error{Code: c.code, Detail: "why"}).Code; got != c.want {
				t.Errorf("the code is %q, want %q", got, c.want)
			}
		})
	}
	got := FromAuth(&auth.Error{Code: auth.CodeUnauthenticated, Reason: "expired", Detail: "the bearer was refused"})
	if !strings.HasPrefix(got.Detail, "expired: ") {
		t.Errorf("the developer detail is %q and does not open with the reason", got.Detail)
	}
	if got := FromAuth(errors.New("something else")).Code; got != CodeInternal {
		t.Errorf("an error that is not a refusal read as %q", got)
	}
}

// recorded writes one refusal through the envelope, with the request id
// middleware in front so the id is on the context the way it is in a served
// request.
func recorded(t *testing.T, err error) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	a := &API{}
	w := httptest.NewRecorder()
	var served *http.Request
	a.requestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = r
		WriteError(w, r, err)
	})).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/trash", nil))
	return w, served
}

// decode reads one error envelope.
func decode(t *testing.T, w *httptest.ResponseRecorder) httpjson.ErrorEnvelope {
	t.Helper()
	var body httpjson.ErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is not an error envelope: %v\n%s", err, w.Body)
	}
	return body
}

// errorRow matches one row of spec 013's error table: the backticked code,
// the status, and the sentence, which is the rest of the line.
var errorRow = regexp.MustCompile("^\\| `([a-z_]+)` \\| (\\d{3}) \\| (.+) \\|$")

// errorsOfSpec013 reads the error table out of specs/013-api.md.
func errorsOfSpec013(t *testing.T) map[string]row {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "specs", "013-api.md"))
	if err != nil {
		t.Fatalf("spec 013: %v", err)
	}
	out := map[string]row{}
	inTable := false
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if !inTable {
			inTable = strings.HasPrefix(line, "| Code | Status | `message` |")
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		m := errorRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		status, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("spec 013's %s row names the status %q", m[1], m[2])
		}
		if _, twice := out[m[1]]; twice {
			t.Errorf("spec 013 names %s twice, so the code has two statuses", m[1])
		}
		out[m[1]] = row{status: status, sentence: strings.TrimSpace(m[3])}
	}
	if !inTable {
		t.Fatal("spec 013 has no error table")
	}
	return out
}
