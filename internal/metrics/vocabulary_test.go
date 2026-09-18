// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// This file is the external test of the package, because the three
// vocabularies below belong to packages that record through this one. The
// table spells them rather than importing them, so a scrape carries every
// series before the recording package is loaded and the reaper process does
// not link the API surface; these tests are what hold the two spellings
// equal, and a word that drifts fails here rather than in production as a
// finding nobody can filter for.
package metrics_test

import (
	"slices"
	"testing"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/events"
	"latere.ai/x/arca/internal/metrics"
	"latere.ai/x/arca/internal/reaper"
)

// TestTheFindingVocabularyIsTheReconcilersOwn holds
// arca_reaper_findings_total's kind to the kinds internal/reaper reports,
// in the order spec 018 lists them.
func TestTheFindingVocabularyIsTheReconcilersOwn(t *testing.T) {
	var want []string
	for _, k := range reaper.Kinds() {
		want = append(want, string(k))
	}
	if !slices.Equal(want, metrics.FindingKinds()) {
		t.Errorf("the reconciler finds %v and the table counts %v", want, metrics.FindingKinds())
	}
}

// TestTheEventVocabularyIsTheLogsOwn holds arca_events_appended_total's kind
// to the actions of spec 010's log, which is what the spec's cell names.
func TestTheEventVocabularyIsTheLogsOwn(t *testing.T) {
	var want []string
	for _, a := range events.Actions() {
		want = append(want, string(a))
	}
	slices.Sort(want)
	have := metrics.EventKinds()
	slices.Sort(have)
	if !slices.Equal(want, have) {
		t.Errorf("the log appends %v and the table counts %v", want, have)
	}
}

// TestTheCodeVocabularyIsTheErrorTable holds arca_requests_total's code to
// every row of spec 013's error table, plus ok for a response that refused
// nothing.
func TestTheCodeVocabularyIsTheErrorTable(t *testing.T) {
	want := append([]string{"ok"}, api.Codes()...)
	slices.Sort(want)
	have := metrics.ErrorCodes()
	slices.Sort(have)
	if !slices.Equal(want, have) {
		t.Errorf("the error table names %v and the table counts %v", want, have)
	}
}
