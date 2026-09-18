// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// spec answers the path of spec 018 from this file's own location.
//
// It is read through runtime.Caller and not through a relative path, because
// the tempdir gate runs the suite from an empty directory and a test that
// opened "../../specs" would pass there by not finding the file.
func spec(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test binary carries no source position")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "specs", "018-observability.md")
}

// row is one row of spec 018's metric table as the document spells it.
type row struct {
	name    string
	kind    string
	bounds  int
	first   float64
	last    float64
	labels  []string
	vocabs  map[string][]string
	lineNum int
}

var (
	tableRow  = regexp.MustCompile("^\\| `(arca_[a-z0-9_]+)` \\| ([^|]*?) \\|([^|]*)\\|")
	histogram = regexp.MustCompile(`^histogram, ([0-9.]+) (\w+) to ([0-9.]+) (\w+), (\d+) bounds$`)
	quoted    = regexp.MustCompile("`([^`]+)`")
)

// units is every unit spec 018's bound ranges are written in, as the number
// of the metric's own unit: seconds for a duration, bytes for a size.
var units = map[string]float64{
	"ms": 0.001, "s": 1, "min": 60,
	"MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40,
}

// readSpec parses the metric table out of spec 018.
func readSpec(t *testing.T) []row {
	t.Helper()
	data, err := os.ReadFile(spec(t))
	if err != nil {
		t.Fatalf("spec 018 is not readable: %v", err)
	}
	var rows []row
	for i, line := range strings.Split(string(data), "\n") {
		m := tableRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		r := row{name: m[1], kind: m[2], lineNum: i + 1, vocabs: map[string][]string{}}
		if h := histogram.FindStringSubmatch(m[2]); h != nil {
			r.kind = "histogram"
			r.bounds = number(t, h[5])
			r.first = scale(t, h[1], h[2])
			r.last = scale(t, h[3], h[4])
		}
		r.labels, r.vocabs = labels(m[3])
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		t.Fatal("spec 018 holds no metric table")
	}
	return rows
}

func number(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("%q is not a count: %v", s, err)
	}
	return n
}

// scale reads one bound of a range, in the unit the spec wrote it in.
func scale(t *testing.T, value, unit string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(value, 64)
	if err != nil {
		t.Fatalf("%q is not a bound: %v", value, err)
	}
	factor, ok := units[unit]
	if !ok {
		t.Fatalf("%q is a unit this test does not know", unit)
	}
	return v * factor
}

// labels reads the label cell: the backticked names at the top level are the
// labels, and a parenthetical holding nothing but backticked words is that
// label's vocabulary. A parenthetical written as prose names a vocabulary
// another package owns, and the tests below hold those equal instead.
func labels(cell string) ([]string, map[string][]string) {
	var names []string
	vocabs := map[string][]string{}
	depth, start := 0, 0
	for i := 0; i < len(cell); i++ {
		switch cell[i] {
		case '(':
			if depth == 0 {
				start = i + 1
			}
			depth++
		case ')':
			depth--
			if depth == 0 && len(names) > 0 {
				if v, ok := vocabulary(cell[start:i]); ok {
					vocabs[names[len(names)-1]] = v
				}
			}
		case '`':
			if depth > 0 {
				continue
			}
			end := strings.IndexByte(cell[i+1:], '`')
			if end < 0 {
				return names, vocabs
			}
			names = append(names, cell[i+1:i+1+end])
			i += end + 1
		}
	}
	return names, vocabs
}

// vocabulary reads a parenthetical as a list of values, and reports false
// for one written as prose.
func vocabulary(inner string) ([]string, bool) {
	var out []string
	for part := range strings.SplitSeq(inner, ",") {
		part = strings.TrimSpace(part)
		m := quoted.FindStringSubmatch(part)
		if m == nil || m[0] != part {
			return nil, false
		}
		out = append(out, m[1])
	}
	return out, len(out) > 0
}

// TestMetricsTable is criterion 1 of spec 018: the registry carries exactly
// the table of that spec, with the same names in the same order, the same
// kind, the same labels, the same closed vocabularies, and histogram bounds
// spanning the range the spec published.
func TestMetricsTable(t *testing.T) {
	want := readSpec(t)
	got := Table()
	if len(want) != len(got) {
		t.Fatalf("the spec names %d metrics and the registry holds %d: %v vs %v",
			len(want), len(got), names(want), Names())
	}
	kinds := map[Kind]string{Counter: "counter", Histogram: "histogram", Gauge: "gauge"}
	for i, w := range want {
		g := got[i]
		if w.name != g.Name {
			t.Errorf("row %d: the spec names %s and the table names %s", i+1, w.name, g.Name)
			continue
		}
		if kinds[g.Kind] != w.kind {
			t.Errorf("%s: the spec says %q and the table registers a %s", w.name, w.kind, kinds[g.Kind])
		}
		if w.kind == "histogram" {
			checkBounds(t, w, g)
		} else if len(g.Buckets) > 0 {
			t.Errorf("%s is not a histogram and carries bounds", w.name)
		}
		checkLabels(t, w, g)
		if g.Help == "" {
			t.Errorf("%s carries no help text", w.name)
		}
	}
}

// checkBounds holds a histogram to the range and the count its row names.
func checkBounds(t *testing.T, w row, g Metric) {
	t.Helper()
	if len(g.Buckets) != w.bounds {
		t.Errorf("%s: the spec names %d bounds and the table has %d", w.name, w.bounds, len(g.Buckets))
		return
	}
	if g.Buckets[0] != w.first {
		t.Errorf("%s: the spec starts at %g and the table at %g", w.name, w.first, g.Buckets[0])
	}
	if last := g.Buckets[len(g.Buckets)-1]; last != w.last {
		t.Errorf("%s: the spec ends at %g and the table at %g", w.name, w.last, last)
	}
	if !slices.IsSorted(g.Buckets) {
		t.Errorf("%s: the bounds are not ascending: %v", w.name, g.Buckets)
	}
}

// checkLabels holds a row's labels and its closed vocabularies to the spec.
func checkLabels(t *testing.T, w row, g Metric) {
	t.Helper()
	var have []string
	for _, l := range g.Labels {
		have = append(have, l.Name)
	}
	if !slices.Equal(w.labels, have) {
		t.Errorf("%s: the spec labels it %v and the table labels it %v", w.name, w.labels, have)
		return
	}
	for _, l := range g.Labels {
		want, spelled := w.vocabs[l.Name]
		if !spelled {
			continue
		}
		if !slices.Equal(want, l.Values) {
			t.Errorf("%s{%s}: the spec names %v and the table names %v", w.name, l.Name, want, l.Values)
		}
	}
}

func names(rows []row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.name
	}
	return out
}

// TestFindingKindsAreTheSpecsThirteen is the other half of criterion 1 for
// the one vocabulary spec 018 writes as a paragraph rather than in a cell.
// The thirteenth member is lease_expired, which spec 010 asked for while
// building pass 3 and this spec absorbed.
func TestFindingKindsAreTheSpecsThirteen(t *testing.T) {
	data, err := os.ReadFile(spec(t))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	const opening = "`arca_reaper_findings_total`'s `kind` is a closed vocabulary"
	start := strings.Index(text, opening)
	if start < 0 {
		t.Fatal("spec 018 no longer spells the finding vocabulary")
	}
	// The vocabulary is the list between "can find:" and the sentence that
	// closes it, so the prose after it may name a member again without the
	// test reading it twice.
	list := text[start:]
	const opens = "can find: "
	from := strings.Index(list, opens)
	if from < 0 {
		t.Fatal("the vocabulary paragraph no longer opens with the list")
	}
	list = list[from+len(opens):]
	to := strings.Index(list, ".")
	if to < 0 {
		t.Fatal("the list is not closed by a full stop")
	}
	var found []string
	for _, m := range quoted.FindAllStringSubmatch(list[:to], -1) {
		found = append(found, m[1])
	}
	if !slices.Equal(found, FindingKinds()) {
		t.Errorf("the spec names %v and the table names %v", found, FindingKinds())
	}
	if len(FindingKinds()) != 13 {
		t.Errorf("the vocabulary has %d members, and spec 010 made it thirteen", len(FindingKinds()))
	}
	if !slices.Contains(FindingKinds(), "lease_expired") {
		t.Error("lease_expired is not in the vocabulary")
	}
}

// TestEveryClosedVocabularyHasAZeroSeries is criterion 1's second sentence:
// right after start-up, with nothing recorded, the exposition carries a zero
// series for every combination of every closed vocabulary, and no series at
// all for a label whose values are not known before the first recording.
func TestEveryClosedVocabularyHasAZeroSeries(t *testing.T) {
	text := expose(t, nil)
	for _, m := range Table() {
		if !strings.Contains(text, m.Name) {
			t.Errorf("%s is not in the exposition", m.Name)
			continue
		}
		for _, set := range combinations(m.Labels) {
			for name, value := range set {
				if !strings.Contains(text, fmt.Sprintf("%s=%q", name, value)) {
					t.Errorf("%s{%s=%q} has no series before the first event", m.Name, name, value)
				}
			}
		}
	}
	// route is the one vocabulary nothing can seed: the mux pattern is not
	// known before a request matches, so no series carries it yet.
	if strings.Contains(text, "route=") {
		t.Error("a route series exists before any request was served")
	}
}
