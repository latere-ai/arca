// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth_test

import (
	"slices"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/auth"
)

// TestTheLimitsAreReadWhole: the limits object of an answer is read in one
// place, member by member, and an answer that carries none limits nothing.
// A member this server does not know is not read, so an endpoint may carry
// one for a newer server.
func TestTheLimitsAreReadWhole(t *testing.T) {
	quota := func(n int64) *int64 { return &n }
	for _, c := range []struct {
		name string
		raw  string
		want auth.Limits
	}{
		{"no limits object", ``, auth.Limits{}},
		{"an empty object", `{}`, auth.Limits{}},
		{"a null object", `null`, auth.Limits{}},
		{"a member this server does not know", `{"seats":3}`, auth.Limits{}},
		{"a byte limit", `{"quota_bytes":42}`, auth.Limits{QuotaBytes: quota(42)}},
		{"a byte limit of nothing", `{"quota_bytes":0}`, auth.Limits{QuotaBytes: quota(0)}},
		{"versions off", `{"versions":false}`, auth.Limits{NoVersions: true}},
		{"versions on", `{"versions":true}`, auth.Limits{}},
		{"versions null", `{"versions":null}`, auth.Limits{}},
		{"the trash off", `{"trash":false}`, auth.Limits{NoTrash: true}},
		{"the trash on", `{"trash":true}`, auth.Limits{}},
		{"an omission", `{"omit":["files/team/payroll","files/agents/7c1e"]}`,
			auth.Limits{Omit: []string{"files/team/payroll", "files/agents/7c1e"}}},
		{"an empty omission", `{"omit":[]}`, auth.Limits{Omit: []string{}}},
		{"a null omission", `{"omit":null}`, auth.Limits{}},
		{"every member", `{"quota_bytes":7,"versions":false,"trash":false,"omit":["files/a"]}`,
			auth.Limits{QuotaBytes: quota(7), NoVersions: true, NoTrash: true, Omit: []string{"files/a"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := auth.ReadLimits([]byte(c.raw))
			if err != nil {
				t.Fatalf("ReadLimits(%s): %v", c.raw, err)
			}
			if (got.QuotaBytes == nil) != (c.want.QuotaBytes == nil) ||
				(got.QuotaBytes != nil && *got.QuotaBytes != *c.want.QuotaBytes) {
				t.Errorf("the byte limit of %s is %v", c.raw, got.QuotaBytes)
			}
			if got.NoVersions != c.want.NoVersions || got.NoTrash != c.want.NoTrash {
				t.Errorf("%s reads as no versions=%t, no trash=%t", c.raw, got.NoVersions, got.NoTrash)
			}
			if !slices.Equal(got.Omit, c.want.Omit) {
				t.Errorf("%s omits %v, want %v", c.raw, got.Omit, c.want.Omit)
			}
		})
	}
}

// TestALimitThatIsNotItsTypeIsAnError: a member that is not the type spec
// 006 gives it is the endpoint saying something this server cannot honor,
// so nothing of the answer's limits is read and the caller is told.
func TestALimitThatIsNotItsTypeIsAnError(t *testing.T) {
	for _, raw := range []string{
		`not json`,
		`[]`,
		`{"quota_bytes":"plenty"}`,
		`{"quota_bytes":1.5}`,
		`{"versions":"false"}`,
		`{"versions":0}`,
		`{"trash":"no"}`,
		`{"trash":[]}`,
		`{"omit":"files/team"}`,
		`{"omit":{"files/team":true}}`,
		`{"omit":[1]}`,
		`{"omit":[null]}`,
		`{"versions":false,"omit":[true]}`,
	} {
		if got, err := auth.ReadLimits([]byte(raw)); err == nil {
			t.Errorf("ReadLimits(%s) = %+v, want an error", raw, got)
		}
	}
}

// TestAnOmitEntryIsAPathOfTheSpace: an entry of limits.omit is a path prefix
// in the shape a path has, under files/. One entry that is not refuses the
// whole answer, so an omission is never applied in part.
func TestAnOmitEntryIsAPathOfTheSpace(t *testing.T) {
	for _, entry := range []string{
		"files/a", "files/team/payroll", "files/team/pay roll/2026", "files/100%_done", "files/a.b/c",
	} {
		if _, err := auth.ReadLimits([]byte(`{"omit":["` + entry + `"]}`)); err != nil {
			t.Errorf("the entry %q was refused: %v", entry, err)
		}
	}
	for _, entry := range []string{
		"", "files", "files/", "/files/a", "files/a/", "files//a", "files/./a", "files/../a", "files/a/..",
		"team/payroll", "memory/a", "Files/a", `files/a\u0000b`, `files/a\nb`, "..",
	} {
		raw := `{"omit":["files/kept","` + entry + `"]}`
		got, err := auth.ReadLimits([]byte(raw))
		if err == nil {
			t.Errorf("the entry %q was accepted", entry)
		}
		if len(got.Omit) != 0 {
			t.Errorf("an answer with the entry %q was applied in part: %v", entry, got.Omit)
		}
	}
}

// TestTheOwnerPolicySendsNoLimits: the built-in policy carries no limits
// object on any answer, so an installation without an authorizer keeps
// versions and the trash everywhere and leaves nothing out of an
// enumeration (spec 028).
func TestTheOwnerPolicySendsNoLimits(t *testing.T) {
	p := policy(auth.PermissionManage, "files")
	for _, subject := range []string{alice, bob, carol} {
		for _, action := range authorizer.Actions() {
			res := authz.NewResource(authorizer.Kind(action), "01J8R4", map[string]any{
				"owner": alice, "path": "files/reports/q3.pdf",
			})
			d, err := p.Authorize(t.Context(), authz.Request{
				Subject: subject, Action: action, Resource: res, Claims: map[string]any{},
			})
			if err != nil {
				t.Fatalf("%s asking %s: %v", subject, action, err)
			}
			if len(d.Limits) != 0 {
				t.Errorf("the owner policy answered %s asking %s with the limits %s", subject, action, d.Limits)
			}
		}
	}
}
