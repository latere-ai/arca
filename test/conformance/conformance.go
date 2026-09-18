// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package conformance is the contract of spec 013 as an executable test
// package (spec 017): Run drives net/http against any base URL, an Arca
// installation or an alternative implementation, and asserts every route of
// that spec's table, every code of its error table the suite can provoke,
// the pagination and the conditional requests it fixes, and every invariant
// of spec 001 that is visible on the wire.
//
// The suite is black box. It holds no import of internal/, opens no database
// connection, and reaches no bucket except through a presigned URL the
// server handed it. A consumer that puts its own front in front of Arca runs
// it against that front to prove the edge did not change what a request
// means, and an alternative implementation runs it to claim it serves the
// Arca API.
//
// Every path, workspace slug, share and link a run creates carries the
// prefix arca-conformance-<run>-, where <run> is drawn at start. The run
// records the id of everything it creates and deletes exactly those ids at
// the end, never by prefix and never by listing, so two runs against one
// installation touch nothing of each other's and a run against a live
// installation leaves what somebody else put there alone.
//
// Three things make a case not run, and they are kept apart because they
// mean different things. A group whose input in [Options] is empty skips,
// once, with the reason in [Report.Reasons]. A case the caller named in
// [Options.Skip] skips, with that as its reason. A case whose routes the
// target does not serve joins the pending group, which fails: a route spec
// 013 names and a build does not answer is a build that does not serve the
// contract yet, and hiding it behind a skip would report a partial build as
// a conforming one.
package conformance

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Options is what a run drives. URL and Token are required; every other
// field admits a group of cases, and a group whose field is empty skips
// with the reason in the report rather than silently.
type Options struct {
	// URL is the base URL of the installation, without /v1.
	URL string

	// Token mints a bearer for a subject. The suite asks for three:
	// "alice" and "bob", two unrelated principals, and whatever subject
	// Admin names. A target with a fixed set of tokens returns them.
	Token func(ctx context.Context, subject string) (string, error)
	// Admin is the subject the target treats as an administrator. Empty
	// skips the administration group and the usage group's overview half.
	Admin string

	// AuthorizerControl is the stub authorizer's control URL, the shared
	// control API of latere.ai/x/pkg/authz/stub. Empty skips the deny, the
	// outage and the byte limit cases.
	AuthorizerControl string
	// Anonymous reports that the target serves public links to an
	// unauthenticated caller. Empty skips the anonymous half of the links
	// group.
	Anonymous bool

	// Skip names group or case names to skip, each reported as skipped by
	// request. A case name is <NNN>/<Name>; a group name skips every case
	// of that group.
	Skip []string
}

// Report is what a run did.
type Report struct {
	// Passed, Failed and Skipped are case names, <NNN>/<Name>.
	Passed, Failed, Skipped []string
	// SkippedGroups names each group that skipped whole, once.
	SkippedGroups []string
	// Reasons is why each skipped case skipped, keyed by case name.
	Reasons map[string]string
	// Created is every object the run made and deleted.
	Created []string
	// Unverified names each assertion the target's shape gave no way to
	// make, rather than one it failed.
	Unverified []string
	// Pending names each route of spec 013 the target does not serve, with
	// the spec that owns it. It is empty against a complete build, and the
	// pending group fails while it is not.
	Pending []string
}

// The groups of spec 017's table. A group is a string on a case, and the
// input it needs is answered by [session.has].
const (
	GroupIdentity      = "identity"
	GroupAuthorizer    = "authorizer"
	GroupPaths         = "paths"
	GroupFiles         = "files"
	GroupBytes         = "bytes"
	GroupVersions      = "versions"
	GroupTrash         = "trash"
	GroupStars         = "stars"
	GroupUploads       = "uploads"
	GroupShares        = "shares"
	GroupLinks         = "links"
	GroupWorkspaces    = "workspaces"
	GroupConditional   = "conditional-writes"
	GroupUsage         = "usage"
	GroupEvents        = "events"
	GroupAdministraton = "administration"
	GroupErrors        = "errors"
	// GroupPending is not a group of the table. It is the one case that
	// reports every route spec 013 names and the target does not serve, and
	// it fails while any is outstanding.
	GroupPending = "pending"
)

// Groups lists every group in the order of spec 017's table, with the
// pending group last.
var Groups = []string{
	GroupIdentity, GroupAuthorizer, GroupPaths, GroupFiles, GroupBytes,
	GroupVersions, GroupTrash, GroupStars, GroupUploads, GroupShares,
	GroupLinks, GroupWorkspaces, GroupConditional, GroupUsage, GroupEvents,
	GroupAdministraton, GroupErrors, GroupPending,
}

// Prefix begins the name of everything a run creates, ahead of the run's own
// value. A target that holds objects under it holds nothing but a run's.
const Prefix = "arca-conformance-"

// The two principals every run drives. They are unrelated: neither owns the
// other's space and neither is an administrator, so a request one makes for
// the other's space proves invariant 6.
const (
	Alice = "alice"
	Bob   = "bob"
)

// caseTimeout bounds one case, so a target that never answers fails the case
// it hung rather than the run.
const caseTimeout = 2 * time.Minute

// testCase is one row: its name under the spec, the group whose input admits
// it, the routes of spec 013 it drives, and the assertion.
type testCase struct {
	name   string
	group  string
	routes []string
	run    func(t *testing.T, s *session)
}

// specCases is one spec's rows.
type specCases struct {
	number string
	cases  []testCase
}

// cases is every row, in run order. The order is the order of the specs
// except that identity runs first, because a target that refuses every
// bearer fails one case there rather than every case everywhere, and
// pending runs last, so its list is read after the cases that could run
// have.
func cases() []specCases {
	return []specCases{
		{"006", cases006()},
		{"013", cases013()},
		{"005", cases005()},
		{"007", cases007()},
		{"008", cases008()},
		{"009", cases009()},
		{"010", cases010()},
		{"012", cases012()},
		{"017", []testCase{{name: "Pending", group: GroupPending, run: case017Pending}}},
	}
}

// session is one run: the options, the client, the tokens it minted, the
// served surface it read, and everything it created.
type session struct {
	options Options
	client  *http.Client

	// served is the set of routes the target answers, read from the
	// document spec 013 publishes. A route of the table that is absent puts
	// every case that drives it into the pending group.
	served surface
	// run is the value that makes this run's names its own.
	run string

	mu         sync.Mutex
	ctx        context.Context
	tokens     map[string]string
	subjects   map[string]string
	created    []created
	skipped    map[string]string
	unverified []string
}

// created is one object a run made, with what it takes to delete it.
type created struct {
	// what names the resource for the report: "workspace 01J8...".
	what string
	// remove deletes it. It answers an error the cleanup reports and the
	// run does not fail on, because a target that refuses a delete has
	// already failed the case that made the object.
	remove func(t testing.TB) error
}

// Run drives every case the options admit against the target and reports
// what happened. Every case is a subtest of t named <NNN>/<Name>.
func Run(t *testing.T, opts Options) (report Report) {
	t.Helper()
	if err := validate(opts); err != nil {
		t.Fatalf("conformance: %v", err)
	}
	opts.URL = strings.TrimRight(opts.URL, "/")
	s := newSession(opts)
	report.Reasons = map[string]string{}
	defer func() { report.Created = s.cleanup(t) }()

	s.served = s.readSurface(t)
	report.Pending = s.served.pending()
	s.warm(t)

	for _, sp := range cases() {
		t.Run(sp.number, func(t *testing.T) {
			for _, c := range sp.cases {
				s.runCase(t, sp.number, c, &report)
			}
		})
	}

	s.mu.Lock()
	report.Unverified = slices.Clone(s.unverified)
	s.mu.Unlock()
	return report
}

// validate answers what a run needs and has not been given. Run states it
// rather than driving a base URL of "" and reporting every case as failed,
// which would read as a target that answers nothing.
func validate(opts Options) error {
	switch {
	case opts.URL == "":
		return errors.New("Options needs a URL, the base URL of the installation without /v1")
	case opts.Token == nil:
		return errors.New("Options needs a Token that mints a bearer for a subject")
	}
	return nil
}

// newSession builds the run's state. The client does not follow a redirect:
// a read above the inline threshold answers one, and the case asserts the
// answer rather than the object behind it.
func newSession(opts Options) *session {
	return &session{
		options:  opts,
		run:      drawRunID(),
		tokens:   map[string]string{},
		subjects: map[string]string{},
		skipped:  map[string]string{},
		client: &http.Client{
			// The transport is stated rather than left to the package
			// default, so the suite drives a connection pool of its own and a
			// consumer that set a default transport for its own calls does
			// not change what a case sends. It carries no tracing: the suite
			// is a client outside the installation, and a trace it started
			// would be a trace of the test rather than of a request the
			// server was asked to serve.
			Transport:     &http.Transport{},
			Timeout:       caseTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// runCase runs one case as a subtest and records its outcome. A case that
// skips is recorded with its reason, and a group that skipped whole is named
// once.
func (s *session) runCase(t *testing.T, number string, c testCase, report *Report) {
	t.Helper()
	name := number + "/" + c.name
	ok := t.Run(c.name, func(t *testing.T) {
		if reason, skip := s.why(name, c); skip {
			s.skip(t, name, reason)
		}
		ctx, cancel := context.WithTimeout(t.Context(), caseTimeout)
		defer cancel()
		s.setContext(ctx)
		c.run(t, s)
	})
	switch reason, skipped := s.reason(name); {
	case skipped:
		report.Skipped = append(report.Skipped, name)
		report.Reasons[name] = reason
		if c.group != "" && !s.has(c.group) && !slices.Contains(report.SkippedGroups, c.group) {
			report.SkippedGroups = append(report.SkippedGroups, c.group)
		}
	case ok:
		report.Passed = append(report.Passed, name)
	default:
		report.Failed = append(report.Failed, name)
	}
}

// setContext puts the running case's deadline on the session, and context
// answers it. Cases run one at a time, so one field carries the deadline of
// whichever is running; the lock is what makes that free of a race rather
// than what makes it correct.
func (s *session) setContext(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctx = ctx
}

// context is the running case's deadline, or the background context outside
// a case, which is where the surface is read and the cleanup runs.
func (s *session) context() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

// why answers the reason a case does not run, and whether it does not. The
// order is the caller's own list first, because a caller that named a case
// gets that reason rather than one about its group; then the group's input;
// then the routes the target does not serve, which is the pending group's
// business and never a skip.
func (s *session) why(name string, c testCase) (string, bool) {
	if slices.Contains(s.options.Skip, name) || (c.group != "" && slices.Contains(s.options.Skip, c.group)) {
		return "skipped by request", true
	}
	if c.group != "" && !s.has(c.group) {
		return "the " + c.group + " group needs " + groupField(c.group) + " on the target", true
	}
	if waiting := s.pendingRoutes(c.routes); len(waiting) > 0 {
		// The case is written and is not run, because the target answers
		// none of the routes it drives. It is not hidden: the pending group
		// fails with every outstanding route and the spec it waits on, so a
		// run against a partial build is red until the route lands.
		return "the pending group holds it: the target does not serve " + strings.Join(waiting, ", "), true
	}
	return "", false
}

// has reports whether the options carry what a group needs.
func (s *session) has(group string) bool {
	switch group {
	case GroupAuthorizer:
		return s.options.AuthorizerControl != ""
	case GroupAdministraton:
		return s.options.Admin != ""
	}
	return true
}

// groupField names the field of [Options] a group needs, so the reason a
// group skipped names what to set rather than that something was missing.
func groupField(group string) string {
	switch group {
	case GroupAuthorizer:
		return "AuthorizerControl"
	case GroupAdministraton:
		return "Admin"
	}
	return "an input"
}

// skip records the skip under the case's name and ends the subtest with the
// reason, which is how a skipped case is reported by name.
func (s *session) skip(t *testing.T, name, reason string) {
	s.mu.Lock()
	s.skipped[name] = reason
	s.mu.Unlock()
	t.Skipf("%s: %s", name, reason)
}

// reason answers why a case skipped, and whether it did.
func (s *session) reason(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reason, ok := s.skipped[name]
	return reason, ok
}

// unverifiable records an assertion the target's shape gave no way to make,
// which is not a failure and not a skip: the case ran and proved what it
// could.
func (s *session) unverifiable(t *testing.T, what, why string) {
	t.Helper()
	s.mu.Lock()
	s.unverified = append(s.unverified, t.Name()+": "+what)
	s.mu.Unlock()
	t.Logf("unverified on this target: %s (%s)", what, why)
}

// record remembers something the run created, so the cleanup deletes it by
// what it is rather than by a listing.
func (s *session) record(what string, remove func(t testing.TB) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created = append(s.created, created{what: what, remove: remove})
}

// cleanup deletes everything the run created, newest first, and answers what
// it made. Newest first because a workspace created inside a space is
// deleted before the objects a case put beside it, and a delete that fails
// is reported and never retried past its own budget.
func (s *session) cleanup(t testing.TB) []string {
	t.Helper()
	// The last case's deadline is spent by now, and a delete sent on a
	// cancelled context would report a failure the target did not make.
	s.setContext(context.WithoutCancel(t.Context()))
	s.mu.Lock()
	made := slices.Clone(s.created)
	s.mu.Unlock()
	names := make([]string, 0, len(made))
	for _, m := range slices.Backward(made) {
		names = append(names, m.what)
		if err := m.remove(t); err != nil {
			t.Errorf("cleanup of %s: %v", m.what, err)
		}
	}
	slices.Reverse(names)
	return names
}
