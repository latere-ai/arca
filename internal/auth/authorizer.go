// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/arca/authorizer"
)

// ProbeID is the reserved resource id every authorizer denies for every
// subject and every action. It is the shared contract's, not a value of
// Arca's own: one id is what lets one check command read one answer from
// every endpoint in the family.
const ProbeID = authz.ProbeID

// ClientOptions configures the client that asks the operator's endpoint.
// URL and Token are ARCA_AUTHORIZER_URL and ARCA_AUTHORIZER_TOKEN.
type ClientOptions struct {
	URL     string
	Token   string
	HTTP    *http.Client
	Timeout time.Duration
	Now     func() time.Time
	// Observe receives every call's result and its duration in seconds, for
	// the metric of spec 018. Optional.
	Observe func(result string, seconds float64)
}

// NewClient builds the authorizer client. It sends nothing. The cache of
// spec 006, the one retry, the five second timeout and the failure rules are
// the shared package's, so an operator who wrote one endpoint for a sibling
// core runs it for Arca by pointing a second URL at it, and Arca reimplements
// none of it. The vocabulary rides along, so an action outside Arca's table
// costs no round trip.
func NewClient(o ClientOptions) (*authz.Client, error) {
	return authz.NewClient(authz.Options{
		URL: o.URL, Token: o.Token, HTTP: o.HTTP, Timeout: o.Timeout, Now: o.Now,
		Observe: o.Observe, Vocabulary: authorizer.Vocabulary(),
	})
}

// Decision is one answer as arcad reads it: how long it may be held, the
// ceilings it granted, and the filter it narrowed a list with. The allow
// itself is not a field, because a Decision only exists for one.
type Decision struct {
	// TTL is how long this answer may be held, which is also how long a
	// quota the answer carried binds a write (spec 010).
	TTL time.Duration
	// Limits is the answer's limits object as it came, nil when the answer
	// carried none. Spec 010 decodes quota_bytes out of it; nothing here
	// reads it.
	Limits json.RawMessage
	// Filter narrows a list action's page to the owners and labels the
	// authorizer named. The handler applies it to its own query, so a
	// selector outside the filter yields an empty page and never a 403.
	Filter *authz.Filter
}

// Authorizer asks one question per request. It is the seam: an operator's
// endpoint behind the shared client, or the owner policy, and the rest of
// arcad cannot tell which.
type Authorizer struct {
	inner authz.Authorizer
	// decided is spec 018's seam: every answer this node acted on, by
	// outcome. The source is fixed when the node is built, because which of
	// the two decides is a property of the deployment and not of a request.
	// Nil records nothing.
	decided func(outcome string)
	// grants is the table the resource's grant is resolved through, bound by
	// [Authorizer.WithGrants] and read by both modes. Nil resolves none.
	grants GrantLookup
}

// NewAuthorizer wraps whichever authorizer this deployment runs.
func NewAuthorizer(inner authz.Authorizer) *Authorizer { return &Authorizer{inner: inner} }

// The three outcomes of spec 018's arca_decisions_total. They are counted
// per decision and not per call: a decision the client answered from its
// cache is a decision this node acted on, and an endpoint that was asked is
// what arca_authorizer_seconds times.
const (
	// OutcomeAllow is an answer the handler acted on.
	OutcomeAllow = "allow"
	// OutcomeDeny is an answer that refused.
	OutcomeDeny = "deny"
	// OutcomeUnavailable is no answer at all, which is never an allow.
	OutcomeUnavailable = "unavailable"
)

// record reports one decision, when the node bound the seam.
func (a *Authorizer) record(outcome string) {
	if a.decided != nil {
		a.decided(outcome)
	}
}

// Envelope is what one call carries: the caller's subject and its claims
// verbatim, the action, the resource, and what is known about the request
// itself. Arca sends no workload member; it mints no token and reads no
// claim that would name one.
func Envelope(c Caller, info authz.Caller, action string, res authz.Resource) authz.Request {
	claims := c.Claims
	if claims == nil {
		claims = map[string]any{}
	}
	return authz.Request{
		Subject: c.Subject, Issuer: c.Issuer, Sub: c.Sub, Claims: claims,
		Action: action, Resource: res, Request: info,
	}
}

// Decide asks one question about the request on ctx and returns the answer
// for an allow. It is the seam every handler calls: the caller and the
// request block are already on the context, so a handler names the action of
// spec 006's table and the resource it touches and nothing else.
//
// A deny on the request's own action is forbidden, with the endpoint's
// reason as the developer detail and never in the user sentence; a call that
// produced no decision is authorizer_unavailable and never an allow.
func (a *Authorizer) Decide(ctx context.Context, action string, res authz.Resource) (Decision, error) {
	return a.decide(ctx, action, res, CodeForbidden)
}

// Lookup asks the question a resolve asks: may this caller use the object
// another request named. A deny is not_found, so a refused object and a
// missing one are the same answer and a request cannot be written to
// enumerate what somebody else owns. That is invariant 6 of spec 001.
func (a *Authorizer) Lookup(ctx context.Context, action string, res authz.Resource) (Decision, error) {
	return a.decide(ctx, action, res, CodeNotFound)
}

func (a *Authorizer) decide(ctx context.Context, action string, res authz.Resource, deny Code) (Decision, error) {
	// The grant the caller holds on the resource rides on the question, so
	// an endpoint reads a grantee as one (spec 006). A table that cannot
	// answer is no decision, and no decision is never an allow.
	res, err := a.granted(ctx, res)
	if err != nil {
		a.record(OutcomeUnavailable)
		return Decision{}, refuse(CodeAuthorizerUnavailable, "%s: %v", action, err)
	}
	req := Envelope(CallerFrom(ctx), RequestFrom(ctx), action, res)
	d, err := a.inner.Authorize(ctx, req)
	if err != nil {
		if _, ok := errors.AsType[*authz.UnknownAction](err); ok {
			// An action outside the vocabulary is a bug in the handler that
			// asked, not an outage, and the client caught it before the wire.
			// Nothing decided it, so nothing is recorded.
			return Decision{}, err
		}
		a.record(OutcomeUnavailable)
		return Decision{}, refuse(CodeAuthorizerUnavailable, "%s: %v", action, err)
	}
	if !d.Allow {
		a.record(OutcomeDeny)
		return Decision{}, refuse(deny, "%s: %s", action, reasonOf(d))
	}
	a.record(OutcomeAllow)
	a.mark(ctx, res)
	return Decision{TTL: d.TTL, Limits: d.Limits, Filter: d.Filter}, nil
}

// mark records an allow that neither ownership nor a covering grant
// explains, which is spec 012's definition of an administrative touch. It
// runs where the answer is read rather than in a handler, so a route added
// later is recorded without being told to be, and no handler can put the
// mark on an allow that was not one.
//
// The test is the resource's own, read after the grant step ran: the caller
// is somebody, the resource names a space, the space is not the caller's,
// and no rung was added. For a kind no grant reaches, a space or a share or
// a link or an event, there is no subtree for a grant to cover and the grant
// step adds nothing, so the absence is settled by the kind rather than by a
// lookup. Arca cannot see why its authorizer said yes and does not guess:
// what it records is that neither explanation it can check is the reason.
func (a *Authorizer) mark(ctx context.Context, res authz.Resource) {
	subject := CallerFrom(ctx).Subject
	owner := res.String("owner")
	if subject == "" || owner == "" || subject == owner {
		return
	}
	if res.String(GrantField) != "" {
		return
	}
	markAdministrative(ctx)
}

// reasonOf is the endpoint's reason, or a word when it named none, so a log
// line always says something.
func reasonOf(d authz.Decision) string {
	if d.Reason == "" {
		return "the authorizer named no reason"
	}
	return d.Reason
}

// Check sends the probe and reports an authorizer that allowed it, which is
// an endpoint that does not read the request. A deny is the one right
// answer, and every authorizer of the family gives it. It is what the
// readiness check named authorizer asks and what arcad check will ask.
func (a *Authorizer) Check(ctx context.Context) error {
	return authz.Check(ctx, a.inner, authorizer.ActionSpaceAdmin, authorizer.KindSpace)
}
