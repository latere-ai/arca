// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package config reads the typed configuration of arcad from the
// environment. Every variable spec 002 names is read once at start-up, and
// a start-up with anything missing or malformed fails with one message that
// lists every problem, so an operator fixes a deployment in one round.
package config

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/arca/internal/api"
)

// Defaults for the optional variables.
const (
	DefaultPublicAddr   = ":8080"
	DefaultInternalAddr = ":8081"
	DefaultBucketPrefix = "arca/"
	// DefaultOIDCAudience is the primary aud every token must carry, spec
	// 006. ARCA_OIDC_AUDIENCE is a list, and this is the whole of it where
	// the variable is unset.
	DefaultOIDCAudience = "arca"
	// DefaultBasePath is the base every route of the surface is registered
	// under where ARCA_BASE_PATH is unset (spec 027): the root of the
	// version, which is what a self-hosted installation serves.
	DefaultBasePath = "/v1"
	// DefaultRequestsPerMinute is the token bucket per subject after
	// authentication, and DefaultUnauthenticatedRequestsPerMinute the one
	// per client address before it (spec 015). Zero disables either.
	DefaultRequestsPerMinute                = 600
	DefaultUnauthenticatedRequestsPerMinute = 60
	// DefaultMaxUploadBytes is the largest object this server accepts at
	// all, and DefaultInlineBytes the largest it streams through itself
	// (specs 005 and 007). Above the inline size the bytes go from the
	// client to the bucket in parts and never through a replica, which is
	// invariant 4 of spec 001.
	DefaultMaxUploadBytes int64 = 5 << 30
	DefaultInlineBytes    int64 = 16 << 20
	// DefaultTrashRetention is how long a trashed object is restorable
	// before the reaper purges it from both stores (spec 005).
	DefaultTrashRetention = 720 * time.Hour
)

// DefaultReapInterval is how often the reconciler of spec 010 runs inside
// serve. A value of zero disables the in-process loop, which is what an
// installation that runs arcad reap as a process of its own sets on its API
// replicas.
const DefaultReapInterval = 5 * time.Minute

// prefixShape is what a bucket prefix may hold: the characters a key is
// built from, and no others, so a prefix cannot smuggle a query string or a
// path traversal into a key.
var prefixShape = regexp.MustCompile(`^[A-Za-z0-9._/-]*$`)

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
	// Bucket is the bucket every key is written to.
	Bucket string
	// BucketEndpoint is the S3 endpoint. Empty leaves the SDK to derive one
	// from the region, so no provider is named here.
	BucketEndpoint string
	// BucketRegion is the signing region.
	BucketRegion string
	// BucketPrefix is what every key carries, ending in a slash. Several
	// installations share one bucket by taking a prefix each.
	BucketPrefix string
	// BucketPathStyle addresses the bucket in the path rather than in the
	// host, for a store without virtual hosts.
	BucketPathStyle bool
	// BucketAccessKey and BucketSecretKey are static credentials. Both
	// empty falls through to the SDK's credential chain.
	BucketAccessKey, BucketSecretKey string
	// PublicCDNURL is the base a public object's redirect points at, with
	// no trailing slash. Empty answers the ordinary presigned redirect.
	PublicCDNURL string
	// DatabaseURL is the Postgres connection string.
	DatabaseURL string
	// PublicURL is the address clients reach the public listener at, and
	// the base of every URL the server writes (spec 013). It is the origin
	// root and carries no base path: arcad check reads /version under it,
	// and the probes sit outside the surface.
	PublicURL string
	// BasePath is the base every route of the surface is registered under
	// (spec 027). The default is the root of the version, which is what a
	// self-hosted installation serves; an installation behind an origin
	// partitioned by capability carries the whole prefix it was given,
	// "/v1/storage", because that literal is what the Ingress rule, the
	// proxy target and the served document all name.
	BasePath string
	// OIDCIssuers are the issuers whose tokens are verified (spec 006).
	OIDCIssuers []string
	// OIDCAudiences are the audiences a token may carry, ARCA_OIDC_AUDIENCE
	// read as a comma list. A token is accepted when its aud names any of
	// them; the first is the primary, the name this core answers to and the
	// one the verifier reports. A hosted installation lists its own name and
	// the origin in front of it, because a platform key and a personal
	// access token are addressed to that origin.
	OIDCAudiences []string
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
	// MaxUploadBytes is the largest object this server accepts by any
	// route, and InlineBytes the boundary between the two size classes of
	// spec 007: at or below it a put streams through the server, above it
	// the parts of a session go straight to the bucket.
	MaxUploadBytes, InlineBytes int64
	// ReapInterval is how often the reconciler of spec 010 runs inside
	// serve. Zero leaves serve with no reconciliation loop.
	ReapInterval time.Duration
	// TrashRetention is how long a trashed object stays restorable (spec
	// 005) before the reconciler purges it from both stores (spec 010).
	TrashRetention time.Duration
	// OTelEndpoint is where traces, logs and metrics go (spec 018). Empty
	// exports nothing: spans are created and discarded, and /metrics still
	// serves, so a self-hoster with no collector loses no local signal.
	//
	// Two variables carry it. ARCA_OTEL_EXPORTER_OTLP_ENDPOINT is spec 002's
	// row and wins wherever it is set, so an operator configures one prefix
	// and a test passes one map. With that row unset the standard
	// OTEL_EXPORTER_OTLP_ENDPOINT is read, because an operator that injects
	// it into every workload of a namespace is following the OpenTelemetry
	// standard, and a core that read only its own name would be the one
	// workload there looking healthy while exporting nothing. Both are read
	// through this function, so the rule holds whatever cmd/arcad later does
	// with the value.
	OTelEndpoint string
	// TestDrift names one way this build is to answer the contract wrong,
	// so the conformance suite of spec 017 is proved to catch a server that
	// does not serve spec 013 rather than only to pass one that does. Every
	// value refuses something the server should serve or answers with the
	// wrong shape, and none grants an authority the server would otherwise
	// withhold, so a value set by accident is a visible defect and never a
	// way past a decision. It is empty in every deployment, arcad says so in
	// its log when it is not, and a value no drift is written for is refused
	// here rather than ignored: such a value is a typo in the one that was
	// meant, and a server that ignored it would report a suite as having
	// caught a drift that never ran.
	TestDrift api.Drift
}

// Database reads the one variable the migrate subcommand needs, so a
// migration job runs with the database and nothing else configured.
func Database(getenv Getenv) (string, error) {
	url := value(getenv("ARCA_DB_URL"))
	if problem := checkDatabaseURL(url); problem != "" {
		return "", errors.New("configuration: " + problem)
	}
	return url, nil
}

// checkDatabaseURL answers the problem with the connection string, or the
// empty string. The migrator selects its driver from the scheme, so a
// keyword and value connection string, which the pool would accept, is
// refused here rather than at the first migration.
func checkDatabaseURL(url string) string {
	switch {
	case url == "":
		return "ARCA_DB_URL is unset, and the database is what decides whether an object exists"
	case !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://"):
		return fmt.Sprintf("ARCA_DB_URL is %q, and a connection string begins with postgres:// or postgresql://", url)
	default:
		return ""
	}
}

// Load reads every variable through getenv and returns the configuration,
// or one error naming every problem found, sorted by variable name.
func Load(getenv Getenv) (Config, error) {
	var problems []string
	// note is the other half of the same slice: a row written with a format
	// string rather than a built one, so a check reads as the sentence an
	// operator gets. Every problem, however it is written, ends in the one
	// sorted message below.
	note := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	// The two spellings of the exporter's endpoint, read here because the
	// shape below is held against one of them and not the other.
	ownEndpoint := value(getenv("ARCA_OTEL_EXPORTER_OTLP_ENDPOINT"))
	injectedEndpoint := value(getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))

	c := Config{
		PublicAddr:      withDefault(getenv("ARCA_PUBLIC_ADDR"), DefaultPublicAddr),
		InternalAddr:    withDefault(getenv("ARCA_INTERNAL_ADDR"), DefaultInternalAddr),
		Bucket:          value(getenv("ARCA_BUCKET")),
		BucketEndpoint:  value(getenv("ARCA_BUCKET_ENDPOINT")),
		BucketRegion:    value(getenv("ARCA_BUCKET_REGION")),
		BucketPrefix:    withDefault(getenv("ARCA_BUCKET_PREFIX"), DefaultBucketPrefix),
		BucketAccessKey: value(getenv("ARCA_BUCKET_ACCESS_KEY")),
		BucketSecretKey: value(getenv("ARCA_BUCKET_SECRET_KEY")),
		PublicCDNURL:    strings.TrimRight(value(getenv("ARCA_PUBLIC_CDN_URL")), "/"),
		DatabaseURL:     value(getenv("ARCA_DB_URL")),

		PublicURL:           value(getenv("ARCA_PUBLIC_URL")),
		BasePath:            withDefault(value(getenv("ARCA_BASE_PATH")), DefaultBasePath),
		OIDCIssuers:         list(getenv("ARCA_OIDC_ISSUERS")),
		OIDCAudiences:       audiences(getenv("ARCA_OIDC_AUDIENCE")),
		OIDCInsecureIssuers: boolean(getenv("ARCA_OIDC_INSECURE_ISSUERS"), "ARCA_OIDC_INSECURE_ISSUERS", note),
		AuthorizerURL:       value(getenv("ARCA_AUTHORIZER_URL")),
		AuthorizerToken:     value(getenv("ARCA_AUTHORIZER_TOKEN")),
		AdminSubjects:       authz.ParseSubjects(getenv("ARCA_ADMIN_SUBJECTS")),
		OTelEndpoint:        cmp.Or(ownEndpoint, injectedEndpoint),
		RequestsPerMinute: count(getenv("ARCA_REQUESTS_PER_MINUTE"),
			DefaultRequestsPerMinute, "ARCA_REQUESTS_PER_MINUTE", note),
		UnauthenticatedRequestsPerMinute: count(getenv("ARCA_UNAUTHENTICATED_REQUESTS_PER_MINUTE"),
			DefaultUnauthenticatedRequestsPerMinute, "ARCA_UNAUTHENTICATED_REQUESTS_PER_MINUTE", note),
		MaxUploadBytes: size(getenv("ARCA_MAX_UPLOAD_BYTES"),
			DefaultMaxUploadBytes, "ARCA_MAX_UPLOAD_BYTES", note),
		InlineBytes: size(getenv("ARCA_INLINE_BYTES"),
			DefaultInlineBytes, "ARCA_INLINE_BYTES", note),
	}
	if c.InlineBytes > c.MaxUploadBytes {
		note("ARCA_INLINE_BYTES is %d and ARCA_MAX_UPLOAD_BYTES is %d, and the largest object streamed "+
			"through the server cannot be larger than the largest object accepted", c.InlineBytes, c.MaxUploadBytes)
	}
	if err := checkAddr(c.PublicAddr); err != nil {
		problems = append(problems, "ARCA_PUBLIC_ADDR "+err.Error())
	}
	if err := checkAddr(c.InternalAddr); err != nil {
		problems = append(problems, "ARCA_INTERNAL_ADDR "+err.Error())
	}
	if sameEndpoint(c.PublicAddr, c.InternalAddr) {
		problems = append(problems, "ARCA_INTERNAL_ADDR must differ from ARCA_PUBLIC_ADDR; both are "+c.PublicAddr)
	}
	if c.Bucket == "" {
		problems = append(problems, "ARCA_BUCKET is unset, and the server writes every object to one bucket")
	}
	if c.BucketRegion == "" {
		problems = append(problems, "ARCA_BUCKET_REGION is unset, and a request to the store is signed for a region")
	}
	if c.BucketEndpoint != "" {
		if err := checkURL(c.BucketEndpoint); err != nil {
			problems = append(problems, "ARCA_BUCKET_ENDPOINT "+err.Error())
		}
	}
	prefix, err := normalizePrefix(c.BucketPrefix)
	if err != nil {
		problems = append(problems, "ARCA_BUCKET_PREFIX "+err.Error())
	}
	c.BucketPrefix = prefix
	if raw := value(getenv("ARCA_BUCKET_PATH_STYLE")); raw != "" {
		pathStyle, err := strconv.ParseBool(raw)
		if err != nil {
			problems = append(problems, fmt.Sprintf("ARCA_BUCKET_PATH_STYLE is %q, not a true or false value", raw))
		}
		c.BucketPathStyle = pathStyle
	}
	if (c.BucketAccessKey == "") != (c.BucketSecretKey == "") {
		problems = append(problems, "ARCA_BUCKET_ACCESS_KEY and ARCA_BUCKET_SECRET_KEY are set as a pair; unset both to use the credentials of the environment")
	}
	if c.PublicCDNURL != "" {
		if err := checkURL(c.PublicCDNURL); err != nil {
			problems = append(problems, "ARCA_PUBLIC_CDN_URL "+err.Error())
		}
	}
	if problem := checkDatabaseURL(c.DatabaseURL); problem != "" {
		problems = append(problems, problem)
	}
	// The shape is held against the table's own row alone. A value that
	// arrived by injection was set for every workload of the namespace and is
	// parsed by the exporter that owns the standard name; a telemetry
	// variable this installation did not write is not a reason a replica
	// refuses to serve bytes.
	if ownEndpoint != "" {
		if err := checkURL(ownEndpoint); err != nil {
			note("ARCA_OTEL_EXPORTER_OTLP_ENDPOINT %s", err)
		}
	}
	if c.PublicURL == "" {
		note("ARCA_PUBLIC_URL is unset, and every URL the server writes is built on it")
	} else if err := checkURL(c.PublicURL); err != nil {
		note("ARCA_PUBLIC_URL %s", err)
	}
	if problem := checkBasePath(c.BasePath); problem != "" {
		problems = append(problems, problem)
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
	c.ReapInterval = duration(getenv("ARCA_REAP_INTERVAL"), DefaultReapInterval, "ARCA_REAP_INTERVAL", true, note)
	c.TrashRetention = duration(getenv("ARCA_TRASH_RETENTION"), DefaultTrashRetention, "ARCA_TRASH_RETENTION", false, note)
	if drift, err := api.ParseDrift(getenv("ARCA_TEST_DRIFT")); err != nil {
		note("ARCA_TEST_DRIFT %s", err)
	} else {
		c.TestDrift = drift
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return Config{}, errors.New("configuration: " + strings.Join(problems, "; "))
	}
	return c, nil
}

func withDefault(v, def string) string {
	if value(v) == "" {
		return def
	}
	return v
}

// value is the variable as the server reads it: a blank value is unset.
func value(v string) string { return strings.TrimSpace(v) }

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

// duration reads one window, in the spelling time.ParseDuration accepts.
// Unset is the default, and anything the clock cannot read is a problem
// rather than a silent fallback. offSwitch says whether zero is a value the
// variable takes: an interval of zero turns a loop off, and a retention of
// zero would purge what was deleted a moment ago.
func duration(raw string, def time.Duration, name string, offSwitch bool, note func(string, ...any)) time.Duration {
	raw = value(raw)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	switch {
	case err != nil:
		note("%s is %q, and a window is written the way 5m, 2h30m or 720h is", name, raw)
	case d < 0:
		note("%s is %q, and a window does not run backwards", name, raw)
	case d == 0 && !offSwitch:
		note("%s is %q, and a window of no time acts on what happened a moment ago", name, raw)
	default:
		return d
	}
	return 0
}

// size reads a byte count. Unset is the default, and a value that is not a
// whole number above zero is a problem rather than a silent default: a
// server that accepted no object at all because a variable read as zero
// would be a deployment nobody could debug from its behavior.
func size(raw string, def int64, name string, note func(string, ...any)) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		note("%s is %q, not a whole number of bytes", name, raw)
		return def
	}
	if v <= 0 {
		note("%s is %d, and a server that accepts no object serves nothing", name, v)
		return def
	}
	return v
}

// audiences reads ARCA_OIDC_AUDIENCE as the comma list spec 027 makes it:
// the names a token's aud may carry, the first of them primary. An unset
// variable is the default alone, so an installation that configures nothing
// answers to the name spec 001 fixes for the core.
func audiences(raw string) []string {
	if names := list(raw); len(names) > 0 {
		return names
	}
	return []string{DefaultOIDCAudience}
}

// checkBasePath is spec 027's rule for ARCA_BASE_PATH: the base begins with
// a slash, carries no trailing one, and its first segment is v1. The version
// is part of the contract of spec 013, so a base that dropped it would serve
// the surface at an address no document of this API describes.
func checkBasePath(base string) string {
	switch {
	case !strings.HasPrefix(base, "/"):
		return fmt.Sprintf("ARCA_BASE_PATH is %q, and a base path is rooted, so it begins with /", base)
	case strings.HasSuffix(base, "/"):
		return fmt.Sprintf("ARCA_BASE_PATH is %q, and a base path carries no trailing slash", base)
	case strings.Split(base, "/")[1] != "v1":
		return fmt.Sprintf(
			"ARCA_BASE_PATH is %q, and the first segment of the base is v1, the version of the API it serves", base)
	default:
		return ""
	}
}

// normalizePrefix is spec 003's rule: a missing trailing slash is appended,
// a leading slash is a configuration error, and the value holds only the
// characters a key is built from.
func normalizePrefix(prefix string) (string, error) {
	if strings.HasPrefix(prefix, "/") {
		return "", fmt.Errorf("is %q, and a key is not rooted, so the prefix carries no leading slash", prefix)
	}
	if !prefixShape.MatchString(prefix) {
		return "", fmt.Errorf("is %q, and a prefix holds letters, digits, and the characters . _ - /", prefix)
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix, nil
}

// checkURL accepts an absolute http or https address and nothing else.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Errorf("is %q, which is no URL: %w", raw, err)
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("is %q, and an address carries the scheme http or https", raw)
	case u.Host == "":
		return fmt.Errorf("is %q, which names no host", raw)
	default:
		return nil
	}
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
