// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/arca/object"
)

// The two ids the cases write, in the canonical text a key derives from.
const (
	idNotes = "0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a1f"
	idLogo  = "0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a20"
)

// two is the manifest every round trip case writes: a private object and a
// public one.
func two() []Entry {
	return []Entry{
		{Key: "drive/u-1/files/notes.md", ID: object.ID(idNotes), Size: 10, Checksum: "3b1f", Public: false},
		{Key: "drive/u-1/files/logo.png", ID: object.ID(idLogo), Size: 84, Checksum: "9ce2", Public: true},
	}
}

// writeTemp puts a manifest in a temporary file and answers its path.
func writeTemp(t *testing.T, prefix string, entries []Entry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.tsv")
	if err := WriteFile(path, prefix, entries); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// text writes a file of literal lines, which is how a case holds a manifest
// no writer of this package would produce.
func text(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.tsv")
	body := strings.Join(lines, "\n")
	if body != "" {
		body += "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAManifestRoundTripsThroughTheFile(t *testing.T) {
	path := writeTemp(t, "drive/", two())

	// Before the trailer the copy is not finished, and the move refuses it.
	partial, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if partial.Complete {
		t.Error("a manifest with no trailer read as complete")
	}
	if len(partial.Entries) != 2 {
		t.Fatalf("the body holds %d entries", len(partial.Entries))
	}

	if err := Complete(path, 2); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	m, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !m.Complete || m.Prefix != "drive/" {
		t.Fatalf("the manifest is %+v", m)
	}
	for i, want := range two() {
		if m.Entries[i] != want {
			t.Errorf("entry %d is %+v, want %+v", i, m.Entries[i], want)
		}
	}
	// The file is text an operator reads and counts, one line per key.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("the file holds %d lines, want the header, two keys and the trailer:\n%s", len(lines), raw)
	}
	if lines[0] != "#arca-manifest\t1\tdrive/" {
		t.Errorf("the header is %q", lines[0])
	}
	if lines[1] != "drive/u-1/files/notes.md\t"+idNotes+"\t10\t3b1f\tfalse" {
		t.Errorf("the first entry is %q", lines[1])
	}
	if lines[3] != "#complete\t2" {
		t.Errorf("the trailer is %q", lines[3])
	}
}

func TestAnEmptyManifestIsAHeaderAndATrailer(t *testing.T) {
	path := writeTemp(t, "drive/", nil)
	if err := Complete(path, 0); err != nil {
		t.Fatal(err)
	}
	m, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !m.Complete || len(m.Entries) != 0 {
		t.Fatalf("the manifest is %+v", m)
	}
}

func TestAWriterRefusesAnEntryNoReaderCouldReadBack(t *testing.T) {
	for _, c := range []struct {
		name  string
		entry Entry
		says  string
	}{
		{"a key with a tab", Entry{Key: "drive/u-1/a\tb", ID: object.ID(idNotes)}, "tab or a newline"},
		{"a key with a newline", Entry{Key: "drive/u-1/a\nb", ID: object.ID(idNotes)}, "tab or a newline"},
		{"no key", Entry{ID: object.ID(idNotes)}, "names no key"},
		{"no object id", Entry{Key: "drive/u-1/a"}, "carries no object id"},
		{"a checksum with a tab", Entry{Key: "drive/u-1/a", ID: object.ID(idNotes), Checksum: "a\tb"}, "checksum"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manifest.tsv")
			err := WriteFile(path, "drive/", []Entry{c.entry})
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("WriteFile = %v, want a refusal naming %q", err, c.says)
			}
		})
	}
}

func TestAReaderRefusesAFileThatIsNotAManifest(t *testing.T) {
	entry := "drive/u-1/files/notes.md\t" + idNotes + "\t10\t3b1f\tfalse"
	for _, c := range []struct {
		name  string
		lines []string
		says  string
	}{
		{"an empty file", nil, "the file is empty"},
		{"another format", []string{"key,id,size"}, "begins with #arca-manifest"},
		{"another version", []string{"#arca-manifest\t2\tdrive/"}, "version"},
		{"no prefix", []string{"#arca-manifest\t1\t"}, "names no bucket prefix"},
		{"a short entry", []string{"#arca-manifest\t1\tdrive/", "drive/u-1/a\t" + idNotes}, "holds 2 fields"},
		{"an id that is not one", []string{"#arca-manifest\t1\tdrive/", "drive/u-1/a\tnot-an-id\t1\tx\tfalse"}, "is not a UUID"},
		{"a size that is not one", []string{"#arca-manifest\t1\tdrive/", "drive/u-1/a\t" + idNotes + "\tten\tx\tfalse"}, "the size"},
		{"a negative size", []string{"#arca-manifest\t1\tdrive/", "drive/u-1/a\t" + idNotes + "\t-1\tx\tfalse"}, "the size"},
		{"a publicity that is neither", []string{"#arca-manifest\t1\tdrive/", "drive/u-1/a\t" + idNotes + "\t1\tx\tmaybe"}, "publicity"},
		{"one key twice", []string{"#arca-manifest\t1\tdrive/", entry, entry}, "named twice"},
		{"a trailer that counts wrong", []string{"#arca-manifest\t1\tdrive/", entry, "#complete\t7"}, "counts 7 keys"},
		{"a trailer that counts nothing", []string{"#arca-manifest\t1\tdrive/", entry, "#complete\tmany"}, "not a number"},
		{"a line after the trailer", []string{"#arca-manifest\t1\tdrive/", entry, "#complete\t1", entry}, "follows the trailer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := ReadFile(text(t, c.lines...))
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("ReadFile = %v, want a refusal naming %q", err, c.says)
			}
		})
	}
}

func TestAFileThatIsNotThereIsNamedInTheError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.tsv")
	if _, err := ReadFile(missing); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadFile of a missing file = %v", err)
	}
	if err := Complete(missing, 0); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Complete of a missing file = %v", err)
	}
	if err := WriteFile(filepath.Join(missing, "under-a-file.tsv"), "drive/", nil); err == nil {
		t.Error("a manifest was written under a path that is not a directory")
	}
}

func TestAnEntryCarriesWhatTheMoveCompares(t *testing.T) {
	path := writeTemp(t, "arca/", []Entry{{
		Key: "drive/u-1/files/big.bin", ID: object.ID(idNotes), Size: 1 << 40,
		Checksum: strings.Repeat("a", 64), Public: true,
	}})
	m, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	e := m.Entries[0]
	if e.Size != 1<<40 {
		t.Errorf("a tebibyte read back as %d", e.Size)
	}
	if !e.Public || len(e.Checksum) != 64 {
		t.Errorf("the entry is %+v", e)
	}
	// The key the move writes to is the id's, under the prefix of the header.
	if got := e.ID.Key(m.Prefix); got != "arca/1f/"+idNotes {
		t.Errorf("the destination is %q", got)
	}
}
