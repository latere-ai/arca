// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"sort"
)

// sourceTables names the Drive table each step of the plan reads, which is
// what the row counts are compared against. space_usage is absent because it
// is recomputed and reads no table of its own.
var sourceTables = map[string]string{
	"subjects":              "principal_directory",
	"files":                 "files",
	"file_versions":         "file_versions",
	"stars":                 "stars",
	"upload_sessions":       "upload_sessions",
	"shares":                "shares",
	"workspaces":            "workspaces",
	"workspace_attachments": "workspace_attachments",
	"events":                "events",
}

// ChecksumSample is how many file rows the verification reads back. They are
// picked by a hash of the id rather than by the order they were written in, so
// the sample spreads over the table instead of sitting at its head.
const ChecksumSample = 100

// Verify checks what the copy wrote against what it read and writes a verdict
// per table into the report. Two counts and one sample:
//
//   - the source holds as many rows as the copy accounted for, copied plus
//     dropped, so nothing went missing between the read and the decision;
//   - the target holds as many rows as the copy wrote, so nothing was lost
//     between the decision and the commit;
//   - a sample of file rows carries the checksum and the size the source row
//     carried, so a column did not shift on the way.
//
// A mismatch is a verdict and not an error: the report prints every table and
// the caller reads Report.OK. An error here is a database that could not be
// read, which is a different thing and stops the run.
func Verify(ctx context.Context, r *Run) error {
	for _, t := range Tables() {
		if err := verifyTable(ctx, r, t.Name); err != nil {
			return fmt.Errorf("migrate-drive: verify %s: %w", t.Name, err)
		}
	}
	return nil
}

func verifyTable(ctx context.Context, r *Run, name string) error {
	if name == "space_usage" {
		return verifyUsage(ctx, r)
	}
	t := r.Report.Table(name)
	accounted := int64(t.Copied + t.DroppedTotal())
	read, err := count(ctx, r.Source, sourceTables[name])
	if err != nil {
		return err
	}
	if read != accounted {
		r.Report.Verify(name, fmt.Sprintf("the source holds %d rows and the copy accounted for %d", read, accounted), false)
		return nil
	}
	if r.DryRun {
		r.Report.Verify(name, "the source adds up, and nothing was written", true)
		return nil
	}
	written, err := count(ctx, r.Target, name)
	if err != nil {
		return err
	}
	if written != int64(t.Copied) {
		r.Report.Verify(name, fmt.Sprintf("the target holds %d rows and the copy wrote %d", written, t.Copied), false)
		return nil
	}
	if name == "files" {
		return verifyChecksums(ctx, r)
	}
	r.Report.Verify(name, "counts hold", true)
	return nil
}

// verifyUsage compares the ledger the copy wrote against the sum it made while
// reading. The ledger is the one number in the target that no source row holds,
// so the check is against the run's own arithmetic rather than a second query.
func verifyUsage(ctx context.Context, r *Run) error {
	if r.DryRun {
		r.Report.Verify("space_usage", fmt.Sprintf("recomputed over %d spaces, and nothing was written", len(r.usage)), true)
		return nil
	}
	written := map[string]int64{}
	if err := each(ctx, r.Target, `SELECT owner, bytes FROM space_usage`, func(rows Rows) error {
		var owner string
		var bytes int64
		if err := rows.Scan(&owner, &bytes); err != nil {
			return err
		}
		written[owner] = bytes
		return nil
	}); err != nil {
		return err
	}
	var wrong []string
	for owner, want := range r.usage {
		if written[owner] != want {
			wrong = append(wrong, owner)
		}
	}
	sort.Strings(wrong)
	if len(written) != len(r.usage) || len(wrong) > 0 {
		r.Report.Verify("space_usage",
			fmt.Sprintf("the ledger holds %d spaces against %d summed, and %d differ", len(written), len(r.usage), len(wrong)), false)
		return nil
	}
	r.Report.Verify("space_usage", fmt.Sprintf("recomputed over %d spaces", len(r.usage)), true)
	return nil
}

// fileCheck is the pair a sampled row is compared on.
type fileCheck struct {
	checksum string
	size     int64
}

// verifyChecksums reads a sample of file rows from both databases and compares
// the checksum and the size each carries. The id is what pairs them, which is
// what keeping every id through the copy is for.
func verifyChecksums(ctx context.Context, r *Run) error {
	want := map[string]fileCheck{}
	var ids []string
	if err := each(ctx, r.Source,
		`SELECT id::text, checksum, size_bytes FROM files ORDER BY md5(id::text) LIMIT $1`,
		func(rows Rows) error {
			var id, checksum string
			var size int64
			if err := rows.Scan(&id, &checksum, &size); err != nil {
				return err
			}
			want[id] = fileCheck{checksum, size}
			ids = append(ids, id)
			return nil
		}, ChecksumSample); err != nil {
		return err
	}
	if len(ids) == 0 {
		r.Report.Verify("files", "counts hold, and there is no row to sample", true)
		return nil
	}

	found := 0
	differ := 0
	if err := each(ctx, r.Target,
		`SELECT id::text, checksum, size_bytes FROM files WHERE id = ANY($1::uuid[])`,
		func(rows Rows) error {
			var id, checksum string
			var size int64
			if err := rows.Scan(&id, &checksum, &size); err != nil {
				return err
			}
			found++
			if w, ok := want[id]; !ok || w.checksum != checksum || w.size != size {
				differ++
			}
			return nil
		}, ids); err != nil {
		return err
	}
	if found != len(ids) || differ > 0 {
		r.Report.Verify("files",
			fmt.Sprintf("%d of %d sampled rows are in the target and %d carry another checksum or size", found, len(ids), differ), false)
		return nil
	}
	r.Report.Verify("files", fmt.Sprintf("counts hold, and %d checksums were sampled", len(ids)), true)
	return nil
}

// count is the rows one table holds. The names come from the plan and from
// sourceTables and never from a flag, so the statement is built and not bound.
func count(ctx context.Context, c Conn, table string) (int64, error) {
	var n int64
	err := each(ctx, c, `SELECT count(*) FROM `+table, func(rows Rows) error {
		return rows.Scan(&n)
	})
	return n, err
}
