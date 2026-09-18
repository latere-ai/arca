// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Command arca-stubs runs the two stubs of spec 014 from flags: the OIDC
// issuer every token is minted at and the authorizer every decision is
// asked of. Arca calls an issuer, an authorizer, and its two stores, and
// nothing else leaves the process, so there is nothing else to stand in
// for.
//
// make run starts it on the loopback interface beside the compose stack,
// and the image of Dockerfile.stubs runs it beside an installation that has
// no issuer of its own yet.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"latere.ai/x/arca/test/stubs/authorizer"
	"latere.ai/x/arca/test/stubs/issuer"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// options is the flag table of spec 014.
type options struct {
	issuerListen, authorizerListen string
	issuerURL, authorizerToken     string
	alg, allow, deny, failMode     string
	limits, filter                 string
	ttl                            int
}

// run parses the flags, starts both stubs, prints one line per listener on
// stdout, and serves until ctx ends. It answers the exit code: 0 on a clean
// stop, 1 when a stub cannot start, 2 on a usage error.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parse(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintln(stderr, "arca-stubs:", err)
		}
		return 2
	}
	stubs, err := build(o)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "arca-stubs:", err)
		return 1
	}
	defer stubs.close()
	if err := stubs.serve(ctx, stdout, stderr); err != nil {
		_, _ = fmt.Fprintln(stderr, "arca-stubs:", err)
		return 1
	}
	return 0
}

// parse reads the flag table. A flag error is reported by the flag set
// itself and returned as it is.
func parse(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("arca-stubs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.issuerListen, "issuer-listen", "0.0.0.0:8081", "the issuer's listen address")
	fs.StringVar(&o.authorizerListen, "authorizer-listen", "0.0.0.0:8082", "the authorizer's listen address")
	fs.StringVar(&o.issuerURL, "issuer-url", "", "the iss of every token and of the discovery document; http://<issuer-listen> by default")
	fs.StringVar(&o.authorizerToken, "authorizer-token", authorizer.DefaultToken, "the bearer the authorizer requires")
	fs.StringVar(&o.alg, "alg", "rs256", "the algorithm the issuer signs with: rs256 or es256")
	fs.StringVar(&o.allow, "allow", "*", "the subjects the authorizer allows when no rule matches, comma separated, * for all")
	fs.StringVar(&o.deny, "deny", "", "an action the authorizer refuses for every subject")
	fs.StringVar(&o.failMode, "fail-mode", "", "an outage the authorizer answers with: timeout, malformed, no-allow, conn-drop, or status:<code>")
	fs.StringVar(&o.limits, "limits", "", "a JSON object every allow carries as its limits")
	fs.StringVar(&o.filter, "filter", "", "a JSON object every allow carries as its filter")
	fs.IntVar(&o.ttl, "ttl", 0, "the seconds every allow is cacheable for")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if o.issuerURL == "" {
		o.issuerURL = "http://" + o.issuerListen
	}
	if o.alg != "rs256" && o.alg != "es256" {
		return o, fmt.Errorf("-alg is %q; the issuer signs with rs256 or es256", o.alg)
	}
	return o, nil
}

// listener is one stub's name, address, and handler.
type listener struct {
	name    string
	addr    string
	handler http.Handler
}

// stubs is what run starts.
type stubs struct {
	issuer    *issuer.Server
	authz     *authorizer.Server
	listeners []listener
}

// build constructs both stubs from the options without listening.
func build(o options) (*stubs, error) {
	s := &stubs{}
	issuerOpts := []issuer.Option{issuer.WithIssuer(o.issuerURL)}
	if o.alg == "es256" {
		issuerOpts = append(issuerOpts, issuer.WithES256())
	}
	s.issuer = issuer.NewHandler(issuerOpts...)
	s.authz = authorizer.NewHandler(
		authorizer.WithToken(o.authorizerToken),
		authorizer.WithAllow(strings.Split(o.allow, ",")...),
	)
	if err := rules(s.authz, o); err != nil {
		return nil, err
	}
	if err := failMode(s.authz, o.failMode); err != nil {
		return nil, err
	}
	s.listeners = []listener{
		{name: "issuer", addr: o.issuerListen, handler: s.issuer.Handler()},
		{name: "authorizer", addr: o.authorizerListen, handler: s.authz.Handler()},
	}
	return s, nil
}

// rules builds the table the flags describe: one allow carrying the ttl,
// the limits, and the filter, and one deny above it when an action is
// named. The table is read from the last row back, so the deny goes last.
func rules(authz *authorizer.Server, o options) error {
	allow := authorizer.Rule{Action: "*", Allow: true, TTL: o.ttl}
	if o.limits != "" {
		if err := json.Unmarshal([]byte(o.limits), &allow.Limits); err != nil {
			return fmt.Errorf("-limits: %w", err)
		}
	}
	if o.filter != "" {
		if err := json.Unmarshal([]byte(o.filter), &allow.Filter); err != nil {
			return fmt.Errorf("-filter: %w", err)
		}
	}
	if o.limits == "" && o.filter == "" && o.ttl == 0 && o.deny == "" {
		return nil
	}
	table := []authorizer.Rule{allow}
	if o.deny != "" {
		table = append(table, authorizer.Rule{Action: o.deny, Allow: false, Reason: "denied by the stub"})
	}
	authz.SetRules(table...)
	return nil
}

// failMode puts the authorizer in one of the outages spec 006 names.
func failMode(authz *authorizer.Server, mode string) error {
	switch {
	case mode == "":
		return nil
	case mode == "timeout":
		authz.Hang()
	case mode == "malformed":
		authz.FailBody(authorizer.BodyMalformed)
	case mode == "no-allow":
		authz.FailBody(authorizer.BodyNoAllow)
	case mode == "conn-drop":
		authz.DropConnections(true)
	case strings.HasPrefix(mode, "status:"):
		status, err := statusOf(mode)
		if err != nil {
			return err
		}
		authz.Fail(status)
	default:
		return fmt.Errorf("-fail-mode is %q; it is timeout, malformed, no-allow, conn-drop, or status:<code>", mode)
	}
	return nil
}

// statusOf reads the code of a status fail mode.
func statusOf(mode string) (int, error) {
	var status int
	if _, err := fmt.Sscanf(mode, "status:%d", &status); err != nil || status < 100 || status > 599 {
		return 0, fmt.Errorf("-fail-mode is %q; a status is three digits", mode)
	}
	return status, nil
}

// close releases what build made.
func (s *stubs) close() {
	if s.issuer != nil {
		s.issuer.Close()
	}
	if s.authz != nil {
		s.authz.Close()
	}
}

// serve binds every listener, prints one line per stub on stdout, and
// serves until ctx ends or a listener fails.
func (s *stubs) serve(ctx context.Context, stdout, stderr io.Writer) error {
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	var lc net.ListenConfig
	var servers []*http.Server
	var open []net.Listener
	failed := make(chan error, len(s.listeners))
	for _, l := range s.listeners {
		ln, err := lc.Listen(ctx, "tcp", l.addr)
		if err != nil {
			for _, bound := range open {
				_ = bound.Close()
			}
			return fmt.Errorf("%s: %w", l.name, err)
		}
		open = append(open, ln)
		srv := &http.Server{
			Handler:           l.handler,
			ReadHeaderTimeout: 10 * time.Second,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		}
		servers = append(servers, srv)
		_, _ = fmt.Fprintf(stdout, "%s listening on %s\n", l.name, ln.Addr())
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				failed <- fmt.Errorf("%s: %w", l.name, err)
			}
		}()
	}
	var err error
	select {
	case <-ctx.Done():
	case err = <-failed:
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdown)
	}
	return err
}
