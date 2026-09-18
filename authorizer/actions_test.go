// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package authorizer

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"
)

// TestVocabularyMatchesSpec006 holds the table to spec 006's, read from the
// spec in this repository rather than from a copy of it: the table's text is
// the contract, an action the spec names and the package does not is a
// question no endpoint can be asked, and an action the package names and the
// spec does not is a row nobody decided on.
func TestVocabularyMatchesSpec006(t *testing.T) {
	want := actionsOfSpec006(t)
	if len(want) != 23 {
		t.Fatalf("spec 006's table names %d actions, want the twenty-three: %v", len(want), want)
	}
	if got := Vocabulary().Actions; !slices.Equal(got, want) {
		t.Errorf("the vocabulary drifted from spec 006:\n got %v\nwant %v", got, want)
	}
	for _, a := range want {
		if !Known(a.Name) {
			t.Errorf("%s: spec 006 names it and the package does not know it", a.Name)
		}
		if got := Kind(a.Name); got != a.Kind {
			t.Errorf("%s acts on %q; spec 006's row says %q", a.Name, got, a.Kind)
		}
	}
}

// TestTheSevenKindsAreSpec006s: the table's kinds are the seven of the spec,
// in its order, and each one is named by at least one action.
func TestTheSevenKindsAreSpec006s(t *testing.T) {
	want := []string{KindFile, KindUpload, KindShare, KindLink, KindWorkspace, KindEvent, KindSpace}
	if got := Vocabulary().Kinds(); !slices.Equal(got, want) {
		t.Errorf("the vocabulary names the kinds %v; spec 006's seven are %v", got, want)
	}
	spec := map[string]bool{}
	for _, a := range actionsOfSpec006(t) {
		spec[a.Kind] = true
	}
	for _, kind := range want {
		if !spec[kind] {
			t.Errorf("%s is a kind of the package and not a row of spec 006", kind)
		}
	}
}

// TestUnknownActionIsUnknown: a string outside the table has no kind and is
// not known, which is what the client refuses before the wire and what an
// endpoint answers a 400 for.
func TestUnknownActionIsUnknown(t *testing.T) {
	for _, action := range []string{
		"", "file", "file.explode", "File.read", "file.read ", "quota.read", "webhook.create",
	} {
		if Known(action) || Kind(action) != "" {
			t.Errorf("%q reads as one of the vocabulary", action)
		}
	}
}

// TestRetiredActionsAreGone: the six actions of the service Arca replaces
// that spec 019 removes are not in the table. The vocabulary shrank from
// twenty-nine to twenty-three, and a stale constant is how an endpoint keeps
// deciding a question nothing asks.
func TestRetiredActionsAreGone(t *testing.T) {
	for _, action := range []string{
		"quota.read", "quota.write",
		"webhook.create", "webhook.read", "webhook.update", "webhook.delete",
	} {
		if Known(action) {
			t.Errorf("%s is retired by spec 019 and the vocabulary still names it", action)
		}
	}
}

// TestVocabularyIsWellFormed: the declared table is one the shared contract
// accepts, which is what proves no row names an action twice or leaves a
// kind empty.
func TestVocabularyIsWellFormed(t *testing.T) {
	v, err := authz.NewVocabulary(Core, table...)
	if err != nil {
		t.Fatalf("the declared table is not a vocabulary: %v", err)
	}
	if got := Vocabulary(); got.Core != v.Core || !slices.Equal(got.Actions, v.Actions) {
		t.Errorf("Vocabulary() = %+v; the validated table is %+v", got, v)
	}
	if Core != "arca" {
		t.Errorf("the core is %q; the grants on a personal key qualify their actions with %q", Core, "arca")
	}
}

// TestListActionsAreNamedList: every action whose answer is a page is named
// with the .list suffix the shared contract reads, and no other action
// carries that suffix. The three pages of spec 013 are a subtree, a space's
// grants, and a space's workspaces.
func TestListActionsAreNamedList(t *testing.T) {
	var lists []string
	for _, a := range table {
		if authz.IsList(a.Name) {
			lists = append(lists, a.Name)
			continue
		}
		if strings.Contains(a.Name, "list") {
			t.Errorf("%s reads as a list action and does not end in .list", a.Name)
		}
	}
	want := []string{ActionFileList, ActionShareList, ActionWorkspaceList}
	if !slices.Equal(lists, want) {
		t.Errorf("the list actions are %v, want %v", lists, want)
	}
}

// TestActionsIsACopy: the table is the package's, and a caller that sorts or
// appends to what it was handed changes nothing here.
func TestActionsIsACopy(t *testing.T) {
	first := Actions()
	first[0] = "file.explode"
	if Actions()[0] != ActionFileRead {
		t.Error("Actions returns the table itself; it returns a copy")
	}
	v := Vocabulary()
	v.Actions[0] = authz.Action{Name: "file.explode", Kind: KindFile}
	if Vocabulary().Actions[0].Name != ActionFileRead {
		t.Error("Vocabulary returns the table itself; it returns a copy")
	}
	if got := Actions(); len(got) != len(table) {
		t.Errorf("Actions lists %d of %d actions", len(got), len(table))
	}
}

// TestEveryKindReadsAsAHeading: a resource kind is a type name, and a person
// picking what a personal key may do reads a heading. The picker groups by
// kind and names each group with the vocabulary's own label, so Arca declares
// one per kind and no console writes a word of it.
func TestEveryKindReadsAsAHeading(t *testing.T) {
	v := Vocabulary()
	want := map[string]string{
		KindFile: "Files", KindUpload: "Uploads", KindShare: "Shares", KindLink: "Links",
		KindWorkspace: "Workspaces", KindEvent: "Events", KindSpace: "Spaces",
	}
	kinds := v.Kinds()
	if len(kinds) != len(want) {
		t.Fatalf("the vocabulary names %d kinds, %v; spec 006's table has %d", len(kinds), kinds, len(want))
	}
	for _, kind := range kinds {
		if got := v.Label(kind); got != want[kind] {
			t.Errorf("Label(%q) = %q, want %q", kind, got, want[kind])
		}
	}
}

// TestTheLabelsAreTheVocabularysOwn: every consumer reads the same headings,
// and a consumer that edits the map it was handed edits nothing here.
func TestTheLabelsAreTheVocabularysOwn(t *testing.T) {
	edited := Vocabulary().WithLabels(map[string]string{KindFile: "Documents"})
	if got := edited.Label(KindFile); got != "Documents" {
		t.Fatalf("a consumer's own labels did not take: Label(%q) = %q", KindFile, got)
	}
	if got := Vocabulary().Label(KindFile); got != "Files" {
		t.Errorf("a consumer's copy changed the published table: Label(%q) = %q", KindFile, got)
	}
}

var (
	// backticked matches one backticked token of a table cell.
	backticked = regexp.MustCompile("`([a-zA-Z.]+)`")
	// parenthesised matches an aside in a cell, which names values and
	// conditions rather than fields: "(absent on a write that creates)".
	parenthesised = regexp.MustCompile(`\([^)]*\)`)
)

// specRow is one row of spec 006's action table: the kind, the actions it
// carries, and the resource fields a question about it names.
type specRow struct {
	kind    string
	actions []string
	fields  []string
}

// rowsOfSpec006 reads the action table of specs/006-identity.md: the rows
// under the "| Kind | Actions | Resource fields |" header, each row's kind
// from its first column, its actions from the backticked tokens of its
// second, and its resource fields from the backticked tokens of its third
// with every parenthesised aside removed, since an aside names a value or a
// condition and not a field.
func rowsOfSpec006(t *testing.T) []specRow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "specs", "006-identity.md"))
	if err != nil {
		t.Fatalf("spec 006: %v", err)
	}
	var out []specRow
	inTable := false
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if !inTable {
			inTable = strings.HasPrefix(line, "| Kind | Actions | Resource fields |")
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 3 || strings.HasPrefix(strings.TrimSpace(cells[0]), "---") {
			continue
		}
		kinds := tokens(cells[0])
		if len(kinds) != 1 {
			t.Fatalf("a row of spec 006's table names %d kinds, want one: %q", len(kinds), line)
		}
		row := specRow{kind: kinds[0], actions: tokens(cells[1]), fields: tokens(parenthesised.ReplaceAllString(cells[2], " "))}
		if len(row.actions) == 0 {
			t.Fatalf("spec 006's %s row names no action", row.kind)
		}
		out = append(out, row)
	}
	if !inTable {
		t.Fatal("spec 006 has no action table")
	}
	return out
}

// actionsOfSpec006 is the table flattened to the actions in its order, each
// paired with the kind of the row it sits in.
func actionsOfSpec006(t *testing.T) []authz.Action {
	t.Helper()
	var out []authz.Action
	for _, row := range rowsOfSpec006(t) {
		for _, name := range row.actions {
			out = append(out, authz.Action{Name: name, Kind: row.kind})
		}
	}
	return out
}

// tokens is the backticked tokens of one table cell, in order.
func tokens(cell string) []string {
	var out []string
	for _, m := range backticked.FindAllStringSubmatch(cell, -1) {
		out = append(out, m[1])
	}
	return out
}
