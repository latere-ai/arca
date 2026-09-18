// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"fmt"
	"slices"
	"strings"

	"latere.ai/x/arca/object"
)

// The drift seam of spec 017. ARCA_TEST_DRIFT names one way this build is to
// answer the contract wrong, so the conformance suite can be proved to catch
// a server that does not serve spec 013 rather than only to pass one that
// does. A suite nobody has seen fail is a suite nobody has tested.
//
// The rule every value is held to: each makes the server refuse something it
// should serve or answer with the wrong shape, and none grants an authority
// the server would otherwise withhold. A seam set by accident on a real
// installation is therefore a visible defect and never a way past a
// decision. Set, it is logged at start, so an operator reading the first
// lines of a log sees it.
//
// Each value is applied at the one place that owns its answer, so a drift
// reaches every route that answer belongs to and no route has to know about
// this file. The seam is a package variable rather than a field of Options
// because the three chokepoints are package functions the whole build calls,
// and the value is a property of the process the way the configuration is.

// Drift is the one named way a build answers the contract wrong.
type Drift string

// The values ARCA_TEST_DRIFT takes. Empty is every deployment.
const (
	// DriftNone is the server as spec 013 fixes it.
	DriftNone Drift = ""
	// DriftPaths accepts a fifth path prefix that is not a plane, so a
	// grant or an object is admitted under a plane this server does not
	// serve. The paths group catches it.
	DriftPaths Drift = "paths"
	// DriftCodes answers 404 where spec 013's table fixes 409, so a caller
	// that would have learned the slug was taken learns the space is not
	// there. The errors and workspaces groups catch it.
	DriftCodes Drift = "codes"
	// DriftETag answers a fresh validator instead of the object's checksum,
	// so a move looks like a rewrite and a conditional request compares
	// against something the object never held. The files and
	// conditional-writes groups catch it.
	DriftETag Drift = "etag"
)

// drifts is every value, for the parser and for the test that walks them.
var drifts = []Drift{DriftPaths, DriftCodes, DriftETag}

// drift is what this process was started with. It is written once, before
// the listeners bind, and read on every request after that.
var drift = DriftNone

// ParseDrift reads the variable. An unknown value is refused rather than
// ignored: a value nobody wrote a drift for is a typo in the one that was
// meant, and a server that ignored it would report a suite as catching a
// drift it never ran.
func ParseDrift(raw string) (Drift, error) {
	value := Drift(strings.TrimSpace(raw))
	if value == DriftNone {
		return DriftNone, nil
	}
	if slices.Contains(drifts, value) {
		return value, nil
	}
	names := make([]string, 0, len(drifts))
	for _, known := range drifts {
		names = append(names, string(known))
	}
	return DriftNone, fmt.Errorf("is %q; it is one of %s, or unset", raw, strings.Join(names, ", "))
}

// SetDrift puts the build in one named drift. It is called once by cmd/arcad
// with what the configuration read, and by nothing else.
func SetDrift(d Drift) { drift = d }

// CurrentDrift answers what this build was started with, for the line arcad
// logs and for the tests that assert the seam is off.
func CurrentDrift() Drift { return drift }

// PlaneServed reports whether a path is rooted in a plane this build serves.
// It is the one place that question is answered, so a package above the
// frame asks rather than reading the two prefixes of spec 001 for itself,
// and the two planes stay one fact rather than one per caller.
func PlaneServed(path string) bool {
	if object.Plane(path).Valid() {
		return true
	}
	if _, _, err := object.SplitPath(path); err == nil {
		return true
	}
	// The drift: a fifth prefix is taken for a plane, which admits a path
	// under a plane this server does not serve.
	return drift == DriftPaths && strings.Contains(path, "/")
}

// driftedCode is the code a refusal goes out under. It is the refusal's own
// everywhere but under DriftCodes, where the one row spec 013 fixes at 409
// goes out at 404.
func driftedCode(code string) string {
	if drift == DriftCodes && code == CodeSlugTaken {
		return CodeNotFound
	}
	return code
}

// driftedETag is the validator a read or a write answers. It is the object's
// checksum everywhere but under DriftETag, where it is a value the object
// never held, so a move looks like a rewrite.
func driftedETag(checksum string) string {
	if drift == DriftETag && checksum != "" {
		return "drifted-" + checksum
	}
	return checksum
}
