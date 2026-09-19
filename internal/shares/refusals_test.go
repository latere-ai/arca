// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"latere.ai/x/pkg/authz/stub"
)

// TestEveryShareLookupDenyIsTheAnswerAnAbsenceGives is criterion 25 of spec 015
// for the three routes of this package that read a row before they ask:
// share.read and share.revoke on a subject grant, and link.revoke on a token
// grant.
//
// Each is driven twice against one identifier: once where the row is there
// and the question is denied at lookup, once where nothing holds that id and
// the handler answers before it asks. The two answers have to be one answer,
// status, code, message and developer detail alike. An identifier this
// service minted is not one a caller can walk, so the oracle was never
// reachable; the answers are collapsed anyway, because a rule that holds on
// most routes is a rule a reader has to check route by route.
func TestEveryShareLookupDenyIsTheAnswerAnAbsenceGives(t *testing.T) {
	for _, c := range []struct {
		name   string
		method string
		// route renders the path from the id of the row held.
		route func(id string) string
		// id makes the row in a harness and answers the identifier it got.
		id func(t *testing.T, h *harness) string
	}{
		{
			name: "share.read", method: http.MethodGet,
			route: func(id string) string { return "/v1/shares/" + id },
			id:    aGrant,
		},
		{
			name: "share.revoke", method: http.MethodDelete,
			route: func(id string) string { return "/v1/shares/" + id },
			id:    aGrant,
		},
		{
			name: "link.revoke", method: http.MethodDelete,
			route: func(id string) string { return "/v1/shares/links/" + id },
			id:    aLink,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			held := newHarness(t)
			id := c.id(t, held)
			held.endpoint.SetRules()
			held.endpoint.Deny(stub.Rule{Subject: "*", Action: "*", Resource: "*"}, "no rule allows it")
			denied := held.do(t, c.method, c.route(id), "alice", nil)

			// The same request against a service holding nothing, where the
			// handler answers before it asks.
			empty := newHarness(t)
			gone := empty.do(t, c.method, c.route(id), "alice", nil)

			if denied.Code != gone.Code {
				t.Errorf("a denied %s answers %d and a missing one %d", c.name, denied.Code, gone.Code)
			}
			if stripped(denied) != stripped(gone) {
				t.Errorf("a denied %s answers\n %s\nand a missing one answers\n %s",
					c.name, denied.Body, gone.Body)
			}
		})
	}
}

// aGrant writes one subject grant and answers its id.
func aGrant(t *testing.T, h *harness) string {
	t.Helper()
	w := h.do(t, http.MethodPost, "/v1/shares", "alice",
		body("files/reports", h.subject("carol"), "read"))
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /v1/shares = %d: %s", w.Code, w.Body)
	}
	return grantOf(t, w).ID
}

// aLink mints one token grant and answers its id, which is what the revoke
// route names: the token is the redemption's identifier and not the revoke's.
func aLink(t *testing.T, h *harness) string {
	t.Helper()
	return mint(t, h, "files/reports", "").ID
}

// stripped renders a refusal without the one field two of them are allowed
// to differ in: the request id. Everything else is compared, the developer
// detail included, because a field a caller can read is a field a caller can
// count.
func stripped(w *httptest.ResponseRecorder) string {
	var envelope map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		return w.Body.String()
	}
	if refusal, ok := envelope["error"].(map[string]any); ok {
		if details, ok := refusal["details"].(map[string]any); ok {
			delete(details, "request_id")
		}
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return w.Body.String()
	}
	return string(out)
}
