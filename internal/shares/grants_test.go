// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/shares"
	"latere.ai/x/arca/internal/store"
)

// body is the create every case below sends, with the fields it changes.
func body(prefix, grantee, permission string) map[string]any {
	return map[string]any{
		"owner": "me", "path_prefix": prefix, "grantee": grantee, "permission": permission,
	}
}

// TestACreateAsksShareCreateCarryingThePermissionAndTheGrantee is criterion
// 3 of spec 008: the question carries what an escalation would be made of,
// so the deciding side can refuse it.
func TestACreateAsksShareCreateCarryingThePermissionAndTheGrantee(t *testing.T) {
	h := newHarness(t)
	carol := h.subject("carol")
	w := h.do(t, http.MethodPost, "/v1/shares", "alice", body("files/reports", carol, "manage"))
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /v1/shares = %d: %s", w.Code, w.Body)
	}
	asked := h.asked()
	if len(asked) != 1 {
		t.Fatalf("the route asked %d questions", len(asked))
	}
	q := asked[0]
	if q.Action != authorizer.ActionShareCreate {
		t.Errorf("the route asked %q", q.Action)
	}
	if q.Resource.Kind != authorizer.KindShare {
		t.Errorf("the resource is a %q", q.Resource.Kind)
	}
	for field, want := range map[string]string{
		"owner": h.subject("alice"), "path": "files/reports",
		"grantee": carol, "permission": "manage",
	} {
		if got := q.Resource.String(field); got != want {
			t.Errorf("the question's %s is %q, want %q", field, got, want)
		}
	}
	if q.Resource.ID != "" {
		t.Errorf("a create carries the id %q, and the grant does not exist yet", q.Resource.ID)
	}
}

// TestACreateWritesTheGrantAndAppendsOneEvent is criterion 10's half that
// belongs here: one create, one row, one event, with the grantee kind in the
// detail.
func TestACreateWritesTheGrantAndAppendsOneEvent(t *testing.T) {
	h := newHarness(t)
	carol := h.subject("carol")
	expires := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	w := h.do(t, http.MethodPost, "/v1/shares", "alice", map[string]any{
		"owner": "me", "path_prefix": "files/reports/", "grantee": carol,
		"permission": "write", "expires_at": expires.Format(time.RFC3339),
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /v1/shares = %d: %s", w.Code, w.Body)
	}
	got := grantOf(t, w)
	if got.Owner != h.subject("alice") || got.Grantee != carol {
		t.Errorf("the grant is %+v", got)
	}
	if got.PathPrefix != "files/reports" {
		t.Errorf("the prefix is %q; a trailing slash is not part of a subtree's name", got.PathPrefix)
	}
	if got.GranteeKind != store.GranteeSubject || got.Permission != "write" || got.Status != store.GrantActive {
		t.Errorf("the grant is %+v", got)
	}
	if got.Token != "" {
		t.Errorf("a subject grant answered the token %q", got.Token)
	}
	if got.ExpiresAt == nil || *got.ExpiresAt != expires.Format(time.RFC3339) {
		t.Errorf("the expiry is %v", got.ExpiresAt)
	}
	if got.CreatedBy != h.subject("alice") {
		t.Errorf("the grant was created by %q", got.CreatedBy)
	}

	entries := h.ledger.all()
	if len(entries) != 1 {
		t.Fatalf("the create appended %d events", len(entries))
	}
	e := entries[0]
	if e.Action != shares.ActionShareCreated || e.Owner != got.Owner || e.Path != "files/reports" {
		t.Errorf("the event is %+v", e)
	}
	if e.Actor != h.subject("alice") {
		t.Errorf("the event's actor is %q", e.Actor)
	}
	if e.Detail["grantee_kind"] != store.GranteeSubject || e.Detail["permission"] != "write" {
		t.Errorf("the detail is %v", e.Detail)
	}
	if h.db.commits != 1 {
		t.Errorf("the grant and its event were written in %d transactions", h.db.commits)
	}
}

// TestACreateRefusesWhatIsNotAGrant walks the request rules of spec 013 as
// this route reads them: one code per way a body can be wrong.
func TestACreateRefusesWhatIsNotAGrant(t *testing.T) {
	carol := "https://issuer.example|carol"
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	for _, c := range []struct {
		name string
		body map[string]any
		code string
	}{
		{"no subtree", body("", carol, "read"), api.CodeMissingField},
		{"a path in no plane", body("reports/q3.pdf", carol, "read"), api.CodeUnknownPlane},
		{"a relative segment", body("files/../etc", carol, "read"), api.CodeInvalidPath},
		{"an absolute path", body("/files/reports", carol, "read"), api.CodeInvalidPath},
		{"an empty segment", body("files//reports", carol, "read"), api.CodeInvalidPath},
		{"no grantee", body("files/reports", "", "read"), api.CodeMissingField},
		{"no permission", body("files/reports", carol, ""), api.CodeMissingField},
		{"a rung off the ladder", body("files/reports", carol, "own"), api.CodeInvalidField},
		{"an expiry that is not a time", map[string]any{
			"owner": "me", "path_prefix": "files/reports", "grantee": carol,
			"permission": "read", "expires_at": "tomorrow",
		}, api.CodeInvalidField},
		{"an expiry already past", map[string]any{
			"owner": "me", "path_prefix": "files/reports", "grantee": carol,
			"permission": "read", "expires_at": past,
		}, api.CodeInvalidField},
		{"a field this endpoint does not know", map[string]any{
			"owner": "me", "path_prefix": "files/reports", "grantee": carol,
			"permission": "read", "grantee_type": "principal",
		}, api.CodeUnknownField},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			w := h.do(t, http.MethodPost, "/v1/shares", "alice", c.body)
			if got := refusalOf(t, w); got != c.code {
				t.Errorf("the refusal is %q, want %q: %s", got, c.code, w.Body)
			}
			if len(h.table.grants) != 0 {
				t.Error("a refused create wrote a grant")
			}
		})
	}
}

// TestACreateRefusesABodyThatIsNotJSON holds the content rules of spec 013
// where this route meets them.
func TestACreateRefusesABodyThatIsNotJSON(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct {
		name, contentType, body, code string
	}{
		{"no content type", "", `{}`, api.CodeUnsupportedMediaType},
		{"another content type", "text/plain", `{}`, api.CodeUnsupportedMediaType},
		{"a body that is not JSON", api.JSONMediaType, `{nope`, api.CodeBadRequest},
		{"no body at all", api.JSONMediaType, ``, api.CodeBadRequest},
		{"two bodies", api.JSONMediaType, `{"owner":"me"}{"owner":"me"}`, api.CodeBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := h.raw(t, http.MethodPost, "/v1/shares", "alice", c.contentType, c.body)
			if got := refusalOf(t, w); got != c.code {
				t.Errorf("the refusal is %q, want %q: %s", got, c.code, w.Body)
			}
		})
	}
}

// TestADeniedCreateWritesNothing: the question comes before the act, so a
// deny is the whole of the answer.
func TestADeniedCreateWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.endpoint.SetRules(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: false,
		Reason: "the caller may not share this subtree"})
	w := h.do(t, http.MethodPost, "/v1/shares", "alice", body("files/reports", h.subject("carol"), "manage"))
	if got := refusalOf(t, w); got != api.CodeForbidden {
		t.Fatalf("a denied create = %q %d: %s", got, w.Code, w.Body)
	}
	if len(h.table.grants) != 0 {
		t.Error("a denied create wrote a grant")
	}
	if len(h.ledger.all()) != 0 {
		t.Error("a denied create appended an event")
	}
}

// TestTheGrantsOfASpacePageAndNarrow: the list envelope of spec 013 over the
// grants of one space.
func TestTheGrantsOfASpacePageAndNarrow(t *testing.T) {
	h := newHarness(t)
	alice := h.subject("alice")
	for i := range 3 {
		h.table.put(store.Grant{
			Owner: alice, PathPrefix: fmt.Sprintf("files/p%d", i), GranteeKind: store.GranteeSubject,
			Grantee: h.subject("carol"), Permission: "read", CreatedBy: alice,
		})
	}
	h.table.put(store.Grant{
		Owner: h.subject("bob"), PathPrefix: "files/elsewhere", GranteeKind: store.GranteeSubject,
		Grantee: alice, Permission: "read", CreatedBy: h.subject("bob"),
	})

	w := h.do(t, http.MethodGet, "/v1/shares?limit=2", "alice", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/shares = %d: %s", w.Code, w.Body)
	}
	first := pageOf[shares.Grant](t, w)
	if len(first.Entries) != 2 || first.NextCursor == "" {
		t.Fatalf("the first page is %+v", first)
	}
	w = h.do(t, http.MethodGet, "/v1/shares?limit=2&cursor="+first.NextCursor, "alice", nil)
	second := pageOf[shares.Grant](t, w)
	if len(second.Entries) != 1 || second.NextCursor != "" {
		t.Fatalf("the second page is %+v", second)
	}
	for _, g := range append(first.Entries, second.Entries...) {
		if g.Owner != alice {
			t.Errorf("the page of one space carries a grant of %q", g.Owner)
		}
	}

	narrowed := pageOf[shares.Grant](t, h.do(t, http.MethodGet, "/v1/shares?path_prefix=files/p1", "alice", nil))
	if len(narrowed.Entries) != 1 || narrowed.Entries[0].PathPrefix != "files/p1" {
		t.Fatalf("the narrowed page is %+v", narrowed)
	}
	if got := refusalOf(t, h.do(t, http.MethodGet, "/v1/shares?limit=5000", "alice", nil)); got != api.CodeInvalidField {
		t.Errorf("a page of five thousand = %q", got)
	}
}

// TestAFilterNarrowsAPageAndNeverRefusesIt is spec 013's rule for a list
// action: a selector outside the filter yields an empty page.
func TestAFilterNarrowsAPageAndNeverRefusesIt(t *testing.T) {
	h := newHarness(t)
	alice := h.subject("alice")
	h.table.put(store.Grant{
		Owner: alice, PathPrefix: "files/reports", GranteeKind: store.GranteeSubject,
		Grantee: h.subject("carol"), Permission: "read", CreatedBy: alice,
	})
	h.endpoint.SetRules(stub.Rule{
		Subject: "*", Action: authorizer.ActionShareList, Resource: "*", Allow: true,
		Filter: &authz.Filter{Owners: []string{h.subject("bob")}},
	})
	w := h.do(t, http.MethodGet, "/v1/shares", "alice", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("a filtered list = %d: %s", w.Code, w.Body)
	}
	page := pageOf[shares.Grant](t, w)
	if len(page.Entries) != 0 {
		t.Errorf("the filter admitted %d grants", len(page.Entries))
	}
}

// TestWithMeAnswersTheGranteesGrants is criterion 8: two subjects of one
// issuer, and the grantee reads what it holds and no token.
func TestWithMeAnswersTheGranteesGrants(t *testing.T) {
	h := newHarness(t)
	alice, carol := h.subject("alice"), h.subject("carol")
	h.table.put(store.Grant{
		Owner: alice, PathPrefix: "files/reports", GranteeKind: store.GranteeSubject,
		Grantee: carol, Permission: "write", CreatedBy: alice,
	})
	h.table.put(store.Grant{
		Owner: alice, PathPrefix: "files/private", GranteeKind: store.GranteeSubject,
		Grantee: alice, Permission: "manage", CreatedBy: alice,
	})
	h.table.put(store.Grant{
		Owner: alice, PathPrefix: "files/public", GranteeKind: store.GranteeLink,
		Permission: "read", Token: "t0ken", CreatedBy: alice,
	})

	w := h.do(t, http.MethodGet, "/v1/shares/with-me", "carol", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/shares/with-me = %d: %s", w.Code, w.Body)
	}
	page := pageOf[shares.Grant](t, w)
	if len(page.Entries) != 1 {
		t.Fatalf("the grantee holds %+v", page.Entries)
	}
	held := page.Entries[0]
	if held.Owner != alice || held.Permission != "write" || held.Token != "" {
		t.Errorf("what is shared with the grantee is %+v", held)
	}

	asked := h.asked()
	if len(asked) != 1 || asked[0].Action != authorizer.ActionShareList {
		t.Fatalf("the route asked %v", asked)
	}
	if got := asked[0].Resource.String("grantee"); got != carol {
		t.Errorf("the question's grantee is %q, want the caller", got)
	}
	// The owner is the caller's own space, so the built-in policy admits a
	// caller asking what it has been given.
	if got := asked[0].Resource.String("owner"); got != carol {
		t.Errorf("the question's owner is %q, want the caller", got)
	}
}

// TestReadingOneGrantAsksWithEveryFieldOfItsRow, and a grant nobody may see
// is the answer a grant that is not there gets.
func TestReadingOneGrantAsksWithEveryFieldOfItsRow(t *testing.T) {
	h := newHarness(t)
	alice, carol := h.subject("alice"), h.subject("carol")
	held := h.table.put(store.Grant{
		Owner: alice, PathPrefix: "files/reports", GranteeKind: store.GranteeSubject,
		Grantee: carol, Permission: "manage", CreatedBy: alice,
	})

	w := h.do(t, http.MethodGet, "/v1/shares/"+held.ID, "alice", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/shares/{id} = %d: %s", w.Code, w.Body)
	}
	if got := grantOf(t, w); got.ID != held.ID || got.Permission != "manage" {
		t.Errorf("the grant is %+v", got)
	}
	asked := h.asked()
	if len(asked) != 1 || asked[0].Action != authorizer.ActionShareRead {
		t.Fatalf("the route asked %v", asked)
	}
	q := asked[0].Resource
	if q.ID != held.ID || q.String("owner") != alice || q.String("path") != "files/reports" ||
		q.String("grantee") != carol || q.String("permission") != "manage" {
		t.Errorf("the question is %+v", q)
	}

	if got := refusalOf(t, h.do(t, http.MethodGet, "/v1/shares/01J8NOBODY", "alice", nil)); got != api.CodeNotFound {
		t.Errorf("a grant that is not there = %q", got)
	}
}

// TestADenyAtLookupIsTheAnswerAMissingGrantGets is invariant 6: a refused
// reference and a missing one are one answer.
func TestADenyAtLookupIsTheAnswerAMissingGrantGets(t *testing.T) {
	h := newHarness(t)
	held := h.table.put(store.Grant{
		Owner: h.subject("alice"), PathPrefix: "files/reports", GranteeKind: store.GranteeSubject,
		Grantee: h.subject("carol"), Permission: "read", CreatedBy: h.subject("alice"),
	})
	h.endpoint.SetRules(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: false, Reason: "not yours"})

	refused := h.do(t, http.MethodGet, "/v1/shares/"+held.ID, "bob", nil)
	absent := h.do(t, http.MethodGet, "/v1/shares/01J8NOBODY", "bob", nil)
	if refusalOf(t, refused) != api.CodeNotFound || refusalOf(t, absent) != api.CodeNotFound {
		t.Fatalf("a refused grant is %d and a missing one %d", refused.Code, absent.Code)
	}
}

// TestALinkIsNotReadOrRevokedThroughTheGrantRoutes: the two kinds ask two
// actions, so a link reached through a grant's route is not a grant that
// route knows.
func TestALinkIsNotReadOrRevokedThroughTheGrantRoutes(t *testing.T) {
	h := newHarness(t)
	link := h.table.put(store.Grant{
		Owner: h.subject("alice"), PathPrefix: "files/reports", GranteeKind: store.GranteeLink,
		Permission: "read", Token: "t0ken", CreatedBy: h.subject("alice"),
	})
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		w := h.do(t, method, "/v1/shares/"+link.ID, "alice", nil)
		if got := refusalOf(t, w); got != api.CodeNotFound {
			t.Errorf("%s /v1/shares/{id} on a link = %q", method, got)
		}
	}
	if held, _ := h.table.held(link.ID); held.Status != store.GrantActive {
		t.Error("the link was revoked through a route that does not know it")
	}
}

// TestARevokeTakesEffectOnTheNextCovering is criterion 7 in the unit tier:
// no sweep runs in between, and the row is kept.
func TestARevokeTakesEffectOnTheNextCovering(t *testing.T) {
	h := newHarness(t)
	alice, carol := h.subject("alice"), h.subject("carol")
	held := h.table.put(store.Grant{
		Owner: alice, PathPrefix: "files/reports", GranteeKind: store.GranteeSubject,
		Grantee: carol, Permission: "write", CreatedBy: alice,
	})
	covering, err := h.table.Covering(t.Context(), nil, alice, "files/reports/q3.pdf")
	if err != nil || len(covering) != 1 {
		t.Fatalf("before the revoke the path is covered by %+v, %v", covering, err)
	}

	w := h.do(t, http.MethodDelete, "/v1/shares/"+held.ID, "alice", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE /v1/shares/{id} = %d: %s", w.Code, w.Body)
	}
	if w.Body.Len() != 0 {
		t.Errorf("a revoke answered a body: %s", w.Body)
	}
	covering, err = h.table.Covering(t.Context(), nil, alice, "files/reports/q3.pdf")
	if err != nil || len(covering) != 0 {
		t.Fatalf("after the revoke the path is covered by %+v, %v", covering, err)
	}
	kept, ok := h.table.held(held.ID)
	if !ok || kept.Status != store.GrantRevoked {
		t.Errorf("the revoked row is %+v", kept)
	}

	entries := h.ledger.all()
	if len(entries) != 1 || entries[0].Action != shares.ActionShareRevoked {
		t.Fatalf("the revoke appended %+v", entries)
	}
	if entries[0].Detail["grantee_kind"] != store.GranteeSubject {
		t.Errorf("the detail is %v", entries[0].Detail)
	}

	// A second revoke is the same answer: the end state is the goal.
	if w := h.do(t, http.MethodDelete, "/v1/shares/"+held.ID, "alice", nil); w.Code != http.StatusNoContent {
		t.Errorf("the second revoke = %d: %s", w.Code, w.Body)
	}
}

// TestAStoreThatCannotAnswerIsNeverANotFound is invariant 2 at the handlers:
// a transient fault is a 500 and never a 404 or an empty page.
func TestAStoreThatCannotAnswerIsNeverANotFound(t *testing.T) {
	boom := errors.New("the connection went away")
	for _, c := range []struct {
		name   string
		set    func(h *harness)
		method string
		path   string
		body   map[string]any
	}{
		{"a create", func(h *harness) { h.table.failCreate = boom },
			http.MethodPost, "/v1/shares", body("files/reports", "https://issuer.example|carol", "read")},
		{"a listing", func(h *harness) { h.table.failList = boom }, http.MethodGet, "/v1/shares", nil},
		{"what is shared with the caller", func(h *harness) { h.table.failList = boom },
			http.MethodGet, "/v1/shares/with-me", nil},
		{"a read", func(h *harness) { h.table.failGet = boom }, http.MethodGet, "/v1/shares/01J8GRANT0001", nil},
		{"a revoke", func(h *harness) { h.table.failGet = boom }, http.MethodDelete, "/v1/shares/01J8GRANT0001", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			c.set(h)
			w := h.do(t, c.method, c.path, "alice", c.body)
			if got := refusalOf(t, w); got != api.CodeInternal {
				t.Errorf("a store that cannot answer = %q %d", got, w.Code)
			}
			if w.Code < 500 {
				t.Errorf("a store that cannot answer = %d", w.Code)
			}
			if body := w.Body.String(); strings.Contains(body, "connection") {
				t.Errorf("a 5xx names what went wrong inside: %s", body)
			}
		})
	}
}

// TestNewRefusesAServiceThatCannotServe: a seam missing at start is a
// wiring failure, not a refusal at a request.
func TestNewRefusesAServiceThatCannotServe(t *testing.T) {
	db := &database{table: newTable()}
	for _, c := range []struct {
		name string
		o    shares.Options
	}{
		{"no authorizer", shares.Options{DB: db, Store: newTable()}},
		{"no database", shares.Options{Authorizer: &auth.Authorizer{}, Store: newTable()}},
		{"no query set", shares.Options{Authorizer: &auth.Authorizer{}, DB: db}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := shares.New(c.o); err == nil {
				t.Error("the service built without a seam it cannot serve without")
			}
		})
	}
	s, err := shares.New(shares.Options{Authorizer: &auth.Authorizer{}, DB: db, Store: newTable()})
	if err != nil || s == nil {
		t.Fatalf("shares.New = %v", err)
	}
}

// TestAGrantMayCoverAWholePlaneAndNothingShallower: the owner of a space may
// hand out a plane, and there is nothing above one, because a path in no
// plane is a path this server does not serve.
func TestAGrantMayCoverAWholePlaneAndNothingShallower(t *testing.T) {
	h := newHarness(t)
	for _, prefix := range []string{"files", "files/", "workspaces"} {
		w := h.do(t, http.MethodPost, "/v1/shares", "alice", body(prefix, h.subject("carol"), "read"))
		if w.Code != http.StatusCreated {
			t.Errorf("a grant on %q = %d: %s", prefix, w.Code, w.Body)
		}
	}
	w := h.do(t, http.MethodPost, "/v1/shares", "alice",
		body("files/re\x00ports", h.subject("carol"), "read"))
	if got := refusalOf(t, w); got != api.CodeInvalidPath {
		t.Errorf("a path holding a control character = %q", got)
	}
}

// TestAListRouteRefusesAPageItCannotServe: the parameters of spec 013 are
// read before the question is asked, so a request that names nothing
// readable costs no decision.
func TestAListRouteRefusesAPageItCannotServe(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct{ name, path, code string }{
		{"a page of no rows", "/v1/shares?limit=0", api.CodeInvalidField},
		{"a page that is not a number", "/v1/shares?limit=many", api.CodeInvalidField},
		{"a subtree in no plane", "/v1/shares?path_prefix=reports", api.CodeUnknownPlane},
		{"a page of no rows, with me", "/v1/shares/with-me?limit=0", api.CodeInvalidField},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := h.do(t, http.MethodGet, c.path, "alice", nil)
			if got := refusalOf(t, w); got != c.code {
				t.Errorf("%s = %q, want %q", c.path, got, c.code)
			}
		})
	}
	if len(h.asked()) != 0 {
		t.Errorf("a request that names no readable page asked %d questions", len(h.asked()))
	}
}

// TestAMutationThatCannotBeRecordedIsNotAMutation: the append runs inside
// the mutation's own transaction, so a log that refuses takes the grant down
// with it. A change to who may act on a space is not a notification that may
// go missing.
func TestAMutationThatCannotBeRecordedIsNotAMutation(t *testing.T) {
	boom := errors.New("the log refused")
	h := newHarness(t)
	h.ledger.fail = boom
	w := h.do(t, http.MethodPost, "/v1/shares", "alice", body("files/reports", h.subject("carol"), "read"))
	if got := refusalOf(t, w); got != api.CodeInternal {
		t.Errorf("a create whose event could not be written = %q %d", got, w.Code)
	}
	if h.db.commits != 0 {
		t.Error("the transaction committed with no event in it")
	}

	h = newHarness(t)
	held := h.table.put(store.Grant{
		Owner: h.subject("alice"), PathPrefix: "files/reports", GranteeKind: store.GranteeSubject,
		Grantee: h.subject("carol"), Permission: "read", CreatedBy: h.subject("alice"),
	})
	h.db.failTx = boom
	if got := refusalOf(t, h.do(t, http.MethodDelete, "/v1/shares/"+held.ID, "alice", nil)); got != api.CodeInternal {
		t.Errorf("a revoke that could not be written = %q", got)
	}
}

// TestADeniedRevokeLeavesTheGrantStanding: the question comes before the act
// on this route too, and a deny at lookup is the answer a missing grant
// gets.
func TestADeniedRevokeLeavesTheGrantStanding(t *testing.T) {
	h := newHarness(t)
	held := h.table.put(store.Grant{
		Owner: h.subject("alice"), PathPrefix: "files/reports", GranteeKind: store.GranteeSubject,
		Grantee: h.subject("carol"), Permission: "read", CreatedBy: h.subject("alice"),
	})
	h.endpoint.SetRules(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: false, Reason: "not yours"})
	w := h.do(t, http.MethodDelete, "/v1/shares/"+held.ID, "mallory", nil)
	if got := refusalOf(t, w); got != api.CodeNotFound {
		t.Fatalf("a denied revoke = %q %d", got, w.Code)
	}
	if kept, _ := h.table.held(held.ID); kept.Status != store.GrantActive {
		t.Error("a denied revoke revoked the grant")
	}
	if len(h.ledger.all()) != 0 {
		t.Error("a denied revoke appended an event")
	}
}

// TestTheExpiryIsReadAgainstTheClockTheNodeGave: the service takes its
// clock, so a test decides what is already past and nothing here reads the
// wall.
func TestTheExpiryIsReadAgainstTheClockTheNodeGave(t *testing.T) {
	at := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	h := newHarness(t, func(o *shares.Options) { o.Now = func() time.Time { return at } })
	create := func(expires time.Time) *httptest.ResponseRecorder {
		return h.do(t, http.MethodPost, "/v1/shares", "alice", map[string]any{
			"owner": "me", "path_prefix": "files/reports", "grantee": h.subject("carol"),
			"permission": "read", "expires_at": expires.Format(time.RFC3339),
		})
	}
	if w := create(at.Add(time.Hour)); w.Code != http.StatusCreated {
		t.Errorf("a grant expiring after the clock = %d: %s", w.Code, w.Body)
	}
	if got := refusalOf(t, create(at.Add(-time.Second))); got != api.CodeInvalidField {
		t.Errorf("a grant expiring before the clock = %q", got)
	}
}

// TestNoLedgerRecordsNothingAndFailsAtNothing is the default of an
// installation whose log has not been bound.
func TestNoLedgerRecordsNothingAndFailsAtNothing(t *testing.T) {
	id, err := shares.NoLedger{}.Append(t.Context(), nil, shares.Event{Action: shares.ActionShareCreated})
	if err != nil || id != 0 {
		t.Errorf("NoLedger.Append = %d, %v", id, err)
	}
	h := newHarness(t, func(o *shares.Options) { o.Ledger = nil })
	if w := h.do(t, http.MethodPost, "/v1/shares", "alice",
		body("files/reports", h.subject("carol"), "read")); w.Code != http.StatusCreated {
		t.Errorf("a create with no log bound = %d: %s", w.Code, w.Body)
	}
}
