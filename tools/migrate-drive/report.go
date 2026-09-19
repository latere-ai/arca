// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// Report is what a run prints: one line per table with the rows it copied, the
// rows it dropped and why, and the verdict of the verification. A dry run
// fills the same report from the same reads, so what an operator reads before
// the copy is what they read after it.
type Report struct {
	Source, Target string
	Issuer, Prefix string
	DryRun         bool

	order  []string
	tables map[string]*TableReport
}

// TableReport is one table's line.
type TableReport struct {
	Name string
	// Copied is the rows the copy wrote, or would write on a dry run.
	Copied int
	// Dropped counts the rows that did not arrive, by the name spec 019
	// removes them under.
	Dropped map[string]int
	// Noted counts what the copy changed without dropping a row, which an
	// operator reads to know the copy was not lossless in that column.
	Noted map[string]int
	// Verified is the verdict the verification wrote, and Failed says whether
	// it held. Both are empty until the verification runs.
	Verified string
	Failed   bool
}

// NewReport answers an empty report over the tables of the plan, in the order
// the plan copies them, so a table with no rows still prints a line.
func NewReport(tables []string) *Report {
	r := &Report{tables: map[string]*TableReport{}}
	for _, name := range tables {
		r.order = append(r.order, name)
		r.tables[name] = &TableReport{
			Name:    name,
			Dropped: map[string]int{},
			Noted:   map[string]int{},
		}
	}
	return r
}

// Table answers one table's line, creating it when the plan did not name it.
func (r *Report) Table(name string) *TableReport {
	t, ok := r.tables[name]
	if !ok {
		t = &TableReport{Name: name, Dropped: map[string]int{}, Noted: map[string]int{}}
		r.tables[name] = t
		r.order = append(r.order, name)
	}
	return t
}

// Copy records one row written.
func (r *Report) Copy(table string) { r.Table(table).Copied++ }

// Drop records one row that did not arrive, under the reason it did not.
func (r *Report) Drop(table, reason string) { r.Table(table).Dropped[reason]++ }

// Note records one row the copy changed in a column, under what it changed.
func (r *Report) Note(table, what string) { r.Table(table).Noted[what]++ }

// Verify records one table's verdict.
func (r *Report) Verify(table, verdict string, ok bool) {
	t := r.Table(table)
	t.Verified, t.Failed = verdict, !ok
}

// DroppedTotal is every row one table did not take.
func (t *TableReport) DroppedTotal() int {
	total := 0
	for _, n := range t.Dropped {
		total += n
	}
	return total
}

// OK reports whether every table verified. A report with no verification is
// not OK: a copy nobody checked is a copy nobody may switch the routes on.
func (r *Report) OK() bool {
	for _, name := range r.order {
		t := r.tables[name]
		if t.Failed || t.Verified == "" {
			return false
		}
	}
	return true
}

// Write prints the report. It renders once into a buffer and writes that,
// because a report half on the terminal and half not is worse than none, and
// because the alignment of the two tables is decided before anything is sent.
func (r *Report) Write(w io.Writer) {
	var b strings.Builder
	mode := "copy"
	if r.DryRun {
		mode = "dry run, nothing is written"
	}
	fmt.Fprintf(&b, "migrate-drive, the row copy of spec 019\n\n")
	head := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(head, "source\t%s\n", r.Source)
	_, _ = fmt.Fprintf(head, "target\t%s\n", r.Target)
	_, _ = fmt.Fprintf(head, "issuer\t%s\n", r.Issuer)
	_, _ = fmt.Fprintf(head, "prefix\t%s\n", r.Prefix)
	_, _ = fmt.Fprintf(head, "mode\t%s\n", mode)
	flush(head)

	fmt.Fprintf(&b, "\n")
	body := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(body, "table\tcopied\tdropped\tverified\n")
	for _, name := range r.order {
		t := r.tables[name]
		verdict := t.Verified
		if verdict == "" {
			verdict = "not verified"
		}
		_, _ = fmt.Fprintf(body, "%s\t%d\t%d\t%s\n", t.Name, t.Copied, t.DroppedTotal(), verdict)
	}
	flush(body)

	r.writeCounts(&b, "rows dropped", func(t *TableReport) map[string]int { return t.Dropped })
	r.writeCounts(&b, "noted", func(t *TableReport) map[string]int { return t.Noted })

	fmt.Fprintf(&b, "\nthe bytes\n%s\n", indent(BytesFinding))
	_, _ = io.WriteString(w, b.String())
}

// flush writes a column block out. The writer under it is the report's own
// buffer, which does not fail, so there is no error here to answer.
func flush(w *tabwriter.Writer) { _ = w.Flush() }

// writeCounts prints one block of per table counters, and nothing at all when
// every table's counter is empty.
func (r *Report) writeCounts(w *strings.Builder, title string, pick func(*TableReport) map[string]int) {
	type line struct {
		table, what string
		n           int
	}
	var lines []line
	for _, name := range r.order {
		counts := pick(r.tables[name])
		reasons := make([]string, 0, len(counts))
		for reason := range counts {
			reasons = append(reasons, reason)
		}
		sort.Strings(reasons)
		for _, reason := range reasons {
			lines = append(lines, line{name, reason, counts[reason]})
		}
	}
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n", title)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, l := range lines {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%d\n", l.table, l.what, l.n)
	}
	flush(tw)
}

// BytesFinding is what the report says about the bucket. It is printed on
// every run, because the copy cannot make it false and an operator reading a
// clean table of counts would otherwise read the cutover as done.
const BytesFinding = `Drive wrote a key from an owner and a path, drive/<owner>/<path>, and
Arca derives a key from an object id, <prefix><shard>/<id> (spec 003). A
Drive key therefore carries no id to copy, and this run mints a fresh
object id for each distinct key it read. The rows are complete and the
bytes are not reachable at the ids they now name: the bucket prefix alone
does not carry them over. Spec 019 records this as the blocking finding
for its criterion 4. Do not switch the routes on this report alone.`

// indent puts two spaces in front of every line of a block.
func indent(block string) string {
	lines := strings.Split(block, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = "  " + l
		}
	}
	return strings.Join(lines, "\n")
}
