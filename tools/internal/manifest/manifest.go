// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package manifest is the one artifact that ties a copied row to a byte.
//
// Drive built a bucket key from an owner and a path and Arca derives one from
// an object id (spec 003), so a Drive key holds no id to keep and
// migrate-drive mints one per distinct key it reads. Nothing in either
// database then says which key became which id. The manifest says it: one
// line per distinct source key, written by the row copy before it commits,
// marked complete only when the copy verifies, and read by the object move of
// spec 019, which refuses to run without it.
//
// The file is tab separated text, because an operator reads it, greps it, and
// counts it, and because a line per key is the whole of the format:
//
//	#arca-manifest	1	drive/
//	drive/u-1/files/notes.md	0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a1f	10	3b1f…	false
//	drive/u-1/files/logo.png	0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a20	84	9ce2…	true
//	#complete	2
//
// The header names the format, its version, and the bucket prefix the keys
// were read under, so a move cannot be run against another installation's
// manifest. The trailer is what says the copy finished and verified: a file
// without it is a copy that was killed or that did not verify, and the move
// refuses it.
package manifest

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"latere.ai/x/arca/object"
)

// Version is the format the header names. A reader that meets another version
// refuses rather than guessing, because a manifest is read once, by a move
// that writes objects.
const Version = "1"

// The two lines that are not entries. Both begin with a hash, which no bucket
// key does under a prefix, so a reader tells them from an entry by the first
// byte.
const (
	header  = "#arca-manifest"
	trailer = "#complete"
)

// Entry is one distinct source key: what the bucket holds it at, the object
// id the copy minted for it, and what the rows say its bytes are.
type Entry struct {
	// Key is the key the source installation wrote, under the prefix of the
	// header.
	Key string
	// ID is the object id the copy minted for the key. Its own key is
	// ID.Key(prefix), which is where the move puts the bytes.
	ID object.ID
	// Size is the length the rows carry for those bytes.
	Size int64
	// Checksum is the digest the rows carry, which is a sha256 for an object
	// written in one piece and the store's composite label for one assembled
	// from parts (spec 003).
	Checksum string
	// Public says a row marked the object public. A copy carries no ACL, so
	// the move re-stamps a public destination through SetPublic.
	Public bool
}

// Manifest is one file read back.
type Manifest struct {
	// Prefix is the bucket prefix of the header, which has to be the prefix
	// the move is run under.
	Prefix string
	// Entries are the lines, in the order the file holds them.
	Entries []Entry
	// Complete says the trailer is there: the copy finished and verified.
	// A manifest without it names an incomplete copy, and the move refuses
	// to read a byte on one.
	Complete bool
}

// WriteFile writes the entries under the prefix, without the trailer. The
// copy calls it before it writes a row, so a manifest exists for every id the
// copy is about to hand out.
func WriteFile(path, prefix string, entries []Entry) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("manifest: write %s: %w", path, err)
	}
	if err := write(f, prefix, entries); err != nil {
		_ = f.Close()
		return fmt.Errorf("manifest: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("manifest: write %s: %w", path, err)
	}
	return nil
}

// write renders the header and one line per entry.
func write(w io.Writer, prefix string, entries []Entry) error {
	b := bufio.NewWriter(w)
	if _, err := fmt.Fprintf(b, "%s\t%s\t%s\n", header, Version, prefix); err != nil {
		return err
	}
	for _, e := range entries {
		if err := e.check(); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(b, "%s\t%s\t%d\t%s\t%t\n", e.Key, e.ID, e.Size, e.Checksum, e.Public); err != nil {
			return err
		}
	}
	return b.Flush()
}

// check refuses an entry no reader could read back. A tab or a newline inside
// a key would split one line into two, and a key is whatever a predecessor
// put in the bucket, so the writer is where that is caught rather than the
// move that reads it.
func (e Entry) check() error {
	if e.Key == "" {
		return fmt.Errorf("an entry names no key")
	}
	if strings.ContainsAny(e.Key, "\t\n\r") {
		return fmt.Errorf("the key %q holds a tab or a newline, which a line cannot carry", e.Key)
	}
	if e.ID == "" {
		return fmt.Errorf("the key %q carries no object id", e.Key)
	}
	if strings.ContainsAny(e.Checksum, "\t\n\r") {
		return fmt.Errorf("the checksum of %q holds a tab or a newline", e.Key)
	}
	return nil
}

// Complete appends the trailer, which is what marks the manifest finished.
// The copy calls it when its report verifies and never before: a manifest the
// move accepts is a copy an operator may switch routes on.
func Complete(path string, count int) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("manifest: complete %s: %w", path, err)
	}
	if _, err := fmt.Fprintf(f, "%s\t%d\n", trailer, count); err != nil {
		_ = f.Close()
		return fmt.Errorf("manifest: complete %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("manifest: complete %s: %w", path, err)
	}
	return nil
}

// ReadFile reads one manifest.
func ReadFile(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("manifest: read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	m, err := Read(f)
	if err != nil {
		return nil, fmt.Errorf("manifest: read %s: %w", path, err)
	}
	return m, nil
}

// Read parses a manifest. A file whose trailer is missing reads back with
// Complete false rather than as an error: which is the caller's to refuse,
// and an operator reading a killed copy wants the lines it did write.
func Read(r io.Reader) (*Manifest, error) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), maxLine)
	m := &Manifest{}
	seen := map[string]bool{}
	for line := 1; s.Scan(); line++ {
		text := s.Text()
		switch {
		case text == "":
			continue
		case line == 1:
			prefix, err := readHeader(text)
			if err != nil {
				return nil, err
			}
			m.Prefix = prefix
		case strings.HasPrefix(text, trailer+"\t"):
			count, err := strconv.Atoi(strings.TrimPrefix(text, trailer+"\t"))
			if err != nil {
				return nil, fmt.Errorf("line %d: the trailer counts %q, which is not a number", line, strings.TrimPrefix(text, trailer+"\t"))
			}
			if count != len(m.Entries) {
				return nil, fmt.Errorf("line %d: the trailer counts %d keys and the file holds %d", line, count, len(m.Entries))
			}
			m.Complete = true
		case m.Complete:
			return nil, fmt.Errorf("line %d: a line follows the trailer, so the file was appended to after it was finished", line)
		default:
			e, err := readEntry(text)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			if seen[e.Key] {
				return nil, fmt.Errorf("line %d: the key %q is named twice, and a manifest holds one line per key", line, e.Key)
			}
			seen[e.Key] = true
			m.Entries = append(m.Entries, e)
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	if m.Prefix == "" {
		return nil, fmt.Errorf("the file is empty, and a manifest begins with %s", header)
	}
	return m, nil
}

// maxLine bounds one line. A key is a bucket key, and no store holds one past
// a kibibyte, so a line past this is a file that is not a manifest.
const maxLine = 1 << 16

// readHeader reads the first line: the format, its version, and the prefix.
func readHeader(text string) (string, error) {
	fields := strings.Split(text, "\t")
	if len(fields) != 3 || fields[0] != header {
		return "", fmt.Errorf("line 1 is %q, and a manifest begins with %s, its version, and the bucket prefix", text, header)
	}
	if fields[1] != Version {
		return "", fmt.Errorf("the manifest is version %q and this build reads version %s", fields[1], Version)
	}
	if fields[2] == "" {
		return "", fmt.Errorf("the header names no bucket prefix")
	}
	return fields[2], nil
}

// readEntry reads one line.
func readEntry(text string) (Entry, error) {
	fields := strings.Split(text, "\t")
	if len(fields) != 5 {
		return Entry{}, fmt.Errorf("the line holds %d fields, and an entry is key, object id, size, checksum, public", len(fields))
	}
	id, err := object.ParseID(fields[1])
	if err != nil {
		return Entry{}, fmt.Errorf("the key %q carries %w", fields[0], err)
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return Entry{}, fmt.Errorf("the key %q carries the size %q", fields[0], fields[2])
	}
	public, err := strconv.ParseBool(fields[4])
	if err != nil {
		return Entry{}, fmt.Errorf("the key %q carries the publicity %q, and a manifest writes true or false", fields[0], fields[4])
	}
	return Entry{Key: fields[0], ID: id, Size: size, Checksum: fields[3], Public: public}, nil
}
