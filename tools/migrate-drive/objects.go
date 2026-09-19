// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"

	"latere.ai/x/arca/object"
	"latere.ai/x/arca/tools/internal/manifest"
)

// The names the report counts a manifest decision under.
const (
	// NoteTwoDescriptions is one key two rows describe differently. Drive
	// overwrote a path in place without changing its key, so a file and the
	// version it superseded can name one key with two sizes. The live row is
	// what the bytes are now, and the count is here because a key the move
	// verifies on a stale size would read as a mismatch on a healthy copy.
	NoteTwoDescriptions = "keys two rows describe with different bytes"
	// NoteNoObjectYet is a key no object lies at: the destination of an open
	// multipart upload, whose parts are invisible to a listing until the
	// upload completes (spec 003). It carries an object id like any other
	// key, and it is not on the manifest, because the move would Head it and
	// find nothing to copy.
	NoteNoObjectYet = "sessions whose parts stay at Drive's key, off the manifest"
)

// Objects reads every key the source holds bytes at, mints the object id each
// one becomes, and records what the rows say about its bytes.
//
// It runs in the preflight, before a row is written, so the manifest is
// decided before the copy commits anything: every id the copy hands out is an
// id the manifest already names, and a run killed part way through the tables
// leaves a manifest that describes what it would have written rather than
// half of it.
//
// One key can be named by a live file and by a version it superseded, because
// Drive overwrote a path in place without minting a new key. The live row
// wins, since it describes the bytes the key holds now, and the query orders
// the live rows first so the first sight of a key is the one that counts.
func Objects(ctx context.Context, r *Run) error {
	return each(ctx, r.Source, `
		SELECT storage_key, size_bytes, checksum, is_public, live FROM (
			SELECT storage_key, size_bytes, checksum, is_public, true AS live FROM files
			UNION ALL
			SELECT storage_key, size_bytes, checksum, false, false FROM file_versions
		) o ORDER BY storage_key, live DESC, size_bytes, checksum`,
		func(rows Rows) error {
			var key, checksum string
			var size int64
			var public, live bool
			if err := rows.Scan(&key, &size, &checksum, &public, &live); err != nil {
				return err
			}
			r.record(key, size, checksum, public, live)
			return nil
		})
}

// record puts one row's description of a key into the manifest, minting the
// key's object id the first time it is seen.
func (r *Run) record(key string, size int64, checksum string, public, live bool) {
	e := r.entry(key)
	if e.bytes && (e.Size != size || e.Checksum != checksum) {
		r.Report.Note("files", NoteTwoDescriptions)
	}
	if !e.bytes || (live && !e.live) {
		e.Size, e.Checksum = size, checksum
	}
	e.bytes, e.live = true, e.live || live
	e.Public = e.Public || public
}

// entry answers the manifest line one key becomes, minting the object id the
// first time the key is seen.
//
// Nothing in a Drive key is an id. The key is drive/<owner>/<path>, with a
// random suffix on a versioned write, so there is no id in it to keep and the
// copy mints one. The manifest is what keeps the pair, and tools/move-objects
// is what makes the bytes reachable at the id.
func (r *Run) entry(key string) *manifestLine {
	if e, held := r.objects[key]; held {
		return e
	}
	e := &manifestLine{Key: key, ID: object.NewID()}
	r.objects[key] = e
	r.order = append(r.order, key)
	return e
}

// manifestLine is one line of the manifest plus what the run needs to decide
// it. The two flags never reach the file: they are how the run picks which of
// two rows describes the bytes, and whether any row describes bytes at all.
type manifestLine struct {
	manifest.Entry

	// bytes says a row that names stored bytes named this key.
	bytes bool
	// live says a live file row named it.
	live bool
}

// objectID answers the object id one Drive key becomes. A key the object pass
// did not see is an open upload's destination, which carries an id like any
// other key and no bytes to move.
func (r *Run) objectID(storageKey string) object.ID { return r.entry(storageKey).ID }

// ManifestEntries is the manifest, in the order the keys were read, which is
// the source's key order and therefore the same on every run.
//
// A key with no object behind it is left out: the parts of an open multipart
// upload are invisible to a listing, so a move that read such a line would
// Head the source and find nothing.
func (r *Run) ManifestEntries() []manifest.Entry {
	entries := make([]manifest.Entry, 0, len(r.order))
	for _, key := range r.order {
		if e := r.objects[key]; e.bytes {
			entries = append(entries, e.Entry)
		}
	}
	return entries
}

// WriteManifest writes the manifest body where -manifest names it, before the
// copy commits a table. A dry run writes none: it commits nothing, so there
// is no id it handed out for a manifest to be the record of.
func WriteManifest(r *Run) error {
	r.Report.ManifestPath, r.Report.ManifestKeys = r.Manifest, 0
	if r.Manifest == "" {
		return nil
	}
	entries := r.ManifestEntries()
	r.Report.ManifestKeys = len(entries)
	if r.DryRun {
		return nil
	}
	if err := manifest.WriteFile(r.Manifest, r.Prefix, entries); err != nil {
		return fmt.Errorf("migrate-drive: %w", err)
	}
	return nil
}

// CompleteManifest marks the manifest finished, which is what the move reads
// as permission to write objects. It runs only after the report verifies:
// a copy the operator may not switch routes on is a copy whose bytes must not
// move either.
func CompleteManifest(r *Run) error {
	if r.Manifest == "" || r.DryRun || !r.Report.OK() {
		return nil
	}
	if err := manifest.Complete(r.Manifest, r.Report.ManifestKeys); err != nil {
		return fmt.Errorf("migrate-drive: %w", err)
	}
	r.Report.ManifestComplete = true
	return nil
}
