// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/arca/internal/api"
)

func TestAReadAtOrBelowTheInlineSizeStreamsAndOneAboveItRedirects(t *testing.T) {
	h := newHarness(t)
	small := strings.Repeat("s", inlineBytes)
	large := strings.Repeat("l", inlineBytes+1)
	h.seed(t, "files/small.md", small)
	// The large object is above the boundary a put accepts, so the row is
	// written through the same commit a session of spec 007 uses.
	h.write(t, "files/large.md", large)

	body := h.call(t, http.MethodGet, h.object("files/small.md"), nil)
	if body.Code != http.StatusOK {
		t.Fatalf("a read at the boundary answered %d: %s", body.Code, body.Body)
	}
	if body.Body.String() != small {
		t.Fatalf("the body is %q", body.Body)
	}
	if got := body.Header().Get("Content-Length"); got != "32" {
		t.Errorf("the read answered Content-Length %q", got)
	}
	if got := body.Header().Get(api.HeaderContentType); got != "text/markdown" {
		t.Errorf("the read answered Content-Type %q", got)
	}
	if got := body.Header().Get(api.HeaderETag); got != `"`+digest(small)+`"` {
		t.Errorf("the read answered the ETag %q", got)
	}

	above := h.call(t, http.MethodGet, h.object("files/large.md"), nil)
	if above.Code != http.StatusFound || above.Header().Get("Location") == "" {
		t.Fatalf("a read above the boundary answered %d to %q", above.Code, above.Header().Get("Location"))
	}
	// Invariant 4 of spec 001 is not a default a caller waives.
	forced := h.call(t, http.MethodGet, h.object("files/large.md")+"?inline=1", nil)
	if forced.Code != http.StatusFound {
		t.Fatalf("?inline=1 above the boundary answered %d", forced.Code)
	}
	// A caller that would rather not hold a connection open asks for the
	// redirect at any size.
	redirected := h.call(t, http.MethodGet, h.object("files/small.md")+"?inline=0", nil)
	if redirected.Code != http.StatusFound {
		t.Fatalf("?inline=0 below the boundary answered %d", redirected.Code)
	}
	if got := code(t, h.call(t, http.MethodGet, h.object("files/small.md")+"?inline=maybe", nil)); got != api.CodeInvalidField {
		t.Fatalf("?inline=maybe is %q", got)
	}
	if got := code(t, h.call(t, http.MethodGet, h.object("files/small.md")+"?download=maybe", nil)); got != api.CodeInvalidField {
		t.Fatalf("?download=maybe is %q", got)
	}
	if w := h.call(t, http.MethodGet, h.object("files/large.md")+"?download=1", nil); w.Code != http.StatusFound {
		t.Fatalf("?download=1 answered %d", w.Code)
	}
}

func TestAReadAnswers304ForTheChecksumTheCallerAlreadyHolds(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "first")
	fresh := h.call(t, http.MethodGet, h.object("files/plan.md"), nil,
		api.HeaderIfNoneMatch, `"`+digest("first")+`"`)
	if fresh.Code != http.StatusNotModified || fresh.Body.Len() != 0 {
		t.Fatalf("a fresh read answered %d with %d bytes", fresh.Code, fresh.Body.Len())
	}
	stale := h.call(t, http.MethodGet, h.object("files/plan.md"), nil,
		api.HeaderIfNoneMatch, `"`+digest("second")+`"`)
	if stale.Code != http.StatusOK {
		t.Fatalf("a stale read answered %d", stale.Code)
	}
}

func TestAHeadAnswersTheHeadersAndNoBody(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "first")
	w := h.call(t, http.MethodHead, h.object("files/plan.md"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("a head answered %d: %s", w.Code, w.Body)
	}
	for header, want := range map[string]string{
		api.HeaderContentType: "text/markdown",
		"Content-Length":      "5",
		api.HeaderETag:        `"` + digest("first") + `"`,
	} {
		if got := w.Header().Get(header); got != want {
			t.Errorf("a head answered %s %q, want %q", header, got, want)
		}
	}
	if w.Header().Get("Last-Modified") == "" {
		t.Error("a head answered no Last-Modified")
	}
}

func TestAPublicObjectRedirectsToTheBaseThatCachesAndAHeadStillAnswersHeaders(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		cfg := configOf(inlineBytes, 1<<20)
		cfg.PublicCDNURL = "https://cdn.example"
		o.Config = cfg
	})
	h.seed(t, "files/public/logo.png", "png")
	row, err := h.store.Get(t.Context(), nil, h.owner, "files/public/logo.png")
	if err != nil {
		t.Fatal(err)
	}
	row.IsPublic = true
	h.store.files[key(h.owner, row.Path)] = row

	w := h.call(t, http.MethodGet, h.object("files/public/logo.png"), nil)
	want := "https://cdn.example/" + row.ObjectID.Key("arca/")
	if w.Code != http.StatusFound || w.Header().Get("Location") != want {
		t.Fatalf("a public read answered %d to %q, want %q", w.Code, w.Header().Get("Location"), want)
	}
	head := h.call(t, http.MethodHead, h.object("files/public/logo.png"), nil)
	if head.Code != http.StatusOK {
		t.Fatalf("a head of a public object answered %d", head.Code)
	}

	// Without a base to redirect to, a public object is served by the
	// ordinary presigned redirect, which is spec 003's fallback.
	plain := newHarness(t)
	plain.seed(t, "files/public/logo.png", "png")
	row, _ = plain.store.Get(t.Context(), nil, plain.owner, "files/public/logo.png")
	row.IsPublic = true
	plain.store.files[key(plain.owner, row.Path)] = row
	w = plain.call(t, http.MethodGet, plain.object("files/public/logo.png"), nil)
	if w.Code != http.StatusFound || strings.HasPrefix(w.Header().Get("Location"), "https://cdn") {
		t.Fatalf("a public read with no base answered %d to %q", w.Code, w.Header().Get("Location"))
	}
}

func TestARowWhoseBytesAreMissingIsAFaultAndNeverAMissingObject(t *testing.T) {
	h := newHarness(t)
	row := h.seed(t, "files/plan.md", "first")
	if err := h.objects.Delete(t.Context(), row.ObjectID.Key("arca/")); err != nil {
		t.Fatal(err)
	}
	w := h.call(t, http.MethodGet, h.object("files/plan.md"), nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a row whose bytes are gone answered %d: %s", w.Code, w.Body)
	}
	if got := code(t, w); got != api.CodeInternal {
		t.Fatalf("a row whose bytes are gone is %q", got)
	}
	// Nothing above 499 names a store, a query or a key.
	for _, leak := range []string{"arca/", string(row.ObjectID), "bucket", "blob"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("the answer names %q:\n%s", leak, w.Body)
		}
	}
}

func TestAReadOfAPathThatIsNotThereAndOfOneInTheTrashIsTheSameAnswer(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "first")
	if w := h.call(t, http.MethodDelete, h.object("files/plan.md"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("the trash answered %d", w.Code)
	}
	trashed := h.call(t, http.MethodGet, h.object("files/plan.md"), nil)
	absent := h.call(t, http.MethodGet, h.object("files/never.md"), nil)
	if trashed.Code != http.StatusNotFound || absent.Code != http.StatusNotFound {
		t.Fatalf("a trashed path answered %d and a missing one %d", trashed.Code, absent.Code)
	}
	if code(t, trashed) != api.CodeNotFound || code(t, absent) != api.CodeNotFound {
		t.Fatal("a trashed path and a missing one are not one answer")
	}
}

// TestAPathIsTheShapeSpec005Names holds the rule itself, which is where the
// cases that never reach a handler belong: the router cleans a path holding
// . or .. or an empty segment and answers a redirect, so those are refused
// twice and only one of the two is this package's.
func TestAPathIsTheShapeSpec005Names(t *testing.T) {
	for path, want := range map[string]string{
		"":                     api.CodeInvalidPath,
		"/files/plan.md":       api.CodeInvalidPath,
		"files/plan.md/":       api.CodeInvalidPath,
		"files//plan.md":       api.CodeInvalidPath,
		"files/./plan.md":      api.CodeInvalidPath,
		"files/../etc/passwd":  api.CodeInvalidPath,
		"files/pl\x01an.md":    api.CodeInvalidPath,
		"agents/plan.md":       api.CodeUnknownPlane,
		"memory/state.json":    api.CodeUnknownPlane,
		"plan.md":              api.CodeUnknownPlane,
		"files":                api.CodeUnknownPlane,
		"workspaces/build":     api.CodeInvalidPath,
		"files/plan.md":        "",
		"workspaces/build/a.c": "",
	} {
		plane, err := ValidatePath(path)
		if want == "" {
			if err != nil || !plane.Valid() {
				t.Errorf("%q is a path this server accepts and read as %v, %v", path, plane, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%q was accepted", path)
			continue
		}
		if got := codeOf(t, err); got != want {
			t.Errorf("%q is %q, want %q", path, got, want)
		}
	}
}

func TestAPathThatIsNotOneIsRefusedBeforeTheBucketIsReached(t *testing.T) {
	h := newHarness(t)
	for path, want := range map[string]string{
		"agents/plan.md":       api.CodeUnknownPlane,
		"workspaces/build":     api.CodeInvalidPath,
		"workspaces/build/a.c": api.CodeNotFound,
	} {
		if got := code(t, h.call(t, http.MethodGet, h.object(path), nil)); got != want {
			t.Errorf("a read of %q is %q, want %q", path, got, want)
		}
	}
	if h.bucket.Total() != 0 {
		t.Error("a path that is not one reached the bucket")
	}
}

func TestAnOwnerLongerThanASubjectOrHoldingWhatOneCannotIsRefused(t *testing.T) {
	h := newHarness(t)
	for _, owner := range []string{strings.Repeat("x", MaxSubject+1), "sub%01ject"} {
		w := h.call(t, http.MethodGet, "/v1/files/"+owner+"/files/plan.md", nil)
		if got := code(t, w); got != api.CodeInvalidField {
			t.Errorf("an owner of %q is %q", owner, got)
		}
	}
}
