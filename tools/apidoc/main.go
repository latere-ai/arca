// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Command apidoc writes api/openapi.yaml from the route table and the error
// table of internal/api. Run it from the repository root, which `make
// openapi` does, and commit the result. A test here fails when the committed
// file is not what a fresh run produces, so a route added without
// regenerating does not reach main.
//
// The document is built as Go values and rendered to JSON by the standard
// library; this command is the one place a YAML encoder is reached, so the
// server's build list carries none (spec 001's ninth invariant). The
// committed file names no server, because a document in a repository
// describes no one installation; the document GET /openapi.json answers
// names ARCA_PUBLIC_URL.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/goccy/go-yaml"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/apidocs"
	"latere.ai/x/arca/internal/workspaces"
)

// Path is where the document is committed, relative to the repository root.
var Path = filepath.Join("api", "openapi.yaml")

func main() { os.Exit(cli(os.Stdout, os.Stderr)) }

// cli writes the document under the working directory, which is the
// repository root when `make openapi` runs it, and returns the process exit
// code: 0 on success, 1 with one line on stderr otherwise.
func cli(stdout, stderr io.Writer) int {
	if err := run("."); err != nil {
		_, _ = fmt.Fprintf(stderr, "apidoc: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "apidoc: wrote %s\n", Path)
	return 0
}

// run renders the document and writes it under root.
func run(root string) error {
	dst := filepath.Join(root, Path)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("make %s: %w", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, Render(), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

// Render is the committed document's bytes: the same document the server
// answers, as YAML. The JSON is built from ordered struct fields and maps
// the standard library sorts, so two runs of one table produce one file.
//
// The committed file is the contract of a build and not of a deployment, so
// it names no server and carries the document's own version rather than a
// binary's.
func Render() []byte {
	return mustYAML(apidocs.Build(apidocs.Options{
		Title: api.Title, Version: api.DocumentVersion, Description: api.Description,
		Routes: routes(), Errors: api.Errors(),
	}).JSON())
}

// routes is the whole surface this build registers: the frame's own rows
// and the rows each owning package declares. The node builds its mux from
// the same declarations, so the committed document and the registrations are
// one list read twice and cannot disagree.
func routes() []apidocs.Route {
	return append(api.Routes(), api.Described(workspaces.Table())...)
}

// mustYAML converts a JSON document to YAML, and panics on bytes that are
// not JSON. Its input is JSON this process built a moment earlier, so the
// conversion cannot fail on it, and a failure would be a programming error
// rather than a runtime condition: writing the file anyway would commit
// bytes nobody checked.
func mustYAML(document []byte) []byte {
	out, err := yaml.JSONToYAML(document)
	if err != nil {
		panic("apidoc: the document does not render as YAML: " + err.Error())
	}
	return out
}
