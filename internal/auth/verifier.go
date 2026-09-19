// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"latere.ai/x/pkg/authkit/jwt"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/bearer"
	"latere.ai/x/pkg/otel"
)

// DefaultAudience is ARCA_OIDC_AUDIENCE's default: the audience every token
// arcad accepts must carry.
const DefaultAudience = "arca"

// DefaultFetchTimeout bounds one start-up read of an issuer.
const DefaultFetchTimeout = 10 * time.Second

// DefaultWarmRetry is the first delay between warms after the start-up one
// failed, and DefaultWarmRetryMax the ceiling the delay doubles to. An
// issuer that comes up a second after this process does is warm about a
// second later, and one that stays away costs one read every half minute.
const (
	DefaultWarmRetry    = time.Second
	DefaultWarmRetryMax = 30 * time.Second
)

// Caller is a verified bearer. Subject is the rendered subject every owner
// field, every event, every lease holder and every authorizer request
// carries: the issuer and the sub joined by the shared package, once.
// Claims is every claim of the token, verbatim and uninterpreted; it is
// handed to the authorizer as it came and read by nothing here.
//
// The zero Caller is the anonymous one, which reaches only the three public
// link routes of spec 013: they carry no bearer, and the grant the token in
// the URL resolves to is the whole of the authorization.
type Caller struct {
	Subject string
	Issuer  string
	Sub     string
	Claims  map[string]any
}

// Anonymous reports a caller that carries no verified token.
func (c Caller) Anonymous() bool { return c.Subject == "" }

// VerifierOptions configures a Verifier. Issuers, Audience and Insecure are
// ARCA_OIDC_ISSUERS, ARCA_OIDC_AUDIENCE and ARCA_OIDC_INSECURE_ISSUERS.
type VerifierOptions struct {
	Issuers  []string
	Audience string
	// Insecure admits an http:// issuer that is not on loopback. It is for
	// the test tiers of spec 014 and for nothing an operator runs.
	Insecure bool
	HTTP     *http.Client
	CacheTTL time.Duration
	// Log receives the one line a failed start-up warm writes and the one
	// line a later warm writes when it succeeds. slog's default when nil.
	Log *slog.Logger
	// WarmRetry and WarmRetryMax bound the retry of a start-up warm that
	// failed: the first delay, and the ceiling it doubles to.
	// DefaultWarmRetry and DefaultWarmRetryMax when zero.
	WarmRetry, WarmRetryMax time.Duration
}

// Verifier verifies a bearer against the listed issuers. It is one
// validator holding the list, each issuer answering for its own tokens
// alone: trusting two issuers does not pool their keys.
//
// arcad mints no token. A sandbox that attaches to a workspace presents a
// token its own issuer minted for the audience arcad verifies; how that
// token reaches the sandbox is the sandbox runtime's and the platform's,
// which is rule R4 of the family.
type Verifier struct {
	audience  string
	issuers   []string
	validator *jwt.Validator
	// warm is the last warm's verdict, stored whole so a reader sees either
	// a failure or its absence and never half of a swap. It is what the
	// readiness check named issuers reports.
	warm atomic.Pointer[warmResult]
}

// warmResult is one warm's verdict: nil err once every listed issuer has
// answered, and the joined failure until then.
type warmResult struct{ err error }

// NewVerifier builds the verifier and warms it: every issuer's discovery
// document and key set is read once at start, so the first request a replica
// serves does not pay for a discovery.
//
// A warm that fails is not a start-up failure. The shared validator's Warm
// is a report and not a verdict, and it fetches on the first token of an
// issuer it has not read, so a replica whose issuer is not up yet serves
// everything that carries no bearer and pays for the discovery on the first
// request that does. Exiting instead would make every installation's start
// order-dependent on its issuer and would crash-loop a replica through a
// transient outage. The failure is one line in the developer register, a
// retry in the background, and a readiness check named issuers that fails
// until a warm succeeds, so the replica stays out of rotation while it
// cannot verify (spec 006).
//
// Afterwards nothing here fetches on a request path. The shared validator
// caches each set for its TTL, refreshes on an unknown kid under its own
// rate limit, and serves a stale set while a refresh fails, so an issuer
// that goes away later degrades to refusing new keys.
func NewVerifier(ctx context.Context, o VerifierOptions) (*Verifier, error) {
	if len(o.Issuers) == 0 {
		return nil, errors.New("ARCA_OIDC_ISSUERS names no issuer, and there is no anonymous access to a space")
	}
	audience := o.Audience
	if audience == "" {
		audience = DefaultAudience
	}
	v := &Verifier{audience: audience}
	for _, raw := range o.Issuers {
		iss := strings.TrimRight(strings.TrimSpace(raw), "/")
		if err := checkIssuerURL(iss, o.Insecure); err != nil {
			return nil, fmt.Errorf("ARCA_OIDC_ISSUERS: %w", err)
		}
		if slices.Contains(v.issuers, iss) {
			return nil, fmt.Errorf("ARCA_OIDC_ISSUERS lists %s twice", iss)
		}
		v.issuers = append(v.issuers, iss)
	}
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: DefaultFetchTimeout, Transport: otel.Transport(nil)}
	}
	v.validator = jwt.New(jwt.Config{
		Issuers:    v.issuers,
		Audiences:  []string{audience},
		CacheTTL:   o.CacheTTL,
		HTTPClient: client,
		// The family's age bound, one rule across the cores: a token is
		// refused once its iat is older than a day, whatever exp it
		// carries, so a caller that wants a longer-lived credential
		// re-mints it at its issuer. RequireIssuedAt makes the bound one
		// rule rather than a claim a token may drop: a token that stamps
		// no iat is refused, because an age no one can read is not an age
		// within the bound. The size bound stays the shared package's.
		MaxTokenAge:     jwt.DefaultMaxTokenAge,
		RequireIssuedAt: true,
		// A personal key is narrower than the person who holds it, and the
		// grants its holder chose ride on it as RFC 9396's
		// authorization_details. The claim says what the credential may not
		// do, so a service that reads the token and applies nothing grants
		// more than its holder asked for, silently; the shared validator
		// therefore refuses such a token with grants_unread until the
		// service promises to read it. arcad promises, and the promise is
		// kept at the decision point: the owner policy intersects its own
		// answer with the grants through authz.Restrict, and an operator's
		// endpoint on latere.ai/x/pkg/authz/server does the same with no
		// code of its own.
		ReadsGrants: true,
	})
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	if err := v.warmOnce(ctx); err != nil {
		log.WarnContext(ctx, "no issuer answered its discovery document at start; the readiness check issuers fails until one does, and the first request that carries a bearer pays for the discovery",
			"variable", "ARCA_OIDC_ISSUERS", "issuers", v.issuers, "error", err)
		go v.warmLoop(ctx, delayOr(o.WarmRetry, DefaultWarmRetry), delayOr(o.WarmRetryMax, DefaultWarmRetryMax), log)
	}
	return v, nil
}

// warmOnce reads every issuer's discovery document and key set once and
// records the verdict where Check reads it. The failure names the variable,
// because a probe that says which variable is wrong is a deployment fixed
// rather than guessed at.
func (v *Verifier) warmOnce(ctx context.Context) error {
	err := v.validator.Warm(ctx)
	if err != nil {
		err = fmt.Errorf("ARCA_OIDC_ISSUERS: %w", err)
	}
	v.warm.Store(&warmResult{err: err})
	return err
}

// warmLoop warms again until one succeeds or ctx ends. The delay doubles
// from retry to max, so an issuer that is a moment late costs a moment and
// an issuer that never arrives costs one read per max.
func (v *Verifier) warmLoop(ctx context.Context, retry, max time.Duration, log *slog.Logger) {
	for delay := retry; ; delay = min(delay*2, max) {
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if err := v.warmOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		log.InfoContext(ctx, "every issuer answered; the key sets are read and the readiness check issuers passes",
			"variable", "ARCA_OIDC_ISSUERS", "issuers", v.issuers)
		return
	}
}

// delayOr is a configured duration or the default, so a zero field means
// the default and a test sets its own.
func delayOr(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

// Check is the readiness check of spec 002 named issuers: it passes once
// every listed issuer's key set has been read and fails until then. It reads
// the last warm's verdict and reaches no network, so a probe on a schedule
// costs nothing and never outlives the probe's budget.
//
// Every listed issuer must have answered. An installation that trusts two
// and can reach one serves half its callers a 401 it cannot explain, which
// is a replica to take out of rotation rather than one to route to.
func (v *Verifier) Check(context.Context) error {
	if r := v.warm.Load(); r != nil {
		return r.err
	}
	return nil
}

// checkIssuerURL holds an issuer to a scheme whose key set cannot be
// rewritten in flight. https everywhere, http on loopback for a test that
// runs one in process, and http anywhere only where the operator set
// ARCA_OIDC_INSECURE_ISSUERS, which the test tiers of spec 014 do and a
// deployment does not.
func checkIssuerURL(iss string, insecure bool) error {
	u, err := url.Parse(iss)
	if err != nil || u.Host == "" {
		return fmt.Errorf("issuer %s is not a URL", strconv.Quote(iss))
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if insecure || isLoopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("issuer %s is http and is not on loopback; set ARCA_OIDC_INSECURE_ISSUERS to admit it", iss)
	default:
		return fmt.Errorf("issuer %s names the scheme %s, and a key set is read over https", iss, strconv.Quote(u.Scheme))
	}
}

// isLoopback reports whether a host names this machine.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Issuers lists the issuers this verifier accepts, in configured order.
func (v *Verifier) Issuers() []string { return slices.Clone(v.issuers) }

// Audience is the aud a token must carry to be accepted.
func (v *Verifier) Audience() string { return v.audience }

// Authenticator is the listed issuers' validator behind the family's
// authkit.Authenticator, which is the shape latere.ai/x/pkg/authkit's
// conformance suite drives: one verified token, one identity, and no call to
// an issuer beyond its discovery document and its key set. It is the
// validator arcad runs, not a second one built for a test.
func (v *Verifier) Authenticator() *jwt.Authenticator {
	return jwt.NewAuthenticator(v.validator)
}

// Authenticate reads the bearer of a request and verifies it. A request with
// no bearer is unauthenticated: Arca has no anonymous access and no API key,
// and the three public link routes of spec 013 do not run behind this.
func (v *Verifier) Authenticate(r *http.Request) (Caller, error) {
	raw, ok := bearer.FromRequest(r)
	if !ok || strings.TrimSpace(raw) == "" {
		return Caller{}, unauthenticated(ReasonMissing, "the request carries no bearer token")
	}
	return v.Verify(raw)
}

// Verify verifies one bearer. The token's own iss selects the key set it is
// checked against, so a token of an issuer ARCA_OIDC_ISSUERS does not list
// is refused before a signature is tried and before any key is read.
//
// A token that names no subject is refused by the shared validator as
// malformed, and authz.Subject renders the empty subject for an empty sub
// besides, so a Caller with a subject is always a token with a sub and a
// space is never addressed by the empty string.
func (v *Verifier) Verify(raw string) (Caller, error) {
	c, err := v.validator.Validate(raw)
	if err != nil {
		return Caller{}, unauthenticated(string(jwt.ReasonOf(err)), "the bearer was refused: %v", err)
	}
	// The claims go to the authorizer verbatim, so they are read off the
	// token as it came rather than off the typed claims the validator built.
	// The token is verified by the line above; nothing here trusts it before
	// that.
	var claims map[string]any
	if err := jwt.DecodePayload(raw, &claims); err != nil {
		return Caller{}, unauthenticated(string(jwt.ReasonMalformed), "the verified token does not read back as claims: %v", err)
	}
	iss := strings.TrimRight(c.Iss, "/")
	return Caller{Subject: authz.Subject(iss, c.Sub), Issuer: iss, Sub: c.Sub, Claims: claims}, nil
}
