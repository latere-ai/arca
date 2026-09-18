// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package config

import (
	"maps"
	"reflect"
	"strings"
	"testing"
)

func env(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

// required is the smallest environment arcad starts in: every variable of
// spec 002's table the specs so far mark required, and nothing else. A case
// adds to it.
func required(extra map[string]string) Getenv {
	m := map[string]string{
		"ARCA_BUCKET":        "arca",
		"ARCA_BUCKET_REGION": "us-east-1",
		"ARCA_DATABASE_URL":  "postgres://arca:arca@db:5432/arca?sslmode=disable",
		"ARCA_PUBLIC_URL":    "https://storage.example",
		"ARCA_OIDC_ISSUERS":  "https://issuer.example",
	}
	maps.Copy(m, extra)
	return env(m)
}

func TestLoadAppliesEveryDefault(t *testing.T) {
	c, err := Load(required(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		PublicAddr:   ":8080",
		InternalAddr: ":8081",
		Bucket:       "arca",
		BucketRegion: "us-east-1",
		BucketPrefix: "arca/",
		DatabaseURL:  "postgres://arca:arca@db:5432/arca?sslmode=disable",

		PublicURL:                        "https://storage.example",
		OIDCIssuers:                      []string{"https://issuer.example"},
		OIDCAudience:                     DefaultOIDCAudience,
		RequestsPerMinute:                DefaultRequestsPerMinute,
		UnauthenticatedRequestsPerMinute: DefaultUnauthenticatedRequestsPerMinute,
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("Load() = %+v, want %+v", c, want)
	}
}

func TestLoadReadsEveryVariable(t *testing.T) {
	c, err := Load(env(map[string]string{
		"ARCA_PUBLIC_ADDR":       "127.0.0.1:9000",
		"ARCA_INTERNAL_ADDR":     "127.0.0.1:9001",
		"ARCA_BUCKET":            "objects",
		"ARCA_BUCKET_ENDPOINT":   "https://store.example:9000",
		"ARCA_BUCKET_REGION":     "eu-central-1",
		"ARCA_BUCKET_PREFIX":     "tenant/one",
		"ARCA_BUCKET_PATH_STYLE": "true",
		"ARCA_BUCKET_ACCESS_KEY": "key",
		"ARCA_BUCKET_SECRET_KEY": "secret",
		"ARCA_PUBLIC_CDN_URL":    "https://cdn.example/",
		"ARCA_DATABASE_URL":      "postgresql://arca@db/arca",

		"ARCA_PUBLIC_URL":                          "https://storage.example/base",
		"ARCA_OIDC_ISSUERS":                        "https://issuer.example, https://other.example ,",
		"ARCA_OIDC_AUDIENCE":                       "arca-test",
		"ARCA_OIDC_INSECURE_ISSUERS":               "true",
		"ARCA_AUTHORIZER_URL":                      "https://authz.example/decide",
		"ARCA_AUTHORIZER_TOKEN":                    "s3cret",
		"ARCA_ADMIN_SUBJECTS":                      "https://issuer.example|root, https://issuer.example|ops",
		"ARCA_REQUESTS_PER_MINUTE":                 "1200",
		"ARCA_UNAUTHENTICATED_REQUESTS_PER_MINUTE": "0",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		PublicAddr:      "127.0.0.1:9000",
		InternalAddr:    "127.0.0.1:9001",
		Bucket:          "objects",
		BucketEndpoint:  "https://store.example:9000",
		BucketRegion:    "eu-central-1",
		BucketPrefix:    "tenant/one/",
		BucketPathStyle: true,
		BucketAccessKey: "key",
		BucketSecretKey: "secret",
		PublicCDNURL:    "https://cdn.example",
		DatabaseURL:     "postgresql://arca@db/arca",

		PublicURL:    "https://storage.example/base",
		OIDCIssuers:  []string{"https://issuer.example", "https://other.example"},
		OIDCAudience: "arca-test", OIDCInsecureIssuers: true,
		AuthorizerURL: "https://authz.example/decide", AuthorizerToken: "s3cret",
		AdminSubjects:     []string{"https://issuer.example|root", "https://issuer.example|ops"},
		RequestsPerMinute: 1200, UnauthenticatedRequestsPerMinute: 0,
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("Load() = %+v, want %+v", c, want)
	}
}

func TestLoadReportsEveryProblemInOneSortedMessage(t *testing.T) {
	_, err := Load(env(map[string]string{
		"ARCA_PUBLIC_ADDR":   "nope",
		"ARCA_INTERNAL_ADDR": "nope",
	}))
	if err == nil {
		t.Fatal("Load() accepted a configuration with four problems")
	}
	got := err.Error()
	for _, want := range []string{
		"configuration: ",
		`ARCA_INTERNAL_ADDR is "nope", not a host:port address`,
		`ARCA_PUBLIC_ADDR is "nope", not a host:port address`,
		"ARCA_BUCKET is unset",
		"ARCA_BUCKET_REGION is unset",
		"ARCA_DATABASE_URL is unset",
		"ARCA_OIDC_ISSUERS names no issuer",
		"ARCA_PUBLIC_URL is unset",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q:\n%s", want, got)
		}
	}
	if !sorted(t, got) {
		t.Errorf("problems are not sorted by variable name:\n%s", got)
	}
}

// sorted reports whether the problems of one message are in the order an
// operator reads a deployment in: by variable name.
func sorted(t *testing.T, message string) bool {
	t.Helper()
	problems := strings.Split(strings.TrimPrefix(message, "configuration: "), "; ")
	for i := 1; i < len(problems); i++ {
		if problems[i-1] > problems[i] {
			return false
		}
	}
	return true
}

func TestLoadTreatsBlankAsUnset(t *testing.T) {
	c, err := Load(required(map[string]string{"ARCA_PUBLIC_ADDR": "  ", "ARCA_INTERNAL_ADDR": "", "ARCA_BUCKET_PREFIX": " "}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicAddr != DefaultPublicAddr || c.InternalAddr != DefaultInternalAddr || c.BucketPrefix != DefaultBucketPrefix {
		t.Fatalf("blank values did not fall back to defaults: %+v", c)
	}
}

func TestLoadRefusesOneSocketForBothListeners(t *testing.T) {
	_, err := Load(required(map[string]string{"ARCA_PUBLIC_ADDR": "127.0.0.1:9000", "ARCA_INTERNAL_ADDR": "127.0.0.1:9000"}))
	if err == nil || !strings.Contains(err.Error(), "must differ from ARCA_PUBLIC_ADDR; both are 127.0.0.1:9000") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadAllowsPortZeroOnBothListeners(t *testing.T) {
	if _, err := Load(required(map[string]string{"ARCA_PUBLIC_ADDR": "127.0.0.1:0", "ARCA_INTERNAL_ADDR": "127.0.0.1:0"})); err != nil {
		t.Fatal(err)
	}
}

func TestThePrefixGainsItsSlashAndRefusesAnythingElse(t *testing.T) {
	for raw, want := range map[string]string{
		"arca":       "arca/",
		"arca/":      "arca/",
		"drive/":     "drive/",
		"tenant/one": "tenant/one/",
		"a.b_c-d":    "a.b_c-d/",
	} {
		c, err := Load(required(map[string]string{"ARCA_BUCKET_PREFIX": raw}))
		if err != nil || c.BucketPrefix != want {
			t.Errorf("a prefix of %q loaded as %q, %v", raw, c.BucketPrefix, err)
		}
	}
	for _, raw := range []string{"/arca", "/", "arca bucket/", "arca?x=1/", "arca\\one/"} {
		_, err := Load(required(map[string]string{"ARCA_BUCKET_PREFIX": raw}))
		if err == nil || !strings.Contains(err.Error(), "ARCA_BUCKET_PREFIX") {
			t.Errorf("a prefix of %q loaded with %v", raw, err)
		}
	}
}

func TestTheBucketVariablesAreCheckedForShape(t *testing.T) {
	for name, cases := range map[string][]string{
		"ARCA_BUCKET_ENDPOINT":   {"store.example", "ftp://store.example", "https://"},
		"ARCA_PUBLIC_CDN_URL":    {"cdn.example", "//cdn.example"},
		"ARCA_BUCKET_PATH_STYLE": {"yes please", "1.5"},
	} {
		for _, raw := range cases {
			_, err := Load(required(map[string]string{name: raw}))
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s = %q loaded with %v", name, raw, err)
			}
		}
	}
	if _, err := Load(required(map[string]string{"ARCA_BUCKET_PATH_STYLE": "1"})); err != nil {
		t.Errorf("a path style of 1: %v", err)
	}
}

func TestTheDatabaseURLIsOneTheMigratorCanReadToo(t *testing.T) {
	for _, raw := range []string{"host=db user=arca dbname=arca", "mysql://db/arca", "db:5432/arca"} {
		_, err := Database(env(map[string]string{"ARCA_DATABASE_URL": raw}))
		if err == nil || !strings.Contains(err.Error(), "ARCA_DATABASE_URL") {
			t.Errorf("a connection string of %q loaded with %v", raw, err)
		}
	}
	url, err := Database(env(map[string]string{"ARCA_DATABASE_URL": " postgres://arca@db/arca "}))
	if err != nil || url != "postgres://arca@db/arca" {
		t.Errorf("Database() = %q, %v", url, err)
	}
	if _, err := Database(env(nil)); err == nil {
		t.Error("a migration job with no database was accepted")
	}
}

func TestTheCredentialsAreSetAsAPairOrNotAtAll(t *testing.T) {
	for _, only := range []string{"ARCA_BUCKET_ACCESS_KEY", "ARCA_BUCKET_SECRET_KEY"} {
		_, err := Load(required(map[string]string{only: "half"}))
		if err == nil || !strings.Contains(err.Error(), "set as a pair") {
			t.Errorf("%s alone loaded with %v", only, err)
		}
	}
}

// TestTheIdentityVariablesAreChecked is spec 006's half of the table: a
// deployment that names an endpoint with no bearer, an issuer list that is
// only commas, or a URL that is not one is refused at start-up with the
// variable named, rather than answering 401 or 503 to every request.
func TestTheIdentityVariablesAreChecked(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		mustSay string
	}{
		{"an endpoint with no bearer", map[string]string{
			"ARCA_AUTHORIZER_URL": "https://authz.example/decide",
		}, "ARCA_AUTHORIZER_TOKEN is unset"},
		{"an endpoint that is not a URL", map[string]string{
			"ARCA_AUTHORIZER_URL": "authz.example", "ARCA_AUTHORIZER_TOKEN": "s3cret",
		}, `ARCA_AUTHORIZER_URL is "authz.example"`},
		{"an issuer list of commas", map[string]string{
			"ARCA_OIDC_ISSUERS": " , ,",
		}, "ARCA_OIDC_ISSUERS names no issuer"},
		{"a public URL with no scheme", map[string]string{
			"ARCA_PUBLIC_URL": "storage.example",
		}, `ARCA_PUBLIC_URL is "storage.example"`},
		{"an insecure-issuers flag that is not a boolean", map[string]string{
			"ARCA_OIDC_INSECURE_ISSUERS": "yes please",
		}, `ARCA_OIDC_INSECURE_ISSUERS is "yes please"`},
		{"a rate that is not a number", map[string]string{
			"ARCA_REQUESTS_PER_MINUTE": "lots",
		}, `ARCA_REQUESTS_PER_MINUTE is "lots"`},
		{"a rate below zero", map[string]string{
			"ARCA_UNAUTHENTICATED_REQUESTS_PER_MINUTE": "-1",
		}, "ARCA_UNAUTHENTICATED_REQUESTS_PER_MINUTE is -1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(required(c.env))
			if err == nil {
				t.Fatal("Load() accepted a deployment it should refuse")
			}
			if !strings.Contains(err.Error(), c.mustSay) {
				t.Errorf("the message is %q, which does not say %q", err, c.mustSay)
			}
		})
	}
}

// TestNoAuthorizerIsNotAProblem: ARCA_AUTHORIZER_URL unset selects the owner
// policy, which is a configuration and not an omission, so no bearer is
// required with it.
func TestNoAuthorizerIsNotAProblem(t *testing.T) {
	c, err := Load(required(map[string]string{"ARCA_ADMIN_SUBJECTS": "https://issuer.example|root"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.AuthorizerURL != "" || c.AuthorizerToken != "" {
		t.Errorf("an unset endpoint read as %q with the bearer %q", c.AuthorizerURL, c.AuthorizerToken)
	}
	if got := c.AdminSubjects; len(got) != 1 || got[0] != "https://issuer.example|root" {
		t.Errorf("the administrators are %v", got)
	}
}

// TestARateOfZeroIsOff: zero is the value spec 002's row gives the variable,
// a limiter that is off, and not an unset variable falling back to a default.
func TestARateOfZeroIsOff(t *testing.T) {
	c, err := Load(required(map[string]string{"ARCA_REQUESTS_PER_MINUTE": "0"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.RequestsPerMinute != 0 {
		t.Errorf("a rate of zero read as %d", c.RequestsPerMinute)
	}
}
