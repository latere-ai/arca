// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package check

import (
	"context"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"

	"latere.ai/x/arca/internal/config"
	"latere.ai/x/arca/internal/store"
)

// One healthy run and one fault per requirement, which is criteria 12 and 13
// of spec 012. Every dependency is a test server or a fake, so the table runs
// with no stack beside it.

// world is one installation as the check reads it: the configuration and the
// seams behind it, healthy unless a case breaks one.
type world struct {
	options Options
	bucket  *faultyBucket
	issuer  *issuertest.Server
}

// healthy builds an installation every requirement passes against.
func healthy(t *testing.T) *world {
	t.Helper()
	iss := issuertest.New(t, issuertest.WithDefaultAudience("arca"))
	authorizer := endpoint(t, false)
	public := versionEndpoint(t, map[string]string{
		"version": "v0.1.0", "commit": "abc1234", "build_time": "2026-09-18T10:02:11Z",
	}, 200)
	b := bucket()
	w := &world{bucket: b, issuer: iss}
	w.options = Options{
		Config: config.Config{
			Bucket: "arca-test", BucketEndpoint: "https://s3.example",
			BucketRegion: "us-east-1", BucketPrefix: "arca/",
			DatabaseURL:   "postgres://arca@127.0.0.1/arca",
			PublicURL:     public.URL,
			BasePath:      config.DefaultBasePath,
			OIDCIssuers:   []string{iss.URL()},
			AuthorizerURL: authorizer.URL, AuthorizerToken: "a-token",
		},
		Bucket: b,
		Open: func(context.Context, string) (Database, error) {
			return &fakeDatabase{version: "18.0"}, nil
		},
		Pending: func(context.Context, store.Querier) ([]string, error) { return nil, nil },
	}
	return w
}

// run answers the report of one installation, keyed by requirement.
func (w *world) run(t *testing.T) map[string]Requirement {
	t.Helper()
	out := map[string]Requirement{}
	found := Run(t.Context(), w.options)
	if len(found) != 5 {
		t.Fatalf("the run answered %d lines, and spec 012's table has five rows", len(found))
	}
	for _, r := range found {
		out[r.Name] = r
	}
	return out
}

// passes fails the test when the named requirement did not pass.
func (w *world) passes(t *testing.T, name string) Requirement {
	t.Helper()
	got := w.run(t)[name]
	if !got.OK {
		t.Fatalf("%s failed: %s", name, got.Detail)
	}
	return got
}

// fails fails the test when the named requirement passed, and answers its
// line so a case reads what it said.
func (w *world) fails(t *testing.T, name string) Requirement {
	t.Helper()
	got := w.run(t)[name]
	if got.OK {
		t.Fatalf("%s passed and the case broke it: %s", name, got.Detail)
	}
	return got
}

// TestEveryRequirementPassesAgainstAHealthyInstallation is the first half of
// criterion 12: five lines, all ok, in the order of spec 012's table.
func TestEveryRequirementPassesAgainstAHealthyInstallation(t *testing.T) {
	w := healthy(t)
	found := Run(t.Context(), w.options)
	want := []string{NameBucket, NameDatabase, NameIssuer, NameAuthorizer, NamePublicURL}
	for i, r := range found {
		if r.Name != want[i] {
			t.Errorf("line %d is %q, and the table's row is %q", i, r.Name, want[i])
		}
		if !r.OK {
			t.Errorf("%s failed: %s", r.Name, r.Detail)
		}
	}
}

// TestTwoRunsOfAHealthyInstallationPrintTheSameThing is the second half:
// nothing on a line is a value that differs between runs, so an operator
// comparing two runs sees a difference only where one appeared.
func TestTwoRunsOfAHealthyInstallationPrintTheSameThing(t *testing.T) {
	w := healthy(t)
	first := lines(Run(t.Context(), w.options))
	for range 3 {
		if got := lines(Run(t.Context(), w.options)); got != first {
			t.Fatalf("two runs printed\n%s\nand\n%s", first, got)
		}
	}
}

// lines renders a report the way it is printed.
func lines(found []Requirement) string {
	var b strings.Builder
	for _, r := range found {
		b.WriteString(r.Line())
		b.WriteString("\n")
	}
	return b.String()
}

// TestTheBucketLineNamesWhatItReached: the line says the bucket, the
// endpoint and the prefix, which is what an operator compares against the
// deployment.
func TestTheBucketLineNamesWhatItReached(t *testing.T) {
	w := healthy(t)
	got := w.passes(t, NameBucket)
	for _, want := range []string{"arca-test", "https://s3.example", "arca/", "wrote, read, deleted"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("the line is %q and does not name %q", got.Detail, want)
		}
	}
}

// TestTheBucketLineNamesTheRegionWhereNoEndpointIsSet: the SDK derives one,
// so the line says which region it derived it from rather than nothing.
func TestTheBucketLineNamesTheRegionWhereNoEndpointIsSet(t *testing.T) {
	w := healthy(t)
	w.options.Config.BucketEndpoint = ""
	if got := w.passes(t, NameBucket); !strings.Contains(got.Detail, "us-east-1") {
		t.Errorf("the line is %q and does not name the region", got.Detail)
	}
}

// TestTheCheckDeletesTheObjectItWrote is criterion 14: the bucket holds
// nothing under the probe prefix once a run ends.
func TestTheCheckDeletesTheObjectItWrote(t *testing.T) {
	w := healthy(t)
	w.passes(t, NameBucket)
	page, err := w.bucket.List(t.Context(), w.options.Config.BucketPrefix+ProbePrefix, "", 10)
	if err != nil {
		t.Fatalf("list the probe prefix: %v", err)
	}
	if len(page.Keys) != 0 {
		t.Errorf("the bucket still holds %v under the probe prefix", page.Keys)
	}
}

// TestASecondRunPassesWhateverThePreviousOneLeftBehind: every put of spec 003
// carries If-None-Match, so a probe key of a fixed name would make the second
// of two overlapping runs fail on a healthy installation, and a run killed
// before its delete would break every run after it. The key carries a fresh
// id per run, and the line does not name it, so the report stays identical.
func TestASecondRunPassesWhateverThePreviousOneLeftBehind(t *testing.T) {
	w := healthy(t)
	// A run that died before its delete, which is the state a killed check
	// or a crashed pod leaves the bucket in.
	w.bucket.del = errRefused
	w.fails(t, NameBucket)
	w.bucket.del = nil

	first := w.passes(t, NameBucket)
	second := w.passes(t, NameBucket)
	if first.Line() != second.Line() {
		t.Errorf("two runs printed\n%s\nand\n%s", first.Line(), second.Line())
	}
}

// TestTheCheckDeletesTheObjectItWroteEvenWhenTheRunFailed: the delete runs on
// every path out, so a run that died at the read leaves nothing behind.
func TestTheCheckDeletesTheObjectItWroteEvenWhenTheRunFailed(t *testing.T) {
	w := healthy(t)
	w.bucket.get = errRefused
	w.fails(t, NameBucket)
	page, err := w.bucket.List(t.Context(), w.options.Config.BucketPrefix+ProbePrefix, "", 10)
	if err != nil {
		t.Fatalf("list the probe prefix: %v", err)
	}
	if len(page.Keys) != 0 {
		t.Errorf("a failed run left %v in the bucket", page.Keys)
	}
}

// TestEachRequirementFailsOnItsOwnFault is criterion 13: one case per failure
// spec 012 names, each naming the requirement that failed.
func TestEachRequirementFailsOnItsOwnFault(t *testing.T) {
	for _, tc := range []struct {
		name  string
		which string
		fault func(*world)
		says  string
	}{
		{
			name: "the bucket is unreachable", which: NameBucket,
			fault: func(w *world) { w.bucket.head = errRefused },
			says:  "arca-test",
		},
		{
			name: "the bucket cannot be written under the prefix", which: NameBucket,
			fault: func(w *world) { w.bucket.put = errRefused },
			says:  "the probe key could not be written",
		},
		{
			name: "the probe key cannot be deleted", which: NameBucket,
			fault: func(w *world) { w.bucket.del = errRefused },
			says:  "could not be deleted",
		},
		{
			name: "the database is unreachable", which: NameDatabase,
			fault: func(w *world) {
				w.options.Open = func(context.Context, string) (Database, error) { return nil, errRefused }
			},
			says: "refused",
		},
		{
			name: "the database does not answer", which: NameDatabase,
			fault: func(w *world) {
				w.options.Open = func(context.Context, string) (Database, error) {
					return &fakeDatabase{ping: errRefused}, nil
				}
			},
			says: "refused",
		},
		{
			name: "the schema is behind the embedded migrations", which: NameDatabase,
			fault: func(w *world) {
				w.options.Pending = func(context.Context, store.Querier) ([]string, error) {
					return []string{"0005_usage_events.up.sql"}, nil
				}
			},
			says: "0005_usage_events is not applied; run arcad migrate",
		},
		{
			name: "the schema is half applied", which: NameDatabase,
			fault: func(w *world) {
				w.options.Pending = func(context.Context, store.Querier) ([]string, error) {
					return nil, errRefused
				}
			},
			says: "refused",
		},
		{
			name: "an issuer's discovery does not answer", which: NameIssuer,
			fault: func(w *world) { w.options.Config.OIDCIssuers = []string{unreachable(t)} },
			says:  "the discovery document",
		},
		{
			name: "no issuer is listed", which: NameIssuer,
			fault: func(w *world) { w.options.Config.OIDCIssuers = nil },
			says:  "ARCA_OIDC_ISSUERS is unset",
		},
		{
			name: "the authorizer allows the probe", which: NameAuthorizer,
			fault: func(w *world) { w.options.Config.AuthorizerURL = endpoint(t, true).URL },
			says:  "allowed the probe resource",
		},
		{
			name: "the authorizer does not answer", which: NameAuthorizer,
			fault: func(w *world) { w.options.Config.AuthorizerURL = unreachable(t) },
			says:  "",
		},
		{
			name: "something else answers the public URL", which: NamePublicURL,
			fault: func(w *world) {
				w.options.Config.PublicURL = versionEndpoint(t, map[string]string{"service": "something else"}, 200).URL
			},
			says: "answered no build identity",
		},
		{
			name: "the public URL answers a status the version endpoint does not", which: NamePublicURL,
			fault: func(w *world) {
				w.options.Config.PublicURL = versionEndpoint(t, map[string]string{}, 503).URL
			},
			says: "503",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := healthy(t)
			tc.fault(w)
			got := w.fails(t, tc.which)
			if tc.says != "" && !strings.Contains(got.Detail, tc.says) {
				t.Errorf("the line is %q and does not say %q", got.Detail, tc.says)
			}
			// A failure of one requirement is a failure of that requirement
			// alone: the operator reads the other four.
			for name, other := range w.run(t) {
				if name != tc.which && !other.OK {
					t.Errorf("%s failed too: %s", name, other.Detail)
				}
			}
		})
	}
}

// TestABucketClientThatCouldNotBeBuiltIsTheBucketLinesFailure: a
// configuration the SDK will not build a client from is reported on the
// bucket's own line, so the other four requirements are still answered.
func TestABucketClientThatCouldNotBeBuiltIsTheBucketLinesFailure(t *testing.T) {
	w := healthy(t)
	got := checkBucket(t.Context(), Options{Config: w.options.Config, bucketErr: errRefused})
	if got.OK {
		t.Fatalf("a run with no client passed: %s", got.Detail)
	}
	if !strings.Contains(got.Detail, "could not be built") || !strings.Contains(got.Detail, "refused") {
		t.Errorf("the line is %q", got.Detail)
	}
}

// TestAnIssuerWithNoUsableKeyIsAFailure: a key set holding no key of an
// algorithm the verifier reads verifies no token this server will accept.
func TestAnIssuerWithNoUsableKeyIsAFailure(t *testing.T) {
	w := healthy(t)
	w.options.Config.OIDCIssuers = []string{keySet(t, `{"keys":[{"kty":"oct","alg":"HS256"}]}`)}
	if got := w.fails(t, NameIssuer); !strings.Contains(got.Detail, "no key of RS256 or ES256") {
		t.Errorf("the line is %q", got.Detail)
	}
}

// TestTheIssuerLineNamesTheKeysAndTheAlgorithms: the line an operator reads
// says how many keys answered and which algorithms they carry.
func TestTheIssuerLineNamesTheKeysAndTheAlgorithms(t *testing.T) {
	w := healthy(t)
	got := w.passes(t, NameIssuer)
	for _, want := range []string{w.issuer.URL(), "discovery ok", "keys", "RS256"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("the line is %q and does not name %q", got.Detail, want)
		}
	}
}

// TestAKeyWithNoAlgorithmIsReadFromItsType: a key set is allowed to leave alg
// out, and the key type says which of the two signatures it is.
func TestAKeyWithNoAlgorithmIsReadFromItsType(t *testing.T) {
	w := healthy(t)
	w.options.Config.OIDCIssuers = []string{keySet(t, `{"keys":[{"kty":"EC"},{"kty":"RSA"}]}`)}
	got := w.passes(t, NameIssuer)
	if !strings.Contains(got.Detail, "2 keys") || !strings.Contains(got.Detail, "RS256 ES256") {
		t.Errorf("the line is %q", got.Detail)
	}
}

// TestADiscoveryDocumentWithNoKeySetIsAFailure: an issuer that answers
// discovery and names no key set serves nothing the verifier can read.
func TestADiscoveryDocumentWithNoKeySetIsAFailure(t *testing.T) {
	w := healthy(t)
	w.options.Config.OIDCIssuers = []string{discovery(t, "")}
	if got := w.fails(t, NameIssuer); !strings.Contains(got.Detail, "names no jwks_uri") {
		t.Errorf("the line is %q", got.Detail)
	}
}

// TestAnAuthorizerThatIsNotConfiguredIsNotAFailure: the owner policy applies,
// and the line says so and names how many subjects it admits, so the table
// holds five rows either way.
func TestAnAuthorizerThatIsNotConfiguredIsNotAFailure(t *testing.T) {
	w := healthy(t)
	w.options.Config.AuthorizerURL = ""
	w.options.Config.AdminSubjects = []string{"https://issuer.example|9ab3", "https://issuer.example|c1d0"}
	got := w.passes(t, NameAuthorizer)
	if !strings.Contains(got.Detail, "the owner policy applies") || !strings.Contains(got.Detail, "(2 listed)") {
		t.Errorf("the line is %q", got.Detail)
	}
}

// TestThePublicURLLineReportsTheBasePath is criterion 10 of spec 027: the
// one line about the address clients reach names the base the surface is
// served under, so an operator running check beside a replica reads where
// its routes answer rather than inferring it from a 404 at the origin.
//
// The prefix is reported and not dialled, on the reachable address and the
// unreachable one alike: this requirement passes on an address nothing
// answers by design, so a dial here could not fail where the prefix is
// wrong. The origin is proved by the release smoke instead.
func TestThePublicURLLineReportsTheBasePath(t *testing.T) {
	w := healthy(t)
	w.options.Config.BasePath = "/v1/storage"
	got := w.passes(t, NamePublicURL)
	if !strings.Contains(got.Detail, "serving under /v1/storage") {
		t.Errorf("the line is %q and does not name the base path", got.Detail)
	}
	if !strings.Contains(got.Detail, w.options.Config.PublicURL) {
		t.Errorf("the line is %q and does not name the origin", got.Detail)
	}

	away := healthy(t)
	away.options.Config.BasePath = "/v1/storage"
	away.options.Config.PublicURL = unreachable(t)
	if line := away.passes(t, NamePublicURL); !strings.Contains(line.Detail, "serving under /v1/storage") {
		t.Errorf("the line of an unreachable origin is %q and does not name the base path", line.Detail)
	}
}

// TestAPublicURLThatCannotBeReachedIsNotAFailure: the check runs beside the
// server as often as in front of it, and a cluster whose ingress does not
// answer from inside a pod is the ordinary case rather than a
// misconfiguration.
func TestAPublicURLThatCannotBeReachedIsNotAFailure(t *testing.T) {
	w := healthy(t)
	w.options.Config.PublicURL = unreachable(t)
	if got := w.passes(t, NamePublicURL); !strings.Contains(got.Detail, "not reachable from here") {
		t.Errorf("the line is %q", got.Detail)
	}
}
