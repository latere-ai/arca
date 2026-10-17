// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/store"
)

// MaxQueryBytes is the longest text a search by name takes (spec 029). A
// name is one path segment, and what a person types to find one is a few
// words of it; the bound keeps a statement's pattern small.
const MaxQueryBytes = 200

// The two parameters of spec 029 on a listing.
const (
	paramQuery  = "q"
	paramRecent = "recent"
)

// find is what a listing asks beyond the plain page: the text a name holds,
// empty for none, and whether the answer is the newest files.
type find struct {
	text   string
	recent bool
}

// findOf reads ?q= and ?recent= off a listing request. A text that is empty
// or only white space is no search, and one that is not is matched as sent.
// recent answers one page, so a cursor beside it is refused rather than
// ignored: a client that sent one expects a page after it.
func findOf(r *http.Request) (find, error) {
	recent, err := flag(r, paramRecent)
	if err != nil {
		return find{}, err
	}
	text := r.URL.Query().Get(paramQuery)
	switch {
	case len(text) > MaxQueryBytes:
		return find{}, api.Refuse(api.CodeInvalidField,
			"q is %d bytes; a search takes at most %d", len(text), MaxQueryBytes).About(paramQuery)
	case !utf8.ValidString(text) || strings.ContainsRune(text, 0):
		return find{}, api.Refuse(api.CodeInvalidField,
			"q holds a byte that is not UTF-8 text or a NUL, which no name holds").About(paramQuery)
	case recent && api.Cursor(r) != "":
		return find{}, api.Refuse(api.CodeExclusiveFields,
			"recent answers the newest files in one page and takes no cursor").About(paramRecent, "cursor")
	}
	if strings.TrimSpace(text) == "" {
		text = ""
	}
	return find{text: text, recent: recent}, nil
}

// found answers a listing narrowed by spec 029, under the answer the
// listing already read: a search is paged on the path as the plain listing
// is, and the newest files are one page with no cursor. Neither carries the
// folders below the prefix, which a page of matches does not imply, nor the
// space's usage, which neither question asks about.
func (s *Service) found(w http.ResponseWriter, r *http.Request, owner, prefix string, f find, limit int, omit []string) {
	ctx := r.Context()
	if f.recent {
		rows, err := s.files.RecentPrefix(ctx, s.db.Querier(), owner, prefix, f.text, limit, omit)
		if err != nil {
			api.WriteError(w, r, fault(ctx, "list the newest files of the subtree", err))
			return
		}
		write(w, http.StatusOK, Listing{Entries: s.entries(rows), Prefixes: []string{}})
		return
	}
	rows, _, err := s.files.SearchPrefix(ctx, s.db.Querier(), owner, prefix, f.text, api.Cursor(r), limit+1, omit)
	if err != nil {
		api.WriteError(w, r, fault(ctx, "search the subtree", err))
		return
	}
	page, next := api.Paginate(rows, limit, func(f store.File) string { return f.Path })
	write(w, http.StatusOK, Listing{Entries: s.entries(page), Prefixes: []string{}, NextCursor: next})
}

// entries renders the rows of a page.
func (s *Service) entries(rows []store.File) []Object {
	out := make([]Object, 0, len(rows))
	for _, f := range rows {
		out = append(out, s.Render(f))
	}
	return out
}
