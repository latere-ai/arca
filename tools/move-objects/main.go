// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/tools/internal/manifest"
)

func main() { os.Exit(cli(context.Background(), os.Args[1:], os.Stdout, os.Stderr)) }

// The exit codes, which are migrate-drive's: an operator scripts the cutover
// around both tools in one sequence, and two siblings that split their codes
// differently is worse than either split. 0 is a move that holds, 1 is
// anything the operator has to answer, and 2 is a flag.
const (
	exitOK      = 0
	exitRefused = 1
	exitUsage   = 2
)

// The variables an installation holds its bucket in. The prefix is compared
// against -prefix, because the objects are moved under one prefix and served
// under another. The credentials are read rather than flagged: a secret on a
// command line is a secret in a shell history.
const (
	BucketPrefixVar = "ARCA_BUCKET_PREFIX"
	AccessKeyVar    = "ARCA_BUCKET_ACCESS_KEY"
	SecretKeyVar    = "ARCA_BUCKET_SECRET_KEY"
)

// DefaultConcurrency is how many keys move at once. A copy is a request the
// store serves without sending a byte anywhere, so the number is about the
// store's request budget and not about bandwidth.
const DefaultConcurrency = 16

// DefaultVerifyMax is the largest object the byte check reads back in full,
// and DefaultVerifySample is the percentage of the larger ones it samples. At
// Drive's sizes almost every object is under the threshold, so the run reads
// the bucket once and the tail costs a tenth of the rest.
const (
	DefaultVerifyMax    = 256 << 20
	DefaultVerifySample = 10
)

// cli runs one move and answers the process exit code.
func cli(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("move-objects", flag.ContinueOnError)
	fs.SetOutput(stderr)
	manifestPath := fs.String("manifest", "", "the manifest migrate-drive wrote, which names every key to move")
	bucket := fs.String("bucket", "", "the bucket holding both the source keys and the destinations")
	endpoint := fs.String("endpoint", "", "the S3 endpoint; empty uses the SDK's default for the region")
	region := fs.String("region", "us-east-1", "the signing region")
	prefix := fs.String("prefix", "drive/", "the bucket prefix, which has to be the manifest's and the installation's")
	pathStyle := fs.Bool("path-style", false, "address the bucket in the path, for a store without virtual hosts")
	concurrency := fs.Int("concurrency", DefaultConcurrency, "how many keys move at once")
	verifyBytes := fs.Bool("verify-bytes", true, "read each destination back and digest it against the manifest's checksum")
	verifyMax := fs.Int64("verify-bytes-max", DefaultVerifyMax, "read back in full every object at or under this size in bytes")
	verifySample := fs.Int("verify-sample", DefaultVerifySample, "the percentage of the objects above that size to read back")
	dryRun := fs.Bool("dry-run", false, "read every destination and every source, and write nothing")
	deleteSources := fs.Bool("delete-sources", false,
		"after every destination is verified, delete each manifest source key whose destination holds its bytes")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "move-objects: %q is not a flag this command takes\n", fs.Arg(0))
		return exitUsage
	}

	if err := run(ctx, options{
		manifest: *manifestPath, bucket: *bucket, endpoint: *endpoint,
		region: *region, prefix: *prefix, pathStyle: *pathStyle,
		concurrency: *concurrency, dryRun: *dryRun, deleteSources: *deleteSources,
		verifyBytes: *verifyBytes, verifyMax: *verifyMax, verifySample: *verifySample,
		bucketPrefix: os.Getenv(BucketPrefixVar),
		accessKey:    os.Getenv(AccessKeyVar), secretKey: os.Getenv(SecretKeyVar),
	}, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return exitRefused
	}
	return exitOK
}

// options is what the flags say, plus the variables the environment does.
type options struct {
	manifest    string
	bucket      string
	endpoint    string
	region      string
	prefix      string
	pathStyle   bool
	concurrency int
	dryRun      bool
	// deleteSources runs the delete pass of the sunset after the move, which
	// removes the source key of every destination the move verified.
	deleteSources bool
	verifyBytes   bool
	verifyMax     int64
	verifySample  int
	bucketPrefix  string
	accessKey     string
	secretKey     string
}

// run reads the manifest, moves every object it names, and prints the report.
func run(ctx context.Context, o options, stdout io.Writer) error {
	prefix, m, err := o.plan()
	if err != nil {
		return err
	}
	bucket, err := blob.NewS3(ctx, blob.Options{
		Bucket:    o.bucket,
		Endpoint:  o.endpoint,
		Region:    o.region,
		AccessKey: o.accessKey,
		SecretKey: o.secretKey,
		PathStyle: o.pathStyle,
	})
	if err != nil {
		return err
	}
	return move(ctx, o, prefix, m, bucket, stdout)
}

// plan reads everything a move needs before it opens a store: the flags that
// have to hold, the manifest, and their agreement. Nothing here reaches a
// bucket, so a refusal costs an operator no request.
func (o options) plan() (string, *manifest.Manifest, error) {
	prefix, err := o.check()
	if err != nil {
		return "", nil, err
	}
	m, err := manifest.ReadFile(o.manifest)
	if err != nil {
		return "", nil, err
	}
	if err := agrees(m, prefix); err != nil {
		return "", nil, err
	}
	return prefix, m, nil
}

// move is the run with the store already open. It is where the report is
// written and the verdict taken, so the whole of that runs against any store,
// the map of spec 003 included.
func move(ctx context.Context, o options, prefix string, m *manifest.Manifest, bucket blob.Store, stdout io.Writer) error {
	report := NewReport(o.manifest, prefix, o.bucket, o.dryRun)
	report.VerifyBytes, report.VerifyMax, report.VerifySample = o.verifyBytes, o.verifyMax, o.verifySample
	mv := &Move{
		Bucket: bucket, Prefix: prefix, DryRun: o.dryRun, Concurrency: o.concurrency,
		VerifyBytes: o.verifyBytes, VerifyMax: o.verifyMax, VerifySample: o.verifySample,
	}
	report.Outcomes = mv.Run(ctx, m.Entries)
	if o.deleteSources {
		// The sunset's pass, and a pass of its own: it begins after the move
		// has read back and verified every destination of the manifest, and it
		// deletes only the sources of the destinations that held.
		report.DeleteSources = true
		sweep := &Sweep{Bucket: bucket, DryRun: o.dryRun, Concurrency: o.concurrency}
		report.Deletions = sweep.Run(ctx, report.Outcomes)
	}
	report.Write(stdout)
	if !report.OK() {
		return errNotClean
	}
	return nil
}

// errNotClean is a move a key did not survive. The report is already printed
// and names every one, so the message here adds the verdict and not a second
// copy of it.
var errNotClean = errors.New("move-objects: the run is not clean; the report above names every key")

// agrees refuses a manifest the move must not act on: one whose copy did not
// finish, and one written under another prefix.
func agrees(m *manifest.Manifest, prefix string) error {
	if !m.Complete {
		return fmt.Errorf("move-objects: the manifest has no completion line, so the copy that wrote it "+
			"was killed or did not verify; rerun migrate-drive against a fresh database and move the objects "+
			"on the manifest it completes (%d keys are listed)", len(m.Entries))
	}
	if m.Prefix != prefix {
		return fmt.Errorf("move-objects: the manifest was written under the prefix %q and -prefix is %q; "+
			"the keys were read under one prefix and would be written under another", m.Prefix, prefix)
	}
	return nil
}

// check reads the flags that have to hold before the store is opened, and
// answers the prefix the destinations are written under.
func (o options) check() (string, error) {
	missing := &Refusal{}
	if o.manifest == "" {
		missing.Refuse("-manifest names the file migrate-drive wrote and is required")
	}
	if o.bucket == "" {
		missing.Refuse("-bucket names the bucket holding the objects and is required")
	}
	if o.region == "" {
		missing.Refuse("-region names the signing region and cannot be empty")
	}
	if o.concurrency < 1 {
		missing.Refuse("-concurrency is %d, and a move copies at least one key at a time", o.concurrency)
	}
	if o.verifyMax < 0 {
		missing.Refuse("-verify-bytes-max is %d, and a size is not negative", o.verifyMax)
	}
	if o.verifySample < 0 || o.verifySample > 100 {
		missing.Refuse("-verify-sample is %d, and a percentage is 0 to 100", o.verifySample)
	}
	// A deleted source is the last other copy of those bytes. The check that
	// proves a destination at a store reporting no checksum of its own is the
	// byte read, so the two flags only make sense together.
	if o.deleteSources && !o.verifyBytes {
		missing.Refuse("-delete-sources deletes the last other copy of every byte it verifies and " +
			"-verify-bytes is off, which leaves the destinations proved on their length alone")
	}
	prefix := normalizePrefix(o.prefix)
	if prefix == "" {
		missing.Refuse("-prefix names the bucket prefix the keys are written under and cannot be empty")
	}
	// The prefix the operator names and the prefix the installation is
	// deployed with are two values that have to agree, because the objects
	// are moved under one and served under the other.
	if deployed := normalizePrefix(o.bucketPrefix); deployed != "" && deployed != prefix {
		missing.Refuse("-prefix is %q and %s is %q; the move and the installation have to name one prefix",
			prefix, BucketPrefixVar, deployed)
	}
	if len(missing.Reasons) > 0 {
		return "", missing
	}
	return prefix, nil
}

// Refusal is every reason a run stopped before it moved anything, answered at
// once, so an operator fixes one command line rather than discovering the
// next reason on the next attempt.
type Refusal struct{ Reasons []string }

func (r *Refusal) Error() string {
	return "move-objects: the move is refused and nothing was written\n  " +
		strings.Join(r.Reasons, "\n  ")
}

// Refuse records one reason.
func (r *Refusal) Refuse(format string, args ...any) {
	r.Reasons = append(r.Reasons, fmt.Sprintf(format, args...))
}

// normalizePrefix ends a prefix in one slash and begins it in none, which is
// the shape a key derivation reads it in (spec 003).
func normalizePrefix(prefix string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}
