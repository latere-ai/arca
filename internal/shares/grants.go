// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/store"
)

// Grant is what a caller reads: one row of the grants table, rendered.
//
// The token is absent from every shape but the one a create answers, which
// is why it is a field of this type and is set in one place. A token is a
// bearer secret, and a listing that carried one would hand the capability to
// everyone who may list (spec 015). The service Arca replaces answered the
// token in the listing of a space's shares and stripped it only from the
// grantee's own listing.
type Grant struct {
	ID          string  `json:"id"`
	Owner       string  `json:"owner"`
	PathPrefix  string  `json:"path_prefix"`
	GranteeKind string  `json:"grantee_kind"`
	Grantee     string  `json:"grantee,omitempty"`
	Permission  string  `json:"permission"`
	Status      string  `json:"status"`
	Token       string  `json:"token,omitempty"`
	ExpiresAt   *string `json:"expires_at,omitempty"`
	CreatedBy   string  `json:"created_by"`
	CreatedAt   string  `json:"created_at"`
}

// view renders one grant without its token, which is every shape but the
// answer to a create.
func view(g store.Grant) Grant {
	v := Grant{
		ID: g.ID, Owner: g.Owner, PathPrefix: g.PathPrefix, GranteeKind: g.GranteeKind,
		Grantee: g.Grantee, Permission: g.Permission, Status: g.Status,
		CreatedBy: g.CreatedBy, CreatedAt: stamp(g.CreatedAt),
	}
	if g.ExpiresAt != nil {
		expires := stamp(*g.ExpiresAt)
		v.ExpiresAt = &expires
	}
	return v
}

// views renders a page.
func views(page []store.Grant) []Grant {
	out := make([]Grant, 0, len(page))
	for _, g := range page {
		out = append(out, view(g))
	}
	return out
}

// stamp renders a time the way spec 013 answers every one: RFC 3339, in UTC.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// createGrant is the body of POST /v1/shares. There is no grantee kind
// field: this route grants to a subject, and a token grant is minted at
// POST /v1/shares/links, where the token it answers is the whole point of
// the route.
type createGrant struct {
	Owner      string `json:"owner"`
	PathPrefix string `json:"path_prefix"`
	Grantee    string `json:"grantee"`
	Permission string `json:"permission"`
	ExpiresAt  string `json:"expires_at"`
}

// CreateGrant answers POST /v1/shares.
//
// The question carries the permission being minted and the grantee, so the
// deciding side sees an attempted escalation and can refuse it. Whether a
// given manage holder may mint a given grant is not decided here (spec 008).
func (s *Service) CreateGrant(w http.ResponseWriter, r *http.Request) {
	body, err := api.DecodeBody[createGrant](r)
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
	if body.Grantee == "" {
		api.WriteError(w, r, api.Refuse(api.CodeMissingField,
			"a grant names the subject it is for").About("grantee"))
		return
	}
	permission, err := rung(body.Permission)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	expires, err := s.expiry(body.ExpiresAt)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}

	caller := auth.CallerFrom(r.Context()).Subject
	res := authorizer.Share{
		Owner: owner, Path: prefix, Grantee: body.Grantee, Permission: string(permission),
	}.Resource()
	if _, err := s.authorizer.Decide(r.Context(), authorizer.ActionShareCreate, res); err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}

	written, err := s.write(r.Context(), store.Grant{
		Owner: owner, PathPrefix: prefix, GranteeKind: store.GranteeSubject,
		Grantee: body.Grantee, Permission: string(permission),
		CreatedBy: caller, ExpiresAt: expires,
	})
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, view(written))
}

// write stores one grant and appends its event in one transaction. A grant
// that was made is a grant that was recorded.
func (s *Service) write(ctx context.Context, g store.Grant) (store.Grant, error) {
	var written store.Grant
	err := s.db.Tx(ctx, func(q store.Querier) error {
		var err error
		if written, err = s.store.Create(ctx, q, g); err != nil {
			return err
		}
		_, err = s.ledger.Append(ctx, q, Event{
			Owner: written.Owner, Path: written.PathPrefix, Action: ActionShareCreated,
			Actor: written.CreatedBy, Detail: map[string]any{
				"share_id":     written.ID,
				"grantee_kind": written.GranteeKind,
				"permission":   written.Permission,
				"path_prefix":  written.PathPrefix,
			},
		})
		return err
	})
	if err != nil {
		// The text of a store failure is dropped by the envelope of spec
		// 013: nothing above 499 names a store, a query, or a key, even in
		// the developer detail.
		return store.Grant{}, fmt.Errorf("shares: write the grant: %w", err)
	}
	return written, nil
}

// ListGrants answers GET /v1/shares: the grants on one space, narrowed to
// one subtree when the caller named one.
func (s *Service) ListGrants(w http.ResponseWriter, r *http.Request) {
	owner, page, err := s.listing(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	prefix := r.URL.Query().Get("path_prefix")
	if prefix != "" {
		if prefix, err = cleanPrefix(prefix); err != nil {
			api.WriteError(w, r, err)
			return
		}
	}
	res := authorizer.Share{Owner: owner, Path: prefix}.Resource()
	decision, err := s.authorizer.Decide(r.Context(), authorizer.ActionShareList, res)
	if err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	if !admits(decision.Filter, owner) {
		// A selector outside the filter yields an empty page and never a
		// 403 (spec 013).
		api.WritePage(w, []Grant{}, "")
		return
	}
	rows, err := s.store.ListSpace(r.Context(), s.db.Querier(), owner, prefix, page.cursor, page.limit+1)
	if err != nil {
		api.WriteError(w, r, fmt.Errorf("shares: the grants of %q: %w", owner, err))
		return
	}
	trimmed, next := api.Paginate(rows, page.limit, func(g store.Grant) string { return g.ID })
	api.WritePage(w, views(trimmed), next)
}

// GrantsWithMe answers GET /v1/shares/with-me: the grants whose grantee is
// the caller.
//
// It is one query on the grantee index and needs no membership anywhere. The
// question names the caller as both the owner and the grantee: the owner is
// what the built-in policy of spec 006 admits a caller's own question by,
// and the grantee is what an operator's endpoint narrows by.
func (s *Service) GrantsWithMe(w http.ResponseWriter, r *http.Request) {
	page, err := pageOf(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	caller := auth.CallerFrom(r.Context()).Subject
	res := authorizer.Share{Owner: caller, Grantee: caller}.Resource()
	decision, err := s.authorizer.Decide(r.Context(), authorizer.ActionShareList, res)
	if err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	rows, err := s.store.ListGrantee(r.Context(), s.db.Querier(), caller, page.cursor, page.limit+1)
	if err != nil {
		api.WriteError(w, r, fmt.Errorf("shares: what is shared with the caller: %w", err))
		return
	}
	trimmed, next := api.Paginate(rows, page.limit, func(g store.Grant) string { return g.ID })
	// The filter narrows this page by the space each grant is on, which is
	// what a filter over a list of other people's spaces can narrow.
	out := make([]Grant, 0, len(trimmed))
	for _, g := range trimmed {
		if admits(decision.Filter, g.Owner) {
			out = append(out, view(g))
		}
	}
	api.WritePage(w, out, next)
}

// ReadGrant answers GET /v1/shares/{id}.
func (s *Service) ReadGrant(w http.ResponseWriter, r *http.Request) {
	g, err := s.subjectGrant(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if _, err := s.authorizer.Lookup(r.Context(), authorizer.ActionShareRead, resourceOf(g)); err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	httpjson.Write(w, http.StatusOK, view(g))
}

// RevokeGrant answers DELETE /v1/shares/{id}.
//
// The effect is immediate and there is no grace window: the next covering
// query does not return the grant. The row is kept so an audit can see the
// grant existed, and a second revoke is the same answer, because the end
// state is the goal.
func (s *Service) RevokeGrant(w http.ResponseWriter, r *http.Request) {
	g, err := s.subjectGrant(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if _, err := s.authorizer.Lookup(r.Context(), authorizer.ActionShareRevoke, resourceOf(g)); err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	if err := s.revoke(r.Context(), g); err != nil {
		api.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// revoke marks one grant revoked and appends its event in one transaction.
func (s *Service) revoke(ctx context.Context, g store.Grant) error {
	actor := auth.CallerFrom(ctx).Subject
	err := s.db.Tx(ctx, func(q store.Querier) error {
		if _, err := s.store.Revoke(ctx, q, g.ID); err != nil {
			return err
		}
		_, err := s.ledger.Append(ctx, q, Event{
			Owner: g.Owner, Path: g.PathPrefix, Action: ActionShareRevoked,
			Actor: actor, Detail: map[string]any{
				"share_id":     g.ID,
				"grantee_kind": g.GranteeKind,
				"path_prefix":  g.PathPrefix,
			},
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("shares: revoke the grant: %w", err)
	}
	return nil
}

// subjectGrant reads the grant a {id} route names, and refuses a token grant
// with the answer a missing one gets: a link is revoked at its own route,
// and the two ask different actions, so a link reached through this one is
// not a grant this route knows.
func (s *Service) subjectGrant(r *http.Request) (store.Grant, error) {
	g, err := s.store.Get(r.Context(), s.db.Querier(), r.PathValue("id"))
	switch {
	case missing(err):
		return store.Grant{}, api.Refuse(api.CodeNotFound, "there is no grant %q", r.PathValue("id"))
	case err != nil:
		return store.Grant{}, fmt.Errorf("shares: read the grant: %w", err)
	case g.GranteeKind != store.GranteeSubject:
		return store.Grant{}, api.Refuse(api.CodeNotFound, "the grant %q is a link, and a link is read at its own route", g.ID)
	}
	return g, nil
}

// resourceOf is the question a grant is asked about: every field spec 008's
// row names, so a deciding side reads the grantee and the permission of the
// grant it is deciding on.
func resourceOf(g store.Grant) authz.Resource {
	return authorizer.Share{
		ID: g.ID, Owner: g.Owner, Path: g.PathPrefix,
		Grantee: g.Grantee, Permission: g.Permission,
	}.Resource()
}

// page is the two parameters every listing reads.
type page struct {
	cursor string
	limit  int
}

// pageOf reads them.
func pageOf(r *http.Request) (page, error) {
	limit, err := api.Limit(r)
	if err != nil {
		return page{}, err
	}
	return page{cursor: api.Cursor(r), limit: limit}, nil
}

// listing reads the space and the page a listing route names.
func (s *Service) listing(r *http.Request) (string, page, error) {
	owner, err := api.OwnerOf(r)
	if err != nil {
		return "", page{}, err
	}
	p, err := pageOf(r)
	if err != nil {
		return "", page{}, err
	}
	return owner, p, nil
}

// rung reads the permission a create asks for. A string outside the ladder
// is invalid_field: the ladder is three rungs and the caller named a fourth.
func rung(permission string) (auth.Permission, error) {
	switch p := auth.Permission(permission); p {
	case auth.PermissionRead, auth.PermissionWrite, auth.PermissionManage:
		return p, nil
	case auth.PermissionNone:
		return "", api.Refuse(api.CodeMissingField, "a grant carries a permission").About("permission")
	default:
		return "", api.Refuse(api.CodeInvalidField,
			"the permission is %q; the ladder is %q, %q, %q", permission,
			auth.PermissionRead, auth.PermissionWrite, auth.PermissionManage).About("permission")
	}
}

// expiry reads the moment a grant stops counting. An absent value is a grant
// with no expiry; a moment already past is refused, because a grant that
// grants nothing from the instant it is written is a caller's mistake and
// not a state worth storing.
func (s *Service) expiry(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, api.Refuse(api.CodeInvalidField,
			"expires_at is %q, and a time is RFC 3339", raw).About("expires_at")
	}
	if !at.After(s.now()) {
		return nil, api.Refuse(api.CodeInvalidField,
			"expires_at is %q, which is already past", raw).About("expires_at")
	}
	return &at, nil
}

// admits reports whether the filter an authorizer answered a list action
// with covers one space. A filter that names no owner narrows nothing.
func admits(f *authz.Filter, owner string) bool {
	return f == nil || len(f.Owners) == 0 || slices.Contains(f.Owners, owner)
}
