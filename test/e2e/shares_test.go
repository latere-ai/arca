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
	"latere.ai/x/arca/test/stubs/authorizer"
	"latere.ai/x/arca/test/stubs/issuer"
)

// The two subjects of the cases below, as the stub issuer names them.
const (
	alice = "alice"
	carol = "carol"
)

// subject renders one of them the way spec 006 renders every principal.
func (i *installation) subject(sub string) string { return i.issuer.URL() + "|" + sub }

// bearer mints a token for one of them.
func (i *installation) bearer(sub string) string {
	return i.issuer.Mint(issuer.Claims{Sub: sub})
}

// call drives one request against the running binary, with a bearer when
// one is given and a JSON body when one is.
func (i *installation) call(t *testing.T, method, path, sub string, body any) (int, string, http.Header) {
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
		req.Header.Set("Authorization", "Bearer "+i.bearer(sub))
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
func (i *installation) databaseURL(t *testing.T) string {
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
func (i *installation) object(t *testing.T, sub, path string) store.File {
	t.Helper()
	db, err := store.Open(t.Context(), i.databaseURL(t))
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer db.Close()
	f := store.File{
		Owner: i.subject(sub), Path: path, ObjectID: object.NewID(), CreatedBy: i.subject(sub),
		ContentType: "application/pdf", SizeBytes: 48213,
		Checksum: strings.Repeat("9", 64), ChecksumKind: object.ChecksumSHA256,
	}
	if _, err := store.NewFiles().Insert(t.Context(), db.Querier(), f); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
	return f
}

// decodeJSON reads a body into v.
func decodeJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("the body is not JSON: %v\n%s", err, body)
	}
}

// link is what a create answers.
type link struct {
	ID          string `json:"id"`
	Owner       string `json:"owner"`
	PathPrefix  string `json:"path_prefix"`
	GranteeKind string `json:"grantee_kind"`
	Permission  string `json:"permission"`
	Token       string `json:"token"`
	URL         string `json:"url"`
}

// TestE2EAPublicLinkIsReadWithNoBearer is criteria 5, 5b, 6 and 7 of spec
// 008 against the running binary: a link is minted with a bearer, redeemed
// without one, refuses what it does not cover, and stops at the revoke with
// no sweep in between.
func TestE2EAPublicLinkIsReadWithNoBearer(t *testing.T) {
	i := start(t)
	i.authorizer.Allow(authorizer.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	held := i.object(t, alice, "files/reports/q3.pdf")
	i.object(t, alice, "files/reports-archive/2025.pdf")

	code, body, _ := i.call(t, http.MethodPost, "/v1/shares/links", alice, map[string]any{
		"owner": "me", "path_prefix": "files/reports",
	})
	if code != http.StatusCreated {
		t.Fatalf("POST /v1/shares/links = %d: %s", code, body)
	}
	var minted link
	decodeJSON(t, body, &minted)
	if minted.Token == "" || minted.GranteeKind != "link" || minted.Permission != "read" {
		t.Fatalf("the link is %+v", minted)
	}
	if minted.URL != "/v1/shares/links/"+minted.Token {
		t.Errorf("the link is redeemed at %q", minted.URL)
	}

	// What a viewer needs before it fetches anything, with no bearer at all.
	code, body, header := i.call(t, http.MethodGet, minted.URL+"/meta", "", nil)
	if code != http.StatusOK {
		t.Fatalf("the meta route = %d: %s", code, body)
	}
	var named struct {
		Kind       string `json:"kind"`
		Owner      string `json:"owner"`
		PathPrefix string `json:"path_prefix"`
	}
	decodeJSON(t, body, &named)
	if named.Kind != "link" || named.Owner != i.subject(alice) || named.PathPrefix != "files/reports" {
		t.Errorf("the meta is %+v", named)
	}
	if got := header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("a link response carries Referrer-Policy %q", got)
	}

	// The listing of the subtree, and nothing beside it.
	code, body, _ = i.call(t, http.MethodGet, minted.URL, "", nil)
	if code != http.StatusOK {
		t.Fatalf("the listing route = %d: %s", code, body)
	}
	var page struct {
		Entries []struct {
			Path     string `json:"path"`
			Size     int64  `json:"size"`
			Checksum string `json:"checksum"`
		} `json:"entries"`
		NextCursor string `json:"next_cursor"`
	}
	decodeJSON(t, body, &page)
	if len(page.Entries) != 1 || page.Entries[0].Path != held.Path {
		t.Fatalf("the listing is %+v", page.Entries)
	}
	if page.Entries[0].Size != held.SizeBytes || page.Entries[0].Checksum != held.Checksum {
		t.Errorf("the entry is %+v", page.Entries[0])
	}
	if page.NextCursor != "" {
		t.Errorf("the last page carries the cursor %q", page.NextCursor)
	}

	// The question the redemption asked carries the grant and no subject.
	var asked int
	for _, q := range i.authorizer.Requests() {
		if q.Action != "link.read" {
			continue
		}
		asked++
		if q.Subject != "" {
			t.Errorf("a link route asked as %q", q.Subject)
		}
		if q.Resource.ID != minted.ID || q.Resource.String("owner") != i.subject(alice) {
			t.Errorf("the question is about %+v", q.Resource)
		}
	}
	if asked == 0 {
		t.Error("a redemption asked no question")
	}

	// A path the grant does not cover, and a token nobody minted, are the
	// same answer as one another.
	for _, path := range []string{
		minted.URL + "/files/files/reports-archive/2025.pdf",
		"/v1/shares/links/t0ken-nobody-minted/meta",
	} {
		if code, body, _ := i.call(t, http.MethodGet, path, "", nil); code != http.StatusNotFound {
			t.Errorf("GET %s = %d: %s", path, code, body)
		}
	}

	// The revoke takes effect on the next request, with no sweep between.
	code, body, _ = i.call(t, http.MethodDelete, "/v1/shares/links/"+minted.ID, alice, nil)
	if code != http.StatusNoContent {
		t.Fatalf("the revoke = %d: %s", code, body)
	}
	for _, path := range []string{minted.URL, minted.URL + "/meta"} {
		code, body, _ = i.call(t, http.MethodGet, path, "", nil)
		if code != http.StatusNotFound {
			t.Errorf("after the revoke GET %s = %d: %s", path, code, body)
		}
		if !strings.Contains(body, `"not_found"`) {
			t.Errorf("the refusal is %s", body)
		}
	}
}

// TestE2EAGrantIsCreatedReadAndRevoked is criteria 8 and 9 against the
// running binary: two subjects of one issuer, the grantee reads what it
// holds, and an organization is a subject like any other.
func TestE2EAGrantIsCreatedReadAndRevoked(t *testing.T) {
	i := start(t)
	i.authorizer.Allow(authorizer.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})

	// An organization is a subject the authorizer names, and it takes the
	// same code path as a person: no group table, no membership, no claim.
	organization := i.issuer.URL() + "|org-6f2c"
	for _, grantee := range []string{i.subject(carol), organization} {
		code, body, _ := i.call(t, http.MethodPost, "/v1/shares", alice, map[string]any{
			"owner": "me", "path_prefix": "files/reports", "grantee": grantee, "permission": "write",
		})
		if code != http.StatusCreated {
			t.Fatalf("a grant to %q = %d: %s", grantee, code, body)
		}
		var g struct {
			ID          string `json:"id"`
			Grantee     string `json:"grantee"`
			GranteeKind string `json:"grantee_kind"`
			Owner       string `json:"owner"`
		}
		decodeJSON(t, body, &g)
		if g.Grantee != grantee || g.GranteeKind != "subject" || g.Owner != i.subject(alice) {
			t.Fatalf("the grant is %+v", g)
		}

		// The grant reads back by id, and the space lists it.
		code, body, _ = i.call(t, http.MethodGet, "/v1/shares/"+g.ID, alice, nil)
		if code != http.StatusOK {
			t.Fatalf("GET /v1/shares/{id} = %d: %s", code, body)
		}
	}

	// The grantee reads what it holds and nothing else, which is one query
	// on the grantee index.
	code, body, _ := i.call(t, http.MethodGet, "/v1/shares/with-me", carol, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/shares/with-me = %d: %s", code, body)
	}
	var page struct {
		Entries []struct {
			Grantee string `json:"grantee"`
			Token   string `json:"token"`
		} `json:"entries"`
	}
	decodeJSON(t, body, &page)
	if len(page.Entries) != 1 || page.Entries[0].Grantee != i.subject(carol) {
		t.Fatalf("what is shared with the grantee is %+v", page.Entries)
	}

	// The space's own listing carries both grants and no token.
	code, body, _ = i.call(t, http.MethodGet, "/v1/shares", alice, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/shares = %d: %s", code, body)
	}
	var owned struct {
		Entries []struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		} `json:"entries"`
	}
	decodeJSON(t, body, &owned)
	if len(owned.Entries) != 2 {
		t.Fatalf("the space holds %d grants", len(owned.Entries))
	}
	for _, g := range owned.Entries {
		if g.Token != "" {
			t.Errorf("the listing carries a token")
		}
	}

	// A revoke takes effect on the next read of what the grantee holds.
	code, body, _ = i.call(t, http.MethodDelete, "/v1/shares/"+owned.Entries[0].ID, alice, nil)
	if code != http.StatusNoContent {
		t.Fatalf("the revoke = %d: %s", code, body)
	}
	code, body, _ = i.call(t, http.MethodGet, "/v1/shares", alice, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/shares = %d: %s", code, body)
	}
	decodeJSON(t, body, &owned)
	if len(owned.Entries) != 1 {
		t.Errorf("after the revoke the space holds %d grants", len(owned.Entries))
	}
}

// TestE2EALinkThatWouldWriteIsRefused is criterion 4 against the binary, and
// the one row of the error table written for it.
func TestE2EALinkThatWouldWriteIsRefused(t *testing.T) {
	i := start(t)
	i.authorizer.Allow(authorizer.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	code, body, _ := i.call(t, http.MethodPost, "/v1/shares/links", alice, map[string]any{
		"owner": "me", "path_prefix": "files/reports", "permission": "write",
	})
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, `"link_read_only"`) {
		t.Fatalf("a link that would write = %d: %s", code, body)
	}
}

// TestE2EAnAuthorizerThatDeniesLinkReadStopsEveryLink: the operator's switch
// for public reading, driven twice against one running binary.
func TestE2EAnAuthorizerThatDeniesLinkReadStopsEveryLink(t *testing.T) {
	i := start(t)
	i.authorizer.Allow(authorizer.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	code, body, _ := i.call(t, http.MethodPost, "/v1/shares/links", alice, map[string]any{
		"owner": "me", "path_prefix": "files/reports",
	})
	if code != http.StatusCreated {
		t.Fatalf("POST /v1/shares/links = %d: %s", code, body)
	}
	var minted link
	decodeJSON(t, body, &minted)
	if code, body, _ := i.call(t, http.MethodGet, minted.URL+"/meta", "", nil); code != http.StatusOK {
		t.Fatalf("an allowed link = %d: %s", code, body)
	}

	// A second link, so the deny below is read from the endpoint rather than
	// from the answer the first redemption cached.
	code, body, _ = i.call(t, http.MethodPost, "/v1/shares/links", alice, map[string]any{
		"owner": "me", "path_prefix": "files/invoices",
	})
	if code != http.StatusCreated {
		t.Fatalf("POST /v1/shares/links = %d: %s", code, body)
	}
	var denied link
	decodeJSON(t, body, &denied)

	// An installation that wants no public reading denies link.read, and
	// every link in the database stops at once. The refusal is the answer a
	// token that never existed gets, so what an operator turned off is not
	// something a caller can see.
	i.authorizer.Deny(authorizer.Rule{Subject: "*", Action: "link.read", Resource: "*"},
		"this installation serves no public links")
	code, body, _ = i.call(t, http.MethodGet, denied.URL+"/meta", "", nil)
	if code != http.StatusNotFound || !strings.Contains(body, `"not_found"`) {
		t.Fatalf("a denied link = %d: %s", code, body)
	}
	// On the wire the refusal names neither the token it arrived on nor the
	// reason the endpoint gave, so a log line that records the developer
	// detail records no capability (spec 015).
	if strings.Contains(body, denied.Token) || strings.Contains(body, "serves no public links") {
		t.Fatalf("the refusal names the token or the reason: %s", body)
	}
	code, body, _ = i.call(t, http.MethodGet, "/v1/shares/links/t0ken-unknown/meta", "", nil)
	if code != http.StatusNotFound || strings.Contains(body, "t0ken-unknown") {
		t.Fatalf("an unknown token = %d: %s", code, body)
	}
}
