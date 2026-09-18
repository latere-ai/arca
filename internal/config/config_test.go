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

// required is the smallest environment arcad starts in: the variables of
// spec 002's table this spec marks required, and nothing else. A case adds
// to it.
func required(extra map[string]string) Getenv {
	m := map[string]string{
		"ARCA_PUBLIC_URL":   "https://storage.example",
		"ARCA_OIDC_ISSUERS": "https://issuer.example",
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
		PublicAddr: ":8080", InternalAddr: ":8081",
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
	c, err := Load(required(map[string]string{
		"ARCA_PUBLIC_ADDR":                         "127.0.0.1:9000",
		"ARCA_INTERNAL_ADDR":                       "127.0.0.1:9001",
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
		PublicAddr: "127.0.0.1:9000", InternalAddr: "127.0.0.1:9001",
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
		t.Fatal("Load() accepted two bad addresses")
	}
	got := err.Error()
	for _, want := range []string{
		"configuration: ",
		`ARCA_INTERNAL_ADDR is "nope", not a host:port address`,
		`ARCA_PUBLIC_ADDR is "nope", not a host:port address`,
		"ARCA_OIDC_ISSUERS names no issuer",
		"ARCA_PUBLIC_URL is unset",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q:\n%s", want, got)
		}
	}
	if i, j := strings.Index(got, "ARCA_INTERNAL_ADDR is"), strings.Index(got, "ARCA_PUBLIC_ADDR"); i > j {
		t.Errorf("problems are not sorted by name:\n%s", got)
	}
}

func TestLoadTreatsBlankAsUnset(t *testing.T) {
	c, err := Load(required(map[string]string{"ARCA_PUBLIC_ADDR": "  ", "ARCA_INTERNAL_ADDR": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicAddr != DefaultPublicAddr || c.InternalAddr != DefaultInternalAddr {
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
