// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package config

import (
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"
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
		"ARCA_DB_URL":        "postgres://arca:arca@db:5432/arca?sslmode=disable",
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
		MaxUploadBytes:                   DefaultMaxUploadBytes,
		InlineBytes:                      DefaultInlineBytes,
		ReapInterval:                     DefaultReapInterval,
		TrashRetention:                   DefaultTrashRetention,
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
		"ARCA_DB_URL":            "postgresql://arca@db/arca",

		"ARCA_PUBLIC_URL":                          "https://storage.example/base",
		"ARCA_OIDC_ISSUERS":                        "https://issuer.example, https://other.example ,",
		"ARCA_OIDC_AUDIENCE":                       "arca-test",
		"ARCA_OIDC_INSECURE_ISSUERS":               "true",
		"ARCA_AUTHORIZER_URL":                      "https://authz.example/decide",
		"ARCA_AUTHORIZER_TOKEN":                    "s3cret",
		"ARCA_ADMIN_SUBJECTS":                      "https://issuer.example|root, https://issuer.example|ops",
		"ARCA_REQUESTS_PER_MINUTE":                 "1200",
		"ARCA_UNAUTHENTICATED_REQUESTS_PER_MINUTE": "0",
		"ARCA_MAX_UPLOAD_BYTES":                    "1073741824",
		"ARCA_INLINE_BYTES":                        "8388608",
		"ARCA_REAP_INTERVAL":                       "90s",
		"ARCA_TRASH_RETENTION":                     "168h",
		"ARCA_OTEL_EXPORTER_OTLP_ENDPOINT":         "https://collector.example:4318",
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
		MaxUploadBytes: 1 << 30, InlineBytes: 8 << 20,
		ReapInterval: 90 * time.Second, TrashRetention: 168 * time.Hour,
		OTelEndpoint: "https://collector.example:4318",
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
		"ARCA_DB_URL is unset",
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
		// The exporter's endpoint is spec 018's and is checked in the same
		// round as the rest: a collector nobody can reach is a deployment
		// fixed at start-up rather than telemetry that quietly goes nowhere.
		"ARCA_OTEL_EXPORTER_OTLP_ENDPOINT": {"collector.example", "grpc://collector.example", "https://"},
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

// TestAnInjectedCollectorEndpointIsReadAndTheTablesRowWins is spec 018's
// export under an operator that instruments a whole namespace: such an
// operator injects the OpenTelemetry standard variables into every workload,
// and OTEL_EXPORTER_OTLP_ENDPOINT is the name it injects. The table's own row
// wins wherever it is set, and with it unset the standard name is what the
// server reads, because a core that ignored the standard name would be the
// one workload in the namespace looking healthy while exporting nothing.
//
// The shape is held against the table's row alone. A value that arrived by
// injection is the platform's and is parsed by the exporter that owns the
// standard name, and a namespace-wide telemetry variable is not a reason this
// replica refuses to serve bytes.
func TestAnInjectedCollectorEndpointIsReadAndTheTablesRowWins(t *testing.T) {
	const injected = "http://10.0.0.7:40318"
	c, err := Load(required(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": injected}))
	if err != nil {
		t.Fatal(err)
	}
	if c.OTelEndpoint != injected {
		t.Errorf("with only the standard variable set, OTelEndpoint = %q, want %q", c.OTelEndpoint, injected)
	}

	const own = "https://collector.example:4318"
	c, err = Load(required(map[string]string{
		"ARCA_OTEL_EXPORTER_OTLP_ENDPOINT": own,
		"OTEL_EXPORTER_OTLP_ENDPOINT":      injected,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.OTelEndpoint != own {
		t.Errorf("with both set, OTelEndpoint = %q, want the table's row %q", c.OTelEndpoint, own)
	}

	if _, err := Load(required(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "collector.example"})); err != nil {
		t.Errorf("an injected endpoint of %q refused the start-up: %v", "collector.example", err)
	}
}

func TestTheDatabaseURLIsOneTheMigratorCanReadToo(t *testing.T) {
	for _, raw := range []string{"host=db user=arca dbname=arca", "mysql://db/arca", "db:5432/arca"} {
		_, err := Database(env(map[string]string{"ARCA_DB_URL": raw}))
		if err == nil || !strings.Contains(err.Error(), "ARCA_DB_URL") {
			t.Errorf("a connection string of %q loaded with %v", raw, err)
		}
	}
	url, err := Database(env(map[string]string{"ARCA_DB_URL": " postgres://arca@db/arca "}))
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

func TestTheWindowsOfTheReconcilerAreReadAndChecked(t *testing.T) {
	for _, c := range []struct {
		name     string
		variable string
		value    string
		want     time.Duration
		problem  string
	}{
		{"an interval", "ARCA_REAP_INTERVAL", "30s", 30 * time.Second, ""},
		{"an interval of zero, which turns the loop off", "ARCA_REAP_INTERVAL", "0s", 0, ""},
		{"an interval in no spelling", "ARCA_REAP_INTERVAL", "often", 0, "written the way"},
		{"an interval that runs backwards", "ARCA_REAP_INTERVAL", "-5m", 0, "backwards"},
		{"a retention", "ARCA_TRASH_RETENTION", "168h", 168 * time.Hour, ""},
		{"a retention in no spelling", "ARCA_TRASH_RETENTION", "a month", 0, "written the way"},
		{"a retention of no time", "ARCA_TRASH_RETENTION", "0", 0, "no time"},
		{"a retention that runs backwards", "ARCA_TRASH_RETENTION", "-1h", 0, "backwards"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := Load(required(map[string]string{c.variable: c.value}))
			if c.problem != "" {
				if err == nil || !strings.Contains(err.Error(), c.problem) {
					t.Fatalf("%s of %q loaded with %v", c.variable, c.value, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			read := got.ReapInterval
			if c.variable == "ARCA_TRASH_RETENTION" {
				read = got.TrashRetention
			}
			if read != c.want {
				t.Fatalf("%s of %q read as %s", c.variable, c.value, read)
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

// TestTheSizeVariablesRefuseWhatIsNotOne: the two byte counts specs 005 and
// 007 read. A value that does not parse, and one that parses to a size no
// object could be written inside, are problems and not silent defaults: a
// server accepting no object is a deployment nobody could debug from its
// behaviour. The third row of those specs, ARCA_TRASH_RETENTION, is a window
// and is checked with the reconciler's above.
func TestTheSizeVariablesRefuseWhatIsNotOne(t *testing.T) {
	for _, c := range []struct{ variable, value, want string }{
		{"ARCA_MAX_UPLOAD_BYTES", "5GB", `ARCA_MAX_UPLOAD_BYTES is "5GB", not a whole number of bytes`},
		{"ARCA_MAX_UPLOAD_BYTES", "0", "ARCA_MAX_UPLOAD_BYTES is 0, and a server that accepts no object serves nothing"},
		{"ARCA_INLINE_BYTES", "-1", "ARCA_INLINE_BYTES is -1, and a server that accepts no object serves nothing"},
	} {
		_, err := Load(required(map[string]string{c.variable: c.value}))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s=%q loaded with %v", c.variable, c.value, err)
		}
	}
}

// TestTheInlineSizeStaysUnderTheLargestObject: above the inline size the
// bytes go to the bucket in parts, and the boundary is the same number on
// both sides of the transfer (spec 007). An inline size above the largest
// object accepted names a class of write no route serves.
func TestTheInlineSizeStaysUnderTheLargestObject(t *testing.T) {
	_, err := Load(required(map[string]string{
		"ARCA_MAX_UPLOAD_BYTES": "1000", "ARCA_INLINE_BYTES": "2000",
	}))
	if err == nil || !strings.Contains(err.Error(),
		"ARCA_INLINE_BYTES is 2000 and ARCA_MAX_UPLOAD_BYTES is 1000") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Load(required(map[string]string{
		"ARCA_MAX_UPLOAD_BYTES": "2000", "ARCA_INLINE_BYTES": "2000",
	})); err != nil {
		t.Fatalf("an inline size equal to the largest object: %v", err)
	}
}
