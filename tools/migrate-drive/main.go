// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

func main() { os.Exit(cli(context.Background(), os.Args[1:], os.Stdout, os.Stderr)) }

// The exit codes. An operator scripts the cutover around them: 0 is a copy
// that holds, 1 is anything the operator has to answer, and 2 is a flag.
const (
	exitOK      = 0
	exitRefused = 1
	exitUsage   = 2
)

// BucketPrefixVar is the variable an installation sets its prefix in. The run
// compares -prefix against it when it is set, so a copy cannot be made under
// one prefix for a server deployed with another.
const BucketPrefixVar = "ARCA_BUCKET_PREFIX"

// cli runs one copy and answers the process exit code.
func cli(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate-drive", flag.ContinueOnError)
	fs.SetOutput(stderr)
	source := fs.String("source", "", "the URL of Drive's database, which the run only reads")
	target := fs.String("target", "", "the URL of Arca's database, already migrated to the newest version")
	issuer := fs.String("issuer", "", "the issuer URL every personal subject is prefixed with")
	orgSubjects := fs.String("org-subjects", "", "a JSON or CSV file mapping Drive organization ids to subjects")
	orgIssuer := fs.String("org-issuer", "", "the issuer every organization subject is prefixed with, instead of a mapping file")
	dryRun := fs.Bool("dry-run", false, "read, rewrite and report, and write nothing")
	prefix := fs.String("prefix", "drive/", "the bucket prefix every key in the source carries")
	manifest := fs.String("manifest", "", "write the manifest of source keys and minted object ids here, which tools/move-objects reads")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "migrate-drive: %q is not a flag this command takes\n", fs.Arg(0))
		return exitUsage
	}
	// The two ways to name an organization's subject are alternatives, and a
	// run given both would have to decide which wins. That is a command line
	// to correct, not a copy to attempt.
	if *orgSubjects != "" && *orgIssuer != "" {
		_, _ = fmt.Fprintf(stderr, "migrate-drive: -org-subjects and -org-issuer both name what an organization's "+
			"subject is; give the file when the platform assigns subjects and the issuer when it derives them\n")
		return exitUsage
	}

	if err := run(ctx, options{
		source: *source, target: *target, issuer: *issuer,
		orgSubjects: *orgSubjects, orgIssuer: *orgIssuer,
		dryRun: *dryRun, prefix: *prefix,
		manifest: *manifest, bucketPrefix: os.Getenv(BucketPrefixVar),
	}, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return exitRefused
	}
	return exitOK
}

// options is what the flags say, plus the one variable the environment does.
type options struct {
	source, target string
	issuer         string
	orgSubjects    string
	orgIssuer      string
	dryRun         bool
	prefix         string
	manifest       string
	bucketPrefix   string
}

// run reads the source, writes the target, and prints the report.
func run(ctx context.Context, o options, stdout io.Writer) error {
	prefix, err := o.check()
	if err != nil {
		return err
	}
	orgs, err := ReadMapping(o.orgSubjects)
	if err != nil {
		return err
	}

	source, err := openPool(ctx, o.source)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := openPool(ctx, o.target)
	if err != nil {
		return err
	}
	defer target.Close()

	report := NewReport(TableNames())
	report.Source, report.Target = redact(o.source), redact(o.target)
	report.Issuer, report.OrgIssuer = o.issuer, o.orgIssuer
	report.Prefix, report.DryRun = prefix, o.dryRun

	r := NewRun(source, target, NewRewriter(o.issuer, o.orgIssuer, orgs), prefix, o.dryRun, report)
	r.Manifest = o.manifest
	if err := Preflight(ctx, r); err != nil {
		return err
	}
	// The manifest is written before the first table commits, so every
	// object id the copy is about to hand out is an id the file already
	// names. It is marked complete after the verification and not before.
	if err := WriteManifest(r); err != nil {
		return err
	}
	if err := r.Copy(ctx); err != nil {
		return err
	}
	if err := Verify(ctx, r); err != nil {
		return err
	}
	if err := CompleteManifest(r); err != nil {
		return err
	}
	report.Write(stdout)
	if !report.OK() {
		return errNotVerified
	}
	return nil
}

// errNotVerified is a run whose report a table did not hold in. The report is
// already printed and names the table, so the message here adds the verdict
// and not a second copy of it.
var errNotVerified = errors.New("migrate-drive: the copy is not verified; the report above names the table")

// check reads the flags that have to hold before a database is opened, and
// answers the prefix the run compares keys against.
func (o options) check() (string, error) {
	missing := &Refusal{}
	if o.source == "" {
		missing.Refuse("-source names Drive's database and is required")
	}
	if o.target == "" {
		missing.Refuse("-target names Arca's database and is required")
	}
	if o.issuer == "" {
		missing.Refuse("-issuer names the issuer every personal subject carries and is required")
	} else if !absolute(o.issuer) {
		missing.Refuse("-issuer is %q, which is not an absolute URL", o.issuer)
	}
	if o.orgIssuer != "" && !absolute(o.orgIssuer) {
		missing.Refuse("-org-issuer is %q, which is not an absolute URL", o.orgIssuer)
	}
	prefix := normalizePrefix(o.prefix)
	if prefix == "" {
		missing.Refuse("-prefix names the bucket prefix the source wrote and cannot be empty")
	}
	// The prefix the operator names and the prefix the installation is
	// deployed with are two values that have to agree, because the copy is
	// read against one and served against the other.
	if deployed := normalizePrefix(o.bucketPrefix); deployed != "" && deployed != prefix {
		missing.Refuse("-prefix is %q and %s is %q; the copy and the installation have to name one prefix",
			prefix, BucketPrefixVar, deployed)
	}
	if len(missing.Reasons) > 0 {
		return "", missing
	}
	return prefix, nil
}

// absolute reports whether a value is an absolute URL, which an issuer has to
// be: a subject is the issuer and the sub, and a relative issuer would render
// a subject no verifier ever produces.
func absolute(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme != "" && u.Host != ""
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
