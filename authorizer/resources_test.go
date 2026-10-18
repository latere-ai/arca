// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package authorizer

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"latere.ai/x/pkg/authz"
)

// filled is one fully populated resource per kind: every field the type can
// render, set to a value a question would carry. The wire form of each is
// what spec 006's "Resource fields" column is read against.
var filled = map[string]interface{ Resource() authz.Resource }{
	KindFile: File{
		ID: "01J8R4", Owner: "https://issuer.example|9ab3", Path: "files/reports/q3.pdf",
		Plane: "files", Size: Bytes(48213), From: "files/drafts/q3.pdf", Grant: "write",
		Recursive: true, Copy: true,
		Grants:          []Holder{{Subject: "https://issuer.example|4c1d", Permission: "write"}},
		GrantsTruncated: true,
	},
	KindUpload: Upload{
		Owner: "https://issuer.example|9ab3", Path: "files/video/keynote.mp4", Size: Bytes(1 << 30), Grant: "write",
		Grants: []Holder{{Subject: "https://issuer.example|4c1d", Permission: "write"}}, GrantsTruncated: true,
	},
	KindShare: Share{ID: "01J8R5", Owner: "https://issuer.example|9ab3", Path: "files/reports", Grantee: "https://issuer.example|4c1d", Permission: "read"},
	KindLink:  Link{ID: "01J8R6", Owner: "https://issuer.example|9ab3", Path: "files/reports"},
	KindEvent: Event{Owner: "https://issuer.example|9ab3"},
	KindSpace: Space{Owner: "https://issuer.example|9ab3"},
}

// extras are the members a kind renders that spec 006's table column does not
// list, each named in the prose of the same spec. "from" is the one: the
// table's rules say a move's resource carries the source path beside the
// target, which the column has no room for.
var extras = map[string][]string{KindFile: {"from"}}

// TestResourceShapesMatchSpec006 holds every resource type to the third
// column of spec 006's table, read from the spec: a field the column names
// and the type cannot render is a field an authorizer would wait forever
// for, and a field the type renders that neither the column nor the row's
// prose names is a field nobody decided to disclose.
func TestResourceShapesMatchSpec006(t *testing.T) {
	for _, row := range rowsOfSpec006(t) {
		value, ok := filled[row.kind]
		if !ok {
			t.Errorf("spec 006 names the kind %s and the package declares no resource type for it", row.kind)
			continue
		}
		want := slices.Concat(row.fields, extras[row.kind])
		slices.Sort(want)
		got := wireKeys(t, value.Resource())
		if !slices.Equal(got, want) {
			t.Errorf("%s renders the fields %v; spec 006's row names %v", row.kind, got, want)
		}
	}
	for kind := range filled {
		if !slices.ContainsFunc(rowsOfSpec006(t), func(r specRow) bool { return r.kind == kind }) {
			t.Errorf("the package declares a resource type for %s, which spec 006's table does not name", kind)
		}
	}
}

// TestEveryActionHasAResourceType: a handler asking any of the fifteen has a
// type to build the question's resource with.
func TestEveryActionHasAResourceType(t *testing.T) {
	for _, action := range Actions() {
		if _, ok := filled[Kind(action)]; !ok {
			t.Errorf("%s acts on %s, and the package declares no resource type for that kind", action, Kind(action))
		}
	}
}

// TestAnEmptyResourceCarriesOnlyItsKind: a field the server does not know is
// left out rather than sent empty, so an authorizer can tell a missing value
// from an unset one.
func TestAnEmptyResourceCarriesOnlyItsKind(t *testing.T) {
	for kind := range filled {
		var res authz.Resource
		switch kind {
		case KindFile:
			res = File{}.Resource()
		case KindUpload:
			res = Upload{}.Resource()
		case KindShare:
			res = Share{}.Resource()
		case KindLink:
			res = Link{}.Resource()
		case KindEvent:
			res = Event{}.Resource()
		case KindSpace:
			res = Space{}.Resource()
		}
		if res.Kind != kind {
			t.Errorf("the zero %s renders the kind %q", kind, res.Kind)
		}
		if got := wireKeys(t, res); len(got) != 0 {
			t.Errorf("the zero %s carries the fields %v; an unknown field is left out", kind, got)
		}
	}
}

// TestAZeroSizeIsSentAndAnAbsentOneIsNot: a zero-byte object has a size and
// an unlooked-up object has none, and the two are different answers to an
// authorizer counting bytes.
func TestAZeroSizeIsSentAndAnAbsentOneIsNot(t *testing.T) {
	zero := File{Owner: "https://issuer.example|9ab3", Path: "files/empty", Size: Bytes(0)}.Resource()
	if got, ok := zero.Fields["size"]; !ok || got.(int64) != 0 {
		t.Errorf("a zero-byte object sends size %v, present %v; it sends 0", got, ok)
	}
	absent := File{Owner: "https://issuer.example|9ab3", Path: "files/empty"}.Resource()
	if _, ok := absent.Fields["size"]; ok {
		t.Error("a question that carries no size sent one")
	}
}

// TestACreateCarriesNoID: the two resources of an action that makes
// something carry no id, because the id does not exist until the action is
// allowed.
func TestACreateCarriesNoID(t *testing.T) {
	if got := (Upload{Owner: "o", Path: "files/x"}).Resource().ID; got != "" {
		t.Errorf("an upload session carries the id %q; the object does not exist yet", got)
	}
	if got := (File{Owner: "o", Path: "files/x", Plane: "files"}).Resource().ID; got != "" {
		t.Errorf("a write that creates carries the id %q", got)
	}
}

// TestProbeIsTheSharedContractsID: the probe every authorizer denies is the
// family's one id of kind Space, so one check command reads one answer from
// every endpoint of the family.
func TestProbeIsTheSharedContractsID(t *testing.T) {
	p := Probe()
	if p.Kind != KindSpace {
		t.Errorf("the probe is of kind %q, want %q", p.Kind, KindSpace)
	}
	if p.ID != authz.ProbeID {
		t.Errorf("the probe id is %q; the shared contract's is %q", p.ID, authz.ProbeID)
	}
	if len(p.Fields) != 0 {
		t.Errorf("the probe carries the fields %v; it names no space", p.Fields)
	}
}

// wireKeys is the field names one resource puts on the wire, sorted, with
// the kind removed: the kind is the envelope's and every resource carries it.
func wireKeys(t *testing.T, res authz.Resource) []string {
	t.Helper()
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("the %s resource does not marshal: %v", res.Kind, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the %s resource does not read back: %v", res.Kind, err)
	}
	delete(out, "kind")
	return slices.Sorted(maps.Keys(out))
}

// TestRecursiveAndCopyAreSentOnlyWhenTrue: the two members of spec 028 are
// present and true or absent, so an endpoint that predates them reads the
// question it always read, and a question that carries from without copy is
// a move.
func TestRecursiveAndCopyAreSentOnlyWhenTrue(t *testing.T) {
	moved := File{Owner: "o", Path: "files/b", From: "files/a", Plane: "files"}.Resource()
	for _, member := range []string{"recursive", "copy"} {
		if _, present := moved.Fields[member]; present {
			t.Errorf("a move carries %s", member)
		}
	}
	copied := File{Owner: "o", Path: "files/b", From: "files/a", Plane: "files", Copy: true, Recursive: true}.Resource()
	for _, member := range []string{"recursive", "copy"} {
		if got, present := copied.Fields[member]; !present || got != true {
			t.Errorf("a folder copy sends %s as %v, present %v; it sends true", member, got, present)
		}
	}
	removed := File{Owner: "o", Path: "files/a", Plane: "files", Recursive: true}.Resource()
	if got := wireKeys(t, removed); !slices.Equal(got, []string{"owner", "path", "plane", "recursive"}) {
		t.Errorf("a folder delete carries %v; it carries no id and no size", got)
	}
}

// TestGrantsAreSentAsAListOfHolders: the grants member is an array of
// {subject, permission} objects in the order given, an empty list is sent
// as one, and a nil list and an untruncated one send nothing, so an
// endpoint tells "nobody holds a grant" from "this question does not say".
func TestGrantsAreSentAsAListOfHolders(t *testing.T) {
	listed := File{Owner: "o", Path: "files/a", Grants: []Holder{
		{Subject: "https://issuer.example|4c1d", Permission: "read"},
		{Subject: "https://issuer.example|9ab3", Permission: "manage"},
	}}.Resource()
	raw, err := json.Marshal(listed)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Grants []map[string]string `json:"grants"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	want := []map[string]string{
		{"subject": "https://issuer.example|4c1d", "permission": "read"},
		{"subject": "https://issuer.example|9ab3", "permission": "manage"},
	}
	if !slices.EqualFunc(wire.Grants, want, maps.Equal) {
		t.Errorf("grants went out as %v, want %v", wire.Grants, want)
	}
	if _, present := listed.Fields["grants_truncated"]; present {
		t.Error("a list that is whole carries grants_truncated")
	}

	none := Upload{Owner: "o", Path: "files/a", Grants: []Holder{}}.Resource()
	if got := wireKeys(t, none); !slices.Equal(got, []string{"grants", "owner", "path"}) {
		t.Errorf("a question nobody holds a grant on carries %v; it sends an empty grants", got)
	}
	if got, ok := none.Fields["grants"].([]any); !ok || len(got) != 0 {
		t.Errorf("an empty list renders as %#v", none.Fields["grants"])
	}
	if got := wireKeys(t, File{Owner: "o", Path: "files/a"}.Resource()); slices.Contains(got, "grants") {
		t.Errorf("a question that resolved no grants carries %v", got)
	}
	if Holders(nil) != nil {
		t.Error("nil holders render as a list")
	}
}
