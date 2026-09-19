// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The tests of the suite itself. They need no installation: what they check
// is that the suite holds the contract it claims to, that every route and
// every group of spec 017's table is reached by a case, and that the
// machinery reports a skip, a pending route and a cleanup the way the report
// says it does.
//
// They run in the ordinary build, so the gate runs them on every commit.
// The cases themselves run against a target, which is the driver of
// contract_test.go under the tiers tag.

// dir answers this package's directory from its own source, because the
// tempdir gate runs the suite from an empty working directory and a
// relative path would resolve against that.
func dir(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test cannot find its own source")
	}
	return filepath.Dir(file)
}

// specsDir answers the deck's directory, three levels above this package.
func specsDir(t testing.TB) string { return filepath.Join(dir(t), "..", "..", "specs") }

// TestErrorTableMatchesTheSpec holds the suite's copy of the error table
// equal to the table in specs/013-api.md, one row at a time. The suite
// declares its own copy because it is black box, and this is what keeps the
// copy from drifting from the document it is a copy of.
func TestErrorTableMatchesTheSpec(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(specsDir(t), "013-api.md"))
	if err != nil {
		t.Fatalf("read spec 013: %v", err)
	}
	// | `code` | status | sentence |
	rowRE := regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\| (\\d{3}) \\| (.+?) \\|$")
	found := map[string]row{}
	for _, m := range rowRE.FindAllStringSubmatch(string(raw), -1) {
		status, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("the spec's row for %s names the status %q", m[1], m[2])
		}
		found[m[1]] = row{status: status, sentence: strings.TrimSpace(m[3])}
	}
	if len(found) < 25 {
		t.Fatalf("read %d rows out of spec 013's error table, which is not the table", len(found))
	}
	for code, want := range found {
		got, ok := codeTable[code]
		if !ok {
			t.Errorf("spec 013 names the code %q and the suite's table does not", code)
			continue
		}
		if got != want {
			t.Errorf("%s: the suite has %d %q and spec 013 has %d %q", code, got.status, got.sentence, want.status, want.sentence)
		}
	}
	for code := range codeTable {
		if _, ok := found[code]; !ok {
			t.Errorf("the suite's table names the code %q and spec 013 does not", code)
		}
	}
}

// TestEveryRouteHasACase: every one of the forty-one rows of spec 013's
// table is driven by at least one case, and every route a case names is a
// row of the table. A case naming a route the table does not hold is a case
// driving something the contract does not fix.
func TestEveryRouteHasACase(t *testing.T) {
	inTable := map[string]bool{}
	for _, r := range surfaceTable {
		inTable[r.key()] = true
	}
	if len(surfaceTable) != 41 {
		t.Errorf("spec 013's table is forty-one routes and the suite declares %d", len(surfaceTable))
	}
	driven := map[string]bool{}
	for _, sp := range cases() {
		for _, c := range sp.cases {
			for _, key := range c.routes {
				if !inTable[key] {
					t.Errorf("%s/%s drives %q, which is not a row of spec 013's table", sp.number, c.name, key)
				}
				driven[key] = true
			}
		}
	}
	var missing []string
	for _, r := range surfaceTable {
		if !driven[r.key()] {
			missing = append(missing, r.key())
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("no case drives these rows of spec 013's table:\n\t%s", strings.Join(missing, "\n\t"))
	}
}

// TestEveryCodeIsProvokedOrNamed is criterion 2's code half: every row of
// spec 013's error table is either provoked by a case, which declares it, or
// recorded in unprovoked with the reason no caller outside the installation
// can make one. A code that is neither is a row the suite claims to cover
// and does not.
//
// The codes are read off the case list rather than off a run, so the rule
// holds against a build that serves none of the routes, and a reader checks
// one list rather than a log.
func TestEveryCodeIsProvokedOrNamed(t *testing.T) {
	provoked := map[string][]string{}
	for _, sp := range cases() {
		for _, c := range sp.cases {
			for _, code := range c.codes {
				if _, known := codeTable[code]; !known {
					t.Errorf("%s/%s claims %q, which is not a row of spec 013's table", sp.number, c.name, code)
				}
				provoked[code] = append(provoked[code], sp.number+"/"+c.name)
			}
		}
	}
	var missing []string
	for code := range codeTable {
		if len(provoked[code]) > 0 {
			if reason, named := unprovoked[code]; named {
				t.Errorf("%s is provoked by %v and is also recorded as unprovoked: %s", code, provoked[code], reason)
			}
			continue
		}
		if _, named := unprovoked[code]; !named {
			missing = append(missing, code)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("no case provokes these rows of spec 013's error table, and none is recorded as unprovoked with a reason:\n\t%s",
			strings.Join(missing, "\n\t"))
	}
	t.Logf("%d of the %d rows are provoked by a case, %d are recorded as unprovoked",
		len(provoked), len(codeTable), len(unprovoked))
}

// TestEveryGroupHasACase: every group of spec 017's table is reached by at
// least one case, and every group a case names is one of the table's. A
// group in the table with no case is a promise the suite does not keep.
func TestEveryGroupHasACase(t *testing.T) {
	known := map[string]bool{}
	for _, g := range Groups {
		known[g] = true
	}
	reached := map[string]bool{}
	for _, sp := range cases() {
		for _, c := range sp.cases {
			if c.group == "" {
				t.Errorf("%s/%s belongs to no group", sp.number, c.name)
				continue
			}
			if !known[c.group] {
				t.Errorf("%s/%s is in the group %q, which spec 017's table does not name", sp.number, c.name, c.group)
			}
			reached[c.group] = true
		}
	}
	for _, g := range Groups {
		if !reached[g] {
			t.Errorf("the group %q has no case", g)
		}
	}
}

// TestCaseNamesAreUnique: a case name is how the report and the skip list
// name a case, so two cases under one spec with one name would make a skip
// ambiguous.
func TestCaseNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, sp := range cases() {
		for _, c := range sp.cases {
			name := sp.number + "/" + c.name
			if seen[name] {
				t.Errorf("%s is declared twice", name)
			}
			seen[name] = true
		}
	}
}

// TestEveryCriterionHasACase is criterion 3 of spec 017: a criterion whose
// test column names the literal caseNNNName is proved by a function of that
// name in this package, and a marker naming a function that is not here is a
// criterion nothing proves.
//
// The rule runs in the direction that catches a lie. A case with no marker
// is not a failure: a case drives a route of spec 013's table, which
// criterion 2 already holds whole, and requiring a marker for each would put
// forty-one rows into the acceptance tables of specs this one does not own.
func TestEveryCriterionHasACase(t *testing.T) {
	declared := caseFunctions(t)
	markerRE := regexp.MustCompile(`\bcase\d{3}[A-Z][A-Za-z0-9]*\b`)
	markers := map[string][]string{}
	for _, root := range []string{specsDir(t), filepath.Join(specsDir(t), ".archive")} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || filepath.Ext(path) != ".md" {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, name := range markerRE.FindAllString(string(raw), -1) {
				markers[name] = append(markers[name], filepath.Base(path))
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	for name, files := range markers {
		if !declared[name] {
			t.Errorf("%s names %s, and this package declares no such case", strings.Join(files, ", "), name)
		}
	}
	t.Logf("%d markers in the deck, %d cases declared", len(markers), len(declared))
}

// caseFunctions reads this package's own source for the case functions it
// declares. The source is read rather than reflected over because a function
// is not a value the runtime can enumerate by name.
func caseFunctions(t testing.TB) map[string]bool {
	t.Helper()
	root := dir(t)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read the suite's own directory: %v", err)
	}
	fset := token.NewFileSet()
	out := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "cases") || !strings.HasSuffix(name, ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(root, name), nil, 0)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "case") {
				out[fn.Name.Name] = true
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("the suite declares no case function, which cannot be right")
	}
	return out
}

// TestRunNeedsATarget: Run states what it needs rather than driving a base
// URL of "" and reporting every case as failed.
func TestRunNeedsATarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"no URL", Options{Token: noToken}, "URL"},
		{"no Token", Options{URL: "http://127.0.0.1:1"}, "Token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(tc.opts)
			if err == nil {
				t.Fatalf("a run with no %s was accepted", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal is %q, which does not name %s", err, tc.want)
			}
		})
	}
	if err := validate(Options{URL: "http://127.0.0.1:1", Token: noToken}); err != nil {
		t.Errorf("a run with a URL and a Token was refused: %v", err)
	}
}

func noToken(context.Context, string) (string, error) { return "t", nil }

// TestSurfaceReadsTheServedDocument: the pending set is the difference
// between spec 013's table and the document the target publishes, and a
// target that serves no document is held to the whole table rather than to
// none of it.
func TestSurfaceReadsTheServedDocument(t *testing.T) {
	served := []route{
		{http.MethodGet, "/v1/events", "010"},
		{http.MethodPost, "/v1/workspaces", "009"},
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openapi.json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		paths := map[string]map[string]any{}
		for _, row := range served {
			item, ok := paths[row.template()]
			if !ok {
				item = map[string]any{}
				paths[row.template()] = item
			}
			item[strings.ToLower(row.method)] = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"openapi": "3.1.0", "paths": paths})
	}))
	defer target.Close()

	s := newSession(Options{URL: target.URL, Token: noToken})
	s.served = s.readSurface(t)
	for _, row := range served {
		if !s.served.serves(row.key()) {
			t.Errorf("the document names %s and the suite reads it as pending", row.key())
		}
	}
	pending := s.served.pending()
	if len(pending) != len(surfaceTable)-len(served) {
		t.Errorf("the document names %d routes and the pending set holds %d of %d", len(served), len(pending), len(surfaceTable))
	}
	if key, ok := s.served.servesAll([]string{"GET /v1/events", "GET /v1/stars"}); ok || key != "GET /v1/stars" {
		t.Errorf("servesAll answered %q, %v", key, ok)
	}
	if got := s.pendingRoutes([]string{"GET /v1/events", "GET /v1/stars"}); !slices.Equal(got, []string{"GET /v1/stars"}) {
		t.Errorf("pendingRoutes answered %v", got)
	}

	// A target with no document is held to the whole table, so its cases run
	// and fail on what it does not answer.
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer silent.Close()
	quiet := newSession(Options{URL: silent.URL, Token: noToken})
	quiet.served = quiet.readSurface(t)
	if len(quiet.served.pending()) != 0 {
		t.Errorf("a target that serves no document has %d pending routes, want none", len(quiet.served.pending()))
	}
}

// TestPendingFailsUntilTheRoutesLand is criterion 6's half that matters on a
// partial build: the pending group is one case that fails while a route of
// spec 013's table is outstanding, naming the route and the spec it waits
// on, and it passes on a build that serves them all.
func TestPendingFailsUntilTheRoutesLand(t *testing.T) {
	s := newSession(Options{URL: "http://127.0.0.1:1", Token: noToken})
	s.served = surface{answered: map[string]bool{}}
	for _, r := range surfaceTable {
		s.served.answered[r.key()] = true
	}
	delete(s.served.answered, "GET /v1/stars")

	partial := watch(t, s.reportPending)
	if !partial.failed {
		t.Fatal("the pending group passed with a route of spec 013's table outstanding")
	}
	if !strings.Contains(partial.message, "GET /v1/stars") || !strings.Contains(partial.message, "spec 005") {
		t.Errorf("the pending group reported %q, which names neither the route nor the spec", partial.message)
	}

	s.served.answered["GET /v1/stars"] = true
	if complete := watch(t, s.reportPending); complete.failed {
		t.Errorf("the pending group failed on a complete build: %s", complete.message)
	}
}

// TestSkipsCarryAReason is criterion 1 of spec 017: a group whose input is
// empty skips with a reason in the report and never silently, and a case the
// caller named skips with that as its reason rather than with the group's.
func TestSkipsCarryAReason(t *testing.T) {
	s := newSession(Options{URL: "http://127.0.0.1:1", Token: noToken})
	s.served = surface{answered: map[string]bool{"GET /v1/events": true}}

	reason, skip := s.why("006/Deny", testCase{name: "Deny", group: GroupAuthorizer})
	if !skip || !strings.Contains(reason, "AuthorizerControl") {
		t.Errorf("a group with no input skipped %v with the reason %q", skip, reason)
	}
	if _, skip := s.why("013/Envelope", testCase{name: "Envelope", group: GroupErrors}); skip {
		t.Error("a group whose input is not needed skipped")
	}

	named := newSession(Options{URL: "x", Token: noToken, Skip: []string{"013/Envelope", GroupEvents}})
	named.served = s.served
	if reason, skip := named.why("013/Envelope", testCase{name: "Envelope", group: GroupErrors}); !skip || reason != "skipped by request" {
		t.Errorf("a case the caller named skipped %v with the reason %q", skip, reason)
	}
	if reason, skip := named.why("010/Tail", testCase{name: "Tail", group: GroupEvents}); !skip || reason != "skipped by request" {
		t.Errorf("a group the caller named skipped %v with the reason %q", skip, reason)
	}

	// A case whose routes the target does not serve says which, and says
	// the pending group holds it rather than that something was missing.
	reason, skip = s.why("005/List", testCase{name: "List", group: GroupFiles, routes: []string{"GET /v1/stars"}})
	if !skip || !strings.Contains(reason, "pending") || !strings.Contains(reason, "GET /v1/stars") {
		t.Errorf("a case for an unserved route skipped %v with the reason %q", skip, reason)
	}

	for _, g := range Groups {
		if field := groupField(g); field == "" {
			t.Errorf("the group %q names no field for its reason", g)
		}
	}
}

// TestNamesAreTheRunsOwn: everything a run creates carries the suite's
// prefix and the run's own value, and two runs draw different values, which
// is what makes two runs against one installation safe.
func TestNamesAreTheRunsOwn(t *testing.T) {
	a, b := newSession(Options{}), newSession(Options{})
	if a.run == b.run {
		t.Fatal("two runs drew the same value")
	}
	name := a.name("thing")
	if !strings.HasPrefix(name, Prefix) || !strings.Contains(name, a.run) || !strings.HasSuffix(name, "-thing") {
		t.Errorf("a run names a thing %q, which is not the prefix, the run and the label", name)
	}
	if strings.Contains(name, b.run) {
		t.Errorf("one run's name holds another run's value: %q", name)
	}
	if got := a.grantPrefix("g"); !strings.HasPrefix(got, "files/"+Prefix) {
		t.Errorf("a grant covers %q, which is not a subtree of this run's own names", got)
	}
	if got := a.filePath("f"); !strings.HasPrefix(got, "files/"+Prefix) {
		t.Errorf("a file path is %q, which is not under this run's own names", got)
	}
}

// TestRunCleansUpWhatItMade is criterion 4: a run deletes exactly the ids it
// recorded, newest first, and reports them. Nothing is deleted by prefix or
// by listing, so a run against a shared installation touches nothing it did
// not make.
func TestRunCleansUpWhatItMade(t *testing.T) {
	s := newSession(Options{URL: "x", Token: noToken})
	var order []string
	for _, what := range []string{"workspace 1", "share 2", "link 3"} {
		s.record(what, func(testing.TB) error {
			order = append(order, what)
			return nil
		})
	}
	made := s.cleanup(t)
	if !slices.Equal(made, []string{"workspace 1", "share 2", "link 3"}) {
		t.Errorf("the run reported %v", made)
	}
	if !slices.Equal(order, []string{"link 3", "share 2", "workspace 1"}) {
		t.Errorf("the cleanup deleted in the order %v, and it deletes newest first", order)
	}
}

// TestCleanupReportsAFailureAndKeepsGoing: one object the target will not
// delete does not leave the rest behind.
func TestCleanupReportsAFailureAndKeepsGoing(t *testing.T) {
	s := newSession(Options{URL: "x", Token: noToken})
	deleted := 0
	s.record("a", func(testing.TB) error { deleted++; return nil })
	s.record("b", func(testing.TB) error { return fmt.Errorf("the target refused") })
	s.record("c", func(testing.TB) error { deleted++; return nil })

	var made []string
	fake := watch(t, func(tb testing.TB) { made = s.cleanup(tb) })
	if len(made) != 3 || deleted != 2 {
		t.Errorf("the cleanup reported %v and deleted %d", made, deleted)
	}
	if !fake.errored {
		t.Error("a delete the target refused was not reported")
	}
}

// TestUnverifiableIsNotASkip: an assertion the target's shape gave no way to
// make is recorded, and the case it was recorded in still passes, because it
// proved what it could.
func TestUnverifiableIsNotASkip(t *testing.T) {
	s := newSession(Options{URL: "x", Token: noToken})
	s.unverifiable(t, "an expired token is refused", "the suite holds no signing key")
	if len(s.unverified) != 1 || !strings.Contains(s.unverified[0], "an expired token is refused") {
		t.Errorf("the run recorded %v", s.unverified)
	}
	if _, skipped := s.reason(t.Name()); skipped {
		t.Error("an unverifiable assertion was recorded as a skip")
	}
}

// TestBodyRendersTheFieldsACaseNames: a case writes the field names spec 013
// fixes against the values it means, and a key with no value cannot be
// written, because the body is a map and not a pair list.
func TestBodyRendersTheFieldsACaseNames(t *testing.T) {
	var got map[string]any
	if err := json.Unmarshal([]byte(body(fields{"owner": "me", "slug": "s", "ttl_seconds": 60})), &got); err != nil {
		t.Fatalf("body rendered %v", err)
	}
	if got["owner"] != "me" || got["slug"] != "s" || got["ttl_seconds"] != float64(60) {
		t.Errorf("body rendered %v", got)
	}
	if body(fields{}) != "{}" {
		t.Errorf("an empty body rendered %q", body(fields{}))
	}
	defer func() {
		if recover() == nil {
			t.Error("a value no JSON encoder can render was written")
		}
	}()
	body(fields{"unrenderable": make(chan int)})
}

// TestReadersAnswerTheZeroValue: a case asserts the value it wanted rather
// than panicking on a shape the target did not answer.
func TestReadersAnswerTheZeroValue(t *testing.T) {
	m := map[string]any{"s": "x", "n": float64(3), "o": map[string]any{"k": "v"}, "l": []any{map[string]any{"id": "1"}, "not an object"}}
	if str(m, "s") != "x" || str(m, "n") != "" || str(m, "absent") != "" {
		t.Error("str does not answer the zero value for a field of another type")
	}
	if num(m, "n") != 3 || num(m, "s") != 0 {
		t.Error("num does not answer the zero value for a field of another type")
	}
	if obj(m, "o")["k"] != "v" || obj(m, "s") != nil {
		t.Error("obj does not answer the zero value for a field of another type")
	}
	if entries := list(m, "l"); len(entries) != 1 || entries[0]["id"] != "1" {
		t.Errorf("list answered %v", entries)
	}
	if len(list(m, "absent")) != 0 {
		t.Error("list does not answer an empty page for a field that is absent")
	}
}

// TestSubjectsAreEscapedForAPathAndAQuery: a subject is <issuer>|<sub>, and
// every character a path segment or a query gives meaning to is escaped, so
// one subject is one segment and not four.
func TestSubjectsAreEscapedForAPathAndAQuery(t *testing.T) {
	subject := "https://issuer.example|9ab3 c"
	for _, got := range []string{escape(subject), pathParam(subject)} {
		if strings.Contains(got, "/") || strings.Contains(got, "|") || strings.Contains(got, " ") {
			t.Errorf("a subject escaped to %q, which a path or a query reads as more than one value", got)
		}
	}
	if strings.Contains(pathParam(subject), "+") {
		t.Errorf("a path segment holds a plus, which is a space in a query and not in a path: %q", pathParam(subject))
	}
}

// TestBucketDialSendsTheSignedHost: a presigned URL is signed over the host
// it names, so the suite cannot rewrite one to reach the store from
// somewhere else. With [Options.BucketDial] set it dials that address
// instead and sends the signed host, and the path and the query, which the
// signature also covers, arrive as the target wrote them. With it empty the
// URL is dialed as it is.
func TestBucketDialSendsTheSignedHost(t *testing.T) {
	type arrived struct{ host, target string }
	seen := make(chan arrived, 2)
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- arrived{host: r.Host, target: r.URL.RequestURI()}
		_, _ = w.Write([]byte("bytes\n"))
	}))
	defer store.Close()

	const signedPath = "/arca-test/arca/49/01J8XYZ"
	const signedQuery = "?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=minioadmin%2F20260919%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Signature=deadbeef"

	t.Run("override", func(t *testing.T) {
		// A name no resolver answers, which is what a cluster-internal host
		// is from a runner: without the override the dial itself fails.
		s := newSession(Options{URL: "http://target.example", BucketDial: store.URL})
		r := s.bucket(t, request{method: http.MethodGet, path: "http://bucket.invalid:9000" + signedPath + signedQuery})
		if r.status != http.StatusOK || string(r.body) != "bytes\n" {
			t.Fatalf("the override answered %d %q", r.status, r.body)
		}
		if !r.foreign {
			t.Error("an answer from the store is not marked foreign, and spec 013 binds the installation's answers alone")
		}
		got := <-seen
		if got.host != "bucket.invalid:9000" {
			t.Errorf("the store saw Host %q, and the signature is over bucket.invalid:9000", got.host)
		}
		if got.target != signedPath+signedQuery {
			t.Errorf("the store saw %q, and the signature is over %q", got.target, signedPath+signedQuery)
		}
	})

	t.Run("none", func(t *testing.T) {
		s := newSession(Options{URL: "http://target.example"})
		r := s.bucket(t, request{method: http.MethodGet, path: store.URL + signedPath + signedQuery})
		if r.status != http.StatusOK {
			t.Fatalf("a URL dialed as given answered %d %q", r.status, r.body)
		}
		got := <-seen
		if want := strings.TrimPrefix(store.URL, "http://"); got.host != want {
			t.Errorf("the store saw Host %q, want %q: with no override the URL is dialed as given", got.host, want)
		}
		if got.target != signedPath+signedQuery {
			t.Errorf("the store saw %q, want %q", got.target, signedPath+signedQuery)
		}
	})
}

// TestStrangerTokenIsAJWTFromNoListedIssuer: the bearer the identity group
// sends is shaped like a token and signed by nobody, so a target refusing it
// is refusing an issuer it does not list rather than a string that is not a
// token at all.
func TestStrangerTokenIsAJWTFromNoListedIssuer(t *testing.T) {
	parts := strings.Split(strangerToken(), ".")
	if len(parts) != 3 {
		t.Fatalf("the token has %d parts, and a JWT has three", len(parts))
	}
	if strings.Contains(strangerToken(), "issuer.example") {
		t.Error("the token names an issuer a target might list")
	}
}

// TestRoutesAreTheTable: the exported list every consumer reads the suite's
// coverage from is the table itself.
func TestRoutesAreTheTable(t *testing.T) {
	got := Routes()
	if len(got) != len(surfaceTable) {
		t.Fatalf("Routes answers %d rows and the table holds %d", len(got), len(surfaceTable))
	}
	if !slices.Contains(got, "GET /v1/events") {
		t.Errorf("Routes does not name the event tail: %v", got)
	}
}

// TestUnprovokedCodesAreNamedWithAReason: a code the suite does not provoke
// carries the reason it cannot, so the gaps in criterion 2 are recorded
// rather than passed over.
func TestUnprovokedCodesAreNamedWithAReason(t *testing.T) {
	for code, reason := range unprovoked {
		if _, ok := codeTable[code]; !ok {
			t.Errorf("%q is recorded as unprovoked and is not a code of the table", code)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%q is recorded as unprovoked with no reason", code)
		}
	}
	if _, named := unprovoked[CodeNotFound]; named {
		t.Error("not_found is recorded as unprovoked, and the identity group provokes it")
	}
}

// TestTableFailsOnACodeOutsideIt: a case asserting a code the table does not
// name is a mistake in the suite, and it stops the case rather than passing.
func TestTableFailsOnACodeOutsideIt(t *testing.T) {
	if fake := watch(t, func(tb testing.TB) { table(tb, "no_such_code") }); !fake.failed {
		t.Error("the table answered a code it does not name")
	}
}

// fatalT records a failure instead of stopping the test, so a test of the
// suite can watch the suite fail a target without failing itself. It embeds
// a testing.TB, which is what makes it one; Fatalf ends the goroutine the
// way testing.T's does, and the caller recovers it.
type fatalT struct {
	testing.TB
	failed  bool
	errored bool
	message string
}

func (f *fatalT) Helper() {}

func (f *fatalT) Fatalf(format string, args ...any) {
	f.failed = true
	f.message = fmt.Sprintf(format, args...)
	panic(stopped{})
}

func (f *fatalT) Errorf(format string, args ...any) {
	f.errored = true
	f.message = fmt.Sprintf(format, args...)
}

// watch runs one assertion of the suite against the recorder and answers
// what it reported, so a test states what the suite says rather than that it
// said something.
func watch(t *testing.T, assert func(testing.TB)) (f *fatalT) {
	t.Helper()
	f = &fatalT{TB: t}
	defer func() {
		if v := recover(); v != nil {
			if _, ok := v.(stopped); !ok {
				panic(v)
			}
		}
	}()
	assert(f)
	return f
}

// stopped is what a recorded Fatalf panics with.
type stopped struct{}
