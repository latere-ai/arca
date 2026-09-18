// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// withDrift puts the build in one drift for the test and takes it out again,
// so a test of the seam does not leave the package answering wrong for the
// tests after it.
func withDrift(t *testing.T, d Drift) {
	t.Helper()
	was := drift
	drift = d
	t.Cleanup(func() { drift = was })
}

// TestTheDriftSeamIsOffByDefault: ARCA_TEST_DRIFT is empty in every
// deployment, so the package answers the contract of spec 013 unless a
// process was started with one.
func TestTheDriftSeamIsOffByDefault(t *testing.T) {
	if CurrentDrift() != DriftNone {
		t.Fatalf("the package starts in the drift %q", CurrentDrift())
	}
	if got := driftedCode(CodeSlugTaken); got != CodeSlugTaken {
		t.Errorf("a refusal goes out as %q with no drift set", got)
	}
	if got := driftedETag("9f2c"); got != "9f2c" {
		t.Errorf("a validator goes out as %q with no drift set", got)
	}
	if !PlaneServed("files/a.txt") || !PlaneServed("workspaces/build/a.txt") {
		t.Error("a path in one of the two planes is not served")
	}
	if PlaneServed("secrets/a.txt") || PlaneServed("nothing") {
		t.Error("a path under a third prefix is served with no drift set")
	}
}

// TestParseDriftReadsTheVocabularyAndRefusesTheRest: every value the seam
// names is read, and a value no drift is written for is refused rather than
// ignored, with the vocabulary in the message so an operator reads what was
// meant.
func TestParseDriftReadsTheVocabularyAndRefusesTheRest(t *testing.T) {
	for _, raw := range []string{"", "   "} {
		got, err := ParseDrift(raw)
		if err != nil || got != DriftNone {
			t.Errorf("ParseDrift(%q) = %q, %v; an unset variable is no drift", raw, got, err)
		}
	}
	for _, want := range drifts {
		got, err := ParseDrift(" " + string(want) + " ")
		if err != nil || got != want {
			t.Errorf("ParseDrift(%q) = %q, %v", want, got, err)
		}
	}
	for _, raw := range []string{"path", "codez", "ETAG", "paths,codes"} {
		got, err := ParseDrift(raw)
		if err == nil {
			t.Errorf("ParseDrift(%q) = %q with no error; a value no drift is written for is a typo", raw, got)
			continue
		}
		for _, name := range drifts {
			if !strings.Contains(err.Error(), string(name)) {
				t.Errorf("the refusal of %q does not name %q: %v", raw, name, err)
			}
		}
	}
}

// TestSetDriftIsWhatTheNodeCalls: the value the configuration read is what
// the three chokepoints answer through.
func TestSetDriftIsWhatTheNodeCalls(t *testing.T) {
	was := CurrentDrift()
	t.Cleanup(func() { SetDrift(was) })
	SetDrift(DriftCodes)
	if CurrentDrift() != DriftCodes {
		t.Fatalf("the build is in the drift %q", CurrentDrift())
	}
	SetDrift(DriftNone)
	if CurrentDrift() != DriftNone {
		t.Fatalf("the drift was not taken off: %q", CurrentDrift())
	}
}

// TestDriftPathsTakesAFifthPrefixForAPlane: the paths drift admits a path
// under a prefix that is not one of the two planes of spec 001, which is a
// path this server does not serve. It grants no authority: the space is
// still the caller's own, and what it changes is that a path the server
// should refuse is taken.
func TestDriftPathsTakesAFifthPrefixForAPlane(t *testing.T) {
	withDrift(t, DriftPaths)
	if !PlaneServed("secrets/a.txt") {
		t.Error("the paths drift does not take a fifth prefix, so the suite has nothing to catch")
	}
	if PlaneServed("nothing") {
		t.Error("the paths drift takes a value that is not a path at all")
	}
	// The two planes still answer, so the drift is one extra prefix and not
	// a server that stopped serving.
	if !PlaneServed("files/a.txt") || !PlaneServed("workspaces/build/a.txt") {
		t.Error("the paths drift stopped the two planes answering")
	}
}

// TestDriftCodesAnswers404WhereTheTableFixes409: the codes drift sends one
// row of spec 013's table out under the wrong code and the wrong status, so
// a caller that would have learned the slug was taken learns the space is
// not there. Every other row is unchanged, which is what makes the drift one
// named answer and not a broken error table.
func TestDriftCodesAnswers404WhereTheTableFixes409(t *testing.T) {
	withDrift(t, DriftCodes)
	if got := driftedCode(CodeSlugTaken); got != CodeNotFound {
		t.Errorf("slug_taken goes out as %q, want not_found", got)
	}
	for _, code := range []string{CodeWriterHeld, CodePathTaken, CodeNotFound, CodeForbidden, CodeInvalidField} {
		if got := driftedCode(code); got != code {
			t.Errorf("%s goes out as %q under the codes drift", code, got)
		}
	}

	// The refusal reaches the wire under the drifted code with that code's
	// status and that code's sentence, so the answer is wrong and is still
	// one row of the table rather than a shape nobody wrote.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/workspaces", nil)
	WriteError(w, r, Refuse(CodeSlugTaken, "the slug %q is taken", "build"))
	if w.Code != Status(CodeNotFound) {
		t.Errorf("the refusal answered %d, want %d", w.Code, Status(CodeNotFound))
	}
	if body := w.Body.String(); !strings.Contains(body, CodeNotFound) || !strings.Contains(body, Sentence(CodeNotFound)) {
		t.Errorf("the refusal answered %s", body)
	}
}

// TestDriftETagAnswersAValidatorTheObjectNeverHeld: the etag drift makes a
// read and a write answer something other than the object's checksum, so a
// move looks like a rewrite and a conditional request compares against a
// value the object never had. It is applied where a validator reaches the
// wire, so it covers every route that answers one.
func TestDriftETagAnswersAValidatorTheObjectNeverHeld(t *testing.T) {
	withDrift(t, DriftETag)
	if got := driftedETag("9f2c"); got == "9f2c" || got == "" {
		t.Errorf("the etag drift answered %q, which is the object's own checksum", got)
	}
	// An object with no checksum still answers no validator: the drift makes
	// an answer wrong and never makes one up where there was none.
	if got := driftedETag(""); got != "" {
		t.Errorf("the etag drift invented a validator for an object with none: %q", got)
	}

	w := httptest.NewRecorder()
	SetETag(w, "9f2c")
	if got := w.Header().Get(HeaderETag); got == `"9f2c"` {
		t.Errorf("SetETag answered the object's own checksum under the etag drift: %s", got)
	}
}

// TestNoDriftGrantsAnAuthority is the rule every value of the seam is held
// to: each refuses something the server should serve or answers with the
// wrong shape, and none opens a door. The two that can be read as an answer
// are checked here, and the third refuses by construction, because it
// admits a prefix in the caller's own space and reaches no other space.
func TestNoDriftGrantsAnAuthority(t *testing.T) {
	for _, d := range drifts {
		withDrift(t, d)
		// A refusal stays a refusal: no drift turns a 4xx into a 2xx,
		// because the seam is read after the decision and never before it.
		for _, code := range []string{CodeUnauthenticated, CodeForbidden, CodeNotFound, CodeQuotaExceeded} {
			if got := driftedCode(code); Status(got) < 400 {
				t.Errorf("the %s drift answers %s as %d, which is not a refusal", d, code, Status(got))
			}
		}
	}
}
