// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"latere.ai/x/arca/authorizer"
)

// TestTheCommittedDocumentIsCurrent is the proved half of criterion 13 of
// spec 013: the file at
// api/openapi.yaml equals a fresh generation, so a route added without
// running `make openapi` does not reach main. It is the drift test the
// predecessor carried, over a route table that is a declaration rather than
// a regular expression over the source.
func TestTheCommittedDocumentIsCurrent(t *testing.T) {
	committed := filepath.Join("..", "..", Path)
	got, err := os.ReadFile(committed)
	if err != nil {
		t.Fatalf("%s: %v; run `make openapi` and commit the result", Path, err)
	}
	if !bytes.Equal(got, Render()) {
		t.Errorf("%s is not what the route table renders; run `make openapi` and commit the result", Path)
	}
}

// TestTheDocumentIsOpenAPIAndParsesAsYAML: the committed file is what a
// client generator reads, so it has to be a document and not a string that
// happens to have been written.
func TestTheDocumentIsOpenAPIAndParsesAsYAML(t *testing.T) {
	var back struct {
		OpenAPI string         `yaml:"openapi"`
		Info    map[string]any `yaml:"info"`
		Paths   map[string]any `yaml:"paths"`
		Servers []any          `yaml:"servers"`
	}
	if err := yaml.Unmarshal(Render(), &back); err != nil {
		t.Fatalf("the document is not YAML: %v", err)
	}
	if back.OpenAPI != "3.1.0" {
		t.Errorf("the document declares OpenAPI %q", back.OpenAPI)
	}
	if back.Info["title"] != "Arca" {
		t.Errorf("the document is titled %v", back.Info["title"])
	}
	if len(back.Paths) == 0 {
		t.Error("the document describes no path")
	}
	if len(back.Servers) != 0 {
		t.Errorf("the committed document names the servers %v; a file in a repository describes no one installation", back.Servers)
	}
}

// TestRenderIsTheSameBytesEveryRun: the drift test above compares bytes, so
// a document that rendered differently every run would fail at random rather
// than when a route changed.
func TestRenderIsTheSameBytesEveryRun(t *testing.T) {
	first := Render()
	for range 10 {
		if !bytes.Equal(first, Render()) {
			t.Fatal("two runs rendered different bytes")
		}
	}
}

// TestRunWritesTheDocumentUnderTheRootItIsGiven: the generator writes one
// file at one path and makes the directory if it is not there, so a clean
// clone regenerates without a step of its own, and running twice is running
// once.
func TestRunWritesTheDocumentUnderTheRootItIsGiven(t *testing.T) {
	root := t.TempDir()
	if err := run(root); err != nil {
		t.Fatalf("the generator failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, Path))
	if err != nil {
		t.Fatalf("the generator wrote no document: %v", err)
	}
	if !bytes.Equal(got, Render()) {
		t.Error("the generator wrote something other than the rendered document")
	}
	if err := run(root); err != nil {
		t.Fatalf("the second run failed: %v", err)
	}
	again, err := os.ReadFile(filepath.Join(root, Path))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, again) {
		t.Error("two runs wrote different documents")
	}
}

// TestRunReportsWhatItCannotWrite: a generator that cannot write says so and
// names the file, rather than exiting zero having written nothing.
func TestRunReportsWhatItCannotWrite(t *testing.T) {
	t.Run("the directory cannot be made", func(t *testing.T) {
		root := t.TempDir()
		// A file where the directory has to go.
		if err := os.WriteFile(filepath.Join(root, "api"), []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := run(root)
		if err == nil {
			t.Fatal("the generator reported success with nowhere to write")
		}
		if !strings.Contains(err.Error(), "api") {
			t.Errorf("the failure is %q and does not name the directory", err)
		}
	})
	t.Run("the file cannot be written", func(t *testing.T) {
		root := t.TempDir()
		// A directory where the file has to go.
		if err := os.MkdirAll(filepath.Join(root, Path), 0o755); err != nil {
			t.Fatal(err)
		}
		err := run(root)
		if err == nil {
			t.Fatal("the generator reported success writing over a directory")
		}
		if !strings.Contains(err.Error(), Path) {
			t.Errorf("the failure is %q and does not name the file", err)
		}
	})
}

// TestTheCommandWritesUnderTheWorkingDirectory: `make openapi` runs the
// command from the repository root, so the working directory is where the
// document lands and the line on stdout names the file written.
func TestTheCommandWritesUnderTheWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	var out, errOut bytes.Buffer
	if code := cli(&out, &errOut); code != 0 {
		t.Fatalf("the command exited %d: %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(root, Path)); err != nil {
		t.Fatalf("the command wrote no document: %v", err)
	}
	if !strings.Contains(out.String(), Path) {
		t.Errorf("stdout is %q and does not name the file written", out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("a successful run wrote to stderr: %q", errOut.String())
	}
}

// TestTheCommandReportsAFailureOnOneLine: an operator or a Makefile reads
// one line on stderr and a non-zero exit, not a stack.
func TestTheCommandReportsAFailureOnOneLine(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.WriteFile(filepath.Join(root, "api"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := cli(&out, &errOut); code != 1 {
		t.Fatalf("the command exited %d with nowhere to write", code)
	}
	if got := errOut.String(); !strings.HasPrefix(got, "apidoc: ") || strings.Count(got, "\n") != 1 {
		t.Errorf("stderr is %q", got)
	}
	if out.Len() != 0 {
		t.Errorf("a failed run wrote to stdout: %q", out.String())
	}
}

// TestBytesThatAreNotJSONPanic is the contract of the conversion: its input
// is JSON this process built a moment earlier, so a failure is a programming
// error, and the file is not written rather than written unchecked.
func TestBytesThatAreNotJSONPanic(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("bytes that are not JSON rendered as YAML")
		}
		if got, _ := r.(string); !strings.Contains(got, "does not render as YAML") {
			t.Errorf("the panic is %v", r)
		}
	}()
	mustYAML([]byte("{this is not JSON"))
}

var (
	// routeRow matches one row of a route table of spec 013: the method,
	// the path, and the action column.
	routeRow = regexp.MustCompile(`^\| (GET|POST|PUT|PATCH|DELETE|HEAD) \| ` + "`([^`]+)`" + ` \| ([^|]+) \|`)
	// tickedAction matches an action a row names.
	tickedAction = regexp.MustCompile("`([a-z]+\\.[a-z]+)`")
)

// specRoutes reads every route table of specs/013-api.md: each row's method
// and path, mapped to every action its third column names. A row whose
// action the request chooses names more than one and a row outside the
// verifier names none, which is why the map holds a list. A row whose path
// carries a query selects a representation of a route already named, so the
// query is dropped and the first row of a path wins.
func specRoutes(t *testing.T) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "specs", "013-api.md"))
	if err != nil {
		t.Fatalf("spec 013: %v", err)
	}
	out := map[string][]string{}
	for line := range strings.Lines(string(raw)) {
		m := routeRow.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		path, _, _ := strings.Cut(m[2], "?")
		key := m[1] + " " + path
		if _, seen := out[key]; seen {
			continue
		}
		var actions []string
		for _, a := range tickedAction.FindAllStringSubmatch(m[3], -1) {
			actions = append(actions, a[1])
		}
		out[key] = actions
	}
	if len(out) == 0 {
		t.Fatal("spec 013 has no route table")
	}
	return out
}

// TestEveryRouteThisBuildRegistersIsOneOfSpec013sTable is criterion 1 of
// spec 013 for the whole surface rather than for one package's half of it.
//
// This is the one place the whole build is visible. internal/api is under
// every package that contributes rows and imports none of them, so the
// frame's own test reads the frame and the rows a test contributes, and each
// owning package's test reads its own; the union is here, and the node
// registers the same union because Routes and Table are two readings of one
// declaration.
//
// The converse — that every row of the table is registered — closed when the
// last handler landed, and is the second half below: the union holds every
// row spec 013 names, so criterion 1 of that spec is proved in both
// directions from the one place every declaration is visible.
func TestEveryRouteThisBuildRegistersIsOneOfSpec013sTable(t *testing.T) {
	spec := specRoutes(t)
	if len(spec) < 41 {
		t.Fatalf("spec 013's tables name %d routes, and the surface is forty-one plus the document", len(spec))
	}
	registered := routes()
	seen := map[string]bool{}
	for _, r := range registered {
		key := r.Method + " " + r.Path
		if seen[key] {
			t.Errorf("%s is registered twice", key)
		}
		seen[key] = true
		actions, named := spec[key]
		if !named {
			t.Errorf("%s is registered and spec 013's table does not name it", key)
			continue
		}
		// A row whose third column names actions is held to them. The rows
		// that follow the action their attach asked name none, and the
		// owning package's own test holds each to the action it puts.
		if len(actions) > 0 && !slices.Contains(actions, r.Action) {
			t.Errorf("%s asks %q; spec 013's row names %v", key, r.Action, actions)
		}
		if len(actions) == 0 && r.Public && r.Action != "" {
			t.Errorf("%s is outside the verifier and asks %q; the grant the token resolves to is the whole of the authorization", key, r.Action)
		}
		if r.Summary == "" {
			t.Errorf("%s carries no summary for the document", key)
		}
	}
	for key := range spec {
		if !seen[key] {
			t.Errorf("spec 013's table names %s and this build registers no such route", key)
		}
	}
	t.Logf("%d of spec 013's %d routes are registered in this build", len(registered), len(spec))
}

// chosenPerRequest are the actions of spec 006's vocabulary no row of spec
// 013's table declares, because the request chooses them.
//
// There is one. POST /v1/workspaces/{id}/attach asks workspace.read for a ro
// mount and workspace.attach for a rw one, and the renew and the release
// that follow it ask the action their attach asked; actionOf in
// internal/workspaces/lease.go is where that is decided, and that package's
// own tests drive both modes and hold each to its answer. The rows carry the
// read, which is the action every one of them asks at least, so the attach
// action reaches no declaration and the count below would be short by one
// without this list.
var chosenPerRequest = []string{authorizer.ActionWorkspaceAttach}

// TestEveryActionOfTheVocabularyIsAskedByARoute is criterion 3 of spec 013:
// the twenty-three actions of spec 006 and the routes of spec 013 cover each
// other, so no action is a permission nothing can exercise and no route asks
// a question no authorizer was given.
//
// It reads the union of every declaration, which is this package's, for the
// reason the criterion above it reads it here: internal/api is under every
// package that contributes rows, so the whole surface is visible in one
// place and that place is the generator's.
func TestEveryActionOfTheVocabularyIsAskedByARoute(t *testing.T) {
	asked := map[string]bool{}
	for _, r := range routes() {
		if r.Action == "" {
			// The three public link routes of spec 008. The grant the token
			// resolves to is the whole of their authorization.
			continue
		}
		if !authorizer.Known(r.Action) {
			t.Errorf("%s %s asks %q, which spec 006's vocabulary does not name", r.Method, r.Path, r.Action)
			continue
		}
		asked[r.Action] = true
	}
	for _, action := range chosenPerRequest {
		if !authorizer.Known(action) {
			t.Errorf("%q is named as chosen per request and spec 006's vocabulary does not name it", action)
			continue
		}
		asked[action] = true
	}

	vocabulary := authorizer.Actions()
	var missing []string
	for _, action := range vocabulary {
		if !asked[action] {
			missing = append(missing, action)
		}
	}
	if len(missing) > 0 {
		t.Errorf("spec 006's vocabulary names %v and no route of this build asks them; "+
			"an action no route reaches is a permission an authorizer can answer and nobody can exercise", missing)
	}
	if len(asked) != len(vocabulary) {
		t.Errorf("the surface asks %d actions and spec 006's vocabulary names %d", len(asked), len(vocabulary))
	}
	t.Logf("the %d routes of this build ask %d of spec 006's %d actions", len(routes()), len(asked), len(vocabulary))
}
