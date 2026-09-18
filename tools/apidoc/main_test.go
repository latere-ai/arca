// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// TestTheCommittedDocumentIsCurrent is criterion 13 of spec 013: the file at
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
