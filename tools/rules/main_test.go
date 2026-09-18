// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"latere.ai/x/arca/internal/metrics"
)

// TestTheCommittedManifestIsCurrent: the file at deploy/base/prometheusrule.yaml
// equals a fresh rendering, so an alert edited in the YAML and left out of the
// table, or added to the table without running `make rules`, does not reach
// main.
func TestTheCommittedManifestIsCurrent(t *testing.T) {
	committed := filepath.Join("..", "..", Path)
	got, err := os.ReadFile(committed)
	if err != nil {
		t.Fatalf("%s: %v; run `make rules` and commit the result", Path, err)
	}
	if !bytes.Equal(got, Render()) {
		t.Errorf("%s is not what the alert table renders; run `make rules` and commit the result", Path)
	}
}

// arcaMetric matches every name of this core's own in an expression. A
// metric another exporter publishes does not carry the prefix.
var arcaMetric = regexp.MustCompile(`arca_[a-z0-9_]+`)

// selected matches one series selector: the metric and the matchers in it.
var selected = regexp.MustCompile(`(arca_[a-z0-9_]+)\{([^}]*)\}`)

// selector matches one label matcher inside a series selector.
var selector = regexp.MustCompile(`(\w+)\s*=~?\s*"([^"]*)"`)

// suffixes are what a histogram's series carry beyond the metric's own name.
var suffixes = []string{"_bucket", "_sum", "_count"}

// TestAlertsNameKnownMetrics is criterion 9 of spec 018: every arca_ metric
// and every label an alert names is in the table internal/metrics registers,
// so an alert cannot watch a series nothing publishes.
func TestAlertsNameKnownMetrics(t *testing.T) {
	rows := map[string]metrics.Metric{}
	for _, m := range metrics.Table() {
		rows[m.Name] = m
	}
	for _, a := range Alerts() {
		for _, token := range arcaMetric.FindAllString(a.Expr, -1) {
			name := token
			for _, s := range suffixes {
				if base, cut := strings.CutSuffix(name, s); cut {
					name = base
					break
				}
			}
			row, known := rows[name]
			if !known {
				t.Errorf("%s names the metric %q, which internal/metrics does not register", a.Name, name)
				continue
			}
			if token != name && row.Kind != metrics.Histogram {
				t.Errorf("%s reads %q, and %s is not a histogram", a.Name, token, name)
			}
		}
		checkSelectors(t, a, rows)
	}
}

// checkSelectors holds every label matcher of one alert to the vocabularies
// of the metric it selects on. A selector on a name without the prefix is
// another exporter's and not this table's to check.
func checkSelectors(t *testing.T, a Alert, rows map[string]metrics.Metric) {
	t.Helper()
	for _, m := range selected.FindAllStringSubmatch(a.Expr, -1) {
		name, inside := strings.TrimSuffix(m[1], "_bucket"), m[2]
		row, known := rows[name]
		if !known {
			continue
		}
		for _, pair := range selector.FindAllStringSubmatch(inside, -1) {
			label, value := pair[1], pair[2]
			if label == "le" {
				continue
			}
			idx := slices.IndexFunc(row.Labels, func(l metrics.Label) bool { return l.Name == label })
			if idx < 0 {
				t.Errorf("%s selects %s{%s=...}, and that metric has no such label", a.Name, name, label)
				continue
			}
			if values := row.Labels[idx].Values; len(values) > 0 && !slices.Contains(values, value) {
				t.Errorf("%s selects %s{%s=%q}, and the vocabulary is %v", a.Name, name, label, value, values)
			}
		}
	}
}

// TestEveryRowOfTheSpecTableIsAnAlert holds the table to spec 018's alert
// table: the same names, in the same order.
func TestEveryRowOfTheSpecTableIsAnAlert(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "specs", "018-observability.md"))
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("(?m)^\\| `(Arca[A-Za-z]+)` \\|")
	var want []string
	for _, m := range row.FindAllStringSubmatch(string(data), -1) {
		want = append(want, m[1])
	}
	var have []string
	for _, a := range Alerts() {
		have = append(have, a.Name)
	}
	if !slices.Equal(want, have) {
		t.Errorf("the spec names\n %v\nand the table names\n %v", want, have)
	}
}

// TestTheDocumentIsWhatPromtoolReads: the rules document is a rules file and
// not a Kubernetes object, which is the whole reason this tool exists.
func TestTheDocumentIsWhatPromtoolReads(t *testing.T) {
	document := Document()
	if !strings.HasPrefix(document, "groups:\n") {
		t.Errorf("the document starts %q", document[:min(20, len(document))])
	}
	if strings.Contains(document, "apiVersion") || strings.Contains(document, "kind: PrometheusRule") {
		t.Error("the Kubernetes object leaked into the rules document")
	}
	var back struct {
		Groups []struct {
			Name  string `yaml:"name"`
			Rules []struct {
				Alert       string            `yaml:"alert"`
				Expr        string            `yaml:"expr"`
				For         string            `yaml:"for"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal([]byte(document), &back); err != nil {
		t.Fatalf("the document is not YAML: %v", err)
	}
	if len(back.Groups) != 1 || back.Groups[0].Name != Group {
		t.Fatalf("the document holds %d groups", len(back.Groups))
	}
	if len(back.Groups[0].Rules) != len(Alerts()) {
		t.Fatalf("%d rules for %d alerts", len(back.Groups[0].Rules), len(Alerts()))
	}
	for i, r := range back.Groups[0].Rules {
		a := Alerts()[i]
		if r.Alert != a.Name || r.Expr != a.Expr || r.For != a.For {
			t.Errorf("rule %d reads %+v", i, r)
		}
		if r.Annotations["summary"] != a.Summary {
			t.Errorf("%s summarises itself as %q", a.Name, r.Annotations["summary"])
		}
	}
}

// TestTheManifestIsAPrometheusRule: the file an operator applies is a
// Kubernetes object with the rules inside its spec, and it carries the SPDX
// header every file in the tree does.
func TestTheManifestIsAPrometheusRule(t *testing.T) {
	rendered := string(Render())
	if !strings.HasPrefix(rendered, "# SPDX-FileCopyrightText:") {
		t.Error("the manifest carries no SPDX header")
	}
	var object struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name   string            `yaml:"name"`
			Labels map[string]string `yaml:"labels"`
		} `yaml:"metadata"`
		Spec struct {
			Groups []struct {
				Name  string           `yaml:"name"`
				Rules []map[string]any `yaml:"rules"`
			} `yaml:"groups"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &object); err != nil {
		t.Fatalf("the manifest is not YAML: %v", err)
	}
	if object.APIVersion != "monitoring.coreos.com/v1" || object.Kind != "PrometheusRule" {
		t.Errorf("the object is %s %s", object.APIVersion, object.Kind)
	}
	if object.Metadata.Name != Group || object.Metadata.Labels["app.kubernetes.io/name"] != Group {
		t.Errorf("the object is named %+v", object.Metadata)
	}
	if len(object.Spec.Groups) != 1 || len(object.Spec.Groups[0].Rules) != len(Alerts()) {
		t.Errorf("the object holds %+v", object.Spec.Groups)
	}
}

// TestWritingAndPrintingAreTheTwoShapes drives the command both ways.
func TestWritingAndPrintingAreTheTwoShapes(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := cli(nil, &out, &errOut); code != 0 {
		t.Fatalf("printing exited %d: %s", code, errOut.String())
	}
	if out.String() != Document() {
		t.Error("what was printed is not the rules document")
	}

	dir := t.TempDir()
	kept, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(kept) })
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(Path)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := cli([]string{"-write"}, &out, &errOut); code != 0 {
		t.Fatalf("writing exited %d: %s", code, errOut.String())
	}
	written, err := os.ReadFile(Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, Render()) {
		t.Error("what was written is not the rendering")
	}

	// A directory that is not there is one line on stderr and exit 1, and a
	// bad flag is exit 2.
	if err := os.RemoveAll(filepath.Join(dir, "deploy")); err != nil {
		t.Fatal(err)
	}
	errOut.Reset()
	if code := cli([]string{"-write"}, &out, &errOut); code != 1 || !strings.HasPrefix(errOut.String(), "rules: ") {
		t.Errorf("a write with nowhere to write exited %d: %q", code, errOut.String())
	}
	if code := cli([]string{"-no-such-flag"}, &out, &errOut); code != 2 {
		t.Errorf("a bad flag exited %d", code)
	}
}

// TestASummaryWithAQuoteSurvivesTheRendering: the scalars are single quoted,
// which is the one YAML style that needs no reading of the value.
func TestASummaryWithAQuoteSurvivesTheRendering(t *testing.T) {
	if got := quote(`a client's own id`); got != `'a client''s own id'` {
		t.Errorf("the scalar rendered as %s", got)
	}
	var back map[string]string
	if err := yaml.Unmarshal([]byte("summary: "+quote(`it's {a="b"} > 0`)), &back); err != nil {
		t.Fatal(err)
	}
	if back["summary"] != `it's {a="b"} > 0` {
		t.Errorf("the scalar read back as %q", back["summary"])
	}
}
