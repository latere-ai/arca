// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package check

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// The five requirements of spec 012's table, one function each. Each answers
// one line and never an error: a dependency that cannot be reached is the
// finding, not a failure of the command.

// ProbePrefix is the subtree of the bucket the write test uses. It is under
// ARCA_BUCKET_PREFIX like every other key, so an installation sharing a
// bucket by prefix writes its probe inside its own share, and the key is
// deleted on every path out of the check.
const ProbePrefix = "_check/"

// acceptedAlgorithms are the two signatures the verifier of spec 006 reads.
// A key set holding none of them verifies no token this server will ever
// accept, which is a misconfiguration the issuer line reports.
var acceptedAlgorithms = []string{"RS256", "ES256"}

// checkBucket writes one small object under the prefix, reads it back, and
// deletes it. It proves the endpoint, the region, the credentials, the bucket
// name and the write permission in one pass, which a listing alone does not:
// a read-only credential lists a bucket happily and fails the first put.
func checkBucket(ctx context.Context, o Options) Requirement {
	cfg := o.Config
	where := fmt.Sprintf("%s at %s, prefix %s", cfg.Bucket, endpointOf(cfg.BucketEndpoint, cfg.BucketRegion), cfg.BucketPrefix)
	if o.Bucket == nil {
		return failed(NameBucket, "%s: the client could not be built from the configuration: %v", where, o.bucketErr)
	}
	if err := o.Bucket.HeadBucket(ctx); err != nil {
		return failed(NameBucket, "%s: %v", where, err)
	}
	key := cfg.BucketPrefix + ProbePrefix + string(object.NewID())
	// The delete runs on every path out, so a run that failed at the read
	// still leaves the bucket holding nothing of its own.
	defer func() { _ = o.Bucket.Delete(context.WithoutCancel(ctx), key) }()

	body := []byte(probeBody)
	if _, err := o.Bucket.Put(ctx, key, strings.NewReader(probeBody), int64(len(body)), blob.PutOptions{}); err != nil {
		return failed(NameBucket, "%s: the probe key could not be written: %v", where, err)
	}
	read, _, err := o.Bucket.Get(ctx, key)
	if err != nil {
		return failed(NameBucket, "%s: the probe key could not be read back: %v", where, err)
	}
	back, err := io.ReadAll(read)
	_ = read.Close()
	switch {
	case err != nil:
		return failed(NameBucket, "%s: the probe key could not be read back: %v", where, err)
	case string(back) != probeBody:
		return failed(NameBucket, "%s: the probe key read back %d bytes and not the %d written", where, len(back), len(body))
	}
	if err := o.Bucket.Delete(ctx, key); err != nil {
		return failed(NameBucket, "%s: the probe key could not be deleted: %v", where, err)
	}
	return passed(NameBucket, "%s: wrote, read, deleted", where)
}

// probeBody is what the probe key holds, which is enough bytes to prove a
// write and few enough to cost nothing.
//
// The key carries a fresh id per run and the line never names it, so the two
// properties this check needs do not fight: every put of spec 003 carries
// If-None-Match, so a fixed name would make a second concurrent run fail on a
// healthy installation and a run killed before its delete would break every
// run after it, while the output stays identical because the id is not
// printed. A key a run left behind is swept by the reconciliation of spec
// 010 like any other key no row names.
const probeBody = "arcad check\n"

// endpointOf names where the bucket was reached: the endpoint the operator
// set, or the region the SDK derives one from.
func endpointOf(endpoint, region string) string {
	if endpoint != "" {
		return endpoint
	}
	return "the default endpoint of " + region
}

// checkDatabase opens the pool, asks whether the database answers, and
// compares its schema against the migrations this binary carries. A schema
// behind the binary is a deploy whose migration job did not run, and serving
// against one is how a write is lost.
func checkDatabase(ctx context.Context, o Options) Requirement {
	db, err := o.Open(ctx, o.Config.DatabaseURL)
	if err != nil {
		return failed(NameDatabase, "%v", err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		return failed(NameDatabase, "%v", err)
	}
	server := serverVersion(ctx, db.Querier())
	pending, err := o.Pending(ctx, db.Querier())
	switch {
	case err != nil:
		// A dirty flag arrives here, named by the store's own sentence.
		return failed(NameDatabase, "%s, %v", server, err)
	case len(pending) > 0:
		return failed(NameDatabase, "%s, schema behind this binary: %s is not applied; run arcad migrate",
			server, migrationName(pending[0]))
	}
	newest, err := store.Newest()
	if err != nil {
		return failed(NameDatabase, "%s, %v", server, err)
	}
	return passed(NameDatabase, "%s, schema at %s, clean", server, migrationName(newest))
}

// serverVersion is what the database says it is, and a word when it will not
// say. The version is a fact of the installation and not of the run, so it
// is stable between two runs of a healthy one.
func serverVersion(ctx context.Context, q store.Querier) string {
	var version string
	if err := q.QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil {
		return "PostgreSQL"
	}
	return "PostgreSQL " + version
}

// migrationName trims the suffix off a migration file, so the line names the
// migration rather than the file it is embedded as.
func migrationName(file string) string { return strings.TrimSuffix(file, ".up.sql") }

// checkIssuers reads every listed issuer's discovery document and key set. It
// is one line for the list, because the count of lines is the count of rows
// in spec 012's table and an installation listing three issuers does not
// print a table of a different shape; the line names each issuer, and a
// failure names the one that failed.
func checkIssuers(ctx context.Context, o Options) Requirement {
	if len(o.Config.OIDCIssuers) == 0 {
		return failed(NameIssuer, "ARCA_OIDC_ISSUERS is unset, and a request carries a token of a listed issuer")
	}
	var found []string
	for _, issuer := range o.Config.OIDCIssuers {
		keys, algorithms, err := readKeySet(ctx, o.HTTP, issuer)
		switch {
		case err != nil:
			return failed(NameIssuer, "%s: %v", issuer, err)
		case keys == 0:
			return failed(NameIssuer, "%s: the key set holds no key of %s",
				issuer, strings.Join(acceptedAlgorithms, " or "))
		}
		found = append(found, fmt.Sprintf("%s: discovery ok, %d keys, %s",
			issuer, keys, strings.Join(algorithms, " ")))
	}
	return passed(NameIssuer, "%s", strings.Join(found, "; "))
}

// readKeySet answers how many keys of an accepted algorithm an issuer serves
// and which algorithms they are.
//
// The discovery path is the one the verifier of spec 006 reads through the
// shared package, so an issuer this passes is an issuer that package can warm
// against, and one it refuses is refused for the same reason the server would
// refuse it at start-up.
func readKeySet(ctx context.Context, client *http.Client, issuer string) (int, []string, error) {
	var discovery struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := readJSON(ctx, client, strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration", &discovery); err != nil {
		return 0, nil, fmt.Errorf("the discovery document: %w", err)
	}
	if discovery.JWKSURI == "" {
		return 0, nil, errors.New("the discovery document names no jwks_uri")
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Alg string `json:"alg"`
		} `json:"keys"`
	}
	if err := readJSON(ctx, client, discovery.JWKSURI, &set); err != nil {
		return 0, nil, fmt.Errorf("the key set: %w", err)
	}
	count := 0
	var algorithms []string
	for _, key := range set.Keys {
		alg := algorithmOf(key.Alg, key.Kty)
		if alg == "" {
			continue
		}
		count++
		if !slices.Contains(algorithms, alg) {
			algorithms = append(algorithms, alg)
		}
	}
	// The order is the accepted list's and not the key set's, so an issuer
	// that reorders its keys does not change this line.
	slices.SortFunc(algorithms, func(a, b string) int {
		return slices.Index(acceptedAlgorithms, a) - slices.Index(acceptedAlgorithms, b)
	})
	return count, algorithms, nil
}

// algorithmOf is the algorithm a key verifies under, and "" for a key this
// server would never read. A key set is allowed to leave alg out, in which
// case the key type says which of the two it is.
func algorithmOf(alg, kty string) string {
	if alg != "" {
		if slices.Contains(acceptedAlgorithms, alg) {
			return alg
		}
		return ""
	}
	switch kty {
	case "RSA":
		return "RS256"
	case "EC":
		return "ES256"
	}
	return ""
}

// checkAuthorizer sends the probe question every authorizer of the family
// denies for every subject.
//
// An endpoint that allows it is a failure and not a warning: an endpoint that
// allows an action it does not recognise allows every action Arca will ever
// add, which is the one misconfiguration that cannot be noticed from the
// outside.
//
// The client is built here rather than through auth.Start, because that warms
// the verifier against every issuer and an issuer that does not answer would
// fail this line too. One requirement, one dependency.
func checkAuthorizer(ctx context.Context, o Options) Requirement {
	cfg := o.Config
	if cfg.AuthorizerURL == "" {
		return passed(NameAuthorizer,
			"not configured; the owner policy applies, administrators are ARCA_ADMIN_SUBJECTS (%d listed)",
			len(cfg.AdminSubjects))
	}
	client, err := auth.NewClient(auth.ClientOptions{
		URL: cfg.AuthorizerURL, Token: cfg.AuthorizerToken, HTTP: o.HTTP, Timeout: Budget,
	})
	if err != nil {
		return failed(NameAuthorizer, "%s: %v", cfg.AuthorizerURL, err)
	}
	switch err := auth.NewAuthorizer(client).Check(ctx); {
	case errors.Is(err, authz.ErrProbeAllowed):
		return failed(NameAuthorizer, "%s: allowed the probe resource", cfg.AuthorizerURL)
	case err != nil:
		return failed(NameAuthorizer, "%s: %v", cfg.AuthorizerURL, err)
	}
	return passed(NameAuthorizer, "%s: denied the probe resource", cfg.AuthorizerURL)
}

// checkPublicURL reads the version endpoint at the address clients reach this
// installation by. It is the one requirement that tests what the outside sees
// rather than what the server reaches, because ARCA_PUBLIC_URL is the base of
// every URL the server writes and a value naming somewhere else sends every
// client there.
//
// A URL that answers something other than this server's version document is a
// failure: something else is at that address. A URL that cannot be reached at
// all is not, because the check runs beside the server as often as in front
// of it, and a cluster whose ingress does not answer from inside a pod is the
// ordinary case rather than a misconfiguration.
//
// The line names ARCA_BASE_PATH beside the origin, which is the whole of what
// this command can say about it (spec 027). The prefix is not dialled: the
// requirement passes on an unreachable address by design, so a dial there
// could not fail where the prefix is wrong, and the address that proves it is
// the origin the release smoke reads from outside the cluster.
func checkPublicURL(ctx context.Context, o Options) Requirement {
	url := strings.TrimRight(o.Config.PublicURL, "/")
	where := fmt.Sprintf("%s, serving under %s", url, o.Config.BasePath)
	var identity struct {
		Version string `json:"version"`
	}
	switch err := readJSON(ctx, o.HTTP, url+"/version", &identity); {
	case errors.Is(err, errUnreachable):
		return passed(NamePublicURL, "%s: not reachable from here, which is what a check beside the server sees", where)
	case err != nil:
		return failed(NamePublicURL, "%s: %v, so something other than this server answers there", where, err)
	case identity.Version == "":
		return failed(NamePublicURL, "%s: answered no build identity, so something other than this server answers there", where)
	}
	return passed(NamePublicURL, "%s: answers the version endpoint", where)
}

// errUnreachable is a dependency that could not be dialled at all, which the
// public URL alone reads as something other than a failure.
var errUnreachable = errors.New("nothing answered")

// readJSON reads one JSON document. A status outside 200 and a body that is
// not the document asked for are both the finding, so every caller above
// writes one sentence and no caller reads a status code.
func readJSON(ctx context.Context, client *http.Client, url string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%s is not a URL this server can read: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		// Both the sentinel and the transport's own words are kept: the
		// public URL branches on the first and every line prints the second.
		return fmt.Errorf("%w: %w", errUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", url, resp.StatusCode)
	}
	// The body is bounded: a document this large is not one of ours, and an
	// endpoint that streams for ever would otherwise hold the budget open.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDocument)).Decode(into); err != nil {
		return fmt.Errorf("%s answered something that is not the document expected", url)
	}
	return nil
}

// maxDocument bounds a document this command reads.
const maxDocument = 1 << 20
