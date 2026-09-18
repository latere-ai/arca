// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

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
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/events"
	"latere.ai/x/arca/internal/store"
)

// fakeLog is the log of spec 010 as a test of the frame needs it: it answers
// the page the case set and records the query it was asked. What the real one
// does with a database is internal/events' own tier.
type fakeLog struct {
	events.Log
	page   events.Page
	err    error
	query  events.Query
	tailed int
}

func (l *fakeLog) Tail(_ context.Context, _ store.Querier, q events.Query) (events.Page, error) {
	l.tailed++
	l.query = q
	return l.page, l.err
}

// withLog wires one log into a harness.
func withLog(log events.Log) func(*Options) {
	return func(o *Options) { o.Events = log }
}

// aMoment is the time an entry of the fixtures below was written.
var aMoment = time.Date(2026, 9, 18, 10, 30, 0, 0, time.UTC)

// TestTheEventTailAnswersThroughTheFrame is the binding of spec 010 into the
// surface: the route registered at /v1/events runs behind the verifier, asks
// event.read, and answers the list envelope of spec 013 with the request id
// every other answer carries.
func TestTheEventTailAnswersThroughTheFrame(t *testing.T) {
	log := &fakeLog{}
	h := newHarness(t, withLog(log))
	log.page = events.Page{
		Entries: []events.Event{
			{ID: 41821, Owner: h.subject(), Path: "files/reports/q3.pdf", Action: events.ActionPut, At: aMoment},
			{ID: 41822, Owner: h.subject(), Action: events.ActionReap, At: aMoment},
		},
		NextCursor: 41822,
	}
	h.endpoint.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})

	w := h.do(t, http.MethodGet, "/v1/events?limit=2", h.bearer())
	if w.Code != http.StatusOK {
		t.Fatalf("the tail answered %d: %s", w.Code, w.Body)
	}
	if w.Header().Get(Header) == "" {
		t.Error("the page carries no request id")
	}
	var page struct {
		Entries []struct {
			ID     int64  `json:"id"`
			Action string `json:"action"`
			Owner  string `json:"owner"`
		} `json:"entries"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("the body is %q: %v", w.Body, err)
	}
	if len(page.Entries) != 2 || page.Entries[0].ID != 41821 || page.Entries[0].Action != "put" {
		t.Fatalf("the page reads %+v", page.Entries)
	}
	if page.NextCursor != "41822" {
		t.Errorf("the cursor reads %q, and a consumer sends it back as it stands", page.NextCursor)
	}
	// The space the question named and the space the query read are the
	// caller's own, because the request named no owner.
	if log.query.Owner != h.subject() || log.query.Limit != 2 {
		t.Errorf("the query reads %+v", log.query)
	}
	asked := h.endpoint.Requests()
	if len(asked) != 1 || asked[0].Action != authorizer.ActionEventRead {
		t.Fatalf("the route asked %v", asked)
	}
	if asked[0].Resource.String("owner") != h.subject() {
		t.Errorf("the question named the space %q", asked[0].Resource.String("owner"))
	}
}

// TestADeniedEventReadIsForbidden is spec 010's divergence from invariant 6:
// the route names a space and not an object, so a deny is 403 and not 404,
// and it is the frame's envelope that says so.
func TestADeniedEventReadIsForbidden(t *testing.T) {
	log := &fakeLog{}
	h := newHarness(t, withLog(log))
	h.endpoint.Deny(stub.Rule{Subject: "*", Action: "*", Resource: "*"}, "the caller is not a member")

	w := h.do(t, http.MethodGet, "/v1/events?owner="+anotherSpace, h.bearer())
	if w.Code != http.StatusForbidden {
		t.Fatalf("a denied read answered %d: %s", w.Code, w.Body)
	}
	body := decode(t, w)
	if body.Error.Code != CodeForbidden {
		t.Fatalf("the code is %q", body.Error.Code)
	}
	if body.Error.Message != Sentence(CodeForbidden) {
		t.Errorf("the sentence is %q", body.Error.Message)
	}
	if body.Error.Details["request_id"] == nil {
		t.Error("the refusal carries no request id")
	}
	// The endpoint's reason is the developer detail and never the sentence,
	// and what asked it is named once.
	if got, _ := body.Error.Details["detail"].(string); got != authorizer.ActionEventRead+": the caller is not a member" {
		t.Errorf("the developer detail is %q", got)
	}
	if log.tailed != 0 {
		t.Error("the log was read after a deny")
	}
}

// TestTheTailAnswersEveryRowThroughTheFrame holds internal/events' rows to
// this package's table: the handler names a row, the frame renders it, and
// the status a caller meets is the table's and not a second copy of it.
func TestTheTailAnswersEveryRowThroughTheFrame(t *testing.T) {
	for _, c := range []struct {
		name  string
		query string
		log   *fakeLog
		fail  func(*stub.Server)
		code  string
	}{
		{"a limit above the cap", "limit=5000", &fakeLog{}, nil, CodeInvalidField},
		{"an authorizer that answered nothing", "", &fakeLog{},
			func(s *stub.Server) { s.Fail(http.StatusBadGateway) }, CodeAuthorizerUnavailable},
		{"a log that did not answer", "", &fakeLog{err: errors.New("the connection went away")},
			nil, CodeStorageUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, withLog(c.log))
			h.endpoint.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
			if c.fail != nil {
				c.fail(h.endpoint)
			}
			w := h.do(t, http.MethodGet, "/v1/events?"+c.query, h.bearer())
			if w.Code != Status(c.code) {
				t.Fatalf("the tail answered %d, and the row of %s is %d: %s", w.Code, c.code, Status(c.code), w.Body)
			}
			body := decode(t, w)
			if body.Error.Code != c.code {
				t.Fatalf("the code is %q", body.Error.Code)
			}
			if body.Error.Message != Sentence(c.code) {
				t.Errorf("the sentence is %q", body.Error.Message)
			}
			if body.Error.Details["request_id"] == nil {
				t.Error("the refusal carries no request id")
			}
		})
	}
}

// TestTheSurfaceRefusesToBuildWithoutTheEventLog: GET /v1/events is a row of
// the table, so a surface built without the log it tails would register a
// route it cannot answer. It is a start-up failure and not a nil dereference
// on the first request, like the two of spec 006.
func TestTheSurfaceRefusesToBuildWithoutTheEventLog(t *testing.T) {
	h := newHarness(t)
	_, err := New(Options{Verifier: h.api.verifier, Authorizer: h.api.authorizer})
	if err == nil || !strings.Contains(err.Error(), "no event log") {
		t.Errorf("a surface with no event log built: %v", err)
	}
}

// TestTheGuardReadsTheCallerTheVerifierLeft: the seam's first method is the
// subject of spec 006 and nothing else, and a request no verifier admitted
// carries none.
func TestTheGuardReadsTheCallerTheVerifierLeft(t *testing.T) {
	g := eventGuard{api: newHarness(t).api}
	if got := g.Caller(request(t, aSpace)); got != aSpace {
		t.Errorf("the guard read the caller %q", got)
	}
	if got := g.Caller(request(t, "")); got != "" {
		t.Errorf("a request with no caller read as %q", got)
	}
}

// TestTheGuardTranslatesEveryAnswer is the seam itself: auth.Decide collapses
// a deny and an outage into errors, and events.Guard reads a deny as a
// decision and an error as no decision at all. Reading either for the other
// would answer 503 to a deny, or turn an outage into something the handler
// reads as an answer.
func TestTheGuardTranslatesEveryAnswer(t *testing.T) {
	res := authorizer.Event{Owner: aSpace}.Resource()

	t.Run("an allow is a decision", func(t *testing.T) {
		h := newHarness(t)
		h.endpoint.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
		d, err := eventGuard{api: h.api}.Ask(request(t, aSpace), authorizer.ActionEventRead, res)
		if err != nil || !d.Allow {
			t.Fatalf("an allow read as %+v, %v", d, err)
		}
	})
	t.Run("a deny is a decision and not an error", func(t *testing.T) {
		h := newHarness(t)
		h.endpoint.Deny(stub.Rule{Subject: "*", Action: "*", Resource: "*"}, "the caller is not a member")
		d, err := eventGuard{api: h.api}.Ask(request(t, aSpace), authorizer.ActionEventRead, res)
		if err != nil {
			t.Fatalf("a deny read as an error: %v", err)
		}
		if d.Allow || d.Reason == "" {
			t.Fatalf("a deny read as %+v", d)
		}
	})
	t.Run("an endpoint that answered nothing is no decision", func(t *testing.T) {
		h := newHarness(t)
		h.endpoint.Fail(http.StatusBadGateway)
		_, err := eventGuard{api: h.api}.Ask(request(t, aSpace), authorizer.ActionEventRead, res)
		if err == nil {
			t.Fatal("an endpoint that answered nothing read as a decision")
		}
		if auth.CodeOf(err) != auth.CodeAuthorizerUnavailable {
			t.Errorf("an outage read as %q", auth.CodeOf(err))
		}
	})
	t.Run("an action outside the vocabulary is no decision", func(t *testing.T) {
		h := newHarness(t)
		h.endpoint.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
		// It is a bug in the caller rather than an outage, and the shared
		// client catches it before the wire.
		_, err := eventGuard{api: h.api}.Ask(request(t, aSpace), "no.such.action", res)
		if err == nil {
			t.Fatal("an action outside the vocabulary read as a decision")
		}
	})
}

// TestARefusalThatNamesNoRowIsInternal: an error internal/events did not
// name a row for says nothing of itself, like every other error the frame
// did not write a row for.
func TestARefusalThatNamesNoRowIsInternal(t *testing.T) {
	for _, err := range []error{
		errors.New("the connection went away"),
		&events.Error{Code: "no_such_row", Detail: "a code the table does not carry"},
	} {
		if got := FromEvents(err).Code; got != CodeInternal {
			t.Errorf("%v rendered as %q", err, got)
		}
	}
}

// request is one verified request, with the caller and the request block the
// frame puts on the context.
func request(t *testing.T, subject string) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/events", nil)
	ctx := auth.WithRequest(r.Context(), authz.Caller{ID: "req_test"})
	if subject != "" {
		ctx = auth.WithCaller(ctx, auth.Caller{Subject: subject})
	}
	return r.WithContext(ctx)
}

// subject is the rendered subject of the caller every harness mints a token
// for, which is the space its own requests name.
func (h *harness) subject() string { return authz.Subject(h.issuer.URL(), "9ab3") }

const (
	// aSpace is a subject a case puts on a context itself, where no token
	// was verified to render one.
	aSpace = "https://issuer.example|9ab3"
	// anotherSpace is a space the caller does not own, written as a query
	// value carries it.
	anotherSpace = "https%3A%2F%2Fissuer.example%7C9ab3c4d5"
)
