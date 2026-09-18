// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
	"latere.ai/x/arca/test/stubs/issuer"
)

// The helpers of spec 008's cases, in their own file and under their own
// names. The tier is one package, so a helper another spec's file declares
// and one of these would be two declarations of one name; the prefix keeps
// them apart without either spec reading the other's file.

// The two subjects of the cases below, as the stub issuer names them.
const (
	alice = "alice"
	carol = "carol"
)

// subject renders one of them the way spec 006 renders every principal.
func (i *installation) sharesSubject(sub string) string { return i.issuer.URL() + "|" + sub }

// bearer mints a token for one of them.
func (i *installation) sharesBearer(sub string) string {
	return i.issuer.Mint(issuer.Claims{Sub: sub})
}

// call drives one request against the running binary, with a bearer when
// one is given and a JSON body when one is.
func (i *installation) sharesCall(t *testing.T, method, path, sub string, body any) (int, string, http.Header) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, i.publicURL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if sub != "" {
		req.Header.Set("Authorization", "Bearer "+i.sharesBearer(sub))
	}
	// A redemption answers a redirect for an object, and the tier reads the
	// answer rather than following it.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw), resp.Header
}

// databaseURL is the schema this run was given, read back off the
// environment the harness built. The tier writes the object rows a link
// lists through it, because the route that writes an object arrives with
// spec 005.
func (i *installation) sharesDatabaseURL(t *testing.T) string {
	t.Helper()
	for _, row := range i.env {
		if url, ok := strings.CutPrefix(row, "ARCA_DATABASE_URL="); ok {
			return url
		}
	}
	t.Fatal("the installation was started with no database")
	return ""
}

// object writes one live path into the space of the subject named.
func (i *installation) sharesObject(t *testing.T, sub, path string) store.File {
	t.Helper()
	db, err := store.Open(t.Context(), i.sharesDatabaseURL(t))
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer db.Close()
	f := store.File{
		Owner: i.sharesSubject(sub), Path: path, ObjectID: object.NewID(), CreatedBy: i.sharesSubject(sub),
		ContentType: "application/pdf", SizeBytes: 48213,
		Checksum: strings.Repeat("9", 64), ChecksumKind: object.ChecksumSHA256,
	}
	if _, err := store.NewFiles().Insert(t.Context(), db.Querier(), f); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
	return f
}

// decodeJSON reads a body into v.
func sharesDecodeJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("the body is not JSON: %v\n%s", err, body)
	}
}

// sharesLink is what a create answers.
type sharesLink struct {
	ID          string `json:"id"`
	Owner       string `json:"owner"`
	PathPrefix  string `json:"path_prefix"`
	GranteeKind string `json:"grantee_kind"`
	Permission  string `json:"permission"`
	Token       string `json:"token"`
	URL         string `json:"url"`
}
