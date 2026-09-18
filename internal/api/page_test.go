// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLimitIsCheckedAndNeverClamped is the change spec 013 makes from the
// service Arca replaces: a limit above the cap is invalid_field rather than
// a silent clamp, because a client asking for five thousand and receiving a
// thousand silently believes it has read everything.
func TestLimitIsCheckedAndNeverClamped(t *testing.T) {
	cases := []struct {
		query string
		want  int
		bad   bool
	}{
		{query: "", want: DefaultLimit},
		{query: "?limit=1", want: 1},
		{query: "?limit=250", want: 250},
		{query: "?limit=1000", want: MaxLimit},
		{query: "?limit=5000", bad: true},
		{query: "?limit=0", bad: true},
		{query: "?limit=-1", bad: true},
		{query: "?limit=lots", bad: true},
		{query: "?limit=", bad: false, want: DefaultLimit},
	}
	for _, c := range cases {
		t.Run("limit"+c.query, func(t *testing.T) {
			got, err := Limit(httptest.NewRequest(http.MethodGet, "/v1/trash"+c.query, nil))
			if c.bad {
				var refusal *Refusal
				if !errors.As(err, &refusal) {
					t.Fatalf("the limit was accepted as %d", got)
				}
				if refusal.Code != CodeInvalidField {
					t.Errorf("the code is %q, want %q", refusal.Code, CodeInvalidField)
				}
				if len(refusal.Fields) != 1 || refusal.Fields[0] != "limit" {
					t.Errorf("the refusal names the fields %v", refusal.Fields)
				}
				return
			}
			if err != nil {
				t.Fatalf("the limit was refused: %v", err)
			}
			if got != c.want {
				t.Errorf("the limit is %d, want %d", got, c.want)
			}
		})
	}
}

// TestCursorIsOpaque: a client sends back what it read, unchanged, and what
// it holds is the server's business.
func TestCursorIsOpaque(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/trash?cursor=01J8R4%2Fabc", nil)
	if got := Cursor(r); got != "01J8R4/abc" {
		t.Errorf("the cursor is %q", got)
	}
	if got := Cursor(httptest.NewRequest(http.MethodGet, "/v1/trash", nil)); got != "" {
		t.Errorf("the first page carries the cursor %q", got)
	}
}

// TestTheListEnvelope: entries is never null and next_cursor is present only
// when a further page exists, so its presence is the only has-more signal
// and a client stops when it is absent.
func TestTheListEnvelope(t *testing.T) {
	t.Run("an empty page", func(t *testing.T) {
		body := page(t, func(w http.ResponseWriter) { WritePage[string](w, nil, "") })
		if got, ok := body["entries"].([]any); !ok || len(got) != 0 {
			t.Errorf("entries is %v; an empty page is an empty array and never null", body["entries"])
		}
		if _, ok := body["next_cursor"]; ok {
			t.Error("the last page carries a next_cursor")
		}
		if !strings.Contains(string(raw(t, NewPage[string](nil, ""))), `"entries":[]`) {
			t.Error("an empty page does not render entries as an empty array")
		}
	})
	t.Run("a page with more behind it", func(t *testing.T) {
		body := page(t, func(w http.ResponseWriter) { WritePage(w, []string{"a", "b"}, "01J8R4") })
		got, ok := body["entries"].([]any)
		if !ok || len(got) != 2 {
			t.Fatalf("entries is %v", body["entries"])
		}
		if body["next_cursor"] != "01J8R4" {
			t.Errorf("next_cursor is %v", body["next_cursor"])
		}
	})
}

// TestPaginateIsKeyset: a query reads one row past the page, and the cursor
// is the sort key of the last row of the page, so a page is stable under a
// concurrent insert and costs the same at row one and row one million.
func TestPaginateIsKeyset(t *testing.T) {
	key := func(s string) string { return s }
	t.Run("a full page with more behind it", func(t *testing.T) {
		rows, next := Paginate([]string{"a", "b", "c"}, 2, key)
		if len(rows) != 2 || rows[1] != "b" {
			t.Fatalf("the page is %v", rows)
		}
		if next != "b" {
			t.Errorf("the cursor is %q; it is the sort key of the last row of the page", next)
		}
	})
	t.Run("the last page", func(t *testing.T) {
		rows, next := Paginate([]string{"a", "b"}, 2, key)
		if len(rows) != 2 || next != "" {
			t.Errorf("the last page is %v with the cursor %q", rows, next)
		}
	})
	t.Run("an empty page", func(t *testing.T) {
		rows, next := Paginate(nil, 100, key)
		if len(rows) != 0 || next != "" {
			t.Errorf("an empty page is %v with the cursor %q", rows, next)
		}
	})
}

// page writes one page and reads the body back.
func page(t *testing.T, write func(http.ResponseWriter)) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	write(w)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the page is not JSON: %v\n%s", err, w.Body)
	}
	if w.Code != http.StatusOK {
		t.Errorf("a page answered %d", w.Code)
	}
	return body
}

// raw renders one page as the bytes a client reads.
func raw(t *testing.T, p Page[string]) []byte {
	t.Helper()
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("the page does not marshal: %v", err)
	}
	return out
}
