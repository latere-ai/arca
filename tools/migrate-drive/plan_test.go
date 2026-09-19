// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/arca/object"
)

const (
	sha       = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	etag      = "0123456789abcdef0123456789abcdef-3"
	notesKey  = "drive/u-9f1/files/notes.md"
	orgOwner  = "https://issuer.example|org-acme"
	selfOwner = "https://issuer.example|9f1"
)

// source is the fixture every case of this file reads: one row of every shape
// spec 019 names, including the ones that do not arrive.
func source() *fake {
	return newFake(
		answer{"FROM principal_directory", [][]any{
			{"9f1", "person@example.com", at},
			{"9f2", "", at},
		}},
		answer{"FROM files ORDER BY id", [][]any{
			{"f1", "principal", "9f1", "files/notes.md", "9f1", "text/markdown",
				int64(10), notesKey, sha, false, nil, at, at},
			{"f2", "principal", "9f1", "memory/agent.md", "9f1", "text/markdown",
				int64(20), "drive/u-9f1/memory/agent.md", sha, false, nil, at, at},
			{"f3", "org", mappedOrg, "repos/site/README.md", "9f2", "text/markdown",
				int64(30), "drive/o-" + mappedOrg + "/repos/site/README.md", etag, true, nil, at, at},
			{"f4", "principal", "9f1", "files/gone.md", "9f1", "text/plain",
				int64(40), "drive/u-9f1/files/gone.md", sha, false, ptr(at), at, at},
		}},
		answer{"FROM file_versions ORDER BY id", [][]any{
			// The same key the live row carries: Drive wrote a non-versioned
			// overwrite in place, so one object backs both rows.
			{"v1", "principal", "9f1", "files/notes.md", int32(1), "text/markdown",
				int64(5), sha, notesKey, "9f1", at},
		}},
		answer{"FROM stars ORDER BY principal_id", [][]any{
			{"9f2", "org", mappedOrg, "repos/site/README.md", at},
		}},
		answer{"FROM upload_sessions ORDER BY id", [][]any{
			{"s1", "principal", "9f1", "files/big.bin", int64(500), "application/octet-stream",
				"drive/u-9f1/files/big.bin@ab12", "upload-1", "9f1", at},
		}},
		answer{"FROM shares ORDER BY id", [][]any{
			{"h1", "principal", "9f1", "files/", "principal", ptr("9f2"), "write", nil, "active", "9f1", nil, at},
			{"h2", "org", mappedOrg, "repos/site/", "org", ptr(mappedOrg), "read", nil, "active", "9f2", ptr(at), at},
			{"h3", "principal", "9f1", "files/pub/", "link", nil, "read", ptr("tok-1"), "active", "9f1", nil, at},
			{"h4", "principal", "9f1", "files/pub/", "public", nil, "read", nil, "revoked", "9f1", nil, at},
			{"h5", "principal", "9f1", "files/", "principal", ptr("9f2"), "read", ptr("invite-1"), "active", "9f1", nil, at},
			{"h6", "principal", "9f1", "files/", "role", nil, "read", nil, "active", "9f1", nil, at},
			{"h7", "principal", "9f1", "files/", "email", nil, "read", ptr("invite-2"), "pending", "9f1", nil, at},
			{"h8", "principal", "9f1", "files/", "principal", ptr("9f2"), "read", nil, "pending", "9f1", nil, at},
			{"h9", "principal", "9f1", "files/", "link", nil, "write", ptr("tok-2"), "active", "9f1", nil, at},
		}},
		answer{"FROM workspaces ORDER BY id", [][]any{
			{"w1", "principal", "9f1", "workspace", "build", "9f1", ptr("sandbox-1"), ptr(at), ptr(at), at, at, nil},
			{"w2", "org", mappedOrg, "repo", "site", "9f2", nil, nil, nil, at, at, ptr(at)},
		}},
		answer{"FROM workspace_attachments ORDER BY id", [][]any{
			{"a1", "w1", "sandbox-1", "9f1", "rw", "active", []byte(`[{"path":"out.txt","checksum":"x","size":1}]`), at, at, nil},
		}},
		answer{"FROM events ORDER BY id", [][]any{
			{int64(1), "principal", "9f1", ptr("files/notes.md"), "put", ptr("9f1"), []byte(`{"n":1}`), at},
			{int64(2), "org", mappedOrg, ptr("repos/site"), "sync", ptr("9f2"), nil, at},
			{int64(3), "principal", "9f1", nil, "quota_exceeded", nil, nil, at},
		}},
	)
}

func TestTheOrderIsADependencyOrder(t *testing.T) {
	names := TableNames()
	want := []string{
		"subjects", "files", "file_versions", "stars", "upload_sessions",
		"shares", "workspaces", "workspace_attachments", "space_usage", "events",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("the plan is %v, want %v", names, want)
	}
	// The order is the plan's contract, and these are what it is for: an
	// attachment references a workspace, and the ledger is recomputed from the
	// two tables that hold the bytes.
	step := func(name string) int { return slices.Index(names, name) }
	if step("workspaces") > step("workspace_attachments") {
		t.Error("an attachment is copied before the workspace it references")
	}
	if step("files") > step("space_usage") || step("file_versions") > step("space_usage") {
		t.Error("the ledger is recomputed before the rows it sums are copied")
	}
	if len(Tables()) != len(want) {
		t.Errorf("the plan holds %d tables, want %d", len(Tables()), len(want))
	}
}

func TestTheTablesSpec019LeavesBehindAreNeverRead(t *testing.T) {
	f := source()
	r := testRun(f, false)
	if err := r.Copy(t.Context()); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	for _, table := range []string{"quotas", "webhooks", "agent_visibility", "admin_audit"} {
		for _, s := range f.sent {
			if strings.Contains(s.sql, table) {
				t.Errorf("a statement names %s, which spec 019 leaves behind: %s", table, s.sql)
			}
		}
	}
}

func TestEachTableIsOneTransaction(t *testing.T) {
	f := source()
	r := testRun(f, false)
	if err := r.Copy(t.Context()); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if f.txs != len(Tables()) {
		t.Errorf("the copy opened %d transactions over %d tables", f.txs, len(Tables()))
	}
}

func TestTheOwnerColumnsBecomeSubjects(t *testing.T) {
	f := source()
	r := testRun(f, false)
	if err := r.Copy(t.Context()); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	files := f.wrote("INSERT INTO files")
	if len(files) != 4 {
		t.Fatalf("the copy wrote %d files, want 4", len(files))
	}
	if got := files[0].args[1]; got != selfOwner {
		t.Errorf("a personal owner is %q, want %q", got, selfOwner)
	}
	if got := files[2].args[1]; got != orgOwner {
		t.Errorf("an organization owner is %q, want %q", got, orgOwner)
	}
	if got := files[0].args[4]; got != selfOwner {
		t.Errorf("created_by is %q, want %q", got, selfOwner)
	}
	if got := f.wrote("INSERT INTO subjects")[0].args[0]; got != selfOwner {
		t.Errorf("a subject row is %q, want %q", got, selfOwner)
	}
	if got := f.one(t, "INSERT INTO stars").args[0]; got != "https://issuer.example|9f2" {
		t.Errorf("a star belongs to %q", got)
	}
}

func TestThePathsTakeTheirPlanesRule(t *testing.T) {
	f := source()
	r := testRun(f, false)
	if err := r.Copy(t.Context()); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	files := f.wrote("INSERT INTO files")
	for i, want := range []string{
		"files/notes.md", "files/memory/agent.md", "workspaces/site/README.md", "files/gone.md",
	} {
		if got := files[i].args[2]; got != want {
			t.Errorf("file %d is at %q, want %q", i, got, want)
		}
	}
	if got := f.one(t, "INSERT INTO stars").args[2]; got != "workspaces/site/README.md" {
		t.Errorf("a star on a repository path is at %q", got)
	}
	if got := f.wrote("INSERT INTO shares")[1].args[2]; got != "workspaces/site/" {
		t.Errorf("a grant over a repository subtree is at %q", got)
	}
	if got := deref(f.wrote("INSERT INTO events")[1].args[2].(*string)); got != "workspaces/site" {
		t.Errorf("an event on a repository path is at %q", got)
	}
}

func TestOneObjectIDPerDriveKey(t *testing.T) {
	f := source()
	r := testRun(f, false)
	if err := r.Copy(t.Context()); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	live := f.wrote("INSERT INTO files")[0].args[3].(string)
	version := f.one(t, "INSERT INTO file_versions").args[4].(string)
	if live != version {
		t.Errorf("one Drive key became two object ids, %q and %q", live, version)
	}
	if _, err := object.ParseID(live); err != nil {
		t.Errorf("the object id is not one: %v", err)
	}
	// Two keys are two objects, which is what keeps a version reachable after
	// the file it superseded is purged.
	if other := f.wrote("INSERT INTO files")[1].args[3].(string); other == live {
		t.Error("two Drive keys became one object id")
	}
}

func TestADriveKeyHoldsNoObjectIDToKeep(t *testing.T) {
	// The blocking finding of spec 019, written as a test so it is not prose.
	// Drive's key is drive/<owner>/<path>; Arca's is <prefix><shard>/<id>.
	// ParseKey reads the second and refuses the first, so the bucket prefix
	// alone does not carry the bytes over.
	if _, err := object.ParseKey("drive/", notesKey); err == nil {
		t.Fatal("a Drive key parsed as an Arca key; the finding of spec 019 would not hold")
	}
	id := object.NewID()
	if _, err := object.ParseKey("drive/", id.Key("drive/")); err != nil {
		t.Fatalf("an Arca key under the same prefix does not parse: %v", err)
	}
}

func TestTheChecksumKindIsReadFromTheShapeDriveStored(t *testing.T) {
	f := source()
	r := testRun(f, false)
	if err := r.Copy(t.Context()); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	files := f.wrote("INSERT INTO files")
	if got := files[0].args[8]; got != "sha256" {
		t.Errorf("a sha256 is stored as %q", got)
	}
	if got := files[2].args[8]; got != "etag" {
		t.Errorf("a composite ETag is stored as %q", got)
	}
}

func TestTheGrantsThatArriveAndTheOnesCounted(t *testing.T) {
	f := source()
	r := testRun(f, false)
	if err := r.Copy(t.Context()); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	shares := f.wrote("INSERT INTO shares")
	if len(shares) != 5 {
		t.Fatalf("the copy wrote %d grants, want 5", len(shares))
	}
	dropped := r.Report.Table("shares").Dropped
	for reason, want := range map[string]int{
		DropRole: 1, DropEmail: 1, DropShareRequests: 1, DropWritableToken: 1,
	} {
		if dropped[reason] != want {
			t.Errorf("%s dropped %d rows, want %d", reason, dropped[reason], want)
		}
	}
	if total := r.Report.Table("shares").DroppedTotal(); total != 4 {
		t.Errorf("the copy dropped %d grants, want 4", total)
	}
	// A subject grant names a grantee and carries no token; a token grant is
	// the other way round. Both constraints are migration 0003's.
	if shares[0].args[3] != "subject" || shares[0].args[4] == nil || shares[0].args[6] != (*string)(nil) {
		t.Errorf("the subject grant is %v", shares[0].args)
	}
	if shares[2].args[3] != "link" || shares[2].args[4] != (*string)(nil) || shares[2].args[6] == nil {
		t.Errorf("the link grant is %v", shares[2].args)
	}
	noted := r.Report.Table("shares").Noted
	if noted["tokens minted for a grant that carried none"] != 1 {
		t.Errorf("minted tokens: %v", noted)
	}
	if noted["invite tokens cleared on a subject grant"] != 1 {
		t.Errorf("cleared tokens: %v", noted)
	}
}

func TestTheWorkspaceKindIsDroppedAndCounted(t *testing.T) {
	f := source()
	r := testRun(f, false)
	if err := r.Copy(t.Context()); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	workspaces := f.wrote("INSERT INTO workspaces")
	if len(workspaces) != 2 {
		t.Fatalf("the copy wrote %d workspaces, want 2", len(workspaces))
	}
	for _, w := range workspaces {
		if strings.Contains(w.sql, "kind") || strings.Contains(w.sql, "agent_access") {
			t.Errorf("a workspace statement names a column spec 019 drops: %s", w.sql)
		}
	}
	// The slug keeps its name; what moves plane are the paths under it.
	if got := workspaces[1].args[2]; got != "site" {
		t.Errorf("the repository's slug is %q, want site", got)
	}
	if got := r.Report.Table("workspaces").Noted["repositories that became workspaces"]; got != 1 {
		t.Errorf("repositories counted: %d, want 1", got)
	}
	if got := f.one(t, "INSERT INTO workspace_attachments").args[3]; got != selfOwner {
		t.Errorf("the attachment's subject is %q", got)
	}
}

func TestTheLedgerIsRecomputedFromTheCopiedRows(t *testing.T) {
	f := source()
	r := testRun(f, false)
	if err := r.Copy(t.Context()); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	usage := map[string]int64{}
	for _, s := range f.wrote("INSERT INTO space_usage") {
		usage[s.args[0].(string)] = s.args[1].(int64)
	}
	// Trashed rows and versions count, which is what events.Recompute sums,
	// so the reaper's reconciliation pass finds nothing to correct.
	if got, want := usage[selfOwner], int64(10+20+40+5); got != want {
		t.Errorf("the personal space holds %d bytes, want %d", got, want)
	}
	if got, want := usage[orgOwner], int64(30); got != want {
		t.Errorf("the organization holds %d bytes, want %d", got, want)
	}
	if len(usage) != 2 {
		t.Errorf("the ledger holds %d spaces, want 2", len(usage))
	}
}

func TestTheLogIsCopiedWithItsIDsAndItsSequenceIsSet(t *testing.T) {
	f := source()
	r := testRun(f, false)
	if err := r.Copy(t.Context()); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	events := f.wrote("INSERT INTO events")
	if len(events) != 3 {
		t.Fatalf("the copy wrote %d events, want 3", len(events))
	}
	if got := events[0].args[0]; got != int64(1) {
		t.Errorf("the first event's id is %v, want 1; a cursor a consumer holds has to keep meaning", got)
	}
	// An action Drive had and Arca does not is copied and counted, never
	// rewritten: which action it becomes is the maintainer's decision.
	if got := events[2].args[3]; got != "quota_exceeded" {
		t.Errorf("the action is %q, want quota_exceeded", got)
	}
	if got := r.Report.Table("events").Noted["actions outside Arca's vocabulary"]; got != 1 {
		t.Errorf("actions outside the vocabulary: %d, want 1", got)
	}
	if len(f.wrote("setval")) != 1 {
		t.Error("the sequence is not set past the copied ids")
	}
}

func TestASessionsPartsStayWhereDriveLeftThem(t *testing.T) {
	f := source()
	r := testRun(f, false)
	if err := r.Copy(t.Context()); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	session := f.one(t, "INSERT INTO upload_sessions")
	if got := session.args[4]; got != "upload-1" {
		t.Errorf("the store's upload id is %v", got)
	}
	if got := session.args[9]; got != at.Add(SessionLifetime) {
		t.Errorf("the session expires at %v, want %v", got, at.Add(SessionLifetime))
	}
	if got := r.Report.Table("upload_sessions").Noted[NoteNoObjectYet]; got != 1 {
		t.Errorf("open sessions counted: %d, want 1", got)
	}
}

func TestADryRunReadsAndRewritesAndWritesNothing(t *testing.T) {
	live, dry := source(), source()
	liveRun, dryRun := testRun(live, false), testRun(dry, true)
	if err := liveRun.Copy(t.Context()); err != nil {
		t.Fatalf("the live copy: %v", err)
	}
	if err := dryRun.Copy(t.Context()); err != nil {
		t.Fatalf("the dry run: %v", err)
	}
	if len(dry.sent) != 0 {
		t.Errorf("a dry run wrote %d statements", len(dry.sent))
	}
	if dry.txs != 0 {
		t.Errorf("a dry run opened %d transactions", dry.txs)
	}
	for _, name := range TableNames() {
		want, got := liveRun.Report.Table(name), dryRun.Report.Table(name)
		if want.Copied != got.Copied || want.DroppedTotal() != got.DroppedTotal() {
			t.Errorf("%s: the dry run reports %d copied and %d dropped, the copy %d and %d",
				name, got.Copied, got.DroppedTotal(), want.Copied, want.DroppedTotal())
		}
	}
}

func TestACopyStopsAtTheTableThatFails(t *testing.T) {
	boom := errors.New("the database went away")
	for _, text := range []string{
		"FROM principal_directory", "FROM files ORDER BY id", "FROM file_versions ORDER BY id",
		"FROM stars ORDER BY", "FROM upload_sessions ORDER BY id", "FROM shares ORDER BY id",
		"FROM workspaces ORDER BY id", "FROM workspace_attachments ORDER BY id",
		"FROM events ORDER BY id",
	} {
		f := source()
		f.queryErr[text] = boom
		err := testRun(f, false).Copy(t.Context())
		if !errors.Is(err, boom) {
			t.Errorf("a failed read of %q answered %v", text, err)
		}
	}
	for _, text := range []string{
		"INSERT INTO subjects", "INSERT INTO files", "INSERT INTO file_versions",
		"INSERT INTO stars", "INSERT INTO upload_sessions", "INSERT INTO shares",
		"INSERT INTO workspaces", "INSERT INTO workspace_attachments",
		"INSERT INTO space_usage", "INSERT INTO events", "setval",
	} {
		f := source()
		f.execErr[text] = boom
		err := testRun(f, false).Copy(t.Context())
		if !errors.Is(err, boom) {
			t.Errorf("a failed write of %q answered %v", text, err)
		}
	}
}

func TestARowTheRewriteRefusesStopsItsTable(t *testing.T) {
	unmapped := "22222222-2222-4222-8222-222222222222"
	for _, c := range []struct{ name, match string }{
		{"files", "FROM files ORDER BY id"},
		{"file_versions", "FROM file_versions ORDER BY id"},
		{"stars", "FROM stars ORDER BY"},
		{"upload_sessions", "FROM upload_sessions ORDER BY id"},
		{"shares", "FROM shares ORDER BY id"},
		{"workspaces", "FROM workspaces ORDER BY id"},
		{"events", "FROM events ORDER BY id"},
	} {
		f := source()
		replace(f, c.match, unmappedRow(c.name, unmapped))
		err := testRun(f, false).Copy(t.Context())
		if err == nil || !strings.Contains(err.Error(), unmapped) {
			t.Errorf("%s over an unmapped organization answered %v", c.name, err)
		}
	}
}

func TestAPathInAPlaneWithNoRuleStopsItsTable(t *testing.T) {
	for _, c := range []struct{ name, match string }{
		{"files", "FROM files ORDER BY id"},
		{"file_versions", "FROM file_versions ORDER BY id"},
		{"stars", "FROM stars ORDER BY"},
		{"upload_sessions", "FROM upload_sessions ORDER BY id"},
		{"shares", "FROM shares ORDER BY id"},
		{"events", "FROM events ORDER BY id"},
	} {
		f := source()
		replace(f, c.match, agentsRow(c.name))
		err := testRun(f, false).Copy(t.Context())
		if err == nil || !strings.Contains(err.Error(), "agents") {
			t.Errorf("%s over an agents path answered %v", c.name, err)
		}
	}
}

func TestAGranteeKindInNeitherSchemaStopsTheShares(t *testing.T) {
	f := source()
	replace(f, "FROM shares ORDER BY id", [][]any{
		{"h1", "principal", "9f1", "files/", "unicorn", nil, "read", nil, "active", "9f1", nil, at},
	})
	if err := testRun(f, false).Copy(t.Context()); err == nil {
		t.Fatal("a grantee kind neither schema names is an error")
	}
}

func TestNullableColumnsReadAndWriteAsNull(t *testing.T) {
	if deref(nil) != "" {
		t.Error("a NULL text column is the empty string")
	}
	if deref(ptr("x")) != "x" {
		t.Error("a text column is its value")
	}
	if nullable("") != nil {
		t.Error("the empty string is written as NULL")
	}
	if got := nullable("x"); got == nil || *got != "x" {
		t.Errorf("nullable(\"x\") = %v", got)
	}
}

// replace swaps the rows one query answers, so a case reads one table over a
// row the fixture does not hold.
func replace(f *fake, match string, rows [][]any) {
	for i := range f.answers {
		if strings.Contains(f.answers[i].match, match) {
			f.answers[i].rows = rows
			return
		}
	}
	panic("no answer matches " + match)
}

// unmappedRow is one row of the named table owned by an organization no
// mapping names.
func unmappedRow(table, org string) [][]any {
	switch table {
	case "files":
		return [][]any{{"f1", "org", org, "files/a.md", "9f1", "text/plain", int64(1), "drive/k", sha, false, nil, at, at}}
	case "file_versions":
		return [][]any{{"v1", "org", org, "files/a.md", int32(1), "text/plain", int64(1), sha, "drive/k", "9f1", at}}
	case "stars":
		return [][]any{{"9f1", "org", org, "files/a.md", at}}
	case "upload_sessions":
		return [][]any{{"s1", "org", org, "files/a.md", int64(1), "text/plain", "drive/k", "u", "9f1", at}}
	case "shares":
		return [][]any{{"h1", "org", org, "files/", "link", nil, "read", ptr("t"), "active", "9f1", nil, at}}
	case "workspaces":
		return [][]any{{"w1", "org", org, "workspace", "s", "9f1", nil, nil, nil, at, at, nil}}
	case "events":
		return [][]any{{int64(1), "org", org, nil, "put", nil, nil, at}}
	}
	panic("no row for " + table)
}

// agentsRow is one row of the named table at a path in the plane spec 019
// gives no rule for.
func agentsRow(table string) [][]any {
	const path = "agents/a.md"
	switch table {
	case "files":
		return [][]any{{"f1", "principal", "9f1", path, "9f1", "text/plain", int64(1), "drive/k", sha, false, nil, at, at}}
	case "file_versions":
		return [][]any{{"v1", "principal", "9f1", path, int32(1), "text/plain", int64(1), sha, "drive/k", "9f1", at}}
	case "stars":
		return [][]any{{"9f1", "principal", "9f1", path, at}}
	case "upload_sessions":
		return [][]any{{"s1", "principal", "9f1", path, int64(1), "text/plain", "drive/k", "u", "9f1", at}}
	case "shares":
		return [][]any{{"h1", "principal", "9f1", "agents/", "link", nil, "read", ptr("t"), "active", "9f1", nil, at}}
	case "events":
		return [][]any{{int64(1), "principal", "9f1", ptr(path), "put", nil, nil, at}}
	}
	panic("no row for " + table)
}
