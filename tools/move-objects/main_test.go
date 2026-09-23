// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/arca/tools/internal/manifest"
)

// writeManifest puts a complete manifest of the entries in a temporary file.
func writeManifest(t *testing.T, at string, entries ...manifest.Entry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.tsv")
	if err := manifest.WriteFile(path, at, entries); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Complete(path, len(entries)); err != nil {
		t.Fatal(err)
	}
	return path
}

// command runs the command line and answers what it wrote and its code.
func command(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errs bytes.Buffer
	code = cli(t.Context(), args, &out, &errs)
	return out.String(), errs.String(), code
}

func TestABadFlagIsExitTwo(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
	}{
		{"a flag the command does not take", []string{"-nonsense"}},
		{"an argument that is not a flag", []string{"-manifest", "m.tsv", "-bucket", "b", "extra"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, errs, code := command(t, c.args...)
			if code != exitUsage {
				t.Fatalf("the command exited %d, want %d: %s", code, exitUsage, errs)
			}
		})
	}
}

func TestEveryReasonAMoveIsRefusedIsNamedAtOnce(t *testing.T) {
	t.Setenv(BucketPrefixVar, "")
	_, errs, code := command(t, "-manifest", "", "-bucket", "", "-region", "", "-prefix", "/", "-concurrency", "0")
	if code != exitRefused {
		t.Fatalf("the command exited %d, want %d", code, exitRefused)
	}
	for _, says := range []string{"-manifest", "-bucket", "-region", "-prefix", "-concurrency"} {
		if !strings.Contains(errs, says) {
			t.Errorf("the refusal does not name %s:\n%s", says, errs)
		}
	}
}

func TestAPrefixTheInstallationDisagreesWithIsRefused(t *testing.T) {
	t.Setenv(BucketPrefixVar, "arca/")
	path := writeManifest(t, "drive/")
	_, errs, code := command(t, "-manifest", path, "-bucket", "arca-test", "-prefix", "drive/")
	if code != exitRefused {
		t.Fatalf("the command exited %d", code)
	}
	if !strings.Contains(errs, BucketPrefixVar) || !strings.Contains(errs, "one prefix") {
		t.Errorf("the refusal is %q", errs)
	}
}

func TestAManifestUnderAnotherPrefixIsRefused(t *testing.T) {
	t.Setenv(BucketPrefixVar, "")
	path := writeManifest(t, "old-drive/")
	_, errs, code := command(t, "-manifest", path, "-bucket", "arca-test", "-prefix", "drive/")
	if code != exitRefused {
		t.Fatalf("the command exited %d", code)
	}
	if !strings.Contains(errs, "old-drive/") || !strings.Contains(errs, "written under the prefix") {
		t.Errorf("the refusal is %q", errs)
	}
}

func TestAManifestWithNoCompletionLineIsRefused(t *testing.T) {
	t.Setenv(BucketPrefixVar, "")
	// A copy that was killed, or one whose verification did not hold, leaves
	// a body with no trailer. The move writes no byte on it.
	path := filepath.Join(t.TempDir(), "manifest.tsv")
	if err := manifest.WriteFile(path, "drive/", []manifest.Entry{entry(notesKey, idNotes, []byte("x"), false)}); err != nil {
		t.Fatal(err)
	}
	_, errs, code := command(t, "-manifest", path, "-bucket", "arca-test", "-prefix", "drive/")
	if code != exitRefused {
		t.Fatalf("the command exited %d", code)
	}
	if !strings.Contains(errs, "no completion line") || !strings.Contains(errs, "1 keys are listed") {
		t.Errorf("the refusal is %q", errs)
	}
}

func TestAManifestThatIsNotThereIsRefused(t *testing.T) {
	t.Setenv(BucketPrefixVar, "")
	missing := filepath.Join(t.TempDir(), "absent.tsv")
	_, errs, code := command(t, "-manifest", missing, "-bucket", "arca-test")
	if code != exitRefused {
		t.Fatalf("the command exited %d", code)
	}
	if !strings.Contains(errs, missing) {
		t.Errorf("the refusal does not name the file: %q", errs)
	}
}

func TestAStoreTheCommandCannotReachFailsEveryKey(t *testing.T) {
	t.Setenv(BucketPrefixVar, "")
	t.Setenv(AccessKeyVar, "key")
	t.Setenv(SecretKeyVar, "secret")
	body := []byte("the bytes of one note")
	path := writeManifest(t, "drive/", entry(notesKey, idNotes, body, false))

	// The endpoint refuses the connection at once, which is the shape of an
	// operator pointing the move at a store that is not there.
	out, _, code := command(t, "-manifest", path, "-bucket", "arca-test",
		"-endpoint", "http://127.0.0.1:1", "-path-style", "-prefix", "drive/", "-concurrency", "1")
	if code != exitRefused {
		t.Fatalf("the command exited %d", code)
	}
	if !strings.Contains(out, "failed") || !strings.Contains(out, notesKey) {
		t.Errorf("the report does not name the key:\n%s", out)
	}
}

func TestACleanMoveExitsZeroAndSaysTheBytesHold(t *testing.T) {
	body := []byte("the bytes of one note")
	b := newBucket(t, map[string][]byte{notesKey: body})
	line := entry(notesKey, idNotes, body, false)
	path := writeManifest(t, "drive/", line)

	var out bytes.Buffer
	o := options{manifest: path, bucket: "arca-test", prefix: prefix, concurrency: 4}
	if err := move(t.Context(), o, prefix, &manifest.Manifest{Prefix: prefix, Entries: []manifest.Entry{line}, Complete: true}, b, &out); err != nil {
		t.Fatalf("move: %v", err)
	}
	for _, says := range []string{"copied", path, "arca-test", "the move holds", "criterion 4"} {
		if !strings.Contains(out.String(), says) {
			t.Errorf("the report holds no %q:\n%s", says, out.String())
		}
	}
}

func TestAMoveThatIsNotCleanIsAnError(t *testing.T) {
	body := []byte("the bytes of one note")
	b := newBucket(t, nil)
	line := entry(notesKey, idNotes, body, false)

	var out bytes.Buffer
	o := options{manifest: "manifest.tsv", bucket: "arca-test", prefix: prefix, concurrency: 1}
	err := move(t.Context(), o, prefix, &manifest.Manifest{Prefix: prefix, Entries: []manifest.Entry{line}, Complete: true}, b, &out)
	if err == nil {
		t.Fatal("a move with a failed key answered no error")
	}
	if !strings.Contains(err.Error(), "not clean") {
		t.Errorf("the error is %v", err)
	}
	if !strings.Contains(out.String(), "the routes do not switch") {
		t.Errorf("the report does not say what to do:\n%s", out.String())
	}
}

func TestThePrefixIsNormalizedTheWayAKeyReadsIt(t *testing.T) {
	for raw, want := range map[string]string{
		"drive":   "drive/",
		"drive/":  "drive/",
		"/drive/": "drive/",
		"":        "",
		"/":       "",
	} {
		if got := normalizePrefix(raw); got != want {
			t.Errorf("normalizePrefix(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestTheCommandReadsItsCredentialsFromTheEnvironment(t *testing.T) {
	// A secret on a command line is a secret in a shell history, so the two
	// variables the server reads are the two the move reads.
	t.Setenv(AccessKeyVar, "key")
	t.Setenv(SecretKeyVar, "secret")
	t.Setenv(BucketPrefixVar, "drive/")
	if os.Getenv(AccessKeyVar) == "" || os.Getenv(SecretKeyVar) == "" {
		t.Fatal("the variables are not what the command reads")
	}
}
