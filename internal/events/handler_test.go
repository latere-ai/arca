// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package events

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/arca/internal/store"
)

// fakeGuard is the seam internal/auth fills after spec 006 lands: it answers
// the caller the verifier left on the request and the decision the
// authorizer gave.
type fakeGuard struct {
	caller   string
	decision authz.Decision
	err      error

	action   string
	resource authz.Resource
	asked    int
}

func (g *fakeGuard) Caller(*http.Request) string { return g.caller }

func (g *fakeGuard) Ask(_ *http.Request, action string, resource authz.Resource) (authz.Decision, error) {
	g.asked++
	g.action, g.resource = action, resource
	return g.decision, g.err
}

// fakeLog answers the page the case set and records the query it was asked.
type fakeLog struct {
	Log
	page   Page
	err    error
	query  Query
	tailed int
}

func (l *fakeLog) Tail(_ context.Context, _ store.Querier, t Query) (Page, error) {
	l.tailed++
	l.query = t
	return l.page, l.err
}

// allowed is the answer of an authorizer that says yes and narrows nothing.
func allowed() authz.Decision { return authz.Decision{Allow: true, TTL: authz.DefaultTTL} }

// ask drives the handler over one query string and answers the response.
func ask(t *testing.T, log Log, g Guard, query string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/events?"+query, nil)
	w := httptest.NewRecorder()
	Handler(log, &fakeQuerier{}, g).ServeHTTP(w, r)
	return w
}

// errorOf reads the code of an error envelope.
func errorOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is %q: %v", w.Body.String(), err)
	}
	if body.Error.Message == "" {
		t.Fatalf("the error carries no sentence: %q", w.Body.String())
	}
	return body.Error.Code
}

func TestTheTailRefusesARequestTheVerifierLeftNoCallerOn(t *testing.T) {
	w := ask(t, &fakeLog{}, &fakeGuard{}, "")
	if w.Code != http.StatusUnauthorized || errorOf(t, w) != "unauthenticated" {
		t.Fatalf("a request with no caller answered %d %s", w.Code, w.Body)
	}
}

func TestTheTailAsksEventReadAboutTheSpaceTheCallerNamed(t *testing.T) {
	for _, c := range []struct {
		name  string
		query string
		want  string
	}{
		{"the caller's own space by default", "", aSpace},
		{"the caller's own space by alias", "owner=me", aSpace},
		{"another space", "owner=" + anotherSpace, anotherSpace},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := &fakeGuard{caller: aSpace, decision: allowed()}
			log := &fakeLog{}
			if w := ask(t, log, g, c.query); w.Code != http.StatusOK {
				t.Fatalf("the read answered %d %s", w.Code, w.Body)
			}
			if g.asked != 1 {
				t.Fatalf("the route asked %d times", g.asked)
			}
			if g.action != "event.read" || g.resource.Kind != "Event" {
				t.Fatalf("the route asked %q of a %q", g.action, g.resource.Kind)
			}
			if g.resource.String("owner") != c.want {
				t.Fatalf("the question named %q and the case wants %q", g.resource.String("owner"), c.want)
			}
			if log.query.Owner != c.want {
				t.Fatalf("the query read %q", log.query.Owner)
			}
		})
	}
}

func TestTheTailRefusesACursorOrALimitItCannotRead(t *testing.T) {
	for _, c := range []struct{ name, query string }{
		{"a cursor that is no id", "cursor=yesterday"},
		{"a cursor before the start of the log", "cursor=-1"},
		{"a limit that is no number", "limit=all"},
		{"a limit of nothing", "limit=0"},
		{"a limit above the cap", "limit=5000"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := &fakeGuard{caller: aSpace, decision: allowed()}
			log := &fakeLog{}
			w := ask(t, log, g, c.query)
			if w.Code != http.StatusBadRequest || errorOf(t, w) != "invalid_field" {
				t.Fatalf("the request answered %d %s", w.Code, w.Body)
			}
			// A request that cannot be read is refused before anything is
			// asked of the authorizer or of the store.
			if g.asked != 0 || log.tailed != 0 {
				t.Fatal("a request that could not be read reached the authorizer or the store")
			}
		})
	}
}

func TestTheTailReadsTheCursorAndTheLimitItWasGiven(t *testing.T) {
	log := &fakeLog{}
	if w := ask(t, log, &fakeGuard{caller: aSpace, decision: allowed()}, "cursor=41822&limit=7"); w.Code != http.StatusOK {
		t.Fatalf("the read answered %d %s", w.Code, w.Body)
	}
	if log.query.Cursor != 41822 || log.query.Limit != 7 {
		t.Fatalf("the query reads %+v", log.query)
	}
	log = &fakeLog{}
	_ = ask(t, log, &fakeGuard{caller: aSpace, decision: allowed()}, "")
	if log.query.Limit != DefaultLimit {
		t.Fatalf("a request naming no limit read %d rows", log.query.Limit)
	}
}

func TestTheTailFailsClosedOnAnAuthorizerThatAnswersNothing(t *testing.T) {
	g := &fakeGuard{caller: aSpace, err: errors.New("the endpoint timed out")}
	log := &fakeLog{}
	w := ask(t, log, g, "")
	if w.Code != http.StatusServiceUnavailable || errorOf(t, w) != "authorizer_unavailable" {
		t.Fatalf("an authorizer that answered nothing gave %d %s", w.Code, w.Body)
	}
	if log.tailed != 0 {
		t.Fatal("the log was read without a decision")
	}
}

func TestTheTailRefusesADeny(t *testing.T) {
	g := &fakeGuard{caller: aSpace, decision: authz.Decision{Reason: "the caller is not a member"}}
	log := &fakeLog{}
	w := ask(t, log, g, "owner="+anotherSpace)
	if w.Code != http.StatusForbidden || errorOf(t, w) != "forbidden" {
		t.Fatalf("a deny answered %d %s", w.Code, w.Body)
	}
	if log.tailed != 0 {
		t.Fatal("the log was read after a deny")
	}
}

// Criterion 10 of spec 010: an answer carrying a filter narrows the page,
// and the handler scopes by nothing else. A selector outside the filter is
// an empty page and never a refusal (spec 013).
func TestEventFilter(t *testing.T) {
	for _, c := range []struct {
		name   string
		filter *authz.Filter
		owner  string
		tailed int
	}{
		{"no filter", nil, aSpace, 1},
		{"a filter naming no owner", &authz.Filter{}, aSpace, 1},
		{"a filter that names the space", &authz.Filter{Owners: []string{aSpace}}, aSpace, 1},
		{"a filter that does not", &authz.Filter{Owners: []string{aSpace}}, anotherSpace, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := allowed()
			d.Filter = c.filter
			log := &fakeLog{page: Page{Entries: pageOf(1)}}
			w := ask(t, log, &fakeGuard{caller: aSpace, decision: d}, "owner="+c.owner)
			if w.Code != http.StatusOK {
				t.Fatalf("the read answered %d %s", w.Code, w.Body)
			}
			if log.tailed != c.tailed {
				t.Fatalf("the log was read %d times and the case wants %d", log.tailed, c.tailed)
			}
			var body envelope
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if c.tailed == 0 && len(body.Entries) != 0 {
				t.Fatalf("a selector outside the filter answered %d entries", len(body.Entries))
			}
		})
	}
}

func TestTheTailRendersTheEnvelopeOfSpec013(t *testing.T) {
	held := Event{ID: 41822, Owner: aSpace, Path: "files/reports/q3.pdf", Action: ActionPut,
		Actor: aSpace, Detail: map[string]any{"size": float64(48213)}, At: aMoment}
	log := &fakeLog{page: Page{Entries: []Event{held}, NextCursor: 41822}}
	w := ask(t, log, &fakeGuard{caller: aSpace, decision: allowed()}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("the read answered %d %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("the page is served as %q", ct)
	}
	var body struct {
		Entries []struct {
			ID     int64           `json:"id"`
			Action string          `json:"action"`
			Owner  string          `json:"owner"`
			Path   string          `json:"path"`
			Actor  string          `json:"actor"`
			Detail json.RawMessage `json:"detail"`
			At     string          `json:"at"`
		} `json:"entries"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is %q: %v", w.Body.String(), err)
	}
	got := body.Entries[0]
	if got.ID != 41822 || got.Action != "put" || got.Owner != aSpace || got.Path != held.Path || got.Actor != aSpace {
		t.Fatalf("the entry reads %+v", got)
	}
	if string(got.Detail) != `{"size":48213}` {
		t.Fatalf("the detail reads %s", got.Detail)
	}
	if got.At != aMoment.Format(time.RFC3339) {
		t.Fatalf("the time reads %q", got.At)
	}
	if body.NextCursor != "41822" {
		t.Fatalf("the cursor reads %q, and a consumer sends it back as it stands", body.NextCursor)
	}
}

func TestTheTailAnswersAnArrayAndNoCursorOnTheLastPage(t *testing.T) {
	log := &fakeLog{page: Page{Entries: nil}}
	w := ask(t, log, &fakeGuard{caller: aSpace, decision: allowed()}, "")
	if w.Body.String() != "{\"entries\":[]}\n" {
		t.Fatalf("an empty last page reads %q, and entries is never null", w.Body.String())
	}
}

func TestTheTailAnswersAStoreThatDidNotAnswer(t *testing.T) {
	log := &fakeLog{err: errors.New("the connection went away")}
	w := ask(t, log, &fakeGuard{caller: aSpace, decision: allowed()}, "")
	if w.Code != http.StatusServiceUnavailable || errorOf(t, w) != "storage_unavailable" {
		t.Fatalf("a store that did not answer gave %d %s", w.Code, w.Body)
	}
	// Nothing above 499 names a store, a query, or a key (spec 013).
	for _, word := range []string{"events", "SELECT", "connection", "pgx"} {
		if strings.Contains(w.Body.String(), word) {
			t.Fatalf("the body names %q: %s", word, w.Body)
		}
	}
}

const anotherSpace = "https://issuer.example|9ab3c4d5"
