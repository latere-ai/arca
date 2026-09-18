// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package admin

import (
	"net/http"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
)

// The rows of spec 013's table this package owns, declared once and read
// twice: [Routes] binds the handlers for the node, and [Table] answers the
// same rows with none for the generator of the committed description, which
// has no service and no stores to build one with.
//
// Both rows ask space.admin, and both ask it before they act. The overview
// names no one space, so its resource carries no owner; the restore names
// the space in its path. Neither is a list action, so neither reads the
// authorizer's filter: an administrator the endpoint admitted reads every
// space, and narrowing the page would answer a question nobody asked
// (spec 013).

// row is one route of this package: the declaration and the handler.
type row struct {
	method, path, action, summary string
	status                        int
	answer                        func(*Service, http.ResponseWriter, *http.Request)
}

// rows is the two routes of spec 013's administration table.
var rows = []row{
	{
		method: http.MethodGet, path: "/v1/admin/overview",
		action: authorizer.ActionSpaceAdmin, status: http.StatusOK,
		summary: "One row per space with its usage and its counts.",
		answer:  (*Service).overview,
	},
	{
		method: http.MethodPost, path: "/v1/admin/spaces/{owner}/restore",
		action: authorizer.ActionSpaceAdmin, status: http.StatusOK,
		summary: "Restore one deleted object or workspace of a space.",
		answer:  (*Service).restore,
	},
}

// Routes is the rows with their handlers bound, for the node.
func Routes(s *Service) []api.Route {
	out := make([]api.Route, 0, len(rows))
	for _, r := range rows {
		answer := r.answer
		out = append(out, api.Route{
			Method: r.method, Path: r.path, Action: r.action,
			Summary: r.summary, Status: r.status,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				// Both routes answer JSON, so a caller that accepts none of
				// it is told before the handler asks anything.
				if err := api.Acceptable(req); err != nil {
					api.WriteError(w, req, err)
					return
				}
				answer(s, w, req)
			}),
		})
	}
	return out
}

// Table is the same rows with no handlers, for the document. The generator
// of the committed description builds no service, so it reads the
// declaration and not the registration.
func Table() []api.Route {
	out := make([]api.Route, 0, len(rows))
	for _, r := range rows {
		out = append(out, api.Route{
			Method: r.method, Path: r.path, Action: r.action,
			Summary: r.summary, Status: r.status,
		})
	}
	return out
}
