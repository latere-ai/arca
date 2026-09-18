// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/store"
)

// HeaderReferrerPolicy and NoReferrer keep a token out of the next request a
// browser makes. The token is a path segment, so a page that a link served
// would otherwise hand it to every host it fetches from (spec 015).
const (
	HeaderReferrerPolicy = "Referrer-Policy"
	NoReferrer           = "no-referrer"
)

// createLink is the body of POST /v1/shares/links.
type createLink struct {
	Owner      string `json:"owner"`
	PathPrefix string `json:"path_prefix"`
	// Kind is "link" or "public", and "link" when absent. A public grant
	// differs in two respects and no more: meta says so, and a prefix that
	// names one object marks that object public in the bucket.
	Kind string `json:"kind"`
	// Permission is read or absent. A token grant carries read, and a
	// create that asks for more is link_read_only.
	Permission string `json:"permission"`
	ExpiresAt  string `json:"expires_at"`
}

// Link is what a create answers: the grant, with the token beside it. It is
// the one shape that carries a token, and it carries it once.
type Link struct {
	Grant
	// URL is the path the token is redeemed at, so a caller pastes what it
	// reads rather than assembling a route. It is relative to the
	// installation's own base, which the caller already knows.
	URL string `json:"url"`
}

// CreateLink answers POST /v1/shares/links: mint a token grant on a subtree.
//
// The token is answered here and nowhere else. Every listing drops it, so a
// caller that loses it revokes the grant and mints another; there is no
// route that reads a token back out of the database.
func (s *Service) CreateLink(w http.ResponseWriter, r *http.Request) {
	body, err := api.Decode[createLink](w, r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	owner, err := api.Owner(r.Context(), body.Owner)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	prefix, err := cleanPrefix(body.PathPrefix)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	kind, err := tokenKind(body.Kind)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if err := readOnly(body.Permission); err != nil {
		api.WriteError(w, r, err)
		return
	}
	expires, err := s.expiry(body.ExpiresAt)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}

	res := authorizer.Link{Owner: owner, Path: prefix}.Resource()
	if _, err := s.authorizer.Decide(r.Context(), authorizer.ActionLinkCreate, res); err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}

	token := newToken()
	written, err := s.mint(r.Context(), store.Grant{
		Owner: owner, PathPrefix: prefix, GranteeKind: kind,
		Permission: string(auth.PermissionRead), Token: token,
		CreatedBy: auth.CallerFrom(r.Context()).Subject, ExpiresAt: expires,
	})
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	view := view(written)
	view.Token = token
	httpjson.Write(w, http.StatusCreated, Link{Grant: view, URL: "/v1/shares/links/" + token})
}

// mint writes one token grant, its event, and, for a public grant whose
// prefix names one object, the public flag on that object's row. The bucket
// is stamped after the transaction commits, because a transaction holds no
// bucket call (spec 004).
//
// The order is the safe one. A public flag on the row with no ACL on the
// object grants less than the caller asked for, and a failed stamp is
// answered as an unavailable store rather than swallowed; an ACL on an
// object no grant covers would be the other way round, and that is a leak.
func (s *Service) mint(ctx context.Context, g store.Grant) (store.Grant, error) {
	var written store.Grant
	var object store.File
	err := s.db.Tx(ctx, func(q store.Querier) error {
		var err error
		if written, err = s.store.Create(ctx, q, g); err != nil {
			return err
		}
		if _, err = s.ledger.Append(ctx, q, Event{
			Owner: written.Owner, Path: written.PathPrefix, Action: ActionShareCreated,
			Actor: written.CreatedBy, Detail: map[string]any{
				"share_id":     written.ID,
				"grantee_kind": written.GranteeKind,
				"permission":   written.Permission,
				"path_prefix":  written.PathPrefix,
			},
		}); err != nil {
			return err
		}
		object, err = s.publicRow(ctx, q, written, true)
		return err
	})
	if err != nil {
		return store.Grant{}, fmt.Errorf("shares: mint the link: %w", err)
	}
	if err := s.stamp(ctx, object, true); err != nil {
		return store.Grant{}, err
	}
	return written, nil
}

// publicRow marks the object a public grant's prefix names, and answers the
// row it wrote. A prefix that names a subtree rather than one object, or
// names nothing yet, marks nothing: publicity is a property of an object's
// row, derived from a grant and never from a path (spec 008).
func (s *Service) publicRow(ctx context.Context, q store.Querier, g store.Grant, public bool) (store.File, error) {
	if g.GranteeKind != store.GranteePublic {
		return store.File{}, nil
	}
	f, err := s.store.MarkPublic(ctx, q, g.Owner, g.PathPrefix, public)
	if missing(err) {
		return store.File{}, nil
	}
	if err != nil {
		return store.File{}, err
	}
	return f, nil
}

// stamp puts the bucket's public-read ACL on the object a public grant
// covers, or takes it off. An object no row named, and a build with no
// bucket bound, stamp nothing.
func (s *Service) stamp(ctx context.Context, f store.File, public bool) error {
	if s.publisher == nil || f.ObjectID == "" {
		return nil
	}
	if err := s.publisher.SetPublic(ctx, f.ObjectID.Key(s.bucketPrefix), public); err != nil {
		return api.Refuse(api.CodeStorageUnavailable, "the object's public flag was not stamped")
	}
	return nil
}

// ListLinks answers GET /v1/shares/links: the token grants on a space.
//
// The Link kind has no list action in the vocabulary of spec 006, so a
// listing of a space's links asks link.read for the prefix it lists, which
// is the whole space.
func (s *Service) ListLinks(w http.ResponseWriter, r *http.Request) {
	owner, page, err := s.listing(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	res := authorizer.Link{Owner: owner}.Resource()
	if _, err := s.authorizer.Decide(r.Context(), authorizer.ActionLinkRead, res); err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	rows, err := s.store.ListTokens(r.Context(), s.db.Querier(), owner, page.cursor, page.limit+1)
	if err != nil {
		api.WriteError(w, r, fmt.Errorf("shares: the links of %q: %w", owner, err))
		return
	}
	trimmed, next := api.Paginate(rows, page.limit, func(g store.Grant) string { return g.ID })
	api.WritePage(w, views(trimmed), next)
}

// RevokeLink answers DELETE /v1/shares/links/{id}.
//
// The next redemption of the token is a not-found, with no sweep in
// between: the token read filters the status in its own statement.
func (s *Service) RevokeLink(w http.ResponseWriter, r *http.Request) {
	g, err := s.tokenGrant(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	res := authorizer.Link{ID: g.ID, Owner: g.Owner, Path: g.PathPrefix}.Resource()
	if _, err := s.authorizer.Lookup(r.Context(), authorizer.ActionLinkRevoke, res); err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	if err := s.withdraw(r.Context(), g); err != nil {
		api.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// withdraw clears the publicity a public grant stamped and then revokes the
// grant. The ACL goes first: a bucket that will not answer leaves the grant
// standing and the caller retrying, which grants no more than before, while
// revoking first and failing to clear would leave an object readable that no
// grant covers.
func (s *Service) withdraw(ctx context.Context, g store.Grant) error {
	if g.GranteeKind == store.GranteePublic {
		var object store.File
		err := s.db.Tx(ctx, func(q store.Querier) error {
			var err error
			object, err = s.publicRow(ctx, q, g, false)
			return err
		})
		if err != nil {
			return fmt.Errorf("shares: clear the public flag: %w", err)
		}
		if err := s.stamp(ctx, object, false); err != nil {
			return err
		}
	}
	return s.revoke(ctx, g)
}

// tokenGrant reads the grant a link route's {id} names, and refuses a
// subject grant with the answer a missing one gets: the two kinds ask two
// actions, and a grant reached through the wrong route is not one that route
// knows.
func (s *Service) tokenGrant(r *http.Request) (store.Grant, error) {
	g, err := s.store.Get(r.Context(), s.db.Querier(), r.PathValue("id"))
	switch {
	case missing(err):
		return store.Grant{}, api.Refuse(api.CodeNotFound, "there is no link %q", r.PathValue("id"))
	case err != nil:
		return store.Grant{}, fmt.Errorf("shares: read the link: %w", err)
	case g.GranteeKind == store.GranteeSubject:
		return store.Grant{}, api.Refuse(api.CodeNotFound,
			"the grant %q is a subject's, and a grant is revoked at its own route", g.ID)
	}
	return g, nil
}

// meta is what a viewer needs before it fetches anything.
type meta struct {
	Kind       string `json:"kind"`
	Owner      string `json:"owner"`
	PathPrefix string `json:"path_prefix"`
}

// LinkMeta answers GET /v1/shares/links/{token}/meta.
func (s *Service) LinkMeta(w http.ResponseWriter, r *http.Request) {
	noReferrer(w)
	link, err := s.redeem(r, "")
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, meta{
		Kind: link.GranteeKind, Owner: link.Owner, PathPrefix: link.PathPrefix,
	})
}

// entry is one object of a link's listing. It is the object body of spec
// 013 without the fields a holder of a link has no use for.
type entry struct {
	Path         string `json:"path"`
	Size         int64  `json:"size"`
	Checksum     string `json:"checksum"`
	ChecksumKind string `json:"checksum_kind"`
	ContentType  string `json:"content_type"`
	Modified     string `json:"modified"`
}

// LinkList answers GET /v1/shares/links/{token}: the subtree the token
// names, paginated.
func (s *Service) LinkList(w http.ResponseWriter, r *http.Request) {
	noReferrer(w)
	page, err := pageOf(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	link, err := s.redeem(r, "")
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	rows, err := s.store.Subtree(r.Context(), s.db.Querier(), link.Owner, link.PathPrefix, page.cursor, page.limit+1)
	if err != nil {
		api.WriteError(w, r, fmt.Errorf("shares: the subtree of a link: %w", err))
		return
	}
	trimmed, next := api.Paginate(rows, page.limit, func(f store.File) string { return f.Path })
	out := make([]entry, 0, len(trimmed))
	for _, f := range trimmed {
		out = append(out, entry{
			Path: f.Path, Size: f.SizeBytes, Checksum: f.Checksum,
			ChecksumKind: string(f.ChecksumKind), ContentType: f.ContentType,
			Modified: f.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	api.WritePage(w, out, next)
}

// LinkFile answers GET /v1/shares/links/{token}/files/{path...}: one object
// under the subtree the token names.
//
// Serving is confined to the grant: only paths the prefix covers, and reads
// only, because a token grant carries read. What a read looks like once it
// is allowed is spec 005's, which this route reaches through ObjectReader.
func (s *Service) LinkFile(w http.ResponseWriter, r *http.Request) {
	noReferrer(w)
	path := r.PathValue("path")
	if err := usablePath(path); err != nil {
		// A path this server does not accept names no object, and a link
		// route says nothing more than that about what it holds.
		api.WriteError(w, r, notFound())
		return
	}
	link, err := s.redeem(r, path)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if s.reader == nil {
		api.WriteError(w, r, api.Refuse(api.CodeNotImplemented,
			"the read path of spec 005 is not bound in this build"))
		return
	}
	if err := s.reader.ServeObject(w, r, link.Owner, path); err != nil {
		api.WriteError(w, r, err)
	}
}

// redeem is the order the three token routes work in, which is fixed.
//
// The token is resolved first, and a token that names nothing live is a
// not-found before any question is asked. A path outside the grant's prefix
// is the same answer. Only then is link.read asked, with the resolved
// grant's id, its owner and the path, and an empty subject, because these
// routes carry no bearer: that is the one exception to invariant 5 of spec
// 001, and the asking half still holds.
//
// A deny is a not-found rather than a refusal, so an operator that turned
// public reading off is indistinguishable from a token that never existed.
func (s *Service) redeem(r *http.Request, path string) (store.Grant, error) {
	token := r.PathValue("token")
	if token == "" {
		return store.Grant{}, notFound()
	}
	link, err := s.store.ByToken(r.Context(), s.db.Querier(), token)
	switch {
	case missing(err):
		return store.Grant{}, notFound()
	case err != nil:
		return store.Grant{}, fmt.Errorf("shares: resolve a link token: %w", err)
	}
	asked := link.PathPrefix
	if path != "" {
		if !covers(link.PathPrefix, path) {
			return store.Grant{}, notFound()
		}
		asked = path
	}
	res := authorizer.Link{ID: link.ID, Owner: link.Owner, Path: asked}.Resource()
	if _, err := s.authorizer.Lookup(r.Context(), authorizer.ActionLinkRead, res); err != nil {
		refusal := api.FromAuth(err)
		if refusal.Code == api.CodeNotFound {
			// A deny answers exactly what an unknown token answers, developer
			// detail included. The endpoint's reason is worth reading on every
			// other route, but here it would say that this token resolved,
			// which is the one fact these three routes withhold.
			return store.Grant{}, notFound()
		}
		return store.Grant{}, refusal
	}
	return link, nil
}

// noReferrer keeps the token out of the next request a browser makes. The
// three link routes are the only ones whose URL carries a secret, so they
// are the only ones that set it.
func noReferrer(w http.ResponseWriter) { w.Header().Set(HeaderReferrerPolicy, NoReferrer) }

// notFound is the one answer every refusal of a link route gives: an unknown
// token, a revoked one, an expired one, a path outside the grant, and a
// denied link.read are indistinguishable, so a caller learns nothing about
// what the space holds by asking.
//
// It names neither the token nor the path it arrived on. The token is a path
// segment of these three routes, so a detail built from the URL would put the
// capability in the developer detail, which is the field an error log and a
// trace carry (spec 015).
func notFound() error {
	return api.Refuse(api.CodeNotFound, "this token names no link this server serves")
}

// tokenKind reads the kind a create asks for.
func tokenKind(kind string) (string, error) {
	switch kind {
	case "", store.GranteeLink:
		return store.GranteeLink, nil
	case store.GranteePublic:
		return store.GranteePublic, nil
	default:
		return "", api.Refuse(api.CodeInvalidField,
			"the kind is %q; a token grant is %q or %q", kind, store.GranteeLink, store.GranteePublic).About("kind")
	}
}

// readOnly refuses a token grant that would carry more than read. It is the
// one code of spec 013 written for this rule, because a link that granted a
// write would be a credential anyone who saw the URL could write with.
func readOnly(permission string) error {
	if permission == "" || auth.Permission(permission) == auth.PermissionRead {
		return nil
	}
	return api.Refuse(api.CodeLinkReadOnly,
		"the link asks for %q, and a token grant carries %q", permission, auth.PermissionRead).About("permission")
}
