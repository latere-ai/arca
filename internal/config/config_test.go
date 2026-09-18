// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package config

import (
	"maps"
	"strings"
	"testing"
)

func env(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

// with is the environment of a server that starts: every required variable,
// plus what the case is about.
func with(overrides map[string]string) Getenv {
	m := map[string]string{
		"ARCA_BUCKET":        "arca",
		"ARCA_BUCKET_REGION": "us-east-1",
	}
	maps.Copy(m, overrides)
	return env(m)
}

func TestLoadAppliesEveryDefault(t *testing.T) {
	c, err := Load(with(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		PublicAddr:   ":8080",
		InternalAddr: ":8081",
		Bucket:       "arca",
		BucketRegion: "us-east-1",
		BucketPrefix: "arca/",
	}
	if c != want {
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
	}
	if c != want {
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
	c, err := Load(with(map[string]string{"ARCA_PUBLIC_ADDR": "  ", "ARCA_INTERNAL_ADDR": "", "ARCA_BUCKET_PREFIX": " "}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicAddr != DefaultPublicAddr || c.InternalAddr != DefaultInternalAddr || c.BucketPrefix != DefaultBucketPrefix {
		t.Fatalf("blank values did not fall back to defaults: %+v", c)
	}
}

func TestLoadRefusesOneSocketForBothListeners(t *testing.T) {
	_, err := Load(with(map[string]string{"ARCA_PUBLIC_ADDR": "127.0.0.1:9000", "ARCA_INTERNAL_ADDR": "127.0.0.1:9000"}))
	if err == nil || !strings.Contains(err.Error(), "must differ from ARCA_PUBLIC_ADDR; both are 127.0.0.1:9000") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadAllowsPortZeroOnBothListeners(t *testing.T) {
	if _, err := Load(with(map[string]string{"ARCA_PUBLIC_ADDR": "127.0.0.1:0", "ARCA_INTERNAL_ADDR": "127.0.0.1:0"})); err != nil {
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
		c, err := Load(with(map[string]string{"ARCA_BUCKET_PREFIX": raw}))
		if err != nil || c.BucketPrefix != want {
			t.Errorf("a prefix of %q loaded as %q, %v", raw, c.BucketPrefix, err)
		}
	}
	for _, raw := range []string{"/arca", "/", "arca bucket/", "arca?x=1/", "arca\\one/"} {
		_, err := Load(with(map[string]string{"ARCA_BUCKET_PREFIX": raw}))
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
			_, err := Load(with(map[string]string{name: raw}))
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s = %q loaded with %v", name, raw, err)
			}
		}
	}
	if _, err := Load(with(map[string]string{"ARCA_BUCKET_PATH_STYLE": "1"})); err != nil {
		t.Errorf("a path style of 1: %v", err)
	}
}

func TestTheCredentialsAreSetAsAPairOrNotAtAll(t *testing.T) {
	for _, only := range []string{"ARCA_BUCKET_ACCESS_KEY", "ARCA_BUCKET_SECRET_KEY"} {
		_, err := Load(with(map[string]string{only: "half"}))
		if err == nil || !strings.Contains(err.Error(), "set as a pair") {
			t.Errorf("%s alone loaded with %v", only, err)
		}
	}
}
