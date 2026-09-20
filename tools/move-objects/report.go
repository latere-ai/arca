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
	// VerifyBytes, VerifyMax and VerifySample are what the run was told to
	// read back. They print in the header, so the counts below are read
	// against the check that produced them.
	VerifyBytes  bool
	VerifyMax    int64
	VerifySample int
	Outcomes     []Outcome
	// DeleteSources says the delete pass of the sunset ran, so a report with
	// no deletion in it still says the pass read every line.
	DeleteSources bool
	// Deletions is what became of every source key, one per outcome, in the
	// manifest's order.
	Deletions []Deletion
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

// OK reports whether the run holds: nothing mismatched, nothing failed, and
// no source the store would not delete. A run with no keys at all is clean,
// because a manifest of no keys is a copy that had no bytes to move.
func (r *Report) OK() bool {
	return r.Count(Mismatched) == 0 && r.Count(Failed) == 0 && r.Sources(DeleteFailed) == 0
}

// Sources answers how many source keys met one fate.
func (r *Report) Sources(f Fate) int {
	n := 0
	for _, d := range r.Deletions {
		if d.Fate == f {
			n++
		}
	}
	return n
}

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
	_, _ = fmt.Fprintf(head, "byte check\t%s\n", r.byteCheck())
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
	r.writeSources(&b)
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

// byteCheck is the header's line for what this run reads back, so a clean
// report is never read without the check that produced it.
func (r *Report) byteCheck() string {
	if !r.VerifyBytes {
		return "off; -verify-bytes reads each destination back and digests it"
	}
	return fmt.Sprintf("on, every object at or under %d bytes and %d%% of the larger ones",
		r.VerifyMax, r.VerifySample)
}

// Proved answers how many keys that arrived carry one proof.
func (r *Report) Proved(p Proof) int {
	n := 0
	for _, o := range r.Outcomes {
		if (o.State == Copied || o.State == Skipped) && o.Verified == p {
			n++
		}
	}
	return n
}

// writeNotes prints how far each key was proved and what the store could not
// do, with a count each, so the weakest proof never reads as the strongest and
// nothing reads as a silent success.
func (r *Report) writeNotes(b *strings.Builder) {
	unstamped := 0
	for _, o := range r.Outcomes {
		if o.Unstamped {
			unstamped++
		}
	}
	onBytes, onLabel, onSize := r.Proved(OnBytes), r.Proved(OnLabel), r.Proved(OnSize)
	if onBytes+onLabel+onSize+unstamped == 0 {
		return
	}
	fmt.Fprintf(b, "\nnoted\n")
	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	if onBytes > 0 {
		_, _ = fmt.Fprintf(tw, "  verified on bytes\t%d\t%s\n", onBytes, r.bytesMeans())
	}
	if onLabel > 0 {
		_, _ = fmt.Fprintf(tw, "  verified on label\t%d\t%s\n", onLabel,
			"the store's own label for the object is the checksum the row carries")
	}
	if onSize > 0 {
		_, _ = fmt.Fprintf(tw, "  verified on size\t%d\t%s\n", onSize,
			"the size and the store's own copy are what hold: the row's checksum is a digest no store "+
				"reports, and this run did not read the object back")
	}
	if unstamped > 0 {
		_, _ = fmt.Fprintf(tw, "  publicity not stamped\t%d\t%s\n", unstamped,
			"the store holds no object ACLs; serve these through a bucket policy (spec 003)")
	}
	flush(tw)
}

// bytesMeans is the byte proof's sentence, which a dry run reads in the
// conditional because it read no destination.
func (r *Report) bytesMeans() string {
	if r.DryRun {
		return "the run will read these destinations back and digest them against the manifest"
	}
	return "read back from the store and digested to the checksum the row carries"
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

// writeSources names what became of every source key, one line each, and
// counts them. It prints only where the delete pass ran: a move that deletes
// nothing has no sources section to read.
func (r *Report) writeSources(b *strings.Builder) {
	if !r.DeleteSources {
		return
	}
	fmt.Fprintf(b, "\nsources\n")
	for _, d := range r.Deletions {
		switch d.Fate {
		case Deleted:
			fmt.Fprintf(b, "  %s %s\n", r.deleteWord(), d.Source)
		case Kept:
			fmt.Fprintf(b, "  kept %s: %s\n", d.Source, d.Why)
		default:
			fmt.Fprintf(b, "  not deleted %s: %s\n", d.Source, d.Why)
		}
	}
	fmt.Fprintf(b, "\n%s\n", r.sourceCount())
}

// deleteWord is what one deleted line reads as, which a rehearsal reads in
// the conditional because it called nothing.
func (r *Report) deleteWord() string {
	if r.DryRun {
		return "would delete"
	}
	return "deleted"
}

// sourceCount is the line an operator counts the delete pass by.
func (r *Report) sourceCount() string {
	deleted, kept := r.Sources(Deleted), r.Sources(Kept)
	if r.DryRun {
		return fmt.Sprintf("%s would be deleted, %d kept", sourceKeys(deleted), kept)
	}
	count := fmt.Sprintf("%s deleted, %d kept, %d the store would not delete",
		sourceKeys(deleted), kept, r.Sources(DeleteFailed))
	if onSize := r.deletedOnSize(); onSize > 0 {
		count += fmt.Sprintf("; %d deleted on the size alone", onSize)
	}
	return count
}

// deletedOnSize answers how many deleted sources had a destination the run
// proved on its length and not on its bytes, which is what the byte check
// could not reach: an object above its threshold that the sample did not
// pick, or a row whose checksum is the composite label of a multipart upload.
func (r *Report) deletedOnSize() int {
	n := 0
	for _, d := range r.Deletions {
		if d.Fate == Deleted && d.Proof == OnSize {
			n++
		}
	}
	return n
}

// sourceKeys is a count of source keys a sentence can carry.
func sourceKeys(n int) string {
	if n == 1 {
		return "1 source key"
	}
	return fmt.Sprintf("%d source keys", n)
}

// verdict is the line an operator reads last: whether the routes may switch
// on this run.
func (r *Report) verdict() string {
	switch {
	case !r.OK():
		v := "the move is not clean. Every key above is one to answer for, and the routes do not switch " +
			"until a rerun is clean; a rerun skips what is already there."
		if r.DeleteSources {
			v += " No source of a key above was deleted."
		}
		return v
	case r.DryRun:
		if r.DeleteSources {
			return "the dry run holds. Run the same command without -dry-run to move the objects and " +
				"delete the source keys it names."
		}
		return "the dry run holds. Run the same command without -dry-run to move the objects."
	default:
		v := "the move holds: every key the manifest names is readable at its object id's key. " +
			"This is the bytes half of criterion 4 of spec 019; the row copy's report is the other."
		if r.DeleteSources {
			v += " The source keys it deleted are gone, which is step 5 of the sunset."
		}
		return v
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
