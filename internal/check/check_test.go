// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package check

import (
	"bytes"
	"strings"
	"testing"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/config"
)

// The report and the subcommand: the shape of a line, the two exits, and
// what the command does with a configuration it cannot read.

// TestALineIsAVerdictARequirementAndASentence is the output format of spec
// 012: a script cuts the first two fields and a human reads down the table.
func TestALineIsAVerdictARequirementAndASentence(t *testing.T) {
	for _, tc := range []struct {
		r    Requirement
		want string
	}{
		{
			r:    Requirement{Name: NameBucket, OK: true, Detail: "arca-prod at https://s3.example: wrote, read, deleted"},
			want: "ok    bucket      arca-prod at https://s3.example: wrote, read, deleted",
		},
		{
			r:    Requirement{Name: NameAuthorizer, Detail: "https://authz.example: allowed the probe resource"},
			want: "fail  authorizer  https://authz.example: allowed the probe resource",
		},
		{
			r:    Requirement{Name: NamePublicURL, OK: true, Detail: "https://arca.example: answers the version endpoint"},
			want: "ok    public-url  https://arca.example: answers the version endpoint",
		},
	} {
		if got := tc.r.Line(); got != tc.want {
			t.Errorf("the line is\n%q\nwant\n%q", got, tc.want)
		}
	}
}

// TestTheReportExitsOneOnAnyFailure: the table goes to stdout and the summary
// to stderr, so a script reads the table and a human reads both.
func TestTheReportExitsOneOnAnyFailure(t *testing.T) {
	found := []Requirement{
		{Name: NameBucket, OK: true, Detail: "reached"},
		{Name: NameDatabase, OK: true, Detail: "reached"},
		{Name: NameAuthorizer, Detail: "allowed the probe resource"},
	}
	var stdout, stderr bytes.Buffer
	if code := Report(found, &stdout, &stderr); code != 1 {
		t.Errorf("a report with a failure exited %d", code)
	}
	if got := strings.Count(stdout.String(), "\n"); got != 3 {
		t.Errorf("the table holds %d lines for 3 requirements", got)
	}
	if want := "arcad: 1 of 3 checks failed"; !strings.Contains(stderr.String(), want) {
		t.Errorf("the summary is %q, want %q", stderr.String(), want)
	}
}

func TestTheReportExitsZeroWhenEveryRequirementPassed(t *testing.T) {
	found := []Requirement{
		{Name: NameBucket, OK: true, Detail: "reached"},
		{Name: NameDatabase, OK: true, Detail: "reached"},
	}
	var stdout, stderr bytes.Buffer
	if code := Report(found, &stdout, &stderr); code != 0 {
		t.Errorf("a report with no failure exited %d", code)
	}
	if want := "arcad: 2 checks passed"; !strings.Contains(stderr.String(), want) {
		t.Errorf("the summary is %q, want %q", stderr.String(), want)
	}
}

// TestTheCommandRefusesAConfigurationItCannotRead: there is nothing to check
// about an installation that will not start, so the command exits 1 with the
// one line naming every problem that a start-up gives.
func TestTheCommandRefusesAConfigurationItCannotRead(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Command(t.Context(), nil, func(string) string { return "" }, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("a bad configuration exited %d, want 1", code)
	}
	if !strings.HasPrefix(stderr.String(), "arcad: ") {
		t.Errorf("the line is %q and does not read as the binary's", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("a refused configuration printed a table: %q", stdout.String())
	}
}

func TestTheCommandRefusesAFlagItDoesNotHave(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Command(t.Context(), []string{"-nosuch"}, func(string) string { return "" }, &stdout, &stderr); code != 2 {
		t.Fatalf("a bad flag exited %d, want 2", code)
	}
}

// TestTheCommandRunsEveryRequirementOfAReadableConfiguration: the whole path
// from the environment to the table, with every dependency absent, so each
// line is a finding and the command still answers one line per requirement.
func TestTheCommandRunsEveryRequirementOfAReadableConfiguration(t *testing.T) {
	gone := unreachable(t)
	env := map[string]string{
		"ARCA_PUBLIC_URL":            gone,
		"ARCA_BUCKET":                "arca-test",
		"ARCA_BUCKET_ENDPOINT":       gone,
		"ARCA_BUCKET_REGION":         "us-east-1",
		"ARCA_BUCKET_PATH_STYLE":     "true",
		"ARCA_DB_URL":                "postgres://arca:arca@127.0.0.1:1/arca?sslmode=disable",
		"ARCA_OIDC_ISSUERS":          gone,
		"ARCA_OIDC_INSECURE_ISSUERS": "true",
	}
	var stdout, stderr bytes.Buffer
	code := Command(t.Context(), nil, func(name string) string { return env[name] }, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("an installation with nothing behind it exited %d, want 1", code)
	}
	for _, name := range []string{NameBucket, NameDatabase, NameIssuer, NameAuthorizer, NamePublicURL} {
		if !strings.Contains(stdout.String(), name) {
			t.Errorf("the table does not hold %q:\n%s", name, stdout.String())
		}
	}
}

// TestTheCheckAndTheNodeOpenOneBucket: the mapping from the configuration to
// the bucket client is the node's too, and a check that reached a different
// bucket would pass an installation the server cannot serve. The test in
// cmd/arcad holds the two functions equal; this one holds this half to every
// bucket variable of spec 002's table.
func TestTheCheckReadsEveryBucketVariable(t *testing.T) {
	cfg := config.Config{
		Bucket: "arca-prod", BucketEndpoint: "https://s3.example", BucketRegion: "eu-central-1",
		BucketAccessKey: "a-key", BucketSecretKey: "a-secret", BucketPathStyle: true,
	}
	want := blob.Options{
		Bucket: "arca-prod", Endpoint: "https://s3.example", Region: "eu-central-1",
		AccessKey: "a-key", SecretKey: "a-secret", PathStyle: true,
	}
	if got := BucketOptions(cfg); got != want {
		t.Errorf("the client is opened with %+v, want %+v", got, want)
	}
}
