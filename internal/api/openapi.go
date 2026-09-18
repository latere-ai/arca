// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
)

// openapi answers the description of the surface this build serves. It takes
// no token: a route name is not a secret, and the existence hiding of
// invariant 6 protects objects and not the shape of the API.
//
// The bytes are rendered once at start from the route table the router was
// built from, so what a client generates against and what the server
// registers are the same table read twice in one process.
func (a *API) openapi(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(a.document)
}

// Document is the OpenAPI description this build serves, for a test and for
// the generator that writes the committed file.
func (a *API) Document() []byte { return a.document }
