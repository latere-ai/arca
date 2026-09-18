// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"latere.ai/x/arca/internal/auth"
)

// alice is the caller every case below carries.
const alice = "https://issuer.example|9ab3"

// asAlice is a request whose caller is alice, the way the verifier leaves
// one.
func asAlice(target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	return r.WithContext(auth.WithCaller(r.Context(), auth.Caller{Subject: alice}))
}

// TestOwnerResolvesTheAliasAndTheSubject is criterion 5 of spec 013 as far
// as this helper reaches: me is the caller, a subject addresses the space it
// names, and a response's owner sent back addresses the same space.
func TestOwnerResolvesTheAliasAndTheSubject(t *testing.T) {
	r := asAlice("/v1/shares")
	for _, c := range []struct{ name, want string }{
		{"", alice},
		{Me, alice},
		{"https://issuer.example|other", "https://issuer.example|other"},
	} {
		got, err := Owner(r.Context(), c.name)
		if err != nil || got != c.want {
			t.Errorf("Owner(%q) = %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

// TestTheAliasNeedsACaller: the three public link routes carry none, so the
// alias is a field a caller cannot use there.
func TestTheAliasNeedsACaller(t *testing.T) {
	anonymous := httptest.NewRequest(http.MethodGet, "/v1/shares/links/t0ken", nil)
	if _, err := Owner(anonymous.Context(), Me); err == nil {
		t.Error("me resolved with no caller")
	}
	if _, err := Owner(anonymous.Context(), ""); err == nil {
		t.Error("an absent owner resolved with no caller")
	}
	if got, err := Owner(anonymous.Context(), alice); err != nil || got != alice {
		t.Errorf("a named space needs no caller: %q, %v", got, err)
	}
}

// TestOwnerOfReadsThePathBeforeTheQuery: one rule, two spellings, and the
// path wins where a route carries both.
func TestOwnerOfReadsThePathBeforeTheQuery(t *testing.T) {
	mux := http.NewServeMux()
	var got string
	mux.HandleFunc("GET /v1/files/{owner}/{path...}", func(_ http.ResponseWriter, r *http.Request) {
		owner, err := OwnerOf(r)
		if err != nil {
			t.Errorf("OwnerOf: %v", err)
		}
		got = owner
	})
	mux.ServeHTTP(httptest.NewRecorder(),
		asAlice("/v1/files/https%3A%2F%2Fissuer.example%7Cother/files/reports?owner=ignored"))
	if got != "https://issuer.example|other" {
		t.Errorf("OwnerOf read %q from the path", got)
	}

	if owner, err := OwnerOf(asAlice("/v1/shares?owner=https%3A%2F%2Fissuer.example%7Cother")); err != nil ||
		owner != "https://issuer.example|other" {
		t.Errorf("OwnerOf read %q from the query, %v", owner, err)
	}
	if owner, err := OwnerOf(asAlice("/v1/shares")); err != nil || owner != alice {
		t.Errorf("OwnerOf with no owner named = %q, %v", owner, err)
	}
}
