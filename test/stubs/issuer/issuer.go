// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package issuer is the stub OIDC issuer of spec 014: the family's
// latere.ai/x/pkg/authkit/issuertest with Arca's defaults, an RS256 key set
// and the audience "arca". The stub itself, its discovery document, its key
// set, and its mint endpoint live in the shared package, so every
// repository's tiers run one issuer.
//
// It serves plain HTTP, so a server that reaches it lists it in
// ARCA_OIDC_INSECURE_ISSUERS (spec 006).
package issuer

import (
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
)

// The shared stub's types, under the names Arca's tiers use.
type (
	Server     = issuertest.Server
	Claims     = issuertest.Claims
	StringList = issuertest.StringList
	Option     = issuertest.Option
)

// DefaultAudience is the aud a minted token carries when Claims names none.
// It is Arca's audience, the value of ARCA_OIDC_AUDIENCE.
const DefaultAudience = "arca"

// The shared stub's options, re-exported.
var (
	WithIssuer = issuertest.WithIssuer
	WithKey    = issuertest.WithKey
	WithES256  = issuertest.WithES256
	WithRS256  = issuertest.WithRS256
	WithClock  = issuertest.WithClock
)

// defaults are applied before the caller's options, so a caller can still
// ask for ES256 or another audience.
func defaults(opts []Option) []Option {
	return append([]Option{issuertest.WithRS256(), issuertest.WithDefaultAudience(DefaultAudience)}, opts...)
}

// New starts a stub on a loopback listener and closes it with the test.
func New(t testing.TB, opts ...Option) *Server { return issuertest.New(t, defaults(opts)...) }

// NewHandler builds a stub without a listener, for the arca-stubs binary.
func NewHandler(opts ...Option) *Server { return issuertest.NewHandler(defaults(opts)...) }
