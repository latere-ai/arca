// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/shares"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// mint asks for one token grant on a subtree.
func mint(t *testing.T, h *harness, prefix, kind string) shares.Link {
	t.Helper()
	body := map[string]any{"owner": "me", "path_prefix": prefix}
	if kind != "" {
		body["kind"] = kind
	}
	w := h.do(t, http.MethodPost, "/v1/shares/links", "alice", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /v1/shares/links = %d: %s", w.Code, w.Body)
	}
	var link shares.Link
	if err := json.Unmarshal(w.Body.Bytes(), &link); err != nil {
		t.Fatalf("the body is not a link: %v\n%s", err, w.Body)
	}
	return link
}

// anObject writes one live path the link routes read.
func anObject(h *harness, path string) store.File {
	f := store.File{
		Owner: h.subject("alice"), Path: path, ObjectID: object.NewID(),
		CreatedBy: h.subject("alice"), ContentType: "application/pdf", SizeBytes: 48213,
		Checksum: strings.Repeat("9", 64), ChecksumKind: object.ChecksumSHA256,
		UpdatedAt: time.Date(2026, 9, 18, 10, 2, 11, 0, time.UTC),
	}
	h.table.file(f)
	return f
}

// TestMintingALinkAnswersTheTokenOnce: the token is the capability, and this
// is the one shape that carries it.
func TestMintingALinkAnswersTheTokenOnce(t *testing.T) {
	h := newHarness(t)
	link := mint(t, h, "files/reports", "")

	if link.GranteeKind != store.GranteeLink || link.Permission != "read" {
		t.Errorf("the link is %+v", link.Grant)
	}
	if link.Grantee != "" {
		t.Errorf("a token grant names the grantee %q; the token is the grantee", link.Grantee)
	}
	if len(link.Token) != shares.TokenLength {
		t.Errorf("the token is %d characters, want %d", len(link.Token), shares.TokenLength)
	}
	if link.URL != "/v1/shares/links/"+link.Token {
		t.Errorf("the link is redeemed at %q", link.URL)
	}

	asked := h.asked()
	if len(asked) != 1 || asked[0].Action != authorizer.ActionLinkCreate {
		t.Fatalf("the route asked %v", asked)
	}
	if got := asked[0].Resource.String("path"); got != "files/reports" {
		t.Errorf("the question's path is %q", got)
	}
	if asked[0].Resource.Kind != authorizer.KindLink {
		t.Errorf("the resource is a %q", asked[0].Resource.Kind)
	}

	entries := h.ledger.all()
	if len(entries) != 1 || entries[0].Action != shares.ActionShareCreated {
		t.Fatalf("minting a link appended %+v", entries)
	}
	if entries[0].Detail["grantee_kind"] != store.GranteeLink {
		t.Errorf("a link is a grant, and its event's detail is %v", entries[0].Detail)
	}

	// Every listing drops it. There is no route that reads a token back out
	// of the database (spec 015).
	page := pageOf[shares.Grant](t, h.do(t, http.MethodGet, "/v1/shares/links", "alice", nil))
	if len(page.Entries) != 1 {
		t.Fatalf("the space holds %+v", page.Entries)
	}
	if page.Entries[0].Token != "" {
		t.Errorf("the listing carries the token %q", page.Entries[0].Token)
	}
	if !strings.Contains(h.do(t, http.MethodGet, "/v1/shares/links", "alice", nil).Body.String(), link.ID) {
		t.Error("the listing does not name the link")
	}
}

// TestALinkThatWouldGrantMoreThanReadingIsRefused is criterion 4: a token
// grant carries read, and a create asking for more is link_read_only.
func TestALinkThatWouldGrantMoreThanReadingIsRefused(t *testing.T) {
	h := newHarness(t)
	for _, permission := range []string{"write", "manage"} {
		w := h.do(t, http.MethodPost, "/v1/shares/links", "alice", map[string]any{
			"owner": "me", "path_prefix": "files/reports", "permission": permission,
		})
		if got := refusalOf(t, w); got != api.CodeLinkReadOnly {
			t.Errorf("a link asking for %q = %q %d", permission, got, w.Code)
		}
	}
	w := h.do(t, http.MethodPost, "/v1/shares/links", "alice", map[string]any{
		"owner": "me", "path_prefix": "files/reports", "kind": "subject",
	})
	if got := refusalOf(t, w); got != api.CodeInvalidField {
		t.Errorf("a token grant of a kind that is not one = %q", got)
	}
	if len(h.table.grants) != 0 {
		t.Error("a refused create wrote a grant")
	}
}

// TestTheTokenCarriesTheEntropySpec015Requires: 256 bits from the system
// random source, rendered base64url, and never the same twice.
func TestTheTokenCarriesTheEntropySpec015Requires(t *testing.T) {
	h := newHarness(t)
	seen := map[string]bool{}
	for range 16 {
		token := mint(t, h, "files/reports", "").Token
		raw, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			t.Fatalf("the token %q is not base64url: %v", token, err)
		}
		if len(raw) != shares.TokenBytes {
			t.Fatalf("the token carries %d bytes, want %d", len(raw), shares.TokenBytes)
		}
		if seen[token] {
			t.Fatalf("the token %q was minted twice", token)
		}
		seen[token] = true
	}
}

// TestTheThreeRoutesRedeemATokenWithNoBearer is criterion 5b: a resolvable
// token asks link.read with the grant's id, its owner and the path, and an
// empty subject.
func TestTheThreeRoutesRedeemATokenWithNoBearer(t *testing.T) {
	h := newHarness(t)
	link := mint(t, h, "files/reports", "")
	object := anObject(h, "files/reports/q3.pdf")
	anObject(h, "files/reports-archive/2025.pdf")
	h.endpoint.ClearRequests()

	meta := h.do(t, http.MethodGet, "/v1/shares/links/"+link.Token+"/meta", "", nil)
	if meta.Code != http.StatusOK {
		t.Fatalf("the meta route = %d: %s", meta.Code, meta.Body)
	}
	var named struct {
		Kind       string `json:"kind"`
		Owner      string `json:"owner"`
		PathPrefix string `json:"path_prefix"`
	}
	if err := json.Unmarshal(meta.Body.Bytes(), &named); err != nil {
		t.Fatal(err)
	}
	if named.Kind != store.GranteeLink || named.Owner != h.subject("alice") || named.PathPrefix != "files/reports" {
		t.Errorf("the meta is %+v", named)
	}
	if got := meta.Header().Get(shares.HeaderReferrerPolicy); got != shares.NoReferrer {
		t.Errorf("a link response carries Referrer-Policy %q", got)
	}

	asked := h.asked()
	if len(asked) != 1 || asked[0].Action != authorizer.ActionLinkRead {
		t.Fatalf("the meta route asked %v", asked)
	}
	if asked[0].Subject != "" || asked[0].Issuer != "" {
		t.Errorf("the question carries the subject %q; a link route has none", asked[0].Subject)
	}
	if asked[0].Resource.ID != link.ID || asked[0].Resource.String("owner") != h.subject("alice") {
		t.Errorf("the question is about %+v", asked[0].Resource)
	}

	listing := h.do(t, http.MethodGet, "/v1/shares/links/"+link.Token, "", nil)
	if listing.Code != http.StatusOK {
		t.Fatalf("the listing route = %d: %s", listing.Code, listing.Body)
	}
	page := pageOf[map[string]any](t, listing)
	if len(page.Entries) != 1 {
		t.Fatalf("the listing is %+v", page.Entries)
	}
	if page.Entries[0]["path"] != object.Path {
		t.Errorf("the listing carries %v", page.Entries[0])
	}
	if page.Entries[0]["checksum"] != object.Checksum || page.Entries[0]["content_type"] != object.ContentType {
		t.Errorf("the entry is %v", page.Entries[0])
	}

	file := h.do(t, http.MethodGet, "/v1/shares/links/"+link.Token+"/files/"+object.Path, "", nil)
	if file.Code != http.StatusOK {
		t.Fatalf("the file route = %d: %s", file.Code, file.Body)
	}
	if h.reader.owner != h.subject("alice") || h.reader.path != object.Path {
		t.Errorf("the read path was asked for %q of %q", h.reader.path, h.reader.owner)
	}
	if got := file.Header().Get(shares.HeaderReferrerPolicy); got != shares.NoReferrer {
		t.Errorf("a link response carries Referrer-Policy %q", got)
	}
}

// TestATokenThatResolvesToNothingIsNotFoundBeforeAnyQuestion is criterion 5:
// an unknown, a revoked, and an expired token are one answer, and no
// authorizer is asked about any of them.
func TestATokenThatResolvesToNothingIsNotFoundBeforeAnyQuestion(t *testing.T) {
	h := newHarness(t)
	revoked := mint(t, h, "files/reports", "")
	if w := h.do(t, http.MethodDelete, "/v1/shares/links/"+revoked.ID, "alice", nil); w.Code != http.StatusNoContent {
		t.Fatalf("the revoke = %d: %s", w.Code, w.Body)
	}
	past := time.Now().Add(-time.Minute)
	expired := h.table.put(store.Grant{
		Owner: h.subject("alice"), PathPrefix: "files/reports", GranteeKind: store.GranteeLink,
		Permission: "read", Token: "t0ken-expired", CreatedBy: h.subject("alice"), ExpiresAt: &past,
	})
	_ = expired
	h.endpoint.ClearRequests()

	for _, token := range []string{"t0ken-unknown", revoked.Token, "t0ken-expired"} {
		for _, path := range []string{"", "/meta", "/files/files/reports/q3.pdf"} {
			w := h.do(t, http.MethodGet, "/v1/shares/links/"+token+path, "", nil)
			if got := refusalOf(t, w); got != api.CodeNotFound {
				t.Errorf("the token %q at %q = %q %d", token, path, got, w.Code)
			}
		}
	}
	if asked := h.asked(); len(asked) != 0 {
		t.Errorf("a token that resolves to nothing asked %v", asked)
	}
}

// TestALinkServesNothingOutsideItsPrefix is criterion 6.
func TestALinkServesNothingOutsideItsPrefix(t *testing.T) {
	h := newHarness(t)
	link := mint(t, h, "files/reports", "")
	anObject(h, "files/reports-archive/2025.pdf")
	anObject(h, "files/elsewhere.txt")
	h.endpoint.ClearRequests()

	// A relative segment and a leading slash do not reach a handler at all:
	// the router cleans the path and answers a redirect, so what is left to
	// refuse here is a path that is covered by nothing and a path no server
	// accepts.
	for _, path := range []string{
		"files/reports-archive/2025.pdf",
		"files/elsewhere.txt",
		"files/reports/%00.pdf",
	} {
		w := h.do(t, http.MethodGet, "/v1/shares/links/"+link.Token+"/files/"+path, "", nil)
		if got := refusalOf(t, w); got != api.CodeNotFound {
			t.Errorf("a path outside the grant, %q, = %q %d", path, got, w.Code)
		}
	}
	if h.reader.asked != 0 {
		t.Errorf("the read path was reached %d times for a path outside the grant", h.reader.asked)
	}
	if asked := h.asked(); len(asked) != 0 {
		t.Errorf("a path outside the grant asked %v", asked)
	}
}

// TestAnAuthorizerThatDeniesLinkReadStopsEveryLink is criterion 5b's second
// half: an installation that wants no public reading denies one action, and
// the deny is indistinguishable from a token that never existed.
func TestAnAuthorizerThatDeniesLinkReadStopsEveryLink(t *testing.T) {
	h := newHarness(t)
	link := mint(t, h, "files/reports", "")
	anObject(h, "files/reports/q3.pdf")
	h.endpoint.SetRules(stub.Rule{
		Subject: "*", Action: authorizer.ActionLinkRead, Resource: "*", Allow: false,
		Reason: "this installation serves no public links",
	})
	for _, path := range []string{"", "/meta", "/files/files/reports/q3.pdf"} {
		w := h.do(t, http.MethodGet, "/v1/shares/links/"+link.Token+path, "", nil)
		if got := refusalOf(t, w); got != api.CodeNotFound {
			t.Errorf("a denied link at %q = %q %d", path, got, w.Code)
		}
	}
	if h.reader.asked != 0 {
		t.Error("the read path ran after a deny")
	}
}

// TestRevokingALinkStopsTheNextRedemption is criterion 7 in the unit tier:
// read, revoke, read again, with no sweep in between.
func TestRevokingALinkStopsTheNextRedemption(t *testing.T) {
	h := newHarness(t)
	link := mint(t, h, "files/reports", "")
	if w := h.do(t, http.MethodGet, "/v1/shares/links/"+link.Token+"/meta", "", nil); w.Code != http.StatusOK {
		t.Fatalf("before the revoke the meta route = %d", w.Code)
	}

	w := h.do(t, http.MethodDelete, "/v1/shares/links/"+link.ID, "alice", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE /v1/shares/links/{id} = %d: %s", w.Code, w.Body)
	}
	if got := refusalOf(t, h.do(t, http.MethodGet, "/v1/shares/links/"+link.Token+"/meta", "", nil)); got != api.CodeNotFound {
		t.Errorf("after the revoke the meta route = %q", got)
	}
	entries := h.ledger.all()
	if len(entries) != 2 || entries[1].Action != shares.ActionShareRevoked {
		t.Fatalf("the revoke appended %+v", entries)
	}
	if entries[1].Detail["grantee_kind"] != store.GranteeLink {
		t.Errorf("the detail is %v", entries[1].Detail)
	}
}

// TestAPublicGrantMarksTheObjectItNames: the public kind differs from the
// link kind in two respects, and this is the second. Publicity is a property
// of the object's row, derived from the grant and never from the path.
func TestAPublicGrantMarksTheObjectItNames(t *testing.T) {
	h := newHarness(t)
	object := anObject(h, "files/reports/q3.pdf")
	link := mint(t, h, object.Path, store.GranteePublic)

	if link.GranteeKind != store.GranteePublic {
		t.Fatalf("the grant is a %q", link.GranteeKind)
	}
	if !h.table.public(object.Path) {
		t.Error("the object's row is not marked public")
	}
	key := object.ObjectID.Key("arca/")
	if public, stamped := h.bucket.stamped(key); !stamped || !public {
		t.Errorf("the bucket holds %t, %t for %q", public, stamped, key)
	}
	meta := h.do(t, http.MethodGet, "/v1/shares/links/"+link.Token+"/meta", "", nil)
	if !strings.Contains(meta.Body.String(), store.GranteePublic) {
		t.Errorf("the meta does not say the link is public: %s", meta.Body)
	}

	// The revoke clears both, and the ACL goes before the row, so a bucket
	// that will not answer leaves the grant standing rather than leaving an
	// object readable that no grant covers.
	if w := h.do(t, http.MethodDelete, "/v1/shares/links/"+link.ID, "alice", nil); w.Code != http.StatusNoContent {
		t.Fatalf("the revoke = %d: %s", w.Code, w.Body)
	}
	if h.table.public(object.Path) {
		t.Error("the object's row is still marked public")
	}
	if public, _ := h.bucket.stamped(key); public {
		t.Error("the bucket still holds the public ACL")
	}
}

// TestAPublicGrantOverASubtreeMarksNoObject: a prefix that names a subtree
// rather than one object marks nothing, because there is no one object to
// mark.
func TestAPublicGrantOverASubtreeMarksNoObject(t *testing.T) {
	h := newHarness(t)
	object := anObject(h, "files/reports/q3.pdf")
	mint(t, h, "files/reports", store.GranteePublic)
	if h.table.public(object.Path) {
		t.Error("a grant over the subtree marked an object inside it")
	}
	if _, stamped := h.bucket.stamped(object.ObjectID.Key("arca/")); stamped {
		t.Error("a grant over the subtree stamped an object inside it")
	}
}

// TestABucketThatWillNotStampIsAnUnavailableStore: the flag is not swallowed.
func TestABucketThatWillNotStampIsAnUnavailableStore(t *testing.T) {
	h := newHarness(t)
	anObject(h, "files/reports/q3.pdf")
	h.bucket.fail = errors.New("the bucket is not answering")
	w := h.do(t, http.MethodPost, "/v1/shares/links", "alice", map[string]any{
		"owner": "me", "path_prefix": "files/reports/q3.pdf", "kind": store.GranteePublic,
	})
	if got := refusalOf(t, w); got != api.CodeStorageUnavailable {
		t.Errorf("a bucket that will not stamp = %q %d", got, w.Code)
	}
}

// TestTheFileRouteNeedsTheReadPathOfSpec005: a build that binds none
// registers the route and says so, which is the phase this spec lands in.
func TestTheFileRouteNeedsTheReadPathOfSpec005(t *testing.T) {
	h := newHarness(t, func(o *shares.Options) { o.Reader = nil })
	link := mint(t, h, "files/reports", "")
	anObject(h, "files/reports/q3.pdf")
	w := h.do(t, http.MethodGet, "/v1/shares/links/"+link.Token+"/files/files/reports/q3.pdf", "", nil)
	if got := refusalOf(t, w); got != api.CodeNotImplemented {
		t.Errorf("the file route with no read path bound = %q %d", got, w.Code)
	}
}

// TestAReadPathThatRefusesIsAnsweredInTheEnvelope: the reader's refusal is
// the answer, and one that is not a refusal says nothing of itself.
func TestAReadPathThatRefusesIsAnsweredInTheEnvelope(t *testing.T) {
	h := newHarness(t)
	link := mint(t, h, "files/reports", "")
	anObject(h, "files/reports/q3.pdf")
	h.reader.fail = api.Refuse(api.CodeNotFound, "there is no object at that path")
	w := h.do(t, http.MethodGet, "/v1/shares/links/"+link.Token+"/files/files/reports/q3.pdf", "", nil)
	if got := refusalOf(t, w); got != api.CodeNotFound {
		t.Errorf("a read path that refused = %q %d", got, w.Code)
	}
}

// TestASubjectGrantIsNotRevokedThroughTheLinkRoute: the two kinds ask two
// actions, so each route knows one kind.
func TestASubjectGrantIsNotRevokedThroughTheLinkRoute(t *testing.T) {
	h := newHarness(t)
	held := h.table.put(store.Grant{
		Owner: h.subject("alice"), PathPrefix: "files/reports", GranteeKind: store.GranteeSubject,
		Grantee: h.subject("carol"), Permission: "read", CreatedBy: h.subject("alice"),
	})
	w := h.do(t, http.MethodDelete, "/v1/shares/links/"+held.ID, "alice", nil)
	if got := refusalOf(t, w); got != api.CodeNotFound {
		t.Errorf("a subject grant through the link route = %q", got)
	}
	if kept, _ := h.table.held(held.ID); kept.Status != store.StatusActive {
		t.Error("the grant was revoked through a route that does not know it")
	}
	if got := refusalOf(t, h.do(t, http.MethodDelete, "/v1/shares/links/01J8NOBODY", "alice", nil)); got != api.CodeNotFound {
		t.Errorf("a link that is not there = %q", got)
	}
}

// TestALinkRouteRefusesWhatItCannotServe walks the remaining refusals: a
// listing that cannot be paged, a subtree the store will not answer, and a
// store that cannot resolve a token.
func TestALinkRouteRefusesWhatItCannotServe(t *testing.T) {
	boom := errors.New("the connection went away")
	h := newHarness(t)
	link := mint(t, h, "files/reports", "")

	if got := refusalOf(t, h.do(t, http.MethodGet, "/v1/shares/links?limit=0", "alice", nil)); got != api.CodeInvalidField {
		t.Errorf("a page of no links = %q", got)
	}
	if got := refusalOf(t, h.do(t, http.MethodGet, "/v1/shares/links/"+link.Token+"?limit=0", "", nil)); got != api.CodeInvalidField {
		t.Errorf("a listing of no rows = %q", got)
	}
	h.table.failSubtree = boom
	w := h.do(t, http.MethodGet, "/v1/shares/links/"+link.Token, "", nil)
	if got := refusalOf(t, w); got != api.CodeInternal {
		t.Errorf("a subtree the store will not answer = %q %d", got, w.Code)
	}
	h.table.failSubtree = nil
	h.table.failList = boom
	if got := refusalOf(t, h.do(t, http.MethodGet, "/v1/shares/links", "alice", nil)); got != api.CodeInternal {
		t.Errorf("a listing the store will not answer = %q", got)
	}
	h.table.failList = nil
	h.table.failGet = boom
	if got := refusalOf(t, h.do(t, http.MethodDelete, "/v1/shares/links/"+link.ID, "alice", nil)); got != api.CodeInternal {
		t.Errorf("a link the store will not read = %q", got)
	}
}

// TestADeniedLinkCreateMintsNothing: the question comes before the act.
func TestADeniedLinkCreateMintsNothing(t *testing.T) {
	h := newHarness(t)
	h.endpoint.SetRules(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: false, Reason: "not yours"})
	w := h.do(t, http.MethodPost, "/v1/shares/links", "alice", map[string]any{
		"owner": "me", "path_prefix": "files/reports",
	})
	if got := refusalOf(t, w); got != api.CodeForbidden {
		t.Fatalf("a denied create = %q %d", got, w.Code)
	}
	if len(h.table.grants) != 0 {
		t.Error("a denied create minted a token")
	}
}

// TestALinkCreateRefusesABodyThatNamesNoSubtree: the request rules of the
// grant route hold here too, because the prefix is the same field.
func TestALinkCreateRefusesABodyThatNamesNoSubtree(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct {
		name string
		body map[string]any
		code string
	}{
		{"no subtree", map[string]any{"owner": "me"}, api.CodeMissingField},
		{"a subtree in no plane", map[string]any{"owner": "me", "path_prefix": "reports"}, api.CodeUnknownPlane},
		{"an expiry that is not a time", map[string]any{
			"owner": "me", "path_prefix": "files/reports", "expires_at": "tomorrow",
		}, api.CodeInvalidField},
		{"a field this endpoint does not know", map[string]any{
			"owner": "me", "path_prefix": "files/reports", "grantee": "somebody",
		}, api.CodeUnknownField},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := h.do(t, http.MethodPost, "/v1/shares/links", "alice", c.body)
			if got := refusalOf(t, w); got != c.code {
				t.Errorf("the refusal is %q, want %q: %s", got, c.code, w.Body)
			}
		})
	}
}

// TestALinkRouteWithNoTokenIsNotFound: the router hands a token of some
// shape to every one of the three, so this is the guard and not the rule.
func TestALinkRouteWithNoTokenIsNotFound(t *testing.T) {
	h := newHarness(t)
	w := httptest.NewRecorder()
	h.service.LinkMeta(w, httptest.NewRequest(http.MethodGet, "/v1/shares/links//meta", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("a redemption of no token = %d", w.Code)
	}
}
