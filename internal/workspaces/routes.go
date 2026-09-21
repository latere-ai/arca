// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package workspaces

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
// Two rows ask an action the request chooses. The attach asks workspace.read
// for a ro mount and workspace.attach for a rw one, and the renew and the
// release that follow it ask the action their attach asked. The row carries
// the read, which is the action every one of them asks at least; a rw
// request asks the attach action instead, and the test in this package
// drives both and holds each to its answer.
//
// The word deleted is a literal beside the {id} wildcard. Go's router prefers
// the more specific pattern, so the two coexist with no ordering rule to
// remember and deleted is not a workspace id.

// row is one route of this package: the declaration and the handler.
type row struct {
	method, path, action, summary, description string
	status                                     int
	answer                                     func(*Service, http.ResponseWriter, *http.Request)
}

// rows is the twelve routes of spec 013's workspace table.
var rows = []row{
	{
		method: http.MethodPost, path: "/v1/workspaces",
		action: authorizer.ActionWorkspaceCreate, status: http.StatusCreated,
		summary:     "Create workspace",
		description: "Create a workspace in a space.",
		answer:      (*Service).create,
	},
	{
		method: http.MethodGet, path: "/v1/workspaces",
		action: authorizer.ActionWorkspaceList, status: http.StatusOK,
		summary:     "List workspaces",
		description: "List the workspaces of a space.",
		answer:      (*Service).list,
	},
	{
		method: http.MethodGet, path: "/v1/workspaces/deleted",
		action: authorizer.ActionWorkspaceList, status: http.StatusOK,
		summary:     "List deleted workspaces",
		description: "List the soft deleted workspaces of a space that are still restorable.",
		answer:      (*Service).listDeleted,
	},
	{
		method: http.MethodGet, path: "/v1/workspaces/{id}",
		action: authorizer.ActionWorkspaceRead, status: http.StatusOK,
		summary:     "Get workspace",
		description: "Read one workspace with its lease and the counters of its subtree.",
		answer:      (*Service).read,
	},
	{
		method: http.MethodPatch, path: "/v1/workspaces/{id}",
		action: authorizer.ActionWorkspaceWrite, status: http.StatusOK,
		summary:     "Rename workspace",
		description: "Rename a workspace, which moves its rows and no bucket key.",
		answer:      (*Service).rename,
	},
	{
		method: http.MethodDelete, path: "/v1/workspaces/{id}",
		action: authorizer.ActionWorkspaceDelete, status: http.StatusNoContent,
		summary:     "Delete workspace",
		description: "Soft delete a workspace, leaving it restorable.",
		answer:      (*Service).remove,
	},
	{
		method: http.MethodPost, path: "/v1/workspaces/{id}/restore",
		action: authorizer.ActionWorkspaceRestore, status: http.StatusOK,
		summary:     "Restore workspace",
		description: "Undo a soft delete.",
		answer:      (*Service).restore,
	},
	{
		method: http.MethodPost, path: "/v1/workspaces/{id}/attach",
		action: authorizer.ActionWorkspaceRead, status: http.StatusCreated,
		summary:     "Attach workspace",
		description: "Open an attachment, taking the writer lease for a rw mount.",
		answer:      (*Service).attach,
	},
	{
		method: http.MethodPost, path: "/v1/workspaces/{id}/attach/{aid}/renew",
		action: authorizer.ActionWorkspaceRead, status: http.StatusOK,
		summary:     "Renew attachment",
		description: "Push the attachment's deadline forward.",
		answer:      (*Service).renew,
	},
	{
		method: http.MethodDelete, path: "/v1/workspaces/{id}/attach/{aid}",
		action: authorizer.ActionWorkspaceRead, status: http.StatusNoContent,
		summary:     "Release attachment",
		description: "Release an attachment and the lease it held.",
		answer:      (*Service).release,
	},
	{
		method: http.MethodGet, path: "/v1/workspaces/{id}/materialize",
		action: authorizer.ActionWorkspaceRead, status: http.StatusOK,
		summary:     "Get workspace manifest",
		description: "The attachment's pinned manifest with a presigned read per file.",
		answer:      (*Service).materialize,
	},
	{
		method: http.MethodPost, path: "/v1/workspaces/{id}/sync",
		action: authorizer.ActionWorkspaceSync, status: http.StatusOK,
		summary:     "Sync workspace",
		description: "Declare the post-state manifest of the subtree; the server reconciles.",
		answer:      (*Service).sync,
	},
}

// Routes is the rows with their handlers bound, for the node.
func Routes(s *Service) []api.Route {
	out := make([]api.Route, 0, len(rows))
	for _, r := range rows {
		answer := r.answer
		out = append(out, api.Route{
			Method: r.method, Path: r.path, Action: r.action,
			Summary: r.summary, Description: r.description, Status: r.status,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				// Every route here answers JSON, so a caller that accepts
				// none of it is told before the handler reads a row.
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
			Summary: r.summary, Description: r.description, Status: r.status,
		})
	}
	return out
}
