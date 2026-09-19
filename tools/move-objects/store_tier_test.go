// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The store tier of spec 019, the bytes half of criterion 4: the two commands
// of step 3 of the cutover, in the order an operator runs them, against the
// real Postgres and the real MinIO of spec 014.
//
// What it proves is what a fake cannot. The row copy mints the object ids and
// writes the manifest; the move reads that file and copies each key inside
// the bucket; and every object is then readable at the key its id derives,
// with the bytes it had under the key the predecessor wrote. The two tools
// are coupled only by that file, and this is where the coupling is proved.
//
// It runs when E2E_DATABASE_URL and E2E_S3_ENDPOINT are set and skips
// otherwise. Every run creates the databases it needs, writes under a bucket
// prefix of its own, and removes both when it ends.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// skipWithoutTheStack names both services and the command that starts them.
const skipWithoutTheStack = "set E2E_DATABASE_URL and E2E_S3_ENDPOINT (make up)"

// The fixture's principal and its files. The ids are kept through the copy,
// so a row and the object it points at pair by id.
const (
	tierPerson = "11111111-1111-4111-8111-aaaaaaaaaaaa"
	tierIssuer = "https://issuer.example"

	tierNotes  = "aaaaaaaa-0000-4000-8000-000000000001"
	tierLogo   = "aaaaaaaa-0000-4000-8000-000000000002"
	tierBig    = "aaaaaaaa-0000-4000-8000-000000000003"
	tierPublic = "aaaaaaaa-0000-4000-8000-000000000004"
)

// tierBodies is what each file holds in the bucket, keyed by the path under
// the run's prefix that Drive built for it.
func tierBodies() map[string][]byte {
	return map[string][]byte{
		"u-" + tierPerson + "/files/notes.md":        []byte("the bytes of one note"),
		"u-" + tierPerson + "/files/my logo@ab12":    []byte("the bytes of one logo, under a key with a space"),
		"u-" + tierPerson + "/files/big.bin":         bytes.Repeat([]byte("z"), 64<<10),
		"u-" + tierPerson + "/files/public/logo.png": []byte("the bytes of one public object"),
	}
}

// TestStoreTheTwoCommandsOfStepThreeLeaveEveryByteAtItsObjectIDsKey is the
// cutover's step 3, run the way docs/operations.md tells an operator to run
// it: the row copy with a manifest, then the move.
func TestStoreTheTwoCommandsOfStepThreeLeaveEveryByteAtItsObjectIDsKey(t *testing.T) {
	endpoint := os.Getenv("E2E_S3_ENDPOINT")
	if endpoint == "" || os.Getenv("E2E_DATABASE_URL") == "" {
		t.Skip(skipWithoutTheStack)
	}
	prefix := fmt.Sprintf("test-%d-%d/drive/", time.Now().UnixNano(), os.Getpid())
	bucket := tierBucket(t, endpoint, prefix)
	source, target := tierDatabases(t, prefix)

	// The bucket holds what the predecessor wrote: keys built from an owner
	// and a path, one of them with a character outside a path's alphabet.
	bodies := map[string][]byte{}
	for path, body := range tierBodies() {
		key := prefix + path
		bodies[key] = body
		if _, err := bucket.Put(t.Context(), key, bytes.NewReader(body), int64(len(body)), blob.PutOptions{ContentType: "application/octet-stream"}); err != nil {
			t.Fatalf("seed %q: %v", key, err)
		}
	}

	manifestPath := filepath.Join(t.TempDir(), "manifest.tsv")
	copyRows(t, source, target, prefix, manifestPath)

	// The second command of step 3.
	t.Setenv(BucketPrefixVar, prefix)
	t.Setenv(AccessKeyVar, envOr("E2E_S3_KEY", "minioadmin"))
	t.Setenv(SecretKeyVar, envOr("E2E_S3_SECRET", "minioadmin"))
	args := []string{
		"-manifest", manifestPath,
		"-bucket", envOr("E2E_S3_BUCKET", "arca-test"),
		"-endpoint", endpoint, "-region", "us-east-1", "-path-style",
		"-prefix", prefix, "-concurrency", "4",
	}

	var rehearsal bytes.Buffer
	if code := cli(t.Context(), append(args, "-dry-run"), &rehearsal, &rehearsal); code != exitOK {
		t.Fatalf("the dry run exited %d\n%s", code, rehearsal.String())
	}
	if !strings.Contains(rehearsal.String(), "the dry run holds") {
		t.Fatalf("the dry run does not hold:\n%s", rehearsal.String())
	}

	var out, errs bytes.Buffer
	if code := cli(t.Context(), args, &out, &errs); code != exitOK {
		t.Fatalf("the move exited %d\n%s\n%s", code, out.String(), errs.String())
	}
	if !strings.Contains(out.String(), "the move holds") {
		t.Fatalf("the move does not hold:\n%s", out.String())
	}

	// Every row points at an object id, and every object id's key holds the
	// bytes the source key held. That is the byte half of criterion 4.
	pool := open(t, target)
	moved := 0
	for _, id := range []string{tierNotes, tierLogo, tierBig, tierPublic} {
		objectID := one[string](t, pool, `SELECT object_id::text FROM files WHERE id = $1`, id)
		key := object.ID(objectID).Key(prefix)
		sourceKey := sourceOf(t, pool, id, prefix)
		rc, held, err := bucket.Get(t.Context(), key)
		if err != nil {
			t.Errorf("read %s back at %s: %v", id, key, err)
			continue
		}
		body, _ := io.ReadAll(rc)
		_ = rc.Close()
		if want := bodies[sourceKey]; !bytes.Equal(body, want) {
			t.Errorf("%s holds %d bytes at %s, want the %d of %s", id, len(body), key, len(want), sourceKey)
		}
		if held.Size != int64(len(bodies[sourceKey])) {
			t.Errorf("%s is %d bytes at %s", id, held.Size, key)
		}
		moved++
		// The move never deletes: the source keys stay until the sunset.
		if _, err := bucket.Head(t.Context(), sourceKey); err != nil {
			t.Errorf("the move removed the source key %s: %v", sourceKey, err)
		}
	}
	if moved != 4 {
		t.Fatalf("%d of 4 objects were read back", moved)
	}

	// A second move is a resume: every destination is already there with the
	// right bytes, so nothing is copied and nothing is overwritten.
	var again bytes.Buffer
	if code := cli(t.Context(), args, &again, &again); code != exitOK {
		t.Fatalf("the second move exited %d\n%s", code, again.String())
	}
	counts := strings.Join(strings.Fields(again.String()), " ")
	if !strings.Contains(counts, "copied 0") || !strings.Contains(counts, "skipped 4") {
		t.Errorf("the second move did not skip every key:\n%s", again.String())
	}
	// Three rows carry the sha256 of their bytes and were read back and
	// digested, which is the only proof a store reporting no checksum of its
	// own can give; the fourth carries the store's own label and took the
	// rung below. Nothing was left on the size alone.
	counted := strings.Join(strings.Fields(out.String()), " ")
	for _, says := range []string{"verified on bytes 3", "verified on label 1"} {
		if !strings.Contains(counted, says) {
			t.Errorf("the move did not report %q:\n%s", says, out.String())
		}
	}
	if strings.Contains(counted, "verified on size") {
		t.Errorf("a key was left on its size alone:\n%s", out.String())
	}
}

// TestStoreACorruptedDestinationFailsTheRunAndNamesTheKey is criterion 4b
// earning its keep at a store that reports no checksum of its own: the
// destination is there, the right length, and wrong, and only a read of the
// bytes tells the difference.
func TestStoreACorruptedDestinationFailsTheRunAndNamesTheKey(t *testing.T) {
	endpoint := os.Getenv("E2E_S3_ENDPOINT")
	if endpoint == "" || os.Getenv("E2E_DATABASE_URL") == "" {
		t.Skip(skipWithoutTheStack)
	}
	prefix := fmt.Sprintf("test-%d-%d/drive/", time.Now().UnixNano(), os.Getpid())
	bucket := tierBucket(t, endpoint, prefix)
	source, target := tierDatabases(t, prefix)

	bodies := map[string][]byte{}
	for path, body := range tierBodies() {
		key := prefix + path
		bodies[key] = body
		if _, err := bucket.Put(t.Context(), key, bytes.NewReader(body), int64(len(body)), blob.PutOptions{}); err != nil {
			t.Fatalf("seed %q: %v", key, err)
		}
	}
	manifestPath := filepath.Join(t.TempDir(), "manifest.tsv")
	copyRows(t, source, target, prefix, manifestPath)

	t.Setenv(BucketPrefixVar, prefix)
	t.Setenv(AccessKeyVar, envOr("E2E_S3_KEY", "minioadmin"))
	t.Setenv(SecretKeyVar, envOr("E2E_S3_SECRET", "minioadmin"))
	args := []string{
		"-manifest", manifestPath,
		"-bucket", envOr("E2E_S3_BUCKET", "arca-test"),
		"-endpoint", endpoint, "-region", "us-east-1", "-path-style",
		"-prefix", prefix, "-concurrency", "4",
	}
	var out, errs bytes.Buffer
	if code := cli(t.Context(), args, &out, &errs); code != exitOK {
		t.Fatalf("the move exited %d\n%s\n%s", code, out.String(), errs.String())
	}

	// One destination is overwritten with other bytes of the same length,
	// which is what a store losing a byte in flight would leave. The size
	// still holds and the store's own copy still holds.
	pool := open(t, target)
	objectID := one[string](t, pool, `SELECT object_id::text FROM files WHERE id = $1`, tierNotes)
	corrupted := object.ID(objectID).Key(prefix)
	held := bodies[prefix+"u-"+tierPerson+"/files/notes.md"]
	other := bytes.Repeat([]byte("x"), len(held))
	if err := bucket.Delete(t.Context(), corrupted); err != nil {
		t.Fatal(err)
	}
	if _, err := bucket.Put(t.Context(), corrupted, bytes.NewReader(other), int64(len(other)), blob.PutOptions{}); err != nil {
		t.Fatalf("corrupt the destination: %v", err)
	}

	var again, againErrs bytes.Buffer
	code := cli(t.Context(), args, &again, &againErrs)
	if code != exitRefused {
		t.Fatalf("a corrupted destination exited %d, want %d\n%s", code, exitRefused, again.String())
	}
	for _, says := range []string{prefix + "u-" + tierPerson + "/files/notes.md", "digest", "the move is not clean"} {
		if !strings.Contains(again.String(), says) {
			t.Errorf("the report holds no %q:\n%s", says, again.String())
		}
	}
	// Nothing is overwritten: what to do with a disagreement is the
	// operator's, and the run leaves the evidence where it found it.
	rc, _, err := bucket.Get(t.Context(), corrupted)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if got, _ := io.ReadAll(rc); !bytes.Equal(got, other) {
		t.Error("the run overwrote the destination it could not verify")
	}
	// The byte check is what caught it: with it off, the same run is clean.
	var without bytes.Buffer
	if code := cli(t.Context(), append(args, "-verify-bytes=false"), &without, &without); code != exitOK {
		t.Fatalf("without the byte check the run exited %d\n%s", code, without.String())
	}
}

// sourceOf answers the Drive key a copied row came from, which the fixture
// knows by the row's path.
func sourceOf(t *testing.T, pool *pgxpool.Pool, id, prefix string) string {
	t.Helper()
	path := one[string](t, pool, `SELECT path FROM files WHERE id = $1`, id)
	// The copy rewrote the plane and nothing else of the path, and every
	// fixture row is in files/, so the source key is the prefix, the owner,
	// and the path the row still carries.
	return prefix + "u-" + tierPerson + "/" + path
}

// copyRows runs migrate-drive, which is a command of its own and so a binary
// this test builds and runs, the way the e2e tier of spec 014 runs arcad.
func copyRows(t *testing.T, source, target, prefix, manifestPath string) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "migrate-drive")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "latere.ai/x/arca/tools/migrate-drive")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build migrate-drive: %v\n%s", err, out)
	}
	cmd := exec.CommandContext(t.Context(), binary,
		"-source", source, "-target", target,
		"-issuer", tierIssuer, "-prefix", prefix, "-manifest", manifestPath)
	cmd.Env = append(os.Environ(), BucketPrefixVar+"="+prefix)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the row copy: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "4 keys, complete") {
		t.Fatalf("the copy did not complete a manifest of four keys:\n%s", out)
	}
}

// tierBucket opens a client against MinIO and removes everything the run
// wrote when it ends.
func tierBucket(t *testing.T, endpoint, prefix string) *blob.S3 {
	t.Helper()
	bucket, err := blob.NewS3(t.Context(), blob.Options{
		Bucket:    envOr("E2E_S3_BUCKET", "arca-test"),
		Endpoint:  endpoint,
		Region:    "us-east-1",
		AccessKey: envOr("E2E_S3_KEY", "minioadmin"),
		SecretKey: envOr("E2E_S3_SECRET", "minioadmin"),
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(func() { sweep(t, bucket, strings.SplitAfter(prefix, "/")[0]) })
	return bucket
}

// sweep removes every key one run wrote.
func sweep(t *testing.T, bucket *blob.S3, prefix string) {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	for {
		page, err := bucket.List(ctx, prefix, "", 1000)
		if err != nil {
			t.Errorf("sweep %q: %v", prefix, err)
			return
		}
		if len(page.Keys) == 0 {
			return
		}
		if err := bucket.DeleteMany(ctx, page.Keys); err != nil {
			t.Errorf("sweep %q: %v", prefix, err)
			return
		}
		if !page.Truncated {
			return
		}
	}
}

// envOr answers the variable or the default of spec 014's table.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// tierDatabases creates a source holding Drive's schema and the fixture and a
// target holding Arca's migrations, and drops both when the test ends. The
// schema is the row copy's own fixture, read from its directory, so the two
// tiers seed one schema and not two.
func tierDatabases(t *testing.T, prefix string) (source, target string) {
	t.Helper()
	admin := os.Getenv("E2E_DATABASE_URL")
	name := fmt.Sprintf("move_objects_%d_%d", time.Now().UnixNano(), os.Getpid())
	source, target = createDatabase(t, admin, name+"_src"), createDatabase(t, admin, name+"_dst")

	schema, err := os.ReadFile(filepath.Join("..", "migrate-drive", "testdata", "drive_schema.sql"))
	if err != nil {
		t.Fatalf("read Drive's schema: %v", err)
	}
	src := open(t, source)
	if _, err := src.Exec(t.Context(), string(schema)); err != nil {
		t.Fatalf("apply Drive's schema: %v", err)
	}
	if _, err := src.Exec(t.Context(), seed(prefix)); err != nil {
		t.Fatalf("seed the source: %v", err)
	}
	if err := store.Migrate(target); err != nil {
		t.Fatalf("apply Arca's migrations: %v", err)
	}
	return source, target
}

// seed is the fixture: four files whose keys the bucket holds, one of them
// public, one of them under a key with a character outside a path's alphabet,
// and one of them with a superseded version pointing at the same key, which
// is one object id and one manifest line for two rows.
//
// The sizes are the lengths of tierBodies, because the manifest carries what
// the rows say and the move compares that against what the bucket holds.
//
// Three rows carry the sha256 of their bytes, which is what the predecessor
// stored for an object written in one piece and what the byte check reads a
// destination back against. The public one carries the store's own label
// instead, so the rung below the byte check runs against the real store too.
func seed(prefix string) string {
	bodies := tierBodies()
	size := func(path string) int { return len(bodies[path]) }
	owner := "u-" + tierPerson
	digest := func(path string) string { return sha(bodies[path]) }
	return fmt.Sprintf(`
INSERT INTO principal_directory (principal_id, email) VALUES ('%[1]s', 'a@example.com');

INSERT INTO files (id, owner_type, owner_id, path, created_by, content_type,
                   size_bytes, storage_key, checksum, is_public, deleted_at) VALUES
  ('%[2]s', 'principal', '%[1]s', 'files/notes.md',        '%[1]s', 'text/markdown',            %[7]d,  '%[6]s%[11]s/files/notes.md',        '%[10]s', false, NULL),
  ('%[3]s', 'principal', '%[1]s', 'files/my logo@ab12',    '%[1]s', 'image/png',                %[8]d,  '%[6]s%[11]s/files/my logo@ab12',    '%[14]s', false, NULL),
  ('%[4]s', 'principal', '%[1]s', 'files/big.bin',         '%[1]s', 'application/octet-stream', %[9]d,  '%[6]s%[11]s/files/big.bin',         '%[15]s', false, NULL),
  ('%[5]s', 'principal', '%[1]s', 'files/public/logo.png', '%[1]s', 'image/png',                %[12]d, '%[6]s%[11]s/files/public/logo.png', '%[13]s', true,  NULL);

-- The version points at the key the live row carries, so one key is one
-- object id and one manifest line.
INSERT INTO file_versions (owner_type, owner_id, path, version_no, content_type,
                           size_bytes, checksum, storage_key, created_by) VALUES
  ('principal', '%[1]s', 'files/notes.md', 1, 'text/markdown', 9, '%[10]s', '%[6]s%[11]s/files/notes.md', '%[1]s');
`, tierPerson, tierNotes, tierLogo, tierBig, tierPublic, prefix,
		size(owner+"/files/notes.md"), size(owner+"/files/my logo@ab12"),
		size(owner+"/files/big.bin"), digest(owner+"/files/notes.md"), owner,
		size(owner+"/files/public/logo.png"), label(bodies[owner+"/files/public/logo.png"]),
		digest(owner+"/files/my logo@ab12"), digest(owner+"/files/big.bin"))
}

// createDatabase makes one database beside the stack's and drops it with its
// connections when the test ends.
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

// one reads a single value, and fails the test when the query does not
// answer.
func one[T any](t *testing.T, pool *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := pool.QueryRow(t.Context(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}
