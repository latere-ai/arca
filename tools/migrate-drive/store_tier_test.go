// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The store tier of spec 019, criterion 3: `migrate-drive` against two real
// Postgres databases, one holding Drive's schema and one holding Arca's.
//
// What it proves is what the fake beside it cannot: that Drive's schema and
// Arca's are what the statements were written against, that every constraint
// of migration 0001 through 0005 admits the rows the rewrite produces, that a
// transaction per table commits, and that a second run over the database the
// first one filled is refused.
//
// It runs when E2E_DATABASE_URL is set and skips otherwise, so a plain go test
// on a clean clone stays green with no services. Every run creates the
// databases it needs and drops them when it ends.
package main

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"latere.ai/x/arca/internal/store"
)

// skipWithoutTheStack is the remediation this tier prints when the stack is
// not there. The tool reaches no bucket, so the database alone is the stack it
// needs.
const skipWithoutTheStack = "set E2E_DATABASE_URL (make up)"

// The fixture's principals and organization. They are the source's ids, and
// the copy rewrites them into subjects under the issuer below.
const (
	personA = "11111111-1111-4111-8111-aaaaaaaaaaaa"
	personB = "22222222-2222-4222-8222-bbbbbbbbbbbb"
	orgID   = "33333333-3333-4333-8333-cccccccccccc"

	tierIssuer     = "https://issuer.example"
	tierOrgSubject = "https://issuer.example|org-acme"
)

func subjectA() string { return tierIssuer + "|" + personA }
func subjectB() string { return tierIssuer + "|" + personB }

// The file ids, kept through the copy so a version and a sample pair with
// their rows.
const (
	fileNotes  = "aaaaaaaa-0000-4000-8000-000000000001"
	fileMemory = "aaaaaaaa-0000-4000-8000-000000000002"
	fileRepo   = "aaaaaaaa-0000-4000-8000-000000000003"
	fileGone   = "aaaaaaaa-0000-4000-8000-000000000004"
)

// notesKeyTier is the key Drive wrote for the first file, and the key its
// version still points at. One key is one object id after the copy.
const notesKeyTier = "drive/u-" + personA + "/files/notes.md"

const tierSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const tierETag = "0123456789abcdef0123456789abcdef-3"

// seed is the fixture: a row of every shape spec 019 names, in every table the
// copy reads, including the grantee kinds and statuses that do not arrive and
// a workspace of kind repo whose paths change plane.
var seed = fmt.Sprintf(`
INSERT INTO principal_directory (principal_id, email) VALUES
  ('%[1]s', 'a@example.com'),
  ('%[2]s', 'b@example.com');

INSERT INTO files (id, owner_type, owner_id, path, created_by, content_type,
                   size_bytes, storage_key, checksum, is_public, deleted_at) VALUES
  ('%[4]s', 'principal', '%[1]s', 'files/notes.md',       '%[1]s', 'text/markdown', 10, '%[8]s',                                       '%[9]s',  false, NULL),
  ('%[5]s', 'principal', '%[1]s', 'memory/agent.md',      '%[1]s', 'text/markdown', 20, 'drive/u-%[1]s/memory/agent.md',               '%[9]s',  false, NULL),
  ('%[6]s', 'org',       '%[3]s', 'repos/site/README.md', '%[2]s', 'text/markdown', 30, 'drive/o-%[3]s/repos/site/README.md',          '%[10]s', true,  NULL),
  ('%[7]s', 'principal', '%[1]s', 'files/gone.md',        '%[1]s', 'text/plain',    40, 'drive/u-%[1]s/files/gone.md',                 '%[9]s',  false, now());

-- The version points at the key the live row carries: Drive wrote a
-- non-versioned overwrite in place, so one object backs both rows.
INSERT INTO file_versions (owner_type, owner_id, path, version_no, content_type,
                           size_bytes, checksum, storage_key, created_by) VALUES
  ('principal', '%[1]s', 'files/notes.md', 1, 'text/markdown', 5, '%[9]s', '%[8]s', '%[1]s');

INSERT INTO stars (principal_id, owner_type, owner_id, path) VALUES
  ('%[2]s', 'org', '%[3]s', 'repos/site/README.md');

INSERT INTO upload_sessions (owner_type, owner_id, path, declared_size, content_type,
                             storage_key, s3_upload_id, created_by) VALUES
  ('principal', '%[1]s', 'files/big.bin', 500, 'application/octet-stream',
   'drive/u-%[1]s/files/big.bin@ab12', 'upload-1', '%[1]s');

INSERT INTO shares (owner_type, owner_id, path_prefix, grantee_type, grantee_id,
                    grantee_email, grantee_role, permission, token, status, created_by) VALUES
  ('principal', '%[1]s', 'files/',      'principal', '%[2]s', NULL,              NULL,    'write',  NULL,       'active',  '%[1]s'),
  ('org',       '%[3]s', 'repos/site/', 'org',       '%[3]s', NULL,              NULL,    'read',   NULL,       'active',  '%[2]s'),
  ('principal', '%[1]s', 'files/pub/',  'link',      NULL,    NULL,              NULL,    'read',   'tok-link', 'active',  '%[1]s'),
  ('principal', '%[1]s', 'files/pub/',  'public',    NULL,    NULL,              NULL,    'read',   NULL,       'revoked', '%[1]s'),
  ('principal', '%[1]s', 'files/inv/',  'principal', '%[2]s', NULL,              NULL,    'read',   'tok-inv',  'active',  '%[1]s'),
  ('principal', '%[1]s', 'files/',      'role',      NULL,    NULL,              'admin', 'read',   NULL,       'active',  '%[1]s'),
  ('principal', '%[1]s', 'files/',      'email',     NULL,    'c@example.com',   NULL,    'read',   'tok-mail', 'pending', '%[1]s'),
  ('principal', '%[1]s', 'files/',      'principal', '%[2]s', NULL,              NULL,    'manage', NULL,       'pending', '%[1]s'),
  ('principal', '%[1]s', 'files/w/',    'link',      NULL,    NULL,              NULL,    'write',  'tok-w',    'active',  '%[1]s');

INSERT INTO workspaces (owner_type, owner_id, kind, slug, created_by,
                        writer_sandbox_id, agent_access) VALUES
  ('principal', '%[1]s', 'workspace', 'build', '%[1]s', 'sandbox-1', 'visible'),
  ('org',       '%[3]s', 'repo',      'site',  '%[2]s', NULL,        'hidden');

INSERT INTO workspace_attachments (workspace_id, sandbox_id, principal_id, mode,
                                   status, manifest, expires_at)
SELECT id, 'sandbox-1', '%[1]s', 'rw', 'active',
       '[{"path":"out.txt","checksum":"x","size":1}]'::jsonb, now() + interval '1 hour'
  FROM workspaces WHERE slug = 'build';

INSERT INTO events (owner_type, owner_id, path, action, actor_id, detail) VALUES
  ('principal', '%[1]s', 'files/notes.md', 'put',            '%[1]s', '{"n":1}'::jsonb),
  ('org',       '%[3]s', 'repos/site',     'sync',           '%[2]s', NULL),
  ('principal', '%[1]s', NULL,             'quota_exceeded', NULL,    NULL);
`, personA, personB, orgID, fileNotes, fileMemory, fileRepo, fileGone,
	notesKeyTier, tierSHA, tierETag)

// TestStoreMigrateDriveCopiesEveryTableSpec019Names is criterion 3 of spec 019
// against the two schemas.
func TestStoreMigrateDriveCopiesEveryTableSpec019Names(t *testing.T) {
	src, dst := tierDatabases(t)
	out, errs, code := runTool(t, src, dst, false)
	if code != exitOK {
		t.Fatalf("the copy exited %d\n%s\n%s", code, out, errs)
	}
	target := open(t, dst)

	t.Run("the counts", func(t *testing.T) {
		for table, want := range map[string]int64{
			"subjects": 2, "files": 4, "file_versions": 1, "stars": 1,
			"upload_sessions": 1, "shares": 5, "workspaces": 2,
			"workspace_attachments": 1, "space_usage": 2, "events": 3,
		} {
			if got := rows(t, target, table); got != want {
				t.Errorf("%s holds %d rows, want %d", table, got, want)
			}
		}
	})

	t.Run("the owners are subjects", func(t *testing.T) {
		if got := one[string](t, target, `SELECT owner FROM files WHERE id = $1`, fileNotes); got != subjectA() {
			t.Errorf("a personal owner is %q, want %q", got, subjectA())
		}
		if got := one[string](t, target, `SELECT owner FROM files WHERE id = $1`, fileRepo); got != tierOrgSubject {
			t.Errorf("an organization owner is %q, want %q", got, tierOrgSubject)
		}
		if got := one[int64](t, target, `SELECT count(*) FROM subjects WHERE subject = $1`, subjectB()); got != 1 {
			t.Error("the subject directory does not hold the second principal")
		}
		if got := one[string](t, target, `SELECT subject FROM stars`); got != subjectB() {
			t.Errorf("the star belongs to %q", got)
		}
	})

	t.Run("the planes", func(t *testing.T) {
		for id, want := range map[string]string{
			fileNotes:  "files/notes.md",
			fileMemory: "files/memory/agent.md",
			fileRepo:   "workspaces/site/README.md",
			fileGone:   "files/gone.md",
		} {
			if got := one[string](t, target, `SELECT path FROM files WHERE id = $1`, id); got != want {
				t.Errorf("the file is at %q, want %q", got, want)
			}
		}
		if got := one[string](t, target, `SELECT path FROM stars`); got != "workspaces/site/README.md" {
			t.Errorf("the star is at %q", got)
		}
		// The one grant the organization made, read by its owner and its
		// grantee alone, so a repos prefix the copy left alone reads back as
		// itself rather than as no row at all.
		if got := one[string](t, target,
			`SELECT path_prefix FROM shares WHERE owner = $1 AND grantee = $1`,
			tierOrgSubject); got != "workspaces/site/" {
			t.Errorf("the grant covers %q", got)
		}
		if got := one[string](t, target, `SELECT path FROM events WHERE action = 'sync'`); got != "workspaces/site" {
			t.Errorf("the event is at %q", got)
		}
	})

	t.Run("one object id per Drive key", func(t *testing.T) {
		live := one[string](t, target, `SELECT object_id::text FROM files WHERE id = $1`, fileNotes)
		version := one[string](t, target, `SELECT object_id::text FROM file_versions`)
		if live != version {
			t.Errorf("one Drive key became two object ids, %q and %q", live, version)
		}
		if got := one[int64](t, target, `SELECT count(DISTINCT object_id) FROM files`); got != 4 {
			t.Errorf("four keys became %d object ids", got)
		}
	})

	t.Run("the checksum kinds", func(t *testing.T) {
		if got := one[string](t, target, `SELECT checksum_kind FROM files WHERE id = $1`, fileNotes); got != "sha256" {
			t.Errorf("a sha256 is stored as %q", got)
		}
		if got := one[string](t, target, `SELECT checksum_kind FROM files WHERE id = $1`, fileRepo); got != "etag" {
			t.Errorf("a composite ETag is stored as %q", got)
		}
	})

	t.Run("the grants that arrive", func(t *testing.T) {
		if got := one[int64](t, target, `SELECT count(*) FROM shares WHERE grantee_kind = 'subject'`); got != 3 {
			t.Errorf("%d subject grants, want 3", got)
		}
		if got := one[int64](t, target, `SELECT count(*) FROM shares WHERE token IS NOT NULL`); got != 2 {
			t.Errorf("%d token grants, want 2", got)
		}
		// The revoked public grant carried no token and was minted one, the way
		// Drive's own migration 000010 minted the others.
		if got := one[string](t, target, `SELECT token FROM shares WHERE grantee_kind = 'public'`); got == "" {
			t.Error("the public grant carries no token")
		}
		// The invite token is gone: the grantee is the subject now.
		if got := one[int64](t, target, `SELECT count(*) FROM shares WHERE token = 'tok-inv'`); got != 0 {
			t.Error("an invite token survived onto a subject grant")
		}
	})

	t.Run("the workspaces", func(t *testing.T) {
		if got := one[int64](t, target, `SELECT count(*) FROM workspaces WHERE slug IN ('build', 'site')`); got != 2 {
			t.Errorf("%d workspaces by name, want 2", got)
		}
		if got := one[string](t, target, `SELECT writer_holder FROM workspaces WHERE slug = 'build'`); got != "sandbox-1" {
			t.Errorf("the lease holder is %q", got)
		}
		if got := one[string](t, target, `SELECT holder FROM workspace_attachments`); got != "sandbox-1" {
			t.Errorf("the attachment holder is %q", got)
		}
		if got := one[string](t, target, `SELECT subject FROM workspace_attachments`); got != subjectA() {
			t.Errorf("the attachment subject is %q", got)
		}
	})

	t.Run("the ledger", func(t *testing.T) {
		if got := one[int64](t, target, `SELECT bytes FROM space_usage WHERE owner = $1`, subjectA()); got != 75 {
			t.Errorf("the personal space holds %d bytes, want 75", got)
		}
		if got := one[int64](t, target, `SELECT bytes FROM space_usage WHERE owner = $1`, tierOrgSubject); got != 30 {
			t.Errorf("the organization holds %d bytes, want 30", got)
		}
	})

	t.Run("the log keeps its ids and its sequence", func(t *testing.T) {
		if got := one[int64](t, target, `SELECT max(id) FROM events`); got != 3 {
			t.Errorf("the last event id is %d, want 3", got)
		}
		next := one[int64](t, target,
			`INSERT INTO events (owner, action) VALUES ($1, 'put') RETURNING id`, subjectA())
		if next != 4 {
			t.Errorf("the next event took id %d; the sequence was not set past the copy", next)
		}
	})

	t.Run("the upload session", func(t *testing.T) {
		gap := one[time.Duration](t, target,
			`SELECT expires_at - created_at FROM upload_sessions`)
		if gap != SessionLifetime {
			t.Errorf("the session lives %v, want %v", gap, SessionLifetime)
		}
	})

	t.Run("the report", func(t *testing.T) {
		for _, text := range []string{
			DropRole, DropEmail, DropShareRequests, DropWritableToken,
			"counts hold", "checksums were sampled", "the bytes",
			"Do not switch the routes on this report alone.",
		} {
			if !strings.Contains(out, text) {
				t.Errorf("the report holds no %q:\n%s", text, out)
			}
		}
	})
}

// TestStoreMigrateDriveRefusesATargetThatHoldsRows is the idempotence of
// criterion 3: a second run over the database the first one filled does not
// begin, so running the tool twice leaves what one run left.
func TestStoreMigrateDriveRefusesATargetThatHoldsRows(t *testing.T) {
	src, dst := tierDatabases(t)
	if _, errs, code := runTool(t, src, dst, false); code != exitOK {
		t.Fatalf("the first copy exited %d: %s", code, errs)
	}
	target := open(t, dst)
	before := rows(t, target, "files")

	out, errs, code := runTool(t, src, dst, false)
	if code != exitRefused {
		t.Fatalf("the second copy exited %d\n%s", code, out)
	}
	if !strings.Contains(errs, "already holds") {
		t.Errorf("the refusal is %q", errs)
	}
	if after := rows(t, target, "files"); after != before {
		t.Errorf("the refused run changed the target from %d rows to %d", before, after)
	}
}

// TestStoreMigrateDriveDryRunWritesNothing reports what a copy would do
// against a target the run leaves empty.
func TestStoreMigrateDriveDryRunWritesNothing(t *testing.T) {
	src, dst := tierDatabases(t)
	out, errs, code := runTool(t, src, dst, true)
	if code != exitOK {
		t.Fatalf("the dry run exited %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "dry run, nothing is written") {
		t.Errorf("the report does not say it wrote nothing:\n%s", out)
	}
	if !strings.Contains(out, DropRole) {
		t.Errorf("the dry run does not report the rows it would drop:\n%s", out)
	}
	target := open(t, dst)
	for _, table := range TableNames() {
		if got := rows(t, target, table); got != 0 {
			t.Errorf("a dry run left %d rows in %s", got, table)
		}
	}
}

// TestStoreMigrateDriveRefusesAnUnmappedOrganization is the refusal before any
// write, against the real schema.
func TestStoreMigrateDriveRefusesAnUnmappedOrganization(t *testing.T) {
	src, dst := tierDatabases(t)
	out, errs, code := runWith(t, src, dst, writeMapping(t, "{}"), false)
	if code != exitRefused {
		t.Fatalf("a copy with no mapping exited %d\n%s", code, out)
	}
	if !strings.Contains(errs, orgID) {
		t.Errorf("the refusal names no organization: %q", errs)
	}
	target := open(t, dst)
	for _, table := range TableNames() {
		if got := rows(t, target, table); got != 0 {
			t.Errorf("a refused run left %d rows in %s", got, table)
		}
	}
}

// tierDatabases creates a source holding Drive's schema and the fixture and a
// target holding Arca's migrations, and drops both when the test ends.
func tierDatabases(t *testing.T) (source, target string) {
	t.Helper()
	admin := os.Getenv("E2E_DATABASE_URL")
	if admin == "" {
		t.Skip(skipWithoutTheStack)
	}
	name := fmt.Sprintf("migrate_drive_%d_%d", time.Now().UnixNano(), os.Getpid())
	source, target = createDatabase(t, admin, name+"_src"), createDatabase(t, admin, name+"_dst")

	schema, err := os.ReadFile(filepath.Join("testdata", "drive_schema.sql"))
	if err != nil {
		t.Fatalf("read Drive's schema: %v", err)
	}
	src := open(t, source)
	if _, err := src.Exec(t.Context(), string(schema)); err != nil {
		t.Fatalf("apply Drive's schema: %v", err)
	}
	if _, err := src.Exec(t.Context(), seed); err != nil {
		t.Fatalf("seed the source: %v", err)
	}
	if err := store.Migrate(target); err != nil {
		t.Fatalf("apply Arca's migrations: %v", err)
	}
	return source, target
}

// createDatabase makes one database beside the stack's and drops it with its
// connections when the test ends, so a run leaves nothing behind.
func createDatabase(t *testing.T, admin, name string) string {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), admin)
	if err != nil {
		t.Fatalf("open the stack's database: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if _, err := pool.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("read E2E_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// open answers a pool on one database, closed when the test ends.
func open(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// runTool runs the command the operator runs, with the mapping the platform
// exports, and answers its output and its exit code.
func runTool(t *testing.T, source, target string, dryRun bool) (stdout, stderr string, code int) {
	t.Helper()
	mapping := writeMapping(t, fmt.Sprintf(`{%q: %q}`, orgID, tierOrgSubject))
	return runWith(t, source, target, mapping, dryRun)
}

func runWith(t *testing.T, source, target, mapping string, dryRun bool) (stdout, stderr string, code int) {
	t.Helper()
	// The tool compares -prefix against the installation's variable, so the
	// tier sets what the installation would be deployed with.
	t.Setenv(BucketPrefixVar, "drive/")
	args := []string{
		"-source", source, "-target", target,
		"-issuer", tierIssuer, "-org-subjects", mapping,
	}
	if dryRun {
		args = append(args, "-dry-run")
	}
	var out, errs bytes.Buffer
	code = cli(t.Context(), args, &out, &errs)
	return out.String(), errs.String(), code
}

// writeMapping puts the organization mapping where a flag can name it.
func writeMapping(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orgs.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the mapping: %v", err)
	}
	return path
}

// rows is what one table holds.
func rows(t *testing.T, pool *pgxpool.Pool, table string) int64 {
	t.Helper()
	return one[int64](t, pool, "SELECT count(*) FROM "+table)
}

// one reads a single value, and fails the test when the query does not answer.
func one[T any](t *testing.T, pool *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := pool.QueryRow(t.Context(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}
