// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package workspaces

import (
	"encoding/json"
	"net/http"
	"testing"

	"latere.ai/x/pkg/authz/stub"
)

// TestEveryWorkspaceLookupDenyIsTheAnswerAnAbsenceGives is criterion 25 of
// spec 015 for the routes of this package that read the workspace row before
// they ask, which is every route naming one.
//
// The route is driven twice against one identifier: once where the workspace
// is there and the question is denied at lookup, once where nothing holds
// that id and the handler answers before it asks. The two answers have to be
// one answer, status, code, message and developer detail alike. An
// identifier this service minted is not one a caller can walk, so the oracle
// was never reachable; the answers are collapsed anyway, because a rule that
// holds on most routes is a rule a reader has to check route by route.
//
// The four routes here reach the question through lookup. The renew, the
// release, the sync and the materialize reach the same ask through
// attachment, with the action that attachment's mode names, and are not
// driven here: opening the attachment they need asks that very action and is
// allowed, and spec 006 caches an allow per subject, action and resource, so
// a deny set afterwards is not the answer the route reads. What that would
// test is the cache rather than the collapse.
func TestEveryWorkspaceLookupDenyIsTheAnswerAnAbsenceGives(t *testing.T) {
	for _, c := range []struct {
		name   string
		method string
		path   func(id string) string
		body   any
	}{
		{"read", http.MethodGet, func(id string) string { return "/v1/workspaces/" + id }, nil},
		{
			"rename", http.MethodPatch,
			func(id string) string { return "/v1/workspaces/" + id },
			map[string]any{"slug": "release"},
		},
		{"delete", http.MethodDelete, func(id string) string { return "/v1/workspaces/" + id }, nil},
		{
			"attach", http.MethodPost,
			func(id string) string { return "/v1/workspaces/" + id + "/attach" },
			map[string]any{"sandbox_id": "sbx", "mode": "ro"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			held := newHarness(t)
			ws := held.create(t, "build")
			held.endpoint.SetRules()
			held.endpoint.Deny(stub.Rule{Subject: "*", Action: "*", Resource: "*"}, "no rule allows it")
			denied := held.do(t, c.method, c.path(ws.ID), c.body)

			// The same request against a service holding nothing, where the
			// handler answers before it asks.
			empty := newHarness(t)
			gone := empty.do(t, c.method, c.path(ws.ID), c.body)

			if denied.code != gone.code {
				t.Errorf("a denied %s answers %d and a missing one %d", c.name, denied.code, gone.code)
			}
			if without(denied.body) != without(gone.body) {
				t.Errorf("a denied %s answers\n %s\nand a missing one answers\n %s",
					c.name, denied.body, gone.body)
			}
		})
	}
}

// without renders a refusal against the one field two of them are allowed to
// differ in: the request id. Everything else is compared, the developer
// detail included, because a field a caller can read is a field a caller can
// count.
func without(body []byte) string {
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		return string(body)
	}
	if refusal, ok := envelope["error"].(map[string]any); ok {
		if details, ok := refusal["details"].(map[string]any); ok {
			delete(details, "request_id")
		}
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return string(body)
	}
	return string(out)
}
