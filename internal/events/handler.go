// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package events

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/arca/internal/store"
)

// Guard is the authorizer seam of spec 006, as this package needs it. The
// verifier runs in front of the route and leaves the caller on the request;
// this package reads no claim, holds no policy, and decides nothing.
//
// internal/auth implements it after spec 006 lands, and internal/api binds
// the implementation when it registers the route. The two methods are the
// whole contract:
//
//   - Caller answers the subject the verified token identifies, rendered as
//     <issuer>|<sub>, and the empty string for a request that carried no
//     usable token.
//   - Ask puts one question of the vocabulary and answers the decision
//     verbatim, Filter included, so the handler narrows its own query. An
//     error is a call that produced no decision, which fails closed as 503
//     and never as an allow.
type Guard interface {
	Caller(r *http.Request) string
	Ask(r *http.Request, action string, resource authz.Resource) (authz.Decision, error)
}

// Refuser writes the refusal this handler decided. The error table of spec
// 013, the status line it carries, the one user sentence of its row and the
// request id beside it are internal/api's, so this package names the row and
// the developer detail and writes no envelope of its own. It is the shape
// internal/auth's refusals already take, one layer over.
//
// internal/api binds the implementation when it registers the route.
type Refuser func(w http.ResponseWriter, r *http.Request, err error)

// Error is one refusal: the row of spec 013's error table this route
// answers, the developer detail, and the field paths at fault where the row
// names fields. It is an error, so the handler decides one and one place
// writes it.
type Error struct {
	// Code is the row of the table. It is one of the five below.
	Code string
	// Detail is the developer sentence. It names values and reasons and
	// never reaches the user sentence, and nothing above 499 names a store,
	// a query or a key.
	Detail string
	// Fields are the field paths at fault, for a code whose row says a
	// refusal carries them.
	Fields []string
}

func (e *Error) Error() string { return e.Code + ": " + e.Detail }

// The five rows of spec 013's error table this route answers. The table
// itself is internal/api's, which is where the status and the user sentence
// of each live; a row is named here and rendered there.
const (
	codeUnauthenticated       = "unauthenticated"
	codeInvalidField          = "invalid_field"
	codeForbidden             = "forbidden"
	codeAuthorizerUnavailable = "authorizer_unavailable"
	codeStorageUnavailable    = "storage_unavailable"
)

// The question this route asks. The vocabulary is spec 006's and the kind is
// the row of its table that carries one field, the space.
const (
	// ActionEventRead is the action every read of the log asks.
	ActionEventRead = "event.read"
	// KindEvent is the resource kind that action names.
	KindEvent = "Event"
)

// The page rules of spec 013, which every list route shares: a limit a
// caller does not name, and the cap above which a limit is refused rather
// than silently clamped.
const (
	// DefaultLimit is the page a caller that names none receives.
	DefaultLimit = 100
	// MaxLimit is the largest page. A limit above it is invalid_field, not a
	// clamp: a client asking for 5000 and receiving 1000 believes it has
	// read everything.
	MaxLimit = 1000
)

// meAlias is the one alias the owner parameter takes: the caller's own
// space. A response renders the subject in full and never an alias, so a
// client that stores what it read can send it back (spec 013).
const meAlias = "me"

// Handler answers GET /v1/events: one page of a space's log, oldest first,
// keyset paginated on the primary key.
//
// It is a handler and not a route. Spec 013 owns the mux and registers this
// on the path, with the verifier of spec 006 in front of it, so nothing here
// names a method or a pattern. It owns no envelope either: every refusal
// goes out through refuse, which is the frame's.
func Handler(log Log, q store.Querier, g Guard, refuse Refuser) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller := g.Caller(r)
		if caller == "" {
			refuse(w, r, &Error{Code: codeUnauthenticated,
				Detail: "the request reached the tail with no verified caller on it"})
			return
		}
		owner := r.URL.Query().Get("owner")
		if owner == "" || owner == meAlias {
			owner = caller
		}
		query, problem := parseQuery(r, owner)
		if problem != nil {
			refuse(w, r, problem)
			return
		}

		decision, err := g.Ask(r, ActionEventRead, authz.NewResource(KindEvent, "", map[string]any{"owner": owner}))
		switch {
		case err != nil:
			refuse(w, r, &Error{Code: codeAuthorizerUnavailable, Detail: err.Error()})
			return
		case !decision.Allow:
			// The reason is the answer's own, verbatim. What asked is the
			// guard's to say, and it says it once.
			refuse(w, r, &Error{Code: codeForbidden, Detail: reasonOf(decision)})
			return
		}
		// A filter narrows the page and never refuses it: a selector outside
		// what the answer allows yields an empty page, which is the rule
		// every list route of spec 013 follows.
		if outsideFilter(decision.Filter, owner) {
			writePage(w, Page{})
			return
		}

		page, err := log.Tail(r.Context(), q, query)
		if err != nil {
			refuse(w, r, &Error{Code: codeStorageUnavailable,
				Detail: "the log did not answer"})
			return
		}
		writePage(w, page)
	})
}

// parseQuery reads the cursor and the limit, answering the refusal of the
// first field at fault.
func parseQuery(r *http.Request, owner string) (Query, *Error) {
	q := Query{Owner: owner, Limit: DefaultLimit}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		cursor, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || cursor < 0 {
			return Query{}, &Error{Code: codeInvalidField, Fields: []string{"cursor"},
				Detail: "cursor is " + strconv.Quote(raw) + "; a cursor is an event id this server answered"}
		}
		q.Cursor = cursor
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > MaxLimit {
			return Query{}, &Error{Code: codeInvalidField, Fields: []string{"limit"},
				Detail: "limit is " + strconv.Quote(raw) + "; a page holds between 1 and " +
					strconv.Itoa(MaxLimit) + " rows"}
		}
		q.Limit = limit
	}
	return q, nil
}

// reasonOf is the reason a deny carried, or a word when it named none, so a
// log line always says something.
func reasonOf(d authz.Decision) string {
	if d.Reason == "" {
		return "the authorizer named no reason"
	}
	return d.Reason
}

// outsideFilter reports whether the answer's filter excludes the space the
// caller selected. A filter naming no owner narrows nothing.
func outsideFilter(filter *authz.Filter, owner string) bool {
	if filter == nil || len(filter.Owners) == 0 {
		return false
	}
	return !slices.Contains(filter.Owners, owner)
}

// entry is one row on the wire. The id is the number a consumer sends back
// as the cursor; the detail is passed through as the writer wrote it.
type entry struct {
	ID     int64           `json:"id"`
	Action string          `json:"action"`
	Owner  string          `json:"owner"`
	Path   string          `json:"path,omitempty"`
	Actor  string          `json:"actor,omitempty"`
	Detail json.RawMessage `json:"detail,omitempty"`
	At     string          `json:"at"`
}

// envelope is the list envelope of spec 013: entries, never null, and a
// cursor present only when a further page exists.
type envelope struct {
	Entries    []entry `json:"entries"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

// writePage renders one page.
func writePage(w http.ResponseWriter, page Page) {
	out := envelope{Entries: make([]entry, 0, len(page.Entries))}
	for _, e := range page.Entries {
		out.Entries = append(out.Entries, entry{
			ID: e.ID, Action: string(e.Action), Owner: e.Owner, Path: e.Path, Actor: e.Actor,
			Detail: detailJSON(e.Detail), At: e.At.UTC().Format(time.RFC3339),
		})
	}
	if page.NextCursor != 0 {
		out.NextCursor = strconv.FormatInt(page.NextCursor, 10)
	}
	httpjson.Write(w, http.StatusOK, out)
}

// detailJSON renders a detail, or nothing when the row carried none. A
// detail that will not encode is dropped rather than failing the page: it
// was written by an append that did encode it, so a failure here is this
// server's and the entry still says what happened.
func detailJSON(detail map[string]any) json.RawMessage {
	if len(detail) == 0 {
		return nil
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return nil
	}
	return b
}

// AsOverLimit reads an over-limit refusal out of an error, so a write
// handler of spec 005 or 007 answers it 413 quota_exceeded without knowing
// how the ledger reported it.
func AsOverLimit(err error) (*OverLimitError, bool) {
	var over *OverLimitError
	ok := errors.As(err, &over)
	return over, ok
}
