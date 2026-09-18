// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The store tier of spec 007: the upload session queries against a real
// Postgres, and the one property the whole table exists for, which is that
// an open session keeps its object out of the reaper's reach.
package store

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/arca/object"
)

func TestStoreAnOpenSessionKeepsItsObjectReferencedAndExpiresOnItsColumn(t *testing.T) {
	db := tier(t)
	uploads := NewSessions()
	owner := "https://issuer.example|uploads"
	open := Session{
		Owner: owner, Path: "files/video/keynote.mp4", ObjectID: object.NewID(),
		UploadID: "2~abc", DeclaredSize: 700 << 20, ContentType: "video/mp4",
		CreatedBy: owner, ExpiresAt: time.Now().Add(24 * time.Hour),
	}
	written, err := uploads.Insert(t.Context(), db.Querier(), open)
	if err != nil {
		t.Fatal(err)
	}

	// This is the omission the predecessor shipped: the parts of an
	// incomplete multipart are invisible to object listing, so the session
	// row is the only thing that says the key is in use.
	referenced, err := ObjectReferenced(t.Context(), db.Querier(), open.ObjectID)
	if err != nil || !referenced {
		t.Fatalf("an object an open session names = %t, %v", referenced, err)
	}

	got, err := uploads.Get(t.Context(), db.Querier(), written.ID)
	if err != nil || got.UploadID != open.UploadID {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := uploads.Get(t.Context(), db.Querier(), "not-an-identifier"); err == nil ||
		!strings.Contains(err.Error(), pgx.ErrNoRows.Error()) {
		t.Fatalf("an id that is not an identifier = %v", err)
	}

	if expired, err := uploads.Expired(t.Context(), db.Querier(), time.Now(), 10); err != nil || len(expired) != 0 {
		t.Fatalf("a live session is expired = %v, %v", expired, err)
	}
	// The count arca_upload_sessions_open reads is that query's complement:
	// a session is open or expired and never both (spec 018).
	if n, err := uploads.CountOpen(t.Context(), db.Querier(), time.Now()); err != nil || n != 1 {
		t.Fatalf("CountOpen = %d, %v", n, err)
	}
	if _, err := db.Querier().Exec(t.Context(),
		`UPDATE upload_sessions SET expires_at = now() - interval '1 hour' WHERE id = $1`, written.ID); err != nil {
		t.Fatal(err)
	}
	expired, err := uploads.Expired(t.Context(), db.Querier(), time.Now(), 10)
	if err != nil || len(expired) != 1 || expired[0].ID != written.ID {
		t.Fatalf("Expired = %v, %v", expired, err)
	}
	if n, err := uploads.CountOpen(t.Context(), db.Querier(), time.Now()); err != nil || n != 0 {
		t.Fatalf("a session past its deadline read as open: %d, %v", n, err)
	}

	if ok, err := uploads.Delete(t.Context(), db.Querier(), written.ID); err != nil || !ok {
		t.Fatalf("Delete = %t, %v", ok, err)
	}
	referenced, err = ObjectReferenced(t.Context(), db.Querier(), open.ObjectID)
	if err != nil || referenced {
		t.Fatalf("a closed session still holds its object = %t, %v", referenced, err)
	}
}

func TestStoreTheUploadsMigrationCreatesTheTableItsSpecOwns(t *testing.T) {
	db := tier(t)
	var exists bool
	if err := db.Querier().QueryRow(t.Context(),
		`SELECT to_regclass(current_schema() || '.upload_sessions') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("the migrations did not create upload_sessions")
	}
	pending, err := Pending(t.Context(), db.Querier())
	if err != nil || len(pending) != 0 {
		t.Fatalf("Pending after the migrations = %v, %v", pending, err)
	}
}
