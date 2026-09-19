// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Report is what a run prints: four counts, then every key that mismatched or
// failed with the reason it did. A dry run fills the same report from the
// same reads, so what an operator reads before the move is what they read
// after it.
type Report struct {
	Manifest string
	Prefix   string
	Bucket   string
	DryRun   bool
	Outcomes []Outcome
}

// NewReport answers an empty report over one run's inputs.
func NewReport(manifestPath, prefix, bucket string, dryRun bool) *Report {
	return &Report{Manifest: manifestPath, Prefix: prefix, Bucket: bucket, DryRun: dryRun}
}

// Count answers how many keys ended in one state.
func (r *Report) Count(s State) int {
	n := 0
	for _, o := range r.Outcomes {
		if o.State == s {
			n++
		}
	}
	return n
}

// OK reports whether the move holds: nothing mismatched and nothing failed. A
// run with no keys at all is clean, because a manifest of no keys is a copy
// that had no bytes to move.
func (r *Report) OK() bool { return r.Count(Mismatched) == 0 && r.Count(Failed) == 0 }

// Write prints the report. It renders once into a buffer and writes that,
// because a report half on the terminal and half not is worse than none.
func (r *Report) Write(w io.Writer) {
	var b strings.Builder
	mode := "move"
	if r.DryRun {
		mode = "dry run, nothing is written"
	}
	fmt.Fprintf(&b, "move-objects, the object move of spec 019\n\n")
	head := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(head, "manifest\t%s, %d keys\n", r.Manifest, len(r.Outcomes))
	_, _ = fmt.Fprintf(head, "bucket\t%s\n", r.Bucket)
	_, _ = fmt.Fprintf(head, "prefix\t%s\n", r.Prefix)
	_, _ = fmt.Fprintf(head, "mode\t%s\n", mode)
	flush(head)

	fmt.Fprintf(&b, "\n")
	body := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(body, "outcome\tkeys\tmeans\n")
	for _, line := range []struct {
		state State
		means string
	}{
		{Copied, r.copiedMeans()},
		{Skipped, "the destination already held the bytes, so this run left it alone"},
		{Mismatched, "the destination holds other bytes, and nothing was overwritten"},
		{Failed, "the store could not answer for the key"},
	} {
		_, _ = fmt.Fprintf(body, "%s\t%d\t%s\n", name(line.state), r.Count(line.state), line.means)
	}
	flush(body)

	r.writeNotes(&b)
	r.writeKeys(&b)
	fmt.Fprintf(&b, "\n%s\n", r.verdict())
	_, _ = io.WriteString(w, b.String())
}

// copiedMeans is the copied row's sentence, which a dry run reads in the
// conditional because it wrote nothing.
func (r *Report) copiedMeans() string {
	if r.DryRun {
		return "the source is there and the destination is not, so the move would copy it"
	}
	return "copied to the object id's key and read back"
}

// writeNotes prints what the run could not prove and what the store could not
// do, with a count each, so neither reads as a silent success.
func (r *Report) writeNotes(b *strings.Builder) {
	sizeOnly, unstamped := 0, 0
	for _, o := range r.Outcomes {
		if o.SizeOnly && (o.State == Copied || o.State == Skipped) {
			sizeOnly++
		}
		if o.Unstamped {
			unstamped++
		}
	}
	if sizeOnly == 0 && unstamped == 0 {
		return
	}
	fmt.Fprintf(b, "\nnoted\n")
	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	if sizeOnly > 0 {
		_, _ = fmt.Fprintf(tw, "  verified on size\t%d\t%s\n", sizeOnly,
			"the row's checksum is a digest the predecessor computed, not a label this store reports, "+
				"so the size and the store's own copy are what hold")
	}
	if unstamped > 0 {
		_, _ = fmt.Fprintf(tw, "  publicity not stamped\t%d\t%s\n", unstamped,
			"the store holds no object ACLs; serve these through a bucket policy (spec 003)")
	}
	flush(tw)
}

// writeKeys names every key an operator has to look at, and nothing else: a
// list of what worked is a list nobody reads.
func (r *Report) writeKeys(b *strings.Builder) {
	for _, s := range []State{Mismatched, Failed} {
		var lines []Outcome
		for _, o := range r.Outcomes {
			if o.State == s {
				lines = append(lines, o)
			}
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(b, "\n%s\n", name(s))
		tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
		for _, o := range lines {
			_, _ = fmt.Fprintf(tw, "  %s\t-> %s\t%s\n", o.Entry.Key, o.Destination, o.Why)
		}
		flush(tw)
	}
}

// verdict is the line an operator reads last: whether the routes may switch
// on this run.
func (r *Report) verdict() string {
	switch {
	case !r.OK():
		return "the move is not clean. Every key above is one to answer for, and the routes do not switch " +
			"until a rerun is clean; a rerun skips what is already there."
	case r.DryRun:
		return "the dry run holds. Run the same command without -dry-run to move the objects."
	default:
		return "the move holds: every key the manifest names is readable at its object id's key. " +
			"This is the bytes half of criterion 4 of spec 019; the row copy's report is the other."
	}
}

// name is the word the report prints one state under.
func name(s State) string {
	switch s {
	case Copied:
		return "copied"
	case Skipped:
		return "skipped"
	case Mismatched:
		return "mismatched"
	default:
		return "failed"
	}
}

// flush writes a column block out. The writer under it is the report's own
// buffer, which does not fail, so there is no error here to answer.
func flush(w *tabwriter.Writer) { _ = w.Flush() }
