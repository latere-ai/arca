// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"latere.ai/x/arca/object"
)

// The unit tier over the Go half of spec 009's queries: the statement each
// one sends, the arguments it binds, the error it maps, and the row it
// scans. What the SQL means, and that two attaches arriving at once leave
// one lease, is the store tier's against Postgres, which is the only place a
// transaction's semantics can be proved at all.

// aWorkspace is one row as the queries read and write it, with a subject
// holding every character spec 004 says a subject column accepts.
func aWorkspace() Workspace {
	holder := "sbx_01J8R4"
	expires := time.Now().Add(time.Hour)
	synced := time.Now().Add(-time.Hour)
	return Workspace{
		ID:              "2b7e1f4c-4a6d-4c3e-9b1e-1f4c4a6d4c3e",
		Owner:           "https://issuer.example/realms/one|user:7/agent",
		Slug:            "build",
		CreatedBy:       "https://issuer.example/realms/one|user:7/agent",
		WriterHolder:    &holder,
		WriterExpiresAt: &expires,
		LastSync:        &synced,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
}

// anAttachment is one attachment row, pinned to a one entry manifest.
func anAttachment() Attachment {
	return Attachment{
		ID:          "9c1f0e2a-77f3-4f0e-9a0c-0e2a77f34f0e",
		WorkspaceID: aWorkspace().ID,
		Holder:      "sbx_01J8R4",
		Subject:     "https://issuer.example/realms/one|user:7/agent",
		Mode:        ModeWrite,
		Status:      StatusActive,
		Manifest:    []byte(`[{"path":"src/main.go","checksum":"4f9a","size":2814}]`),
		ExpiresAt:   time.Now().Add(time.Hour),
		CreatedAt:   time.Now(),
	}
}

// workspaceScan answers the scan of one workspace row, in the order of
// workspaceColumns.
func workspaceScan(w Workspace) func(dest ...any) error {
	return func(dest ...any) error {
		return assign(dest, []any{
			w.ID, w.Owner, w.Slug, w.CreatedBy, w.WriterHolder, w.WriterExpiresAt,
			w.LastSync, w.CreatedAt, w.UpdatedAt, w.DeletedAt,
		})
	}
}

// attachmentScan answers the scan of one attachment row, in the order of
// attachmentColumns.
func attachmentScan(a Attachment) func(dest ...any) error {
	return func(dest ...any) error {
		return assign(dest, []any{
			a.ID, a.WorkspaceID, a.Holder, a.Subject, a.Mode, a.Status, a.Manifest,
			a.ExpiresAt, a.CreatedAt, a.ReleasedAt,
		})
	}
}

// workspaceRowsOf answers the rows of the workspaces the case named.
func workspaceRowsOf(ws ...Workspace) *fakeRows {
	rows := &fakeRows{}
	for _, w := range ws {
		rows.scans = append(rows.scans, workspaceScan(w))
	}
	return rows
}

// badArgument is the refusal Postgres answers for a string that is not the
// type the column holds, which is what a client's wrong id and a cursor from
// another listing both produce.
func badArgument() error { return &pgconn.PgError{Code: "22P02"} }

// taken is the refusal a unique constraint answers.
func taken() error { return &pgconn.PgError{Code: "23505"} }

func TestTheTwoModesAndThreeStatusesAreTheOnesTheColumnsHold(t *testing.T) {
	for _, m := range []Mode{ModeRead, ModeWrite} {
		if !m.Valid() {
			t.Errorf("%q is a mode the column holds and Valid refuses it", m)
		}
	}
	if Mode("rwx").Valid() {
		t.Error("a mode outside the two passed")
	}
	if StatusActive != "active" || StatusReleased != "released" || StatusReaped != "reaped" {
		t.Error("a status drifted from the value the column constrains")
	}
}

func TestCreatingAWorkspaceWhoseSlugIsTakenIsAConflictAndNotAnOverwrite(t *testing.T) {
	want := aWorkspace()
	q := &fakeQuerier{row: fakeRow{scan: workspaceScan(want)}}
	got, err := NewWorkspaces().Create(t.Context(), q, want)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Slug != want.Slug || got.Owner != want.Owner {
		t.Fatalf("Create = %+v", got)
	}
	if !strings.Contains(q.statements[0], "ON CONFLICT (owner, slug) DO NOTHING") {
		t.Errorf("the insert is not conditional: %s", q.statements[0])
	}
	if len(q.args[0]) != 3 || q.args[0][1] != want.Slug {
		t.Errorf("the insert bound %v", q.args[0])
	}

	// A taken slug returns no row, which is the conflict a caller branches
	// on. The uniqueness is not conditional on deleted_at, so a tombstone's
	// slug reaches this same answer.
	held := &fakeQuerier{row: failing(pgx.ErrNoRows)}
	if _, err := NewWorkspaces().Create(t.Context(), held, want); !errors.Is(err, ErrConflict) {
		t.Fatalf("a taken slug = %v", err)
	}
	broken := &fakeQuerier{row: failing(errors.New("the connection failed"))}
	if _, err := NewWorkspaces().Create(t.Context(), broken, want); errors.Is(err, ErrConflict) {
		t.Fatalf("a connection failure answered as a conflict: %v", err)
	}
	clash := &fakeQuerier{row: failing(taken())}
	if _, err := NewWorkspaces().Create(t.Context(), clash, want); !errors.Is(err, ErrConflict) {
		t.Fatalf("a unique violation = %v", err)
	}
}

func TestReadingAWorkspaceAnswersTheRowOrNoRowAtAll(t *testing.T) {
	want := aWorkspace()
	q := &fakeQuerier{row: fakeRow{scan: workspaceScan(want)}}
	got, err := NewWorkspaces().Get(t.Context(), q, want.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != want.ID || got.WriterHolder == nil || *got.WriterHolder != *want.WriterHolder {
		t.Fatalf("Get = %+v", got)
	}

	locking := &fakeQuerier{row: fakeRow{scan: workspaceScan(want)}}
	if _, err := NewWorkspaces().GetForUpdate(t.Context(), locking, want.ID); err != nil {
		t.Fatalf("GetForUpdate: %v", err)
	}
	if !strings.Contains(locking.statements[0], "FOR UPDATE") {
		t.Errorf("the read takes no row lock: %s", locking.statements[0])
	}

	absent := &fakeQuerier{row: failing(pgx.ErrNoRows)}
	if _, err := NewWorkspaces().Get(t.Context(), absent, want.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("an id that names no row = %v", err)
	}
	// A string that is not an id names nothing, which is the same answer a
	// wrong id gets rather than a fault a caller cannot act on.
	nonsense := &fakeQuerier{row: failing(badArgument())}
	if _, err := NewWorkspaces().Get(t.Context(), nonsense, "not-an-id"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a string that is not an id = %v", err)
	}
	broken := &fakeQuerier{row: failing(errors.New("the connection failed"))}
	if _, err := NewWorkspaces().Get(t.Context(), broken, want.ID); errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a connection failure answered as a missing row: %v", err)
	}
}

func TestListingAWorkspacePageIsKeysetAndRefusesACursorItDidNotMint(t *testing.T) {
	want := aWorkspace()
	q := &fakeQuerier{rows: workspaceRowsOf(want, want)}
	page, err := NewWorkspaces().List(t.Context(), q, want.Owner, "", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("the page holds %d rows", len(page))
	}
	if !strings.Contains(q.statements[0], "deleted_at IS NULL") || !strings.Contains(q.statements[0], "ORDER BY id") {
		t.Errorf("the listing is not the live rows in id order: %s", q.statements[0])
	}
	// The first page binds a null cursor, so it needs no second statement.
	if cursor, ok := q.args[0][1].(*string); !ok || cursor != nil {
		t.Errorf("the first page bound the cursor %v", q.args[0][1])
	}

	next := &fakeQuerier{rows: workspaceRowsOf(want)}
	if _, err := NewWorkspaces().List(t.Context(), next, want.Owner, want.ID, 10); err != nil {
		t.Fatalf("the second page: %v", err)
	}
	if cursor, ok := next.args[0][1].(*string); !ok || cursor == nil || *cursor != want.ID {
		t.Errorf("the second page bound the cursor %v", next.args[0][1])
	}

	deleted := &fakeQuerier{rows: workspaceRowsOf(want)}
	if _, err := NewWorkspaces().ListDeleted(t.Context(), deleted, want.Owner, "", 10); err != nil {
		t.Fatalf("ListDeleted: %v", err)
	}
	if !strings.Contains(deleted.statements[0], "deleted_at IS NOT NULL") {
		t.Errorf("the deleted listing reads the live rows: %s", deleted.statements[0])
	}

	stray := &fakeQuerier{queryErr: badArgument()}
	if _, err := NewWorkspaces().List(t.Context(), stray, want.Owner, "not-a-cursor", 10); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("a cursor from nowhere = %v", err)
	}
	if _, err := NewWorkspaces().List(t.Context(), &fakeQuerier{}, want.Owner, "", 0); err == nil {
		t.Fatal("a page of no rows was accepted")
	}
	broken := &fakeQuerier{queryErr: errors.New("the connection failed")}
	if _, err := NewWorkspaces().List(t.Context(), broken, want.Owner, "", 10); errors.Is(err, ErrBadCursor) {
		t.Fatalf("a connection failure answered as a bad cursor: %v", err)
	}
	torn := &fakeQuerier{rows: &fakeRows{err: errors.New("the stream broke")}}
	if _, err := NewWorkspaces().List(t.Context(), torn, want.Owner, "", 10); err == nil {
		t.Fatal("a broken stream read as an empty page")
	}
	misread := &fakeQuerier{rows: &fakeRows{scans: []func(...any) error{func(...any) error {
		return errors.New("the row does not fit")
	}}}}
	if _, err := NewWorkspaces().List(t.Context(), misread, want.Owner, "", 10); err == nil {
		t.Fatal("a row that does not scan read as a row")
	}
}

func TestARenameAndADeleteAreRefusedWhileTheLeaseIsHeld(t *testing.T) {
	want := aWorkspace()
	for _, c := range []struct {
		name string
		run  func(Querier) (bool, error)
		want string
	}{
		{"rename", func(q Querier) (bool, error) {
			return NewWorkspaces().Rename(t.Context(), q, want.ID, "release", time.Now())
		}, "writer_holder IS NULL OR writer_expires_at <="},
		{"delete", func(q Querier) (bool, error) {
			return NewWorkspaces().SoftDelete(t.Context(), q, want.ID, time.Now())
		}, "writer_holder IS NULL OR writer_expires_at <="},
	} {
		t.Run(c.name, func(t *testing.T) {
			done := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 1")}
			ok, err := c.run(done)
			if err != nil || !ok {
				t.Fatalf("%s = %v, %v", c.name, ok, err)
			}
			// The condition is in the statement, so a lease taken between
			// the read and the write is a zero row count and not a write
			// that ran anyway.
			if !strings.Contains(done.statements[0], c.want) {
				t.Errorf("the write carries no lease condition: %s", done.statements[0])
			}
			if !strings.Contains(done.statements[0], "deleted_at IS NULL") {
				t.Errorf("the write does not require a live workspace: %s", done.statements[0])
			}
			held := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 0")}
			if ok, err := c.run(held); ok || err != nil {
				t.Fatalf("a held lease = %v, %v", ok, err)
			}
			clash := &fakeQuerier{execErr: taken()}
			if _, err := c.run(clash); !errors.Is(err, ErrConflict) {
				t.Fatalf("a unique violation = %v", err)
			}
			nonsense := &fakeQuerier{execErr: badArgument()}
			if ok, err := c.run(nonsense); ok || err != nil {
				t.Fatalf("a string that is not an id = %v, %v", ok, err)
			}
			broken := &fakeQuerier{execErr: errors.New("the connection failed")}
			if _, err := c.run(broken); err == nil {
				t.Fatal("a connection failure read as a refusal")
			}
		})
	}
}

func TestRestoreNeedsNoCollisionGuardAndRefusesALiveWorkspace(t *testing.T) {
	want := aWorkspace()
	done := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 1")}
	ok, err := NewWorkspaces().Restore(t.Context(), done, want.ID)
	if err != nil || !ok {
		t.Fatalf("Restore = %v, %v", ok, err)
	}
	// The tombstone kept its slug reserved, so the un-delete names the slug
	// nowhere and guards against no live row.
	if strings.Contains(done.statements[0], "slug") {
		t.Errorf("the restore guards against a slug it cannot collide with: %s", done.statements[0])
	}
	if !strings.Contains(done.statements[0], "deleted_at IS NOT NULL") {
		t.Errorf("the restore does not require a deleted workspace: %s", done.statements[0])
	}
	live := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 0")}
	if ok, err := NewWorkspaces().Restore(t.Context(), live, want.ID); ok || err != nil {
		t.Fatalf("restoring a live workspace = %v, %v", ok, err)
	}
	broken := &fakeQuerier{execErr: errors.New("the connection failed")}
	if _, err := NewWorkspaces().Restore(t.Context(), broken, want.ID); err == nil {
		t.Fatal("a connection failure read as a refusal")
	}
}

func TestTheLeaseIsTakenByACompareAndSwapThatALapsedHolderDoesNotBlock(t *testing.T) {
	want := aWorkspace()
	until := time.Now().Add(time.Hour)
	taken := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 1")}
	ok, err := NewWorkspaces().TakeLease(t.Context(), taken, want.ID, "sbx_a", time.Now(), until)
	if err != nil || !ok {
		t.Fatalf("TakeLease = %v, %v", ok, err)
	}
	statement := taken.statements[0]
	if !strings.Contains(statement, "writer_holder IS NULL") {
		t.Errorf("the attach is not a compare and swap: %s", statement)
	}
	if !strings.Contains(statement, "writer_expires_at <= $4") {
		t.Errorf("a lapsed lease blocks the next writer: %s", statement)
	}
	if !strings.Contains(statement, "deleted_at IS NULL") {
		t.Errorf("a deleted workspace can be attached: %s", statement)
	}
	// The loser of a race matches no row, which is the conflict spec 009's
	// invariant rests on.
	lost := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 0")}
	if ok, err := NewWorkspaces().TakeLease(t.Context(), lost, want.ID, "sbx_b", time.Now(), until); ok || err != nil {
		t.Fatalf("the second attach = %v, %v", ok, err)
	}
	broken := &fakeQuerier{execErr: errors.New("the connection failed")}
	if _, err := NewWorkspaces().TakeLease(t.Context(), broken, want.ID, "sbx_b", time.Now(), until); err == nil {
		t.Fatal("a connection failure read as a lost race")
	}
}

func TestARenewStampsTheExpiryAndAReleaseFreesOnlyItsOwnHolder(t *testing.T) {
	want := aWorkspace()
	until := time.Now().Add(2 * time.Hour)
	renewed := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 1")}
	if ok, err := NewWorkspaces().RenewLease(t.Context(), renewed, want.ID, "sbx_a", until); err != nil || !ok {
		t.Fatalf("RenewLease = %v, %v", ok, err)
	}
	// A renew stamps a new expiry and nothing else.
	if strings.Contains(renewed.statements[0], "writer_holder =") &&
		!strings.Contains(renewed.statements[0], "WHERE id = $1 AND writer_holder = $2") {
		t.Errorf("the renew writes the holder: %s", renewed.statements[0])
	}
	released := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 1")}
	if ok, err := NewWorkspaces().ReleaseLease(t.Context(), released, want.ID, "sbx_a"); err != nil || !ok {
		t.Fatalf("ReleaseLease = %v, %v", ok, err)
	}
	// The release is scoped to its holder, so one that arrives after the
	// lease moved on frees the new writer's lease from nobody.
	if !strings.Contains(released.statements[0], "writer_holder = $2") {
		t.Errorf("the release is not scoped to its holder: %s", released.statements[0])
	}
	moved := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 0")}
	if ok, err := NewWorkspaces().ReleaseLease(t.Context(), moved, want.ID, "sbx_a"); ok || err != nil {
		t.Fatalf("a release after the lease moved on = %v, %v", ok, err)
	}
	brokenRenew := &fakeQuerier{execErr: errors.New("the connection failed")}
	if _, err := NewWorkspaces().RenewLease(t.Context(), brokenRenew, want.ID, "sbx_a", until); err == nil {
		t.Fatal("a connection failure read as a lapsed lease")
	}
	brokenRelease := &fakeQuerier{execErr: errors.New("the connection failed")}
	if _, err := NewWorkspaces().ReleaseLease(t.Context(), brokenRelease, want.ID, "sbx_a"); err == nil {
		t.Fatal("a connection failure read as a lease that moved on")
	}
}

func TestACompletedSyncStampsTheBoundaryItTook(t *testing.T) {
	want := aWorkspace()
	at := time.Now()
	q := &fakeQuerier{row: values(at)}
	got, err := NewWorkspaces().StampSync(t.Context(), q, want.ID)
	if err != nil {
		t.Fatalf("StampSync: %v", err)
	}
	if !got.Equal(at) {
		t.Fatalf("StampSync = %v, want %v", got, at)
	}
	absent := &fakeQuerier{row: failing(pgx.ErrNoRows)}
	if _, err := NewWorkspaces().StampSync(t.Context(), absent, want.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stamping a workspace that is gone = %v", err)
	}
}

func TestTheReaperReadsTheLeasesAndTheAttachmentsWhoseTimeHasPassed(t *testing.T) {
	want := aWorkspace()
	leases := &fakeQuerier{rows: workspaceRowsOf(want)}
	got, err := NewWorkspaces().ExpiredLeases(t.Context(), leases, time.Now(), 100)
	if err != nil || len(got) != 1 {
		t.Fatalf("ExpiredLeases = %v, %v", got, err)
	}
	// A lease can outlive the attachment that took it, so the sweep reads
	// the workspace row and not only the attachments.
	if !strings.Contains(leases.statements[0], "writer_expires_at < $1") {
		t.Errorf("the lease sweep reads no deadline: %s", leases.statements[0])
	}
	if _, err := NewWorkspaces().ExpiredLeases(t.Context(), &fakeQuerier{}, time.Now(), 0); err == nil {
		t.Fatal("a page of no rows was accepted")
	}
	broken := &fakeQuerier{queryErr: errors.New("the connection failed")}
	if _, err := NewWorkspaces().ExpiredLeases(t.Context(), broken, time.Now(), 100); err == nil {
		t.Fatal("a connection failure read as an empty sweep")
	}
	torn := &fakeQuerier{rows: &fakeRows{err: errors.New("the stream broke")}}
	if _, err := NewWorkspaces().ExpiredLeases(t.Context(), torn, time.Now(), 100); err == nil {
		t.Fatal("a broken stream read as an empty sweep")
	}
	misread := &fakeQuerier{rows: &fakeRows{scans: []func(...any) error{func(...any) error {
		return errors.New("the row does not fit")
	}}}}
	if _, err := NewWorkspaces().ExpiredLeases(t.Context(), misread, time.Now(), 100); err == nil {
		t.Fatal("a row that does not scan read as a row")
	}

	a := anAttachment()
	attached := &fakeQuerier{rows: &fakeRows{scans: []func(...any) error{attachmentScan(a)}}}
	stale, err := NewAttachments().Expired(t.Context(), attached, time.Now(), 100)
	if err != nil || len(stale) != 1 || stale[0].ID != a.ID {
		t.Fatalf("Expired = %v, %v", stale, err)
	}
	if !strings.Contains(attached.statements[0], "status = 'active' AND expires_at < $1") {
		t.Errorf("the attachment sweep reads the wrong rows: %s", attached.statements[0])
	}
	if _, err := NewAttachments().Expired(t.Context(), &fakeQuerier{}, time.Now(), 0); err == nil {
		t.Fatal("a page of no rows was accepted")
	}
	brokenAttached := &fakeQuerier{queryErr: errors.New("the connection failed")}
	if _, err := NewAttachments().Expired(t.Context(), brokenAttached, time.Now(), 100); err == nil {
		t.Fatal("a connection failure read as an empty sweep")
	}
	tornAttached := &fakeQuerier{rows: &fakeRows{err: errors.New("the stream broke")}}
	if _, err := NewAttachments().Expired(t.Context(), tornAttached, time.Now(), 100); err == nil {
		t.Fatal("a broken stream read as an empty sweep")
	}
	misreadAttached := &fakeQuerier{rows: &fakeRows{scans: []func(...any) error{func(...any) error {
		return errors.New("the row does not fit")
	}}}}
	if _, err := NewAttachments().Expired(t.Context(), misreadAttached, time.Now(), 100); err == nil {
		t.Fatal("a row that does not scan read as a row")
	}
}

func TestAnAttachmentIsWrittenReadAndEndedByItsWorkspace(t *testing.T) {
	a := anAttachment()
	inserted := &fakeQuerier{row: fakeRow{scan: attachmentScan(a)}}
	got, err := NewAttachments().Insert(t.Context(), inserted, a)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if got.Mode != ModeWrite || got.Status != StatusActive || string(got.Manifest) != string(a.Manifest) {
		t.Fatalf("Insert = %+v", got)
	}
	if len(inserted.args[0]) != 6 || inserted.args[0][0] != a.WorkspaceID {
		t.Errorf("the insert bound %v", inserted.args[0])
	}
	broken := &fakeQuerier{row: failing(errors.New("the connection failed"))}
	if _, err := NewAttachments().Insert(t.Context(), broken, a); err == nil {
		t.Fatal("a connection failure read as an attachment")
	}

	// An attachment is read through its workspace, so an id from another
	// workspace names no row rather than one a caller may act on.
	read := &fakeQuerier{row: fakeRow{scan: attachmentScan(a)}}
	if _, err := NewAttachments().Get(t.Context(), read, a.WorkspaceID, a.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !strings.Contains(read.statements[0], "id = $1 AND workspace_id = $2") {
		t.Errorf("the read is not scoped to its workspace: %s", read.statements[0])
	}
	elsewhere := &fakeQuerier{row: failing(pgx.ErrNoRows)}
	if _, err := NewAttachments().Get(t.Context(), elsewhere, "other", a.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("an attachment of another workspace = %v", err)
	}
	nonsense := &fakeQuerier{row: failing(badArgument())}
	if _, err := NewAttachments().Get(t.Context(), nonsense, a.WorkspaceID, "not-an-id"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a string that is not an id = %v", err)
	}

	for _, c := range []struct {
		name   string
		run    func(Querier) (bool, error)
		status string
	}{
		{"release", func(q Querier) (bool, error) { return NewAttachments().Release(t.Context(), q, a.ID) }, "released"},
		{"reap", func(q Querier) (bool, error) { return NewAttachments().Reap(t.Context(), q, a.ID) }, "reaped"},
	} {
		t.Run(c.name, func(t *testing.T) {
			done := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 1")}
			if ok, err := c.run(done); err != nil || !ok {
				t.Fatalf("%s = %v, %v", c.name, ok, err)
			}
			if done.args[0][1] != AttachmentStatus(c.status) {
				t.Errorf("the write bound the status %v", done.args[0][1])
			}
			// Only an active attachment ends, so a second release is a
			// false and changes nothing.
			if !strings.Contains(done.statements[0], "status = 'active'") {
				t.Errorf("the write ends an attachment that already ended: %s", done.statements[0])
			}
			again := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 0")}
			if ok, err := c.run(again); ok || err != nil {
				t.Fatalf("a second %s = %v, %v", c.name, ok, err)
			}
			broken := &fakeQuerier{execErr: errors.New("the connection failed")}
			if _, err := c.run(broken); err == nil {
				t.Fatalf("a connection failure read as a second %s", c.name)
			}
		})
	}

	pinned := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 1")}
	if ok, err := NewAttachments().SetManifest(t.Context(), pinned, a.ID, a.Manifest); err != nil || !ok {
		t.Fatalf("SetManifest = %v, %v", ok, err)
	}
	if !strings.Contains(pinned.statements[0], "status = 'active'") {
		t.Errorf("an ended attachment can be re-pinned: %s", pinned.statements[0])
	}
	ended := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 0")}
	if ok, err := NewAttachments().SetManifest(t.Context(), ended, a.ID, a.Manifest); ok || err != nil {
		t.Fatalf("pinning an ended attachment = %v, %v", ok, err)
	}
	brokenPin := &fakeQuerier{execErr: errors.New("the connection failed")}
	if _, err := NewAttachments().SetManifest(t.Context(), brokenPin, a.ID, a.Manifest); err == nil {
		t.Fatal("a connection failure read as an ended attachment")
	}
}

func TestTheManifestAndTheCountersReadOneSubtree(t *testing.T) {
	owner := aWorkspace().Owner
	id := object.NewID()
	rows := &fakeRows{scans: []func(...any) error{func(dest ...any) error {
		return assign(dest, []any{"workspaces/build/src/main.go", strings.Repeat("4", 64), int64(2814), id})
	}}}
	q := &fakeQuerier{rows: rows}
	manifest, err := NewWorkspaceObjects().Manifest(t.Context(), q, owner, "workspaces/build/")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if len(manifest) != 1 || manifest[0].ObjectID != id || manifest[0].Size != 2814 {
		t.Fatalf("Manifest = %+v", manifest)
	}
	// The manifest carries the object id, because materialize presigns the
	// key the row names and never a key recomputed from the path.
	if !strings.Contains(q.statements[0], "object_id") || !strings.Contains(q.statements[0], "ORDER BY path") {
		t.Errorf("the manifest is not the ordered rows with their objects: %s", q.statements[0])
	}
	// A prefix holding a percent or an underscore matches itself and not
	// everything under the space.
	if pattern, ok := q.args[0][1].(string); !ok || !strings.HasSuffix(pattern, "%") {
		t.Errorf("the manifest bound the pattern %v", q.args[0][1])
	}
	broken := &fakeQuerier{queryErr: errors.New("the connection failed")}
	if _, err := NewWorkspaceObjects().Manifest(t.Context(), broken, owner, "workspaces/build/"); err == nil {
		t.Fatal("a connection failure read as an empty workspace")
	}
	torn := &fakeQuerier{rows: &fakeRows{err: errors.New("the stream broke")}}
	if _, err := NewWorkspaceObjects().Manifest(t.Context(), torn, owner, "workspaces/build/"); err == nil {
		t.Fatal("a broken stream read as an empty workspace")
	}
	misread := &fakeQuerier{rows: &fakeRows{scans: []func(...any) error{func(...any) error {
		return errors.New("the row does not fit")
	}}}}
	if _, err := NewWorkspaceObjects().Manifest(t.Context(), misread, owner, "workspaces/build/"); err == nil {
		t.Fatal("a row that does not scan read as a row")
	}

	counted := &fakeQuerier{row: values(int64(812), int64(40122388))}
	files, bytes, err := NewWorkspaceObjects().Stat(t.Context(), counted, owner, "workspaces/build/")
	if err != nil || files != 812 || bytes != 40122388 {
		t.Fatalf("Stat = %d, %d, %v", files, bytes, err)
	}
	brokenStat := &fakeQuerier{row: failing(errors.New("the connection failed"))}
	if _, _, err := NewWorkspaceObjects().Stat(t.Context(), brokenStat, owner, "workspaces/build/"); err == nil {
		t.Fatal("a connection failure read as an empty workspace")
	}
}

func TestARenameMovesTheRowsAndTheBookmarksAndReachesNoBucket(t *testing.T) {
	owner := aWorkspace().Owner
	q := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 3")}
	moved, err := NewWorkspaceObjects().MoveSubtree(t.Context(), q, owner, "workspaces/build/", "workspaces/release/")
	if err != nil || moved != 3 {
		t.Fatalf("MoveSubtree = %d, %v", moved, err)
	}
	if len(q.statements) != 2 {
		t.Fatalf("the rename sent %d statements", len(q.statements))
	}
	// A star keys on the path too, so it moves with the subtree or it
	// orphans; nothing here names a bucket key, which is invariant 8.
	if !strings.Contains(q.statements[0], "UPDATE files") || !strings.Contains(q.statements[1], "UPDATE stars") {
		t.Errorf("the rename moves %v", q.statements)
	}
	// A trashed row under the old root follows the rename: it is still an
	// object of the workspace and its path has to keep naming it.
	if strings.Contains(q.statements[0], "deleted_at") {
		t.Errorf("the rename leaves the trashed rows behind: %s", q.statements[0])
	}
	clash := &fakeQuerier{execErr: taken()}
	if _, err := NewWorkspaceObjects().MoveSubtree(t.Context(), clash, owner, "workspaces/build/", "workspaces/release/"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a path already taken under the new root = %v", err)
	}
}

// failingSecond fails the second statement of a call, which is where the
// bookmarks move.
type failingSecond struct {
	fakeQuerier
	seen int
}

func (q *failingSecond) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	q.seen++
	if q.seen == 2 {
		return pgconn.CommandTag{}, errors.New("the connection failed")
	}
	return q.fakeQuerier.Exec(ctx, sql, args...)
}

func TestARenameThatCannotMoveTheBookmarksIsAFailureAndNotAHalfMove(t *testing.T) {
	q := &failingSecond{tag: pgconn.NewCommandTag("UPDATE 3")}
	if _, err := NewWorkspaceObjects().MoveSubtree(t.Context(), q, "owner", "workspaces/build/", "workspaces/release/"); err == nil {
		t.Fatal("a rename that moved the rows and not the stars reported success")
	}
}

func TestASyncDropsTheNamedRowsAndAnswersWhatTheyHeld(t *testing.T) {
	owner := aWorkspace().Owner
	first, second := object.NewID(), object.NewID()
	rows := &fakeRows{scans: []func(...any) error{
		func(dest ...any) error { return assign(dest, []any{first, int64(10)}) },
		func(dest ...any) error { return assign(dest, []any{second, int64(32)}) },
	}}
	q := &fakeQuerier{rows: rows}
	freed, bytes, err := NewWorkspaceObjects().Drop(t.Context(), q, owner, []string{"a", "b"})
	if err != nil || len(freed) != 2 || bytes != 42 {
		t.Fatalf("Drop = %v, %d, %v", freed, bytes, err)
	}
	// One statement for the rows, whatever the size of the tree: dropping a
	// large subtree costs a constant number of round trips.
	if len(q.statements) != 1 || !strings.Contains(q.statements[0], "path = ANY($2)") {
		t.Errorf("the drop is not one statement over the set: %v", q.statements)
	}
	empty, zero, err := NewWorkspaceObjects().Drop(t.Context(), &fakeQuerier{}, owner, nil)
	if err != nil || empty != nil || zero != 0 {
		t.Fatalf("dropping nothing = %v, %d, %v", empty, zero, err)
	}
	broken := &fakeQuerier{queryErr: errors.New("the connection failed")}
	if _, _, err := NewWorkspaceObjects().Drop(t.Context(), broken, owner, []string{"a"}); err == nil {
		t.Fatal("a connection failure read as nothing dropped")
	}
	torn := &fakeQuerier{rows: &fakeRows{err: errors.New("the stream broke")}}
	if _, _, err := NewWorkspaceObjects().Drop(t.Context(), torn, owner, []string{"a"}); err == nil {
		t.Fatal("a broken stream read as nothing dropped")
	}
	misread := &fakeQuerier{rows: &fakeRows{scans: []func(...any) error{func(...any) error {
		return errors.New("the row does not fit")
	}}}}
	if _, _, err := NewWorkspaceObjects().Drop(t.Context(), misread, owner, []string{"a"}); err == nil {
		t.Fatal("a row that does not scan read as a row")
	}
}

func TestBytesGoOnlyWhenNoRowStillNamesThem(t *testing.T) {
	held, free := object.NewID(), object.NewID()
	rows := &fakeRows{scans: []func(...any) error{
		func(dest ...any) error { return assign(dest, []any{free}) },
	}}
	q := &fakeQuerier{rows: rows}
	got, err := NewWorkspaceObjects().Unreferenced(t.Context(), q, []object.ID{held, free})
	if err != nil || len(got) != 1 || got[0] != free {
		t.Fatalf("Unreferenced = %v, %v", got, err)
	}
	// The union is the one ObjectReferenced reads, asked of many ids at
	// once, so a sync that drops a large tree asks it in one round trip.
	if !strings.Contains(q.statements[0], "FROM files") || !strings.Contains(q.statements[0], "FROM file_versions") {
		t.Errorf("the reference check reads %s", q.statements[0])
	}
	if len(q.statements) != 1 {
		t.Errorf("the reference check sent %d statements", len(q.statements))
	}
	none, err := NewWorkspaceObjects().Unreferenced(t.Context(), &fakeQuerier{}, nil)
	if err != nil || none != nil {
		t.Fatalf("asking about nothing = %v, %v", none, err)
	}
	broken := &fakeQuerier{queryErr: errors.New("the connection failed")}
	if _, err := NewWorkspaceObjects().Unreferenced(t.Context(), broken, []object.ID{free}); err == nil {
		t.Fatal("a connection failure read as an unreferenced object")
	}
	torn := &fakeQuerier{rows: &fakeRows{err: errors.New("the stream broke")}}
	if _, err := NewWorkspaceObjects().Unreferenced(t.Context(), torn, []object.ID{free}); err == nil {
		t.Fatal("a broken stream read as an unreferenced object")
	}
	misread := &fakeQuerier{rows: &fakeRows{scans: []func(...any) error{func(...any) error {
		return errors.New("the row does not fit")
	}}}}
	if _, err := NewWorkspaceObjects().Unreferenced(t.Context(), misread, []object.ID{free}); err == nil {
		t.Fatal("a row that does not scan read as a row")
	}
}
