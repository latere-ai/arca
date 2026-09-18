// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares

import (
	"net/http"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
)

// The rows of spec 013's table this spec owns, declared beside the handlers
// that answer them. The node registers them through api.Options.Routes and
// the generator of the committed document reads the same list, so a route
// cannot exist in the mux and not in the description.
//
// Eight rows are here and three are not. The three that redeem a token sit
// outside the verifier, which no contributed row may do, so the frame
// declares those itself and takes this service as its Links seam.

// row is one of this package's rows before its handler is bound.
type row struct {
	method, path string
	action       string
	summary      string
	status       int
	answer       func(*Service, http.ResponseWriter, *http.Request)
}

var rows = []row{
	{
		method: http.MethodPost, path: "/v1/shares",
		action: authorizer.ActionShareCreate, status: http.StatusCreated,
		summary: "Grant a subject a permission on a subtree of a space.",
		answer:  (*Service).CreateGrant,
	},
	{
		method: http.MethodGet, path: "/v1/shares",
		action: authorizer.ActionShareList, status: http.StatusOK,
		summary: "The grants on a space.",
		answer:  (*Service).ListGrants,
	},
	{
		method: http.MethodGet, path: "/v1/shares/with-me",
		action: authorizer.ActionShareList, status: http.StatusOK,
		summary: "The grants whose grantee is the caller.",
		answer:  (*Service).GrantsWithMe,
	},
	{
		method: http.MethodGet, path: "/v1/shares/{id}",
		action: authorizer.ActionShareRead, status: http.StatusOK,
		summary: "One grant.",
		answer:  (*Service).ReadGrant,
	},
	{
		method: http.MethodDelete, path: "/v1/shares/{id}",
		action: authorizer.ActionShareRevoke, status: http.StatusNoContent,
		summary: "Revoke a grant.",
		answer:  (*Service).RevokeGrant,
	},
	{
		method: http.MethodPost, path: "/v1/shares/links",
		action: authorizer.ActionLinkCreate, status: http.StatusCreated,
		summary: "Mint a token grant on a subtree; the token is answered once.",
		answer:  (*Service).CreateLink,
	},
	{
		method: http.MethodGet, path: "/v1/shares/links",
		action: authorizer.ActionLinkRead, status: http.StatusOK,
		summary: "The token grants on a space.",
		answer:  (*Service).ListLinks,
	},
	{
		method: http.MethodDelete, path: "/v1/shares/links/{id}",
		action: authorizer.ActionLinkRevoke, status: http.StatusNoContent,
		summary: "Revoke a token grant.",
		answer:  (*Service).RevokeLink,
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
