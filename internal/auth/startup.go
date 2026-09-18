// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"latere.ai/x/pkg/otel"
)

// Mode is which authorizer this deployment runs, for the line the node logs
// at start. An operator reads it to know whether the endpoint they
// configured was picked up.
type Mode string

const (
	// ModeAuthorizer is ARCA_AUTHORIZER_URL set: every decision is one call
	// to the operator's endpoint.
	ModeAuthorizer Mode = "authorizer"
	// ModeOwnerPolicy is ARCA_AUTHORIZER_URL unset: the built-in policy of
	// spec 006 decides, from ARCA_ADMIN_SUBJECTS, the owner of each space,
	// and the grants and links of spec 008.
	ModeOwnerPolicy Mode = "owner policy"
)

// Options is everything spec 006's variables carry, as the node hands them
// over. The field names are the variables without the ARCA_ prefix, so the
// mapping in cmd/arcad is a line per variable.
type Options struct {
	Issuers         []string
	Audience        string
	InsecureIssuers bool
	AuthorizerURL   string
	AuthorizerToken string
	AdminSubjects   []string
	// Grants and Links are the two tables the owner policy reads, which
	// arrive with spec 008. Both nil is an installation that has issued
	// neither, which is every installation until then.
	Grants GrantLookup
	Links  LinkResolver
	// HTTP sends the discovery reads and the authorizer's calls. An
	// instrumented client is built when none is given, so the authorizer's
	// transport carries the request's trace and every call is a span.
	HTTP *http.Client
	// Now is the clock the decision cache runs on.
	Now func() time.Time
	// Observe receives every authorizer call's result and duration, for the
	// metric of spec 018. Optional.
	Observe func(result string, seconds float64)
}

// Identity is what the node holds once spec 006 is wired: who a caller is,
// and who decides what that caller may do.
type Identity struct {
	Verifier   *Verifier
	Authorizer *Authorizer
	Mode       Mode
}

// Start builds the two and refuses to start on anything spec 006 says is a
// start-up failure: an issuer that does not answer or names an unusable
// scheme, and an authorizer URL with no bearer. Every refusal names the
// variable, so a deployment is fixed rather than guessed at.
//
// It is the only place the two are built, so a node, a test tier and the
// check command all get the same wiring.
func Start(ctx context.Context, o Options) (*Identity, error) {
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: DefaultFetchTimeout, Transport: otel.Transport(nil)}
	}
	verifier, err := NewVerifier(ctx, VerifierOptions{
		Issuers: o.Issuers, Audience: o.Audience, Insecure: o.InsecureIssuers, HTTP: client,
	})
	if err != nil {
		return nil, err
	}
	id := &Identity{Verifier: verifier, Mode: ModeOwnerPolicy}
	if o.AuthorizerURL == "" {
		// ARCA_ADMIN_SUBJECTS is read here and read nowhere else; with an
		// authorizer set it is read and unused, because an administrator is
		// then whoever that endpoint says.
		id.Authorizer = NewAuthorizer(&OwnerPolicy{Admins: o.AdminSubjects, Grants: o.Grants, Links: o.Links})
		return id, nil
	}
	if o.AuthorizerToken == "" {
		return nil, errors.New("ARCA_AUTHORIZER_TOKEN is unset while ARCA_AUTHORIZER_URL is set, and the endpoint requires a bearer")
	}
	asking, err := NewClient(ClientOptions{
		URL: o.AuthorizerURL, Token: o.AuthorizerToken, HTTP: client, Now: o.Now, Observe: o.Observe,
	})
	if err != nil {
		return nil, err
	}
	id.Authorizer = NewAuthorizer(asking)
	id.Mode = ModeAuthorizer
	return id, nil
}
