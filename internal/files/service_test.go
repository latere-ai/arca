// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// routes are the twelve registrations of spec 013's first table, each with a
// request that reaches its handler, so one table drives every handler
// through the cases every handler shares.
func (h *harness) routes() []struct {
	name, method, target, body string
} {
	return []struct{ name, method, target, body string }{
		{"put", http.MethodPut, h.object("files/plan.md"), "x"},
		{"get", http.MethodGet, h.object("files/plan.md"), ""},
		{"list", http.MethodGet, h.object("files") + "?list=1", ""},
		{"versions", http.MethodGet, h.object("files/plan.md") + "?versions=1", ""},
		{"version", http.MethodGet, h.object("files/plan.md") + "?version=1", ""},
		{"head", http.MethodHead, h.object("files/plan.md"), ""},
		{"move", http.MethodPost, h.object("files/plan.md"), `{"move_to":"files/other.md"}`},
		{"restore version", http.MethodPost, h.object("files/plan.md"), `{"restore_version":1}`},
		{"delete", http.MethodDelete, h.object("files/plan.md"), ""},
		{"materialize", http.MethodGet, "/v1/files/materialize", ""},
		{"trash", http.MethodGet, "/v1/trash", ""},
		{"restore", http.MethodPost, "/v1/trash/restore", `{"owner":"me","path":"files/gone.md"}`},
		{"purge", http.MethodDelete, "/v1/trash", ""},
		{"star", http.MethodPut, "/v1/stars", `{"owner":"me","path":"files/plan.md"}`},
		{"unstar", http.MethodDelete, "/v1/stars?owner=me&path=files/plan.md", ""},
		{"stars", http.MethodGet, "/v1/stars", ""},
	}
}

// drive sends one row of the table above.
func (h *harness) drive(t *testing.T, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	if body == "" {
		return h.call(t, method, target, nil)
	}
	return h.call(t, method, target, strings.NewReader(body), api.HeaderContentType, api.JSONMediaType)
}

// TestEveryHandlerAsksExactlyOneActionBeforeItActs is criterion 15 of spec
// 005 at the unit tier: one question per handler, with the action spec 006's
// table names, and a deny is the whole of the answer.
func TestEveryHandlerAsksExactlyOneActionBeforeItActs(t *testing.T) {
	want := map[string]string{
		"put": "file.write", "get": "file.read", "list": "file.list",
		"versions": "file.read", "version": "file.read", "head": "file.read",
		"move": "file.write", "restore version": "file.restore", "delete": "file.delete",
		"materialize": "file.list", "trash": "file.list", "restore": "file.restore",
		"purge": "file.delete", "star": "file.write", "unstar": "file.write",
		"stars": "file.list",
	}
	for i, name := range routeNames {
		t.Run(name, func(t *testing.T) {
			h := seeded(t)
			r := h.routes()[i]
			h.asked.asked = nil
			w := h.drive(t, r.method, r.target, r.body)
			if w.Code >= 400 {
				t.Fatalf("%s answered %d: %s", r.name, w.Code, w.Body)
			}
			if got := h.asked.actions(); len(got) != 1 || got[0] != want[name] {
				t.Fatalf("%s asked %v; spec 006's table says %q", name, got, want[name])
			}

			// The question comes before the act, so a deny is the whole of
			// the answer and the handler's work never runs.
			denied := seeded(t)
			denied.asked.refuse = &auth.Error{Code: auth.CodeForbidden, Detail: "no rule allows it"}
			dr := denied.routes()[i]
			refused := denied.drive(t, dr.method, dr.target, dr.body)
			if refused.Code != http.StatusForbidden {
				t.Fatalf("a denied %s answered %d: %s", name, refused.Code, refused.Body)
			}
			if r.method == http.MethodHead {
				return
			}
			if got := code(t, refused); got != api.CodeForbidden {
				t.Fatalf("a denied %s is %q", name, got)
			}
		})
	}
}

// routeNames are the rows of the table above, in its order, so a case runs
// one row against a harness of its own.
var routeNames = []string{
	"put", "get", "list", "versions", "version", "head", "move", "restore version",
	"delete", "materialize", "trash", "restore", "purge", "star", "unstar", "stars",
}

// seeded is a harness holding one path with a history and one entry in the
// trash, which is the state every row of the table reaches its handler
// against.
func seeded(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.seed(t, "files/plan.md", "seed")
	h.seed(t, "files/plan.md", "again")
	h.seed(t, "files/gone.md", "gone")
	if w := h.call(t, http.MethodDelete, h.object("files/gone.md"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("seeding the trash answered %d: %s", w.Code, w.Body)
	}
	return h
}

// TestADenyOnAnotherSpaceIsAMissingObject is invariant 6 of spec 001: a
// refusal never tells a caller that a space it may not see exists, so a deny
// on a space that is not the caller's own is the answer a missing object
// gives.
//
// The two requests name one path. In the stranger's space it is there, so
// the request reaches the question and is denied at lookup; in the caller's
// own space it is not, so the request never reaches a question at all. The
// answers have to be one answer. A developer detail that named the action,
// or the refused path, would be an oracle: a caller could ask for any path
// in a space it cannot see and read back whether somebody holds it.
func TestADenyOnAnotherSpaceIsAMissingObject(t *testing.T) {
	h := newHarness(t)
	h.endpoint.SetRules(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: false, Reason: "no rule allows it"})
	stranger := "https://issuer.example|someone-else"
	if err := h.store.Upsert(t.Context(), nil, store.File{
		Owner: stranger, Path: "files/plan.md", ObjectID: object.NewID(),
		CreatedBy: stranger, ContentType: "text/markdown", SizeBytes: 4,
		Checksum: digest("seed"), ChecksumKind: object.ChecksumSHA256,
	}); err != nil {
		t.Fatalf("seeding the stranger's row: %v", err)
	}
	w := h.call(t, http.MethodGet, "/v1/files/"+url.PathEscape(stranger)+"/files/plan.md", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("a deny on another space answered %d: %s", w.Code, w.Body)
	}
	if got := code(t, w); got != api.CodeNotFound {
		t.Fatalf("a deny on another space is %q", got)
	}
	// Byte for byte the answer an absence gives, but for the request id.
	absent := h.call(t, http.MethodGet, h.object("files/plan.md"), nil)
	if strip(w.Body.String()) != strip(absent.Body.String()) {
		t.Errorf("a deny answers\n %s\nand an absence answers\n %s", w.Body, absent.Body)
	}
	if w.Header().Get(api.HeaderContentType) != absent.Header().Get(api.HeaderContentType) {
		t.Errorf("a deny is typed %q and an absence %q",
			w.Header().Get(api.HeaderContentType), absent.Header().Get(api.HeaderContentType))
	}
}

// TestEveryLookupDenyIsTheAnswerAnAbsenceGives walks the rows that read a
// row before they ask, which are the rows where the order could tell a
// caller something. Each is driven twice against one path: once where the
// path is there and the question is denied at lookup, once where the path is
// not there and no question is asked at all. The two answers have to be one
// answer, status, code, message and developer detail alike. Anything that
// differs is a probe: ask for a path in a space you cannot see, and read
// back whether somebody holds it.
func TestEveryLookupDenyIsTheAnswerAnAbsenceGives(t *testing.T) {
	// The rows whose handler reads a row and can answer a 404 of its own.
	// A listing answers a page, a put creates, and an unstar reads nothing,
	// so none of the three has an absence to be told apart from.
	for _, name := range []string{"get", "head", "move", "restore version", "delete", "restore", "star"} {
		t.Run(name, func(t *testing.T) {
			i := slices.Index(routeNames, name)
			held := seeded(t)
			held.asked.refuse = &auth.Error{Code: auth.CodeNotFound, Detail: "no rule allows it"}
			r := held.routes()[i]
			denied := held.drive(t, r.method, r.target, r.body)

			// The same request against a space holding nothing, where the
			// handler answers before it asks.
			empty := newHarness(t)
			gone := empty.drive(t, r.method, r.target, r.body)

			if denied.Code != gone.Code {
				t.Errorf("a denied %s answers %d and a missing one %d", name, denied.Code, gone.Code)
			}
			if strip(denied.Body.String()) != strip(gone.Body.String()) {
				t.Errorf("a denied %s answers\n %s\nand a missing one answers\n %s",
					name, denied.Body, gone.Body)
			}
		})
	}
}

// strip renders a refusal without the one field two of them are allowed to
// differ in: the request id. Everything else is compared, the developer
// detail included, because a field a caller can read is a field a caller can
// count.
func strip(body string) string {
	var envelope map[string]any
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		return body
	}
	if refusal, ok := envelope["error"].(map[string]any); ok {
		if details, ok := refusal["details"].(map[string]any); ok {
			delete(details, "request_id")
		}
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return body
	}
	return string(out)
}

// TestAStoreThatCannotAnswerIsAnOutageAndNeverAVerdict is invariant 2 of
// spec 001 at the handler: a database that will not answer is not a missing
// object, and a bucket that will not sign is not one either.
func TestAStoreThatCannotAnswerIsAnOutageAndNeverAVerdict(t *testing.T) {
	h := seeded(t)
	h.store.failGet = errors.New("the database is unreachable")
	for _, r := range h.routes() {
		// A listing and an unstar read no row: a listing reads a page, and
		// an unstar is idempotent by design, so a star whose target is gone
		// is still one the caller may drop.
		if slices.Contains([]string{"list", "materialize", "trash", "purge", "stars", "unstar"}, r.name) {
			continue
		}
		w := h.drive(t, r.method, r.target, r.body)
		if w.Code < 500 {
			t.Errorf("a %s against a database that would not answer is %d: %s", r.name, w.Code, w.Body)
		}
	}
}

// TestTheDefaultsOfABuildWhoseLaterSpecsHaveNotLanded: with no ledger, no
// workspace table and no reference check bound, the surface still serves and
// refuses nothing for a reason it cannot know.
func TestTheDefaultsOfABuildWhoseLaterSpecsHaveNotLanded(t *testing.T) {
	s := New(Options{DB: newMemory(), Config: configOf(8, 16)})
	if _, err := s.ledger.Charge(t.Context(), nil, "space", 1<<40, Limit{Bytes: 1, Set: true}); err != nil {
		t.Errorf("a build that counts nothing refused a charge: %v", err)
	}
	if _, err := s.ledger.Release(t.Context(), nil, "space", 1); err != nil {
		t.Errorf("a build that counts nothing refused a release: %v", err)
	}
	s.ledger.Append(t.Context(), nil, Event{Action: EventPut})
	live, err := s.workspaces.Live(t.Context(), nil, "space", "build")
	if err != nil || !live {
		t.Errorf("a build with no workspace table answered %t, %v", live, err)
	}
	if s.files == nil || s.versions == nil || s.stars == nil || s.references == nil {
		t.Error("a surface built with no query sets bound none")
	}
	if s.Now().IsZero() {
		t.Error("a surface built with no clock reads none")
	}
	if s.DB() == nil || s.Bucket() != nil || s.Config().InlineBytes != 8 ||
		s.Ledger() == nil || s.Files() == nil || s.Prefix() != "arca/" || s.Querier() != nil {
		t.Error("the seams spec 007's package reaches are not the ones it was given")
	}
}

// TestAWorkspaceThatIsNotLiveIsAMissingObjectToEveryone: spec 009 owns the
// table and spec 005 applies its rule, so a path under a workspace that is
// gone answers as absent whoever asks.
func TestAWorkspaceThatIsNotLiveIsAMissingObjectToEveryone(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Workspaces = deleted{} })
	w := h.call(t, http.MethodGet, h.object("workspaces/build/main.go"), nil)
	if got := code(t, w); got != api.CodeNotFound {
		t.Fatalf("a path under a deleted workspace is %q", got)
	}
	broken := newHarness(t, func(o *Options) { o.Workspaces = unreachable{} })
	if got := code(t, broken.call(t, http.MethodGet, broken.object("workspaces/build/main.go"), nil)); got != api.CodeStorageUnavailable {
		t.Fatalf("a workspace table that would not answer is %q", got)
	}
}

// deleted answers that every workspace is gone.
type deleted struct{}

func (deleted) Live(context.Context, store.Querier, string, string) (bool, error) {
	return false, nil
}

// unreachable answers nothing at all, which is not a verdict.
type unreachable struct{}

func (unreachable) Live(context.Context, store.Querier, string, string) (bool, error) {
	return false, errors.New("the database is unreachable")
}

// TestTheAnswersLimitIsReadOffTheAnswerAndNowhereElse: Arca stores no limit,
// so an answer with none leaves the space unlimited and an answer whose
// limits object is not one is a fault and not a refusal.
func TestTheAnswersLimitIsReadOffTheAnswerAndNowhereElse(t *testing.T) {
	for name, c := range map[string]struct {
		limits string
		want   Limit
		fails  bool
	}{
		"no limits at all":        {limits: "", want: Limit{}},
		"limits naming no quota":  {limits: `{"seats":3}`, want: Limit{}},
		"a quota":                 {limits: `{"quota_bytes":42}`, want: Limit{Bytes: 42, Set: true}},
		"limits that are not one": {limits: `not json`, fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := LimitOf(auth.Decision{Limits: []byte(c.limits)})
			if c.fails {
				if err == nil {
					t.Fatal("limits that are not one were read")
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("LimitOf = %+v, %v, want %+v", got, err, c.want)
			}
		})
	}
	over := &OverLimit{Owner: "space", Used: 10, Limit: 12, Delta: 5}
	if !strings.Contains(over.Error(), "10") || !strings.Contains(over.Error(), "12") {
		t.Errorf("the refusal does not name the two figures: %s", over.Error())
	}
}

// TestAFilterNarrowsAPageAndAnAnswerWithNoneNarrowsNothing.
func TestAFilterNarrowsAPageAndAnAnswerWithNoneNarrowsNothing(t *testing.T) {
	for name, c := range map[string]struct {
		filter *authz.Filter
		owner  string
		want   bool
	}{
		"no filter":            {filter: nil, owner: "a", want: true},
		"a filter naming none": {filter: &authz.Filter{}, owner: "a", want: true},
		"inside the filter":    {filter: &authz.Filter{Owners: []string{"a"}}, owner: "a", want: true},
		"outside it":           {filter: &authz.Filter{Owners: []string{"b"}}, owner: "a", want: false},
	} {
		if got := within(c.filter, c.owner); got != c.want {
			t.Errorf("%s = %t", name, got)
		}
	}
}

// TestTheRowsAreTheRoutesSpec013NamesForThisSpec reads the route table out of
// the spec and holds this package's declarations to it, which is criterion 1
// of spec 013 for the rows this spec owns.
func TestTheRowsAreTheRoutesSpec013NamesForThisSpec(t *testing.T) {
	spec := routesOfSpec013(t)
	for _, row := range Table() {
		key := row.Method + " " + row.Path
		action, named := spec[key]
		if !named {
			t.Errorf("%s is registered and spec 013's table does not name it", key)
			continue
		}
		if action != row.Action {
			t.Errorf("%s asks %q; spec 013's row says %q", key, row.Action, action)
		}
		if row.Summary == "" || row.Status == 0 {
			t.Errorf("%s is described as %+v", key, row)
		}
	}
	if len(Table()) != 12 {
		t.Errorf("this spec declares %d rows", len(Table()))
	}
}

var (
	// routeRow matches one row of a route table of spec 013: the method, the
	// path, and the action column.
	routeRow = regexp.MustCompile(`^\| (GET|POST|PUT|PATCH|DELETE|HEAD) \| ` + "`([^`]+)`" + ` \| ([^|]+) \|`)
	// tickedAction matches the action a row names, where it names one.
	tickedAction = regexp.MustCompile("`([a-z]+\\.[a-z]+)`")
)

// routesOfSpec013 reads every route table of specs/013-api.md: each row's
// method and path, mapped to the action its third column names. A row whose
// path carries a query selects a representation of a route already named, so
// the query is dropped and the first row of a path wins, which is the row
// that names the route's action for the method.
func routesOfSpec013(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "specs", "013-api.md"))
	if err != nil {
		t.Fatalf("spec 013: %v", err)
	}
	out := map[string]string{}
	for line := range strings.Lines(string(raw)) {
		m := routeRow.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		path, _, _ := strings.Cut(m[2], "?")
		key := m[1] + " " + path
		if _, seen := out[key]; seen {
			continue
		}
		action := ""
		if a := tickedAction.FindStringSubmatch(m[3]); a != nil {
			action = a[1]
		}
		out[key] = action
	}
	if len(out) == 0 {
		t.Fatal("spec 013 has no route table")
	}
	return out
}
