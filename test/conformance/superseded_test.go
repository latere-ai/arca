// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// What TestPreviousSuitePasses of spec 024 decides before and after it runs
// the previous release's suite: which release that is, which of its cases a
// later release superseded, what those cases are in its source, the pattern
// that keeps them from running, and the evidence the release body carries.
// Every function here is held by the unit run; the run itself is
// previous_tier_test.go's.

// superseded is one case of a previous release's suite that asserted a
// refusal a later release turned into an operation. It is skipped by the
// run of that suite against the later binary, and reported under its
// reason, because a request that was refused and now does what it names
// broke nothing a consumer relied on (spec 024).
type superseded struct {
	// Name is the case's exact name in the previous suite, <NNN>/<Name>.
	Name string
	// Refusal is the code of spec 013's table the previous case expected
	// and the later release no longer answers. Only a 4xx is admitted.
	Refusal string
	// Since is the tag that turned the refusal into an operation.
	Since string
	// Reason is one line, rendered after Since in the evidence.
	Reason string
}

// supersededCases is the list. An entry applies while the previous release
// is below Since; a later previous suite was written after the change and
// runs the case.
var supersededCases = []superseded{
	{
		Name: "028/FolderCopy", Refusal: CodeInvalidField, Since: "v0.8.0",
		Reason: "a move with recursive moves a folder (spec 030)",
	},
}

// release is a plain tag, vMAJOR.MINOR.PATCH, which is every tag the
// release pipeline cuts.
type release [3]int

// releaseRE is that shape, with no leading zero and no suffix.
var releaseRE = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// parseRelease reads a plain tag.
func parseRelease(tag string) (release, error) {
	m := releaseRE.FindStringSubmatch(tag)
	if m == nil {
		return release{}, fmt.Errorf("%q is not a tag a release cuts, vMAJOR.MINOR.PATCH", tag)
	}
	var r release
	for i := range r {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return release{}, fmt.Errorf("%q: %w", tag, err)
		}
		r[i] = n
	}
	return r, nil
}

// compare orders two releases.
func (r release) compare(o release) int { return slices.Compare(r[:], o[:]) }

// previousTag answers the highest plain tag below tag in the same major. It
// answers none, with the reason, when there is no such tag or when tag moves
// the major, which is what a major says: the previous suite is not held to
// it. A tag that is not plain is an error; the other tags of the repository
// that are not plain are not releases and are passed over.
func previousTag(tag string, tags []string) (string, string, error) {
	cut, err := parseRelease(tag)
	if err != nil {
		return "", "", err
	}
	best, below := "", false
	var bestRelease release
	for _, candidate := range tags {
		r, err := parseRelease(candidate)
		if err != nil || r.compare(cut) >= 0 {
			continue
		}
		below = true
		if r[0] == cut[0] && (best == "" || r.compare(bestRelease) > 0) {
			best, bestRelease = candidate, r
		}
	}
	switch {
	case best != "":
		return best, "", nil
	case below:
		return "", fmt.Sprintf("%s moves the major, so no earlier release's suite is held to it", tag), nil
	default:
		return "", fmt.Sprintf("no v%d tag is below %s", cut[0], tag), nil
	}
}

// caseNameRE is a case's name as Run names its subtest, <NNN>/<Name>.
var caseNameRE = regexp.MustCompile(`^[0-9]{3}/[A-Z][A-Za-z0-9]*$`)

// applying splits the list into the entries that apply to a run of the
// previous release's suite against the tag being cut and the ones that do
// not, and refuses a list that is not one: an entry with no refusal, no
// reason, a reason of more than one line, a name or a tag in the wrong
// shape, or a case listed twice.
func applying(entries []superseded, previous, tag string) (apply, inert []superseded, err error) {
	from, err := parseRelease(previous)
	if err != nil {
		return nil, nil, err
	}
	to, err := parseRelease(tag)
	if err != nil {
		return nil, nil, err
	}
	var problems []error
	seen := map[string]bool{}
	for _, e := range entries {
		since, err := parseRelease(e.Since)
		switch {
		case !caseNameRE.MatchString(e.Name):
			problems = append(problems, fmt.Errorf("%q is not a case name, <NNN>/<Name>", e.Name))
			continue
		case seen[e.Name]:
			problems = append(problems, fmt.Errorf("%s is listed twice", e.Name))
			continue
		case err != nil:
			problems = append(problems, fmt.Errorf("%s: %w", e.Name, err))
			continue
		case e.Refusal == "":
			problems = append(problems, fmt.Errorf("%s names no refusal it supersedes", e.Name))
			continue
		case strings.TrimSpace(e.Reason) == "" || strings.ContainsAny(e.Reason, "\r\n"):
			problems = append(problems, fmt.Errorf("%s needs a reason of one line", e.Name))
			continue
		}
		seen[e.Name] = true
		if from.compare(since) < 0 && since.compare(to) <= 0 {
			apply = append(apply, e)
		} else {
			inert = append(inert, e)
		}
	}
	if err := errors.Join(problems...); err != nil {
		return nil, nil, fmt.Errorf("the superseded list: %w", err)
	}
	return apply, inert, nil
}

// checkSuperseded holds each applying entry to the previous suite it is
// applied to: the name is a case of that suite, the refusal is a code the
// case declares it provokes, and the code is a 4xx row of spec 013's table.
// A name the suite does not hold is an entry that rotted; a success or a
// server error that stopped answering is a break no entry can excuse.
func checkSuperseded(apply []superseded, suite map[string][]string) error {
	var problems []error
	for _, e := range apply {
		codes, held := suite[e.Name]
		if !held {
			problems = append(problems, fmt.Errorf("%s is not a case of the previous suite", e.Name))
			continue
		}
		if !slices.Contains(codes, e.Refusal) {
			problems = append(problems, fmt.Errorf("%s does not declare %s, so it never expected that refusal", e.Name, e.Refusal))
			continue
		}
		r, known := codeTable[e.Refusal]
		switch {
		case !known:
			problems = append(problems, fmt.Errorf("%s supersedes %q, which is not a code of spec 013's table", e.Name, e.Refusal))
		case r.status < 400 || r.status > 499:
			problems = append(problems, fmt.Errorf("%s supersedes %s, which answers %d; only a refusal, a 4xx, is superseded", e.Name, e.Refusal, r.status))
		}
	}
	return errors.Join(problems...)
}

// suiteCases reads the cases of a suite from its source in dir: each case
// cases() runs, by its name <NNN>/<Name>, with the codes it declares. The
// source is read because a previous release's cases are not a value this
// release can ask it for, and the shape read is the one cases() has had
// since the suite was written: a list of {number, cases} pairs whose cases
// are a []testCase literal or a function that returns one.
func suiteCases(dir string) (map[string][]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read the suite at %s: %w", dir, err)
	}
	fset := token.NewFileSet()
	consts := map[string]string{}
	funcs := map[string]*ast.FuncDecl{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, fmt.Errorf("read the suite at %s: %w", dir, err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					funcs[d.Name.Name] = d
				}
			case *ast.GenDecl:
				if d.Tok == token.CONST {
					readConsts(d, consts)
				}
			}
		}
	}
	list := returned(funcs["cases"])
	if list == nil {
		return nil, fmt.Errorf("the suite at %s has no cases function returning a list", dir)
	}
	out := map[string][]string{}
	for _, elt := range list.Elts {
		pair, ok := elt.(*ast.CompositeLit)
		if !ok || len(pair.Elts) != 2 {
			return nil, fmt.Errorf("the suite at %s lists a spec in a shape this reader does not know", dir)
		}
		number, ok := stringOf(pair.Elts[0], consts)
		if !ok {
			return nil, fmt.Errorf("the suite at %s numbers a spec with something that is not a string", dir)
		}
		rows := literalOf(pair.Elts[1], funcs)
		if rows == nil {
			return nil, fmt.Errorf("the suite at %s lists the cases of %s in a shape this reader does not know", dir, number)
		}
		for _, row := range rows.Elts {
			name, codes, err := caseOf(row, consts)
			if err != nil {
				return nil, fmt.Errorf("the suite at %s, spec %s: %w", dir, number, err)
			}
			out[number+"/"+name] = codes
		}
	}
	return out, nil
}

// readConsts records every string constant of a declaration.
func readConsts(d *ast.GenDecl, into map[string]string) {
	for _, spec := range d.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, name := range vs.Names {
			if i < len(vs.Values) {
				if s, ok := stringOf(vs.Values[i], into); ok {
					into[name.Name] = s
				}
			}
		}
	}
}

// returned answers the composite literal a function's last top-level return
// gives back, or nil.
func returned(fn *ast.FuncDecl) *ast.CompositeLit {
	if fn == nil || fn.Body == nil {
		return nil
	}
	for _, stmt := range slices.Backward(fn.Body.List) {
		if ret, ok := stmt.(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
			lit, _ := ret.Results[0].(*ast.CompositeLit)
			return lit
		}
	}
	return nil
}

// literalOf answers the []testCase literal an expression is, or the one the
// function it calls returns.
func literalOf(expr ast.Expr, funcs map[string]*ast.FuncDecl) *ast.CompositeLit {
	switch e := expr.(type) {
	case *ast.CompositeLit:
		return e
	case *ast.CallExpr:
		if id, ok := e.Fun.(*ast.Ident); ok {
			return returned(funcs[id.Name])
		}
	}
	return nil
}

// caseOf reads one case literal: its name, and the codes it declares.
func caseOf(expr ast.Expr, consts map[string]string) (string, []string, error) {
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return "", nil, errors.New("a case that is not a literal")
	}
	var (
		name  string
		codes []string
	)
	for _, field := range lit.Elts {
		kv, ok := field.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, _ := kv.Key.(*ast.Ident)
		switch {
		case key == nil:
		case key.Name == "name":
			name, _ = stringOf(kv.Value, consts)
		case key.Name == "codes":
			list, ok := kv.Value.(*ast.CompositeLit)
			if !ok {
				return "", nil, fmt.Errorf("the codes of %q are not a list", name)
			}
			for _, code := range list.Elts {
				s, ok := stringOf(code, consts)
				if !ok {
					return "", nil, fmt.Errorf("a code of %q is not a string", name)
				}
				codes = append(codes, s)
			}
		}
	}
	if name == "" {
		return "", nil, errors.New("a case with no name")
	}
	return name, codes, nil
}

// stringOf answers the string an expression is: a literal, or a constant
// read already.
func stringOf(expr ast.Expr, consts map[string]string) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.Ident:
		s, ok := consts[e.Name]
		return s, ok
	}
	return "", false
}

// skipPattern is go test's -skip for the applying entries: each case named
// at the three levels of its subtest, TestContract, the spec and the case,
// and the cases joined at the top, where -skip reads an alternation of
// whole names rather than the product of one alternation per level.
func skipPattern(apply []superseded) string {
	parts := make([]string, 0, len(apply))
	for _, e := range apply {
		number, name, _ := strings.Cut(e.Name, "/")
		parts = append(parts, "^TestContract$/^"+regexp.QuoteMeta(number)+"$/^"+regexp.QuoteMeta(name)+"$")
	}
	return strings.Join(parts, "|")
}

// ranRE is the line go test -v prints when a case of the suite starts.
var ranRE = regexp.MustCompile(`(?m)^=== RUN\s+TestContract/([^/\s]+)/([^/\s]+)\s*$`)

// ranCases answers the cases a verbose run started, sorted.
func ranCases(out string) []string {
	var ran []string
	for _, m := range ranRE.FindAllStringSubmatch(out, -1) {
		name := m[1] + "/" + m[2]
		if !slices.Contains(ran, name) {
			ran = append(ran, name)
		}
	}
	slices.Sort(ran)
	return ran
}

// countRE is the count TestContract logs at the end of a run.
var countRE = regexp.MustCompile(`[0-9]+ passed, [0-9]+ failed, [0-9]+ skipped[^\n]*`)

// countLine answers the suite's own count, or "" for a run that never
// reached it.
func countLine(out string) string { return strings.TrimSpace(countRE.FindString(out)) }

// evidence is the section of release-evidence/conformance.md that names the
// previous release's suite the run was held to, its count, and every
// superseded case it skipped or no longer needed to.
func evidence(previous, tag, count string, apply, inert []superseded) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## The previous release's suite\n\n")
	fmt.Fprintf(&b, "The suite of `%s`, with the driver it shipped with, against the binary of `%s` (spec 024). It counted: %s.\n",
		previous, tag, count)
	if len(apply) > 0 {
		fmt.Fprintf(&b, "\nThe count is the driver's, which counts a case `go test -skip` kept from running as passed.\n")
		fmt.Fprintf(&b, "\nSkipped, each a refusal a later release turned into an operation:\n\n")
		for _, e := range apply {
			fmt.Fprintf(&b, "- `%s`, which expected `%s`: %s: %s\n", e.Name, e.Refusal, e.Since, e.Reason)
		}
	}
	if len(inert) > 0 {
		fmt.Fprintf(&b, "\nListed and no longer applying to these two releases, so run:\n\n")
		for _, e := range inert {
			fmt.Fprintf(&b, "- `%s`: %s: %s\n", e.Name, e.Since, e.Reason)
		}
	}
	return b.String()
}

// skippedEvidence is the section for a release no previous suite is held
// to, with the reason.
func skippedEvidence(tag, why string) string {
	return fmt.Sprintf("## The previous release's suite\n\nNot run for `%s`: %s.\n", tag, why)
}
