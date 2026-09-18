// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package config reads the typed configuration of arcad from the
// environment. Every variable spec 002 names is read once at start-up, and
// a start-up with anything missing or malformed fails with one message that
// lists every problem, so an operator fixes a deployment in one round.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"latere.ai/x/pkg/authz"
)

// Defaults for the optional variables.
const (
	DefaultPublicAddr   = ":8080"
	DefaultInternalAddr = ":8081"
	// DefaultOIDCAudience is the aud every token must carry, spec 006.
	DefaultOIDCAudience = "arca"
	// DefaultRequestsPerMinute is the token bucket per subject after
	// authentication, and DefaultUnauthenticatedRequestsPerMinute the one
	// per client address before it (spec 015). Zero disables either.
	DefaultRequestsPerMinute                = 600
	DefaultUnauthenticatedRequestsPerMinute = 60
)

// Getenv is the environment lookup Load reads through, so a test passes a
// map and never touches the process environment.
type Getenv func(string) string

// Config is the resolved configuration. Field names follow the variable
// names in spec 002 without the ARCA_ prefix. arcad keeps no state on
// local disk: the bucket and the database of later specs are the only
// stores, and their variables join here with the specs that read them.
type Config struct {
	// PublicAddr is where the /v1 API and the public probes listen.
	PublicAddr string
	// InternalAddr is where the four probes listen for the cluster.
	InternalAddr string
	// PublicURL is the address clients reach the public listener at, and
	// the base of every URL the server writes (spec 013).
	PublicURL string
	// OIDCIssuers are the issuers whose tokens are verified (spec 006).
	OIDCIssuers []string
	// OIDCAudience is the audience every token must carry.
	OIDCAudience string
	// OIDCInsecureIssuers admits an http:// issuer that is not on loopback,
	// for the test tiers of spec 014.
	OIDCInsecureIssuers bool
	// AuthorizerURL is the authorizer endpoint; unset selects the owner
	// policy of spec 006. AuthorizerToken is the bearer it expects.
	AuthorizerURL   string
	AuthorizerToken string
	// AdminSubjects are the rendered subjects the owner policy treats as
	// administrators of the installation.
	AdminSubjects []string
	// RequestsPerMinute and UnauthenticatedRequestsPerMinute are the two
	// token buckets of spec 015. Zero disables one.
	RequestsPerMinute                int
	UnauthenticatedRequestsPerMinute int
}

// Load reads every variable through getenv and returns the configuration,
// or one error naming every problem found, sorted by variable name.
func Load(getenv Getenv) (Config, error) {
	var problems []string
	note := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	c := Config{
		PublicAddr:          withDefault(getenv("ARCA_PUBLIC_ADDR"), DefaultPublicAddr),
		InternalAddr:        withDefault(getenv("ARCA_INTERNAL_ADDR"), DefaultInternalAddr),
		PublicURL:           strings.TrimSpace(getenv("ARCA_PUBLIC_URL")),
		OIDCIssuers:         list(getenv("ARCA_OIDC_ISSUERS")),
		OIDCAudience:        withDefault(getenv("ARCA_OIDC_AUDIENCE"), DefaultOIDCAudience),
		OIDCInsecureIssuers: boolean(getenv("ARCA_OIDC_INSECURE_ISSUERS"), "ARCA_OIDC_INSECURE_ISSUERS", note),
		AuthorizerURL:       strings.TrimSpace(getenv("ARCA_AUTHORIZER_URL")),
		AuthorizerToken:     strings.TrimSpace(getenv("ARCA_AUTHORIZER_TOKEN")),
		AdminSubjects:       authz.ParseSubjects(getenv("ARCA_ADMIN_SUBJECTS")),
		RequestsPerMinute: count(getenv("ARCA_REQUESTS_PER_MINUTE"),
			DefaultRequestsPerMinute, "ARCA_REQUESTS_PER_MINUTE", note),
		UnauthenticatedRequestsPerMinute: count(getenv("ARCA_UNAUTHENTICATED_REQUESTS_PER_MINUTE"),
			DefaultUnauthenticatedRequestsPerMinute, "ARCA_UNAUTHENTICATED_REQUESTS_PER_MINUTE", note),
	}

	if err := checkAddr(c.PublicAddr); err != nil {
		note("ARCA_PUBLIC_ADDR %s", err)
	}
	if err := checkAddr(c.InternalAddr); err != nil {
		note("ARCA_INTERNAL_ADDR %s", err)
	}
	if sameEndpoint(c.PublicAddr, c.InternalAddr) {
		note("ARCA_INTERNAL_ADDR must differ from ARCA_PUBLIC_ADDR; both are %s", c.PublicAddr)
	}
	if c.PublicURL == "" {
		note("ARCA_PUBLIC_URL is unset, and every URL the server writes is built on it")
	} else if err := checkURL(c.PublicURL); err != nil {
		note("ARCA_PUBLIC_URL %s", err)
	}
	if len(c.OIDCIssuers) == 0 {
		note("ARCA_OIDC_ISSUERS names no issuer, and there is no anonymous access to a space")
	}
	if c.AuthorizerURL != "" {
		if err := checkURL(c.AuthorizerURL); err != nil {
			note("ARCA_AUTHORIZER_URL %s", err)
		}
		if c.AuthorizerToken == "" {
			note("ARCA_AUTHORIZER_TOKEN is unset while ARCA_AUTHORIZER_URL is set, and the endpoint requires a bearer")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return Config{}, errors.New("configuration: " + strings.Join(problems, "; "))
	}
	return c, nil
}

func withDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// list reads a comma separated variable, dropping blanks, so a trailing
// comma and a value written with spaces both read as the operator meant.
func list(raw string) []string {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// boolean reads a flag variable. Unset is false, and anything that does not
// read as a boolean is a problem rather than a silent false, because an
// operator who wrote "yes" meant yes.
func boolean(raw, name string, note func(string, ...any)) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		note("%s is %q, not true or false", name, raw)
		return false
	}
	return v
}

// count reads a whole number that is not negative. Unset is the default, and
// zero is what the variable's own row says it is rather than an absence.
func count(raw string, def int, name string, note func(string, ...any)) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		note("%s is %q, not a whole number", name, raw)
		return def
	}
	if v < 0 {
		note("%s is %d, and a rate below zero is not a rate", name, v)
		return def
	}
	return v
}

// sameEndpoint reports whether two valid addresses name one socket. Port
// 0 asks the kernel for any free port, so two ":0" addresses are two
// sockets and a test that binds both on loopback is not refused.
func sameEndpoint(a, b string) bool {
	if a != b {
		return false
	}
	_, port, err := net.SplitHostPort(a)
	return err == nil && port != "0"
}

// checkAddr accepts what net.Listen accepts for a TCP address: host:port
// with the host optional.
func checkAddr(addr string) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("is %q, not a host:port address", addr)
	}
	return nil
}

// checkURL accepts an absolute http or https URL, which is what a base a
// client reaches and an endpoint arcad posts to both have to be.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("is %q, not an absolute http or https URL", raw)
	}
	return nil
}
