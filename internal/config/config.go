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
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Defaults for the optional variables.
const (
	DefaultPublicAddr   = ":8080"
	DefaultInternalAddr = ":8081"
	DefaultBucketPrefix = "arca/"
)

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
}

// Database reads the one variable the migrate subcommand needs, so a
// migration job runs with the database and nothing else configured.
func Database(getenv Getenv) (string, error) {
	url := value(getenv("ARCA_DATABASE_URL"))
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
		return "ARCA_DATABASE_URL is unset, and the database is what decides whether an object exists"
	case !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://"):
		return fmt.Sprintf("ARCA_DATABASE_URL is %q, and a connection string begins with postgres:// or postgresql://", url)
	default:
		return ""
	}
}

// Load reads every variable through getenv and returns the configuration,
// or one error naming every problem found, sorted by variable name.
func Load(getenv Getenv) (Config, error) {
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
		DatabaseURL:     value(getenv("ARCA_DATABASE_URL")),
	}
	var problems []string
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
	prefix, err := normalisePrefix(c.BucketPrefix)
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

// normalisePrefix is spec 003's rule: a missing trailing slash is appended,
// a leading slash is a configuration error, and the value holds only the
// characters a key is built from.
func normalisePrefix(prefix string) (string, error) {
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
