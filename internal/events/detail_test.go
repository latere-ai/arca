// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package events

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Criterion 11 of spec 012: no event detail carries a byte of object content
// and none carries a link token. The log is a tail a consumer reads with
// event.read on a whole space, so a detail that carried content would hand a
// reader of the log what a reader of the object was refused, and one that
// carried a token would hand a reader of the log a bearer.
//
// What holds it is the shape the writers pass rather than a rule at the
// insert: a detail is built at the call site, so the call site is where a
// key can be read, and a key that has to be declared here is a key somebody
// had to think about.

// detailVocabulary is every key an event detail may carry, with what it is.
// A writer that adds one adds it here, which is the review this test exists
// to force. Nothing in this table is content and nothing is a credential:
// every member is a count, a size, an identifier the caller already holds,
// an enumeration, or a path the caller named.
var detailVocabulary = map[string]string{
	DetailAdmin:     "the mark of spec 012: neither ownership nor a grant explained the allow",
	"trashed":       "whether a delete was soft",
	"version_no":    "which version a prune or a restore named",
	"size":          "the bytes an object holds",
	"multipart":     "whether the object arrived through an upload session",
	"from":          "the path a move came from, which the caller sent",
	"share_id":      "the identifier of the grant, which its creator holds",
	"grantee_kind":  "subject, token or public",
	"permission":    "the rung, read, write or admin",
	"path_prefix":   "the subtree a grant covers, which the caller sent",
	"slug":          "the name of a workspace, which the caller chose",
	"files":         "a count of paths",
	"bytes":         "a count of bytes",
	"deleted":       "a count of paths a sync dropped",
	"attachment_id": "the identifier of an attachment, which its holder holds",
	"sandbox_id":    "the sandbox a lease is held by, which the caller sent",
	"mode":          "reader or writer",
}

// builtDetails names the one writer that passes a detail it built rather
// than a literal, and what its keys are. The reaper counts one member of its
// own closed vocabulary per kind it changed, so the keys are that vocabulary
// and nothing else. They are spelled out here rather than imported, because
// internal/reaper reads this package and the dependency cannot run the other
// way; a kind renamed there and not here fails the list below.
var builtDetails = map[string]string{
	"internal/reaper/reaper.go": "one count per reaper kind the run changed",
}

// reapKinds is internal/reaper's Kind vocabulary, which is the key set of
// the detail above.
var reapKinds = []string{
	"orphan_object", "orphan_candidate", "missing_bytes", "workspace_purged",
	"file_purged", "trash_purged", "share_expired", "event_pruned",
	"star_pruned", "version_pruned", "upload_aborted", "usage_corrected",
	"lease_expired",
}

func TestEventDetailIsMetadataOnly(t *testing.T) {
	root := moduleRoot(t)
	built := map[string]bool{}
	err := walkSources(root, func(path string) error {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		rel := filepath.ToSlash(mustRel(t, root, path))
		for _, written := range appendedDetails(file) {
			if written.carried {
				continue
			}
			if !written.literal {
				built[rel] = true
				if _, declared := builtDetails[rel]; !declared {
					t.Errorf("%s passes an event detail it built, and no entry here says what its keys are", rel)
				}
				continue
			}
			for _, key := range written.keys {
				if _, ok := detailVocabulary[key]; !ok {
					t.Errorf("%s appends the detail key %q, which is in no entry of the vocabulary", rel, key)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	for file := range builtDetails {
		if !built[file] {
			t.Errorf("%s is named as building a detail and builds none; drop the entry", file)
		}
	}
	// The one built detail's keys, which no literal in the tree spells.
	for _, kind := range reapKinds {
		if _, ok := detailVocabulary[kind]; ok {
			t.Errorf("the reap kind %q is also a literal detail key; the two vocabularies overlap", kind)
		}
	}
}

// TestTheVocabularyNamesEveryKeyItDeclares: a key with no sentence beside it
// is a key nobody reviewed, which is the whole mechanism.
func TestTheVocabularyNamesEveryKeyItDeclares(t *testing.T) {
	for key, why := range detailVocabulary {
		if strings.TrimSpace(why) == "" {
			t.Errorf("the key %q carries no sentence saying what it is", key)
		}
	}
}

// TestTheGuardReadsTheDetailOfEveryEventType holds the walk above to what it
// claims to read. Every seam of the tree has an Event of its own that the
// ledger converts, so a guard that read only this package's would pass a
// writer in internal/files without looking at it.
func TestTheGuardReadsTheDetailOfEveryEventType(t *testing.T) {
	for _, c := range []struct {
		name    string
		source  string
		keys    []string
		literal bool
		carried bool
		found   bool
	}{
		{
			name: "this package's event",
			source: `package events
				var e = Event{Action: ActionPut, Detail: map[string]any{"size": 1}}`,
			keys: []string{"size"}, literal: true, found: true,
		},
		{
			name: "the files seam's own event",
			source: `package files
				var e = Event{Action: EventDelete, Detail: map[string]any{"trashed": true}}`,
			keys: []string{"trashed"}, literal: true, found: true,
		},
		{
			name: "an event built through a package",
			source: `package reaper
				var e = events.Event{Detail: map[string]any{"size": 1}}`,
			keys: []string{"size"}, literal: true, found: true,
		},
		{
			name: "a detail the writer built",
			source: `package reaper
				var e = events.Event{Detail: detail}`,
			literal: false, found: true,
		},
		{
			name: "an adapter carrying a seam's detail through",
			source: `package main
				var e = events.Event{Owner: s.Owner, Detail: s.Detail}`,
			carried: true, found: true,
		},
		{
			name: "an event with no detail",
			source: `package files
				var e = Event{Action: EventPut}`,
			found: false,
		},
		{
			name: "another type with a detail field",
			source: `package api
				var r = Refusal{Code: "not_found", Detail: "there is no object there"}`,
			found: false,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "p.go", c.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			got := appendedDetails(file)
			if !c.found {
				if len(got) != 0 {
					t.Fatalf("the guard read %v and the case holds nothing", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("the guard read %d details and the case holds one", len(got))
			}
			if got[0].literal != c.literal {
				t.Fatalf("the guard read literal=%v and the case holds %v", got[0].literal, c.literal)
			}
			if got[0].carried != c.carried {
				t.Fatalf("the guard read carried=%v and the case holds %v", got[0].carried, c.carried)
			}
			if !slices.Equal(got[0].keys, c.keys) {
				t.Fatalf("the guard read the keys %v and the case holds %v", got[0].keys, c.keys)
			}
		})
	}
}

// detail is one Detail field a site passes: the keys when it is a map
// literal, that it carries another event's detail unchanged, or neither.
type detail struct {
	literal bool
	carried bool
	keys    []string
}

// appendedDetails answers every event detail a file passes.
//
// Any composite literal whose type is spelled Event counts, bare or
// qualified. Each seam of the tree has an Event of its own that the ledger
// converts into this package's, so reading only this package's type would
// leave every writer outside it unread, and a type called Event that is not
// an event of the log does not exist in this tree.
func appendedDetails(file *ast.File) []detail {
	var found []detail
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !namedEvent(lit.Type) {
			return true
		}
		for _, element := range lit.Elts {
			pair, ok := element.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := pair.Key.(*ast.Ident)
			if !ok || key.Name != "Detail" {
				continue
			}
			found = append(found, readDetail(pair.Value))
		}
		return true
	})
	return found
}

// namedEvent reports whether a type expression is spelled Event.
func namedEvent(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name == "Event"
	case *ast.SelectorExpr:
		return t.Sel.Name == "Event"
	}
	return false
}

// readDetail answers the keys of a map literal, a detail carried through
// from another event, or neither.
//
// The three adapters of cmd/arcad convert a seam's event into this package's
// and pass its detail along as it came. They write no key, so the keys are
// read where they were written and reading them again here would say nothing
// about them.
func readDetail(expr ast.Expr) detail {
	if sel, ok := expr.(*ast.SelectorExpr); ok && sel.Sel.Name == "Detail" {
		return detail{carried: true}
	}
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return detail{}
	}
	if _, isMap := lit.Type.(*ast.MapType); !isMap {
		return detail{}
	}
	d := detail{literal: true}
	for _, element := range lit.Elts {
		pair, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		basic, ok := pair.Key.(*ast.BasicLit)
		if !ok || basic.Kind != token.STRING {
			// A key that is not spelled out is a key this test cannot read,
			// so it reads as a detail the writer built.
			return detail{}
		}
		text, err := strconv.Unquote(basic.Value)
		if err != nil {
			return detail{}
		}
		d.keys = append(d.keys, text)
	}
	sort.Strings(d.keys)
	return d
}

// mustRel is the path of a file inside the tree, for a message that names it.
func mustRel(t *testing.T, root, path string) string {
	t.Helper()
	rel, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatalf("name %s inside the tree: %v", path, err)
	}
	return rel
}
