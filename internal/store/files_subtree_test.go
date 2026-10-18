// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"errors"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"latere.ai/x/arca/internal/store/migrations"
	"latere.ai/x/arca/object"
)

// The unit half of the folder statements of spec 028: what each sends, what
// it binds, what it scans and what it wraps. What the SQL means over real
// rows is the store tier's.

// TestOmittedIsASegmentMatch: an omitted prefix covers its own path and
// everything under it, and never a sibling whose name begins with it.
func TestOmittedIsASegmentMatch(t *testing.T) {
	omit := []string{"files/team/pay", "files/agents/7c1e"}
	for path, want := range map[string]bool{
		"files/team/pay":            true,
		"files/team/pay/may.pdf":    true,
		"files/team/pay/2026/a.pdf": true,
		"files/agents/7c1e/out.txt": true,
		"files/team/payroll":        false,
		"files/team/payroll/a.pdf":  false,
		"files/team":                false,
		"files/agents/7c1":          false,
		"files/other.md":            false,
	} {
		if got := Omitted(omit, path); got != want {
			t.Errorf("Omitted(%q) = %t, want %t", path, got, want)
		}
	}
	if Omitted(nil, "files/team/pay") {
		t.Error("an answer that omits nothing left a path out")
	}
}

// TestAnEnumerationCarriesTheOmissionInItsStatement: every statement that
// enumerates binds the omitted prefixes as an array and filters on them in
// the query, so a page is never short for a row the answer left out. An
// answer with none binds an empty array and never NULL.
func TestAnEnumerationCarriesTheOmissionInItsStatement(t *testing.T) {
	omit := []string{"files/team/payroll"}
	for name, run := range map[string]func(q Querier, omit []string) error{
		"ListPrefix": func(q Querier, omit []string) error {
			_, _, err := NewFiles().ListPrefix(t.Context(), q, "space", "files/", "", 10, omit)
			return err
		},
		"SearchPrefix": func(q Querier, omit []string) error {
			_, _, err := NewFiles().SearchPrefix(t.Context(), q, "space", "files/", "may", "", 10, omit)
			return err
		},
		"RecentPrefix": func(q Querier, omit []string) error {
			_, err := NewFiles().RecentPrefix(t.Context(), q, "space", "files/", "", 10, omit)
			return err
		},
		"RecentPrefix with a text": func(q Querier, omit []string) error {
			_, err := NewFiles().RecentPrefix(t.Context(), q, "space", "files/", "may", 10, omit)
			return err
		},
		"ListTrash": func(q Querier, omit []string) error {
			_, err := NewFiles().ListTrash(t.Context(), q, "space", TrashCursor{}, 10, time.Time{}, omit)
			return err
		},
		"PurgeTrash": func(q Querier, omit []string) error {
			_, err := NewFiles().PurgeTrash(t.Context(), q, "space", "", omit)
			return err
		},
		"DropSubtree": func(q Querier, omit []string) error {
			_, err := NewFiles().DropSubtree(t.Context(), q, "space", "files/team", omit)
			return err
		},
		"Stars.List": func(q Querier, omit []string) error {
			_, err := NewStars().List(t.Context(), q, "sub", nil, StarCursor{}, 10, omit)
			return err
		},
		"ListSpace": func(q Querier, omit []string) error {
			_, err := NewShares().ListSpace(t.Context(), q, "space", "", "", 10, omit)
			return err
		},
		"ListGrantee": func(q Querier, omit []string) error {
			_, err := NewShares().ListGrantee(t.Context(), q, "sub", "", 10, omit)
			return err
		},
		"ListTokens": func(q Querier, omit []string) error {
			_, err := NewShares().ListTokens(t.Context(), q, "space", "", 10, omit)
			return err
		},
		"Subtree": func(q Querier, omit []string) error {
			_, err := NewShares().Subtree(t.Context(), q, "space", "files/team", "", 10, omit)
			return err
		},
	} {
		for _, given := range [][]string{omit, nil} {
			q := &fakeQuerier{rows: &fakeRows{}}
			if err := run(q, given); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			statement := q.statements[0]
			if !strings.Contains(statement, "unnest($") || !strings.Contains(statement, "omitted.prefix || '/'") {
				t.Errorf("%s does not filter the omission in its statement:\n%s", name, statement)
			}
			var bound []string
			found := false
			for _, arg := range q.args[0] {
				if list, ok := arg.([]string); ok {
					bound, found = list, true
				}
			}
			if !found || bound == nil || !slices.Equal(bound, omitted(given)) {
				t.Errorf("%s bound the omission %v (found=%t), want %v", name, bound, found, omitted(given))
			}
		}
	}
	// A purge of one named path is a question about that path and reads no
	// omission.
	q := &fakeQuerier{rows: &fakeRows{}}
	if _, err := NewFiles().PurgeTrash(t.Context(), q, "space", "files/team/payroll/may.pdf", omit); err != nil {
		t.Fatal(err)
	}
	if bound := q.args[0][2].([]string); len(bound) != 0 {
		t.Errorf("a purge of one path bound the omission %v", bound)
	}
}

// TestTheFolderStatementsNameEverythingUnderTheFolderAndNothingElse: the
// pattern a folder statement binds is the folder and a slash, with what a
// LIKE would read as a wildcard escaped, so the folder's own path and a
// sibling whose name begins with it are outside.
func TestTheFolderStatementsNameEverythingUnderTheFolderAndNothingElse(t *testing.T) {
	for folder, want := range map[string]string{
		"files/notes":     `files/notes/%`,
		"files/notes/":    `files/notes/%`,
		"files/100%_done": `files/100\%\_done/%`,
		`files/a\b`:       `files/a\\b/%`,
	} {
		if got := under(folder); got != want {
			t.Errorf("under(%q) = %q, want %q", folder, got, want)
		}
	}

	counted := &fakeQuerier{row: values(int64(3), int64(4096))}
	stat, err := NewFiles().StatSubtree(t.Context(), counted, "space", "files/notes", nil)
	if err != nil || stat.Files != 3 || stat.Bytes != 4096 {
		t.Fatalf("StatSubtree = %+v, %v", stat, err)
	}
	if counted.args[0][1] != `files/notes/%` || !strings.Contains(counted.statements[0], "deleted_at IS NULL") {
		t.Errorf("the count read %v with\n%s", counted.args[0], counted.statements[0])
	}
	if _, err := NewFiles().StatSubtree(t.Context(), &fakeQuerier{row: failing(errFault)}, "space", "files/notes", nil); err == nil {
		t.Error("a count that failed reported success")
	}

	trashed := &fakeQuerier{row: values(int64(2), int64(10))}
	sub, err := NewFiles().TrashSubtree(t.Context(), trashed, "space", "files/notes", nil)
	if err != nil || sub.Files != 2 || sub.Bytes != 10 {
		t.Fatalf("TrashSubtree = %+v, %v", sub, err)
	}
	if !strings.Contains(trashed.statements[0], "SET deleted_at = now()") ||
		!strings.Contains(trashed.statements[0], "deleted_at IS NULL") {
		t.Errorf("the trash statement is\n%s", trashed.statements[0])
	}
	if _, err := NewFiles().TrashSubtree(t.Context(), &fakeQuerier{row: failing(errFault)}, "space", "files/notes", nil); err == nil {
		t.Error("a trash that failed reported success")
	}
}

// TestDroppingASubtreeAnswersTheRowsTheHistoryAndTheBytes: the rows of
// files are counted, the superseded contents are not, and the bytes and the
// objects are of both, because the ledger counts both tables and the bucket
// holds a key for each.
func TestDroppingASubtreeAnswersTheRowsTheHistoryAndTheBytes(t *testing.T) {
	live, trashed, version := object.NewID(), object.NewID(), object.NewID()
	rows := &fakeRows{scans: []func(...any) error{
		func(dest ...any) error { return assign(dest, []any{live, int64(100), true}) },
		func(dest ...any) error { return assign(dest, []any{trashed, int64(20), true}) },
		func(dest ...any) error { return assign(dest, []any{version, int64(3), false}) },
	}}
	q := &fakeQuerier{rows: rows}
	gone, err := NewFiles().DropSubtree(t.Context(), q, "space", "files/notes", nil)
	if err != nil {
		t.Fatalf("DropSubtree: %v", err)
	}
	if gone.Files != 2 || gone.Bytes != 123 || !slices.Equal(gone.Objects, []object.ID{live, trashed, version}) {
		t.Errorf("the drop answered %+v", gone)
	}
	if !strings.Contains(q.statements[0], "DELETE FROM files") || !strings.Contains(q.statements[0], "DELETE FROM file_versions") {
		t.Errorf("the drop does not take the history with the rows:\n%s", q.statements[0])
	}
	if !rows.closed {
		t.Error("the rows were not closed")
	}
	for name, broken := range map[string]*fakeQuerier{
		"a failed statement":       {queryErr: errFault},
		"a row that will not scan": {rows: &fakeRows{scans: []func(...any) error{failing(errFault).scan}}},
		"rows cut short":           {rows: &fakeRows{err: errFault}},
	} {
		if _, err := NewFiles().DropSubtree(t.Context(), broken, "space", "files/notes", nil); err == nil {
			t.Errorf("%s reported success", name)
		}
	}
}

// TestUnreferencedReadsEveryTableThatHoldsAnObjectID: the batched reference
// check names the tables the single one names, so the two readings of the
// union move together and a folder delete removes no byte a row still
// names.
func TestUnreferencedReadsEveryTableThatHoldsAnObjectID(t *testing.T) {
	schema := tablesWithColumn(t, "object_id")
	if missing := tablesMissing(schema, unreferencedSQL); len(missing) > 0 {
		t.Fatalf("the batched reference check leaves out %v", missing)
	}
	free := object.NewID()
	q := &fakeQuerier{rows: &fakeRows{scans: []func(...any) error{
		func(dest ...any) error { return assign(dest, []any{free}) },
	}}}
	got, err := NewFiles().Unreferenced(t.Context(), q, []object.ID{free, object.NewID()})
	if err != nil || !slices.Equal(got, []object.ID{free}) {
		t.Fatalf("Unreferenced = %v, %v", got, err)
	}
	if none, err := NewFiles().Unreferenced(t.Context(), &fakeQuerier{}, nil); err != nil || none != nil {
		t.Errorf("no objects asked about = %v, %v", none, err)
	}
	for name, broken := range map[string]*fakeQuerier{
		"a failed statement":       {queryErr: errFault},
		"a row that will not scan": {rows: &fakeRows{scans: []func(...any) error{failing(errFault).scan}}},
		"rows cut short":           {rows: &fakeRows{err: errFault}},
	} {
		if _, err := NewFiles().Unreferenced(t.Context(), broken, []object.ID{free}); err == nil {
			t.Errorf("%s reported success", name)
		}
	}
}

// TestDeletingTheGrantsUnderAFolderLeavesWhatAnOmissionKeeps: the statement
// takes a grant on the folder or under it, and leaves one at, under or above
// an omitted subtree, because the folder delete left that subtree in place.
func TestDeletingTheGrantsUnderAFolderLeavesWhatAnOmissionKeeps(t *testing.T) {
	q := &fakeQuerier{tag: pgconn.NewCommandTag("DELETE 3")}
	n, err := NewShares().DeleteUnder(t.Context(), q, "space", "files/team", []string{"files/team/payroll"})
	if err != nil || n != 3 {
		t.Fatalf("DeleteUnder = %d, %v", n, err)
	}
	statement := q.statements[0]
	for _, want := range []string{
		"path_prefix = $2 OR path_prefix LIKE $3",
		"path_prefix = omitted.prefix",
		"starts_with(path_prefix, omitted.prefix || '/')",
		"starts_with(omitted.prefix, path_prefix || '/')",
	} {
		if !strings.Contains(statement, want) {
			t.Errorf("the statement does not hold %q:\n%s", want, statement)
		}
	}
	if q.args[0][1] != "files/team" || q.args[0][2] != `files/team/%` {
		t.Errorf("the statement bound %v", q.args[0])
	}
	if _, err := NewShares().DeleteUnder(t.Context(), &fakeQuerier{execErr: errFault}, "space", "files/team", nil); err == nil {
		t.Error("a failed delete reported success")
	}
}

// columnOf reads the name a column definition opens with.
var columnOf = regexp.MustCompile(`^\s*([a-z_]+)\s+[A-Z]`)

// TestNoColumnHoldsALimit is criterion 11 of spec 028 for the schema: Arca
// stores no limit. A limit reaches it on an authorizer's answer and lives
// for that answer, so no table of any migration carries a column for a byte
// allowance, for versions or the trash being off, or for an omission.
func TestNoColumnHoldsALimit(t *testing.T) {
	names, err := fs.Glob(migrations.Files, "*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	columns := 0
	for _, name := range names {
		source, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToUpper(string(source)), "ADD COLUMN") {
			t.Errorf("%s adds a column this test does not read; extend it", name)
		}
		for _, m := range createTable.FindAllStringSubmatch(string(source), -1) {
			for line := range strings.SplitSeq(m[2], "\n") {
				found := columnOf.FindStringSubmatch(line)
				if found == nil {
					continue
				}
				columns++
				column := found[1]
				for _, word := range []string{"limit", "quota", "omit", "allowance"} {
					if strings.Contains(column, word) {
						t.Errorf("%s: the table %s holds the column %s, and Arca stores no limit", name, m[1], column)
					}
				}
				if column == "versions" || column == "trash" || column == "keep_versions" || column == "keep_trash" {
					t.Errorf("%s: the table %s holds the column %s, and Arca stores no limit", name, m[1], column)
				}
			}
		}
	}
	if columns < 40 {
		t.Fatalf("the test read %d columns out of the migrations, which is not the schema", columns)
	}
}

// The unit half of the folder move of spec 030: what each statement binds,
// what it answers, and that every failure reaches the caller.

// moves is the folder move the cases below rename.
var moves = []Rename{
	{From: "files/notes/a.md", To: "files/archive/a.md"},
	{From: "files/notes/deep/b.md", To: "files/archive/deep/b.md"},
}

// TestAFolderMoveBindsEachSourceBesideItsDestination: every statement of a
// folder move joins on the moves as two arrays in one order, so the n-th
// source is renamed to the n-th destination and no other.
func TestAFolderMoveBindsEachSourceBesideItsDestination(t *testing.T) {
	from, to := ends(moves)
	if !slices.Equal(from, []string{"files/notes/a.md", "files/notes/deep/b.md"}) ||
		!slices.Equal(to, []string{"files/archive/a.md", "files/archive/deep/b.md"}) {
		t.Fatalf("the moves split into %v and %v", from, to)
	}
	for name, run := range map[string]func(Querier) (int64, error){
		"files":    func(q Querier) (int64, error) { return NewFiles().MoveMany(t.Context(), q, "space", moves) },
		"versions": func(q Querier) (int64, error) { return NewVersions().MoveMany(t.Context(), q, "space", moves) },
		"stars":    func(q Querier) (int64, error) { return NewStars().MoveMany(t.Context(), q, "space", moves) },
	} {
		q := &steps{tags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 2"), pgconn.NewCommandTag("UPDATE 2")}}
		if _, err := run(q); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for i, statement := range q.statements {
			if !strings.Contains(statement, renames) || !strings.Contains(statement, "m.src") || !strings.Contains(statement, "m.dst") {
				t.Errorf("%s statement %d does not join on the moves:\n%s", name, i, statement)
			}
			args := q.args[i]
			if args[0] != "space" || !slices.Equal(args[1].([]string), from) || !slices.Equal(args[2].([]string), to) {
				t.Errorf("%s statement %d bound %v", name, i, args)
			}
		}
	}
}

// TestAFolderMoveRenamesLiveRowsAndSaysWhenADestinationIsTaken: the rename
// touches live rows at the path that was read, answers how many it took, and
// reads a unique violation as the conflict it is.
func TestAFolderMoveRenamesLiveRowsAndSaysWhenADestinationIsTaken(t *testing.T) {
	renamed := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 2")}
	n, err := NewFiles().MoveMany(t.Context(), renamed, "space", moves)
	if err != nil || n != 2 {
		t.Fatalf("MoveMany = %d, %v", n, err)
	}
	statement := renamed.statements[0]
	for _, want := range []string{"f.path = m.src", "deleted_at IS NULL", "updated_at = now()"} {
		if !strings.Contains(statement, want) {
			t.Errorf("the rename does not hold %q:\n%s", want, statement)
		}
	}
	if strings.Contains(statement, "object_id") {
		t.Errorf("the rename touches the object a row names:\n%s", statement)
	}
	for name, run := range map[string]func(Querier) (int64, error){
		"files":    func(q Querier) (int64, error) { return NewFiles().MoveMany(t.Context(), q, "space", moves) },
		"versions": func(q Querier) (int64, error) { return NewVersions().MoveMany(t.Context(), q, "space", moves) },
	} {
		if _, err := run(&fakeQuerier{execErr: conflictError}); !errors.Is(err, ErrConflict) {
			t.Errorf("%s onto a taken destination = %v", name, err)
		}
		if _, err := run(&fakeQuerier{execErr: errFault}); err == nil || errors.Is(err, ErrConflict) {
			t.Errorf("%s against a failed connection = %v", name, err)
		}
	}
	history := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 5")}
	if n, err := NewVersions().MoveMany(t.Context(), history, "space", moves); err != nil || n != 5 {
		t.Errorf("the history moved %d, %v", n, err)
	}
}

// TestAFolderMoveCarriesTheStarsAndLeavesOnePerSubject: the colliding star
// goes first, then the rename, and a failure of either reaches the caller.
func TestAFolderMoveCarriesTheStarsAndLeavesOnePerSubject(t *testing.T) {
	moved := &steps{tags: []pgconn.CommandTag{
		pgconn.NewCommandTag("DELETE 1"), pgconn.NewCommandTag("UPDATE 3"),
	}}
	n, err := NewStars().MoveMany(t.Context(), moved, "space", moves)
	if err != nil || n != 3 {
		t.Fatalf("MoveMany = %d, %v", n, err)
	}
	if len(moved.statements) != 2 || !strings.HasPrefix(strings.TrimSpace(moved.statements[0]), "DELETE") ||
		!strings.Contains(moved.statements[0], "s.subject = d.subject") {
		t.Fatalf("the colliding bookmark is not dropped first:\n%v", moved.statements)
	}
	firstFailed := &steps{errs: []error{errFault}}
	if _, err := NewStars().MoveMany(t.Context(), firstFailed, "space", moves); err == nil {
		t.Error("a failed drop reported success")
	}
	secondFailed := &steps{tags: []pgconn.CommandTag{pgconn.NewCommandTag("DELETE 0")}, errs: []error{nil, errFault}}
	if _, err := NewStars().MoveMany(t.Context(), secondFailed, "space", moves); err == nil {
		t.Error("a failed rename reported success")
	}
}

// TestTakenNamesTheFirstPathARowHolds: the check reads live and trashed rows
// alike, answers the first held path in path order, tells no row from a
// fault, and surfaces the fault.
func TestTakenNamesTheFirstPathARowHolds(t *testing.T) {
	paths := []string{"files/archive/a.md", "files/archive/deep/b.md"}
	held := &fakeQuerier{row: values("files/archive/a.md")}
	path, taken, err := NewFiles().Taken(t.Context(), held, "space", paths)
	if err != nil || !taken || path != "files/archive/a.md" {
		t.Fatalf("Taken = %q, %t, %v", path, taken, err)
	}
	statement := held.statements[0]
	if strings.Contains(statement, "deleted_at") || !strings.Contains(statement, "ORDER BY path") {
		t.Errorf("the check does not read every row in path order:\n%s", statement)
	}
	if !slices.Equal(held.args[0][1].([]string), paths) {
		t.Errorf("the check bound %v", held.args[0])
	}
	free := &fakeQuerier{row: failing(pgx.ErrNoRows)}
	if path, taken, err := NewFiles().Taken(t.Context(), free, "space", paths); err != nil || taken || path != "" {
		t.Errorf("no row holding a destination = %q, %t, %v", path, taken, err)
	}
	if _, taken, err := NewFiles().Taken(t.Context(), &fakeQuerier{row: failing(errFault)}, "space", paths); err == nil || taken {
		t.Errorf("a failed check = %t, %v", taken, err)
	}
}

// TestMovingTheGrantsUnderAFolderLeavesWhatStaysBehind: the statement takes
// a grant on the folder or under it to the same prefix below the
// destination, and leaves one at, under or above an omitted subtree at
// either end and one on the path of an object still there.
func TestMovingTheGrantsUnderAFolderLeavesWhatStaysBehind(t *testing.T) {
	q := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 2")}
	n, err := NewShares().MoveUnder(t.Context(), q, "space", "files/team", "files/archive/team", []string{"files/team/payroll"})
	if err != nil || n != 2 {
		t.Fatalf("MoveUnder = %d, %v", n, err)
	}
	statement := q.statements[0]
	for _, want := range []string{
		"path_prefix = $2 OR path_prefix LIKE $4",
		"$3::text || substring(path_prefix FROM char_length($2::text) + 1)",
		"starts_with(omitted.prefix, path_prefix || '/')",
		"starts_with(omitted.prefix, ($3::text || substring(path_prefix FROM char_length($2::text) + 1)) || '/')",
		"f.path = shares.path_prefix",
	} {
		if !strings.Contains(statement, want) {
			t.Errorf("the statement does not hold %q:\n%s", want, statement)
		}
	}
	args := q.args[0]
	if args[1] != "files/team" || args[2] != "files/archive/team" || args[3] != `files/team/%` ||
		!slices.Equal(args[4].([]string), []string{"files/team/payroll"}) {
		t.Errorf("the statement bound %v", args)
	}
	none := &fakeQuerier{}
	if _, err := NewShares().MoveUnder(t.Context(), none, "space", "files/team", "files/archive/team", nil); err != nil {
		t.Fatal(err)
	}
	if got, ok := none.args[0][4].([]string); !ok || got == nil || len(got) != 0 {
		t.Errorf("an answer with no omission bound %#v", none.args[0][4])
	}
	if _, err := NewShares().MoveUnder(t.Context(), &fakeQuerier{execErr: errFault}, "space", "files/team", "files/b", nil); err == nil {
		t.Error("a failed move reported success")
	}
}
