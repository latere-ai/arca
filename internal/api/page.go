// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
	"strconv"

	"latere.ai/x/pkg/httpjson"
)

// The bounds of spec 013's list envelope. A page is a hundred rows unless
// the caller asked for another size, and never more than a thousand.
const (
	DefaultLimit = 100
	MaxLimit     = 1000
)

// Page is the one shape every list route answers. Entries is never null and
// is an empty array on an empty page. NextCursor is present only when a
// further page exists, so its presence is the only has-more signal and a
// client stops when it is absent.
type Page[T any] struct {
	Entries    []T    `json:"entries"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// NewPage renders one page. A nil slice becomes an empty array, because a
// client that reads null where it expected a list is a client that crashes
// on an empty space.
func NewPage[T any](entries []T, next string) Page[T] {
	if entries == nil {
		entries = []T{}
	}
	return Page[T]{Entries: entries, NextCursor: next}
}

// WritePage sends one page of a list route.
func WritePage[T any](w http.ResponseWriter, entries []T, next string) {
	httpjson.Write(w, http.StatusOK, NewPage(entries, next))
}

// Limit reads the page size a request asked for. Unset is DefaultLimit;
// anything that is not a whole number in [1, MaxLimit] is invalid_field
// rather than a silent clamp, which is the change spec 013 makes from the
// service Arca replaces: a client asking for five thousand and receiving a
// thousand silently believes it has read everything.
func Limit(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return DefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, Refuse(CodeInvalidField, "limit is %q, not a whole number", raw).About("limit")
	}
	if n < 1 || n > MaxLimit {
		return 0, Refuse(CodeInvalidField,
			"limit is %d; a page holds between 1 and %d entries", n, MaxLimit).About("limit")
	}
	return n, nil
}

// Cursor reads the cursor a client is continuing from, "" on the first page.
// It is opaque: a client sends back what it read unchanged, and what it
// holds is the server's business.
func Cursor(r *http.Request) string { return r.URL.Query().Get("cursor") }

// Paginate trims a page read one row long to its size and derives the next
// cursor from the last row returned. A query asks for limit+1 rows so that
// the presence of a further page is known without a second count, and the
// cursor is the sort key of the last row of the page, which is what makes a
// page stable under a concurrent insert and the same cost at row one and at
// row one million.
func Paginate[T any](rows []T, limit int, key func(T) string) ([]T, string) {
	if len(rows) <= limit {
		return rows, ""
	}
	page := rows[:limit]
	return page, key(page[limit-1])
}
