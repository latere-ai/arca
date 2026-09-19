// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"net/http"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
)

// Table are the fifteen rows of spec 013's first table, declared without
// their handlers so tools/apidoc describes the same surface the node mounts.
// Routes binds them.
//
// The action of a row is the action of its first row in spec 013's table,
// which is the action the route asks for the method. Three parameters select
// another representation of a GET and carry their own action with them:
// ?list= asks file.list, and ?versions= and ?version= stay at file.read. The
// spec's table says so in the rows below the first.
func Table() []api.Route {
	return []api.Route{
		{
			Method: http.MethodPut, Path: "/v1/files/{owner}/{path...}",
			Action: authorizer.ActionFileWrite, Status: http.StatusCreated,
			Summary: "Write one object; the body is the bytes.",
		},
		{
			Method: http.MethodGet, Path: "/v1/files/{owner}/{path...}",
			Action: authorizer.ActionFileRead, Status: http.StatusOK,
			Summary: "Read one object, its subtree with ?list=1, which carries the space's usage at a plane root, or its versions with ?versions=1.",
		},
		{
			Method: http.MethodHead, Path: "/v1/files/{owner}/{path...}",
			Action: authorizer.ActionFileRead, Status: http.StatusOK,
			Summary: "The object's headers with no body.",
		},
		{
			Method: http.MethodPost, Path: "/v1/files/{owner}/{path...}",
			Action: authorizer.ActionFileWrite, Status: http.StatusOK,
			Summary: "Move the object with move_to, or bring a version forward with restore_version.",
		},
		{
			Method: http.MethodDelete, Path: "/v1/files/{owner}/{path...}",
			Action: authorizer.ActionFileDelete, Status: http.StatusNoContent,
			Summary: "Trash the object; ?permanent=1 removes it and ?version=N prunes one version.",
		},
		{
			Method: http.MethodGet, Path: "/v1/files/materialize",
			Action: authorizer.ActionFileList, Status: http.StatusOK,
			Summary: "One space as a manifest of presigned URLs.",
		},
		{
			Method: http.MethodGet, Path: "/v1/trash",
			Action: authorizer.ActionFileList, Status: http.StatusOK,
			Summary: "What is trashed and still restorable, newest first.",
		},
		{
			Method: http.MethodPost, Path: "/v1/trash/restore",
			Action: authorizer.ActionFileRestore, Status: http.StatusOK,
			Summary: "Return one object to its path.",
		},
		{
			Method: http.MethodDelete, Path: "/v1/trash",
			Action: authorizer.ActionFileDelete, Status: http.StatusOK,
			Summary: "Empty the trash, or one path with ?path=.",
		},
		{
			Method: http.MethodPut, Path: "/v1/stars",
			Action: authorizer.ActionFileWrite, Status: http.StatusNoContent,
			Summary: "Star an object; idempotent.",
		},
		{
			Method: http.MethodDelete, Path: "/v1/stars",
			Action: authorizer.ActionFileWrite, Status: http.StatusNoContent,
			Summary: "Unstar an object; idempotent.",
		},
		{
			Method: http.MethodGet, Path: "/v1/stars",
			Action: authorizer.ActionFileList, Status: http.StatusOK,
			Summary: "The caller's stars across every space.",
		},
	}
}

// Routes are the same rows with this package's handlers bound, which is what
// the node registers. The order is Table's, so the document and the router
// read one declaration.
func Routes(o Options) []api.Route { return Bind(New(o)) }

// Bind attaches one service's handlers to the declared rows, for a node that
// has already built the service because another package reaches it too.
func Bind(s *Service) []api.Route {
	handlers := []http.Handler{
		http.HandlerFunc(s.put), http.HandlerFunc(s.get), http.HandlerFunc(s.head),
		http.HandlerFunc(s.post), http.HandlerFunc(s.remove), http.HandlerFunc(s.materialize),
		http.HandlerFunc(s.listTrash), http.HandlerFunc(s.restoreTrash), http.HandlerFunc(s.purgeTrash),
		http.HandlerFunc(s.star), http.HandlerFunc(s.unstar), http.HandlerFunc(s.listStars),
	}
	rows := Table()
	out := make([]api.Route, len(rows))
	for i, row := range rows {
		row.Handler = handlers[i]
		out[i] = row
	}
	return out
}
