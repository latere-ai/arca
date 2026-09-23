// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// TestAQuestionAboutAnotherSpaceIsALookup: the two halves of the Decide seam
// are chosen by whose space the question is about, so a refusal on another
// space is the answer a missing object gives and one on the caller's own is
// a refusal it can act on.
func TestAQuestionAboutAnotherSpaceIsALookup(t *testing.T) {
	h := newHarness(t)
	stranger := "https://issuer.example|someone-else"
	for _, target := range []string{
		"/v1/trash?owner=" + url.QueryEscape(stranger),
		"/v1/files/" + url.PathEscape(stranger) + "/files/plan.md?list=1",
		"/v1/files/materialize?owner=" + url.QueryEscape(stranger),
	} {
		h.asked.asked = nil
		if w := h.call(t, http.MethodGet, target, nil); w.Code != http.StatusOK {
			t.Fatalf("a read of another space answered %d: %s", w.Code, w.Body)
		}
		if len(h.asked.asked) != 1 || !h.asked.asked[0].lookup {
			t.Errorf("a question about another space asked %+v", h.asked.asked)
		}
	}
	h.asked.asked = nil
	if w := h.call(t, http.MethodGet, "/v1/trash", nil); w.Code != http.StatusOK {
		t.Fatalf("a read of the caller's own space answered %d", w.Code)
	}
	if len(h.asked.asked) != 1 || h.asked.asked[0].lookup {
		t.Errorf("a question about the caller's own space asked %+v", h.asked.asked)
	}
}

// TestTheReferenceCheckOverTheSchemaReadsTheDatabaseAndNothingElse: the
// default the node binds is the statement of spec 004, and a database that
// will not answer it is not a license to delete.
func TestTheReferenceCheckOverTheSchemaReadsTheDatabaseAndNothingElse(t *testing.T) {
	referenced, err := schemaReferences{}.Referenced(t.Context(), refusing{}, object.NewID())
	if err == nil || referenced {
		t.Fatalf("a database that would not answer read as %t, %v", referenced, err)
	}
}

// refusing is a querier that answers nothing at all.
type refusing struct{}

func (refusing) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errOutage
}

func (refusing) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, errOutage }

func (refusing) QueryRow(context.Context, string, ...any) pgx.Row { return refusingRow{} }

type refusingRow struct{}

func (refusingRow) Scan(...any) error { return errOutage }

// TestTheParametersThatShapeAReadWithoutChangingWhatItNames.
func TestTheParametersThatShapeAReadWithoutChangingWhatItNames(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "first")
	// ?permanent=0 is a delete that is not permanent, which is the soft one.
	if w := h.call(t, http.MethodDelete, h.object("files/plan.md")+"?permanent=0", nil); w.Code != http.StatusNoContent {
		t.Fatalf("a delete with ?permanent=0 answered %d: %s", w.Code, w.Body)
	}
	row, err := h.store.Get(t.Context(), nil, h.owner, "files/plan.md")
	if err != nil || row.DeletedAt == nil {
		t.Fatalf("a delete with ?permanent=0 left %+v, %v", row, err)
	}
	if got := code(t, h.call(t, http.MethodDelete, h.object("files/plan.md")+"?permanent=maybe", nil)); got != api.CodeInvalidField {
		t.Errorf("?permanent=maybe is %q", got)
	}
	if got := code(t, h.call(t, http.MethodGet, h.object("files/plan.md")+"?versions=1&limit=0", nil)); got != api.CodeInvalidField {
		t.Errorf("a version listing of no rows is %q", got)
	}
	if got := code(t, h.call(t, http.MethodGet, h.object("files/plan.md")+"?versions=1&cursor=-1", nil)); got != api.CodeInvalidField {
		t.Errorf("a version cursor below zero is %q", got)
	}
	if got := code(t, h.call(t, http.MethodGet, "/v1/trash?limit=0", nil)); got != api.CodeInvalidField {
		t.Errorf("a trash listing of no rows is %q", got)
	}
	if got := code(t, h.call(t, http.MethodGet, "/v1/stars?limit=5000", nil)); got != api.CodeInvalidField {
		t.Errorf("a star listing above the cap is %q", got)
	}
	if got := code(t, h.call(t, http.MethodGet, "/v1/trash?cursor="+url.QueryEscape("bm90LWEtY3Vyc29y"), nil)); got != api.CodeInvalidField {
		t.Errorf("a trash cursor this listing did not write is %q", got)
	}
}

// TestAnObjectWithNoMediaTypeReadsAsTheDefaultAndAPublicOneWithNoBaseIsStillPublic.
func TestAnObjectWithNoMediaTypeReadsAsTheDefaultAndAPublicOneWithNoBaseIsStillPublic(t *testing.T) {
	h := newHarness(t)
	id := object.NewID()
	if _, err := h.objects.Put(t.Context(), id.Key("arca/"), strings.NewReader("bytes"), 5,
		blob.PutOptions{ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	// A row written before a content type was recorded reads the store's.
	if err := h.store.Upsert(t.Context(), nil, store.File{
		Owner: h.owner, Path: "files/old.bin", ObjectID: id, CreatedBy: h.owner,
		SizeBytes: 5, Checksum: digest("bytes"), ChecksumKind: object.ChecksumSHA256,
		IsPublic: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Public with no base to redirect to still redirects, and the body names
	// no URL a caller could keep.
	w := h.call(t, http.MethodGet, h.object("files/old.bin"), nil)
	if w.Code != http.StatusFound {
		t.Fatalf("a public object with no base answered %d: %s", w.Code, w.Body)
	}
	head := h.call(t, http.MethodHead, h.object("files/old.bin"), nil)
	if got := head.Header().Get(api.HeaderContentType); got != blob.DefaultContentType {
		t.Errorf("a row with no media type answers %q", got)
	}
	var page Listing
	decode(t, h.call(t, http.MethodGet, h.object("files")+"?list=1", nil), &page)
	if len(page.Entries) != 1 || !page.Entries[0].IsPublic || page.Entries[0].URL != "" {
		t.Fatalf("a public object with no base is listed as %+v", page.Entries)
	}
}
