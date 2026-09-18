// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/arca/internal/auth"
)

// TestEveryRequestCarriesAnId is criterion 15 of spec 013: a client's own id
// within the rule is kept and echoed everywhere, and one outside it is
// replaced with an id this server minted.
func TestEveryRequestCarriesAnId(t *testing.T) {
	cases := []struct {
		name   string
		given  string
		kept   bool
		reason string
	}{
		{name: "none given", given: "", kept: false},
		{name: "a client's own", given: "client-01J8R4", kept: true},
		{name: "one at the bound", given: strings.Repeat("a", MaxClientRequestID), kept: true},
		{name: "one past the bound", given: strings.Repeat("a", MaxClientRequestID+1), kept: false,
			reason: "an id without a bound is a log line without a bound"},
		{name: "one carrying a newline", given: "one\ntwo", kept: false,
			reason: "an id that breaks a log line is an id that forges one"},
		{name: "one carrying a control character", given: "one\x00two", kept: false},
		{name: "one carrying a character outside ASCII", given: "réquest", kept: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &API{}
			r := httptest.NewRequest(http.MethodGet, "/v1/trash", nil)
			if c.given != "" {
				r.Header.Set(Header, c.given)
			}
			w := httptest.NewRecorder()
			var seen string
			a.requestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen = RequestID(r.Context())
			})).ServeHTTP(w, r)

			if seen == "" {
				t.Fatal("the handler saw no request id")
			}
			if got := w.Header().Get(Header); got != seen {
				t.Errorf("the response carries %q and the handler saw %q", got, seen)
			}
			if c.kept && seen != c.given {
				t.Errorf("the client's id %q was replaced with %q", c.given, seen)
			}
			if !c.kept {
				if seen == c.given {
					t.Errorf("the id %q was kept; %s", c.given, c.reason)
				}
				if !strings.HasPrefix(seen, Prefix) {
					t.Errorf("a minted id is %q and does not open with %q", seen, Prefix)
				}
			}
		})
	}
}

// TestAMintedIdIsUniqueAndSortsByTime: the time leads, so ids sort by the
// order they were minted, and the rest is random, so two replicas minting in
// one millisecond do not collide.
func TestAMintedIdIsUniqueAndSortsByTime(t *testing.T) {
	clock := time.Date(2026, 9, 18, 10, 2, 11, 0, time.UTC)
	a := &API{clock: func() time.Time { return clock }}

	seen := map[string]bool{}
	for range 1000 {
		id := a.newID()
		if seen[id] {
			t.Fatalf("the id %q was minted twice in one millisecond", id)
		}
		seen[id] = true
		if len(id) != 26 {
			t.Fatalf("the id %q is %d characters, and a ULID is 26", id, len(id))
		}
		for _, r := range id {
			if !strings.ContainsRune(crockford, r) {
				t.Fatalf("the id %q carries %q, which is not one of the alphabet", id, r)
			}
		}
	}

	var ordered []string
	for range 5 {
		ordered = append(ordered, a.newID())
		clock = clock.Add(time.Second)
	}
	if !slices.IsSorted(ordered) {
		t.Errorf("ids minted a second apart do not sort by time: %v", ordered)
	}
}

// TestTheClockIsTheOneGiven: time.Now unless a test handed one over, so
// nothing here reads the wall clock a test cannot control.
func TestTheClockIsTheOneGiven(t *testing.T) {
	a := &API{}
	if got := a.now(); time.Since(got) > time.Minute {
		t.Errorf("the default clock answered %v", got)
	}
	want := time.Date(2026, 9, 18, 10, 2, 11, 0, time.UTC)
	b := &API{clock: func() time.Time { return want }}
	if got := b.now(); !got.Equal(want) {
		t.Errorf("the clock answered %v, want %v", got, want)
	}
}

// TestTheIdIsOnTheContextTheAuthorizerReads: one id, carried by the error
// envelope, the authorizer's question and the trace alike.
func TestTheIdIsOnTheContextTheAuthorizerReads(t *testing.T) {
	a := &API{}
	r := httptest.NewRequest(http.MethodGet, "/v1/trash", nil)
	r.RemoteAddr = "203.0.113.4:51000"
	r.Header.Set("User-Agent", "curl/8.7")
	var ctx context.Context
	a.requestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ctx = r.Context()
	})).ServeHTTP(httptest.NewRecorder(), r)

	info := requestOf(ctx)
	if info.ID != RequestID(ctx) {
		t.Errorf("the question carries the id %q and the context carries %q", info.ID, RequestID(ctx))
	}
	if info.IP != "203.0.113.4" {
		t.Errorf("the question carries the address %q; a port is not part of one", info.IP)
	}
	if info.UserAgent != "curl/8.7" {
		t.Errorf("the question carries the user agent %q", info.UserAgent)
	}
}

// requestOf reads the request block the authorizer sees off a context.
func requestOf(ctx context.Context) authz.Caller { return auth.RequestFrom(ctx) }

// TestNoIdOutsideARequest: reading the id where no request set one answers
// the empty string rather than a value that reads as an id.
func TestNoIdOutsideARequest(t *testing.T) {
	if got := RequestID(context.Background()); got != "" {
		t.Errorf("a context outside a request carries the id %q", got)
	}
}
