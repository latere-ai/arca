// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package events

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestTheVocabularyIsClosedAndInTheOrderTheSpecListsIt(t *testing.T) {
	want := []Action{
		"put", "move", "delete", "restore", "purge",
		"attach", "release", "sync", "reap", "share_created", "share_revoked",
	}
	if got := Actions(); !slices.Equal(got, want) {
		t.Fatalf("the vocabulary is %v, and spec 010 lists %v", got, want)
	}
	if len(Actions()) != 11 {
		t.Fatalf("the vocabulary holds %d actions, and spec 010 names eleven", len(Actions()))
	}
	for _, a := range want {
		if !a.Valid() {
			t.Errorf("%q is in the table and reports itself invalid", a)
		}
	}
	// The three the predecessor had and Arca does not: an approval it no
	// longer records, a refusal that is not something that happened to the
	// space, and a spelling.
	for _, a := range []Action{"", "share_resolved", "quota_exceeded", "Put", "webhook.sent"} {
		if a.Valid() {
			t.Errorf("%q is outside the table and reports itself valid", a)
		}
	}
}

func TestActionsAnswersACopy(t *testing.T) {
	Actions()[0] = "rewritten"
	if Actions()[0] != ActionPut {
		t.Fatal("a caller that rewrites the answer rewrites the table")
	}
}

// Criterion 8 of spec 010: every action a writer appends is in the closed
// table, and no writer appends one outside it. A writer that names a
// constant is the compiler's to check; what this test catches is the writer
// that builds an action out of a string literal instead.
func TestEveryAppendedActionIsInTheTable(t *testing.T) {
	root := moduleRoot(t)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		rel, _ := filepath.Rel(root, path)
		for _, named := range appendedActions(file) {
			if !named.Valid() {
				t.Errorf("%s appends the action %q, which is outside the table of spec 010", rel, named)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
}

// TestTheGuardReadsALiteralOutOfBothShapes holds the walk above to what it
// claims to read: the Action field of an Event and a conversion of a literal
// to the type. The predecessor's vocabulary is the fixture, because
// share_resolved is exactly the action a port would carry across.
func TestTheGuardReadsALiteralOutOfBothShapes(t *testing.T) {
	for _, c := range []struct {
		name   string
		source string
		want   []Action
	}{
		{"an event built with a literal", `package p
			var e = Event{Owner: "o", Action: "share_resolved"}`, []Action{"share_resolved"}},
		{"an event built through the package", `package p
			import "latere.ai/x/arca/internal/events"
			var e = events.Event{Action: "quota_exceeded"}`, []Action{"quota_exceeded"}},
		{"a conversion", `package p
			var a = Action("share_resolved")`, []Action{"share_resolved"}},
		{"a constant of the table", `package p
			var e = Event{Action: ActionPut}`, nil},
		{"another type with an action field", `package p
			var r = Rule{Action: "*", Allow: true}`, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "p.go", c.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := appendedActions(file); !slices.Equal(got, c.want) {
				t.Fatalf("the guard read %v and the case holds %v", got, c.want)
			}
		})
	}
}

// appendedActions answers every action a file names as a literal: the Action
// field of an Event it builds, and every conversion of a literal to the
// type.
func appendedActions(file *ast.File) []Action {
	var named []Action
	ast.Inspect(file, func(n ast.Node) bool {
		if lit, ok := n.(*ast.CompositeLit); ok && typeName(lit.Type) == "Event" {
			named = append(named, actionFields(lit)...)
		}
		if call, ok := n.(*ast.CallExpr); ok && typeName(call.Fun) == "Action" && len(call.Args) == 1 {
			named = append(named, literal(call.Args[0])...)
		}
		return true
	})
	return named
}

// actionFields answers the literals an event's Action field is built from.
func actionFields(lit *ast.CompositeLit) []Action {
	var named []Action
	for _, element := range lit.Elts {
		pair, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := pair.Key.(*ast.Ident); ok && key.Name == "Action" {
			named = append(named, literal(pair.Value)...)
		}
	}
	return named
}

// typeName answers the bare name of a type expression, so events.Event and
// Event read the same.
func typeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	}
	return ""
}

// literal answers the string a node spells out, or nothing when the node is
// not a string literal.
func literal(expr ast.Expr) []Action {
	basic, ok := expr.(*ast.BasicLit)
	if !ok || basic.Kind != token.STRING {
		return nil
	}
	text, err := strconv.Unquote(basic.Value)
	if err != nil {
		return nil
	}
	return []Action{Action(text)}
}

// moduleRoot answers the repository this test is compiled from. The tempdir
// gate runs the suite from an empty directory, so the source's own path is
// the only way back to the tree.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the source of this test names no file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
