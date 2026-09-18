// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package metrics

import (
	"slices"
	"time"

	"latere.ai/x/arca/internal/reaper"
)

// The reconciler's half of spec 018's table. [Set] implements
// [reaper.Metrics], the seam spec 010 declared and left for this spec to
// bind, so that package registers nothing and a process exporting nothing
// still runs its passes.

// Finding records n of one kind and outcome. The kind is the closed
// vocabulary of the table above, thirteen members, and a kind outside it is
// dropped rather than opening a series nobody can alert on.
func (s *Set) Finding(kind reaper.Kind, outcome reaper.Outcome, n int) {
	if n <= 0 {
		return
	}
	k, action := string(kind), string(outcome)
	if !slices.Contains(findingKinds, k) || !slices.Contains(findingActions, action) {
		return
	}
	s.ReaperFindings.Add(map[string]string{"kind": k, "action": action}, uint64(n))
}

// Usage observes one space's bytes, once per run.
//
// The observation goes into a distribution and into the band counts of the
// run being walked, never into a series of that space's own. A space is
// addressed by the subject "<issuer>|<sub>" and has no second identifier, so
// a space label would put a principal's name on an endpoint anyone who can
// scrape the namespace reads, and would grow a series for every principal
// that ever touched the installation. Which space is the large one is
// answered by spec 012's overview, under an administrator's token.
func (s *Set) Usage(bytes int64) {
	s.SpaceUsage.Observe(nil, float64(bytes))
	s.mu.Lock()
	defer s.mu.Unlock()
	for band, floor := range bands {
		if bytes >= floor {
			s.pending[band]++
		}
	}
}

// Run records one finished sequence and how long it took, and publishes the
// bands the sequence counted.
//
// The bands are replaced and not added to. They are a gauge over the
// installation as the last run saw it, so a run that counted fewer spaces
// than the one before it must report fewer, which is what a fresh map per
// run gives and what an increment in place would never give.
func (s *Set) Run(ok bool, took time.Duration) {
	outcome := "error"
	if ok {
		outcome = "ok"
	}
	s.ReaperRuns.Inc(map[string]string{"outcome": outcome})
	s.ReaperDuration.Observe(nil, took.Seconds())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current, s.pending = s.pending, map[string]float64{}
}
