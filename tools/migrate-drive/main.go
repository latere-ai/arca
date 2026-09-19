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
	dryRun := fs.Bool("dry-run", false, "read, rewrite and report, and write nothing")
	prefix := fs.String("prefix", "drive/", "the bucket prefix every key in the source carries")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "migrate-drive: %q is not a flag this command takes\n", fs.Arg(0))
		return exitUsage
	}

	if err := run(ctx, options{
		source: *source, target: *target, issuer: *issuer,
		orgSubjects: *orgSubjects, dryRun: *dryRun, prefix: *prefix,
		bucketPrefix: os.Getenv(BucketPrefixVar),
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
	dryRun         bool
	prefix         string
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
	report.Issuer, report.Prefix, report.DryRun = o.issuer, prefix, o.dryRun

	r := NewRun(source, target, NewRewriter(o.issuer, orgs), prefix, o.dryRun, report)
	if err := Preflight(ctx, r); err != nil {
		return err
	}
	if err := r.Copy(ctx); err != nil {
		return err
	}
	if err := Verify(ctx, r); err != nil {
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
	} else if u, err := url.Parse(o.issuer); err != nil || u.Scheme == "" || u.Host == "" {
		missing.Refuse("-issuer is %q, which is not an absolute URL", o.issuer)
	}
	prefix := normalisePrefix(o.prefix)
	if prefix == "" {
		missing.Refuse("-prefix names the bucket prefix the source wrote and cannot be empty")
	}
	// The prefix the operator names and the prefix the installation is
	// deployed with are two values that have to agree, because the copy is
	// read against one and served against the other.
	if deployed := normalisePrefix(o.bucketPrefix); deployed != "" && deployed != prefix {
		missing.Refuse("-prefix is %q and %s is %q; the copy and the installation have to name one prefix",
			prefix, BucketPrefixVar, deployed)
	}
	if len(missing.Reasons) > 0 {
		return "", missing
	}
	return prefix, nil
}

// normalisePrefix ends a prefix in one slash and begins it in none, which is
// the shape a key derivation reads it in (spec 003).
func normalisePrefix(prefix string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}
