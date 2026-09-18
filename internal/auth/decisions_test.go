// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// This file is the internal test of the package: the seam below is set by
// [Start] and is not a field a caller sets, so a test that drove it from
// outside would be driving something that does not exist.
package auth

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/arca/authorizer"
)

// TestEveryDecisionIsRecordedByItsOutcome is spec 018's arca_decisions_total
// at this package's seam: an allow, a deny and an endpoint that did not
// answer are three outcomes, and an action outside the vocabulary is none of
// them because nothing decided it.
func TestEveryDecisionIsRecordedByItsOutcome(t *testing.T) {
	var got []string
	a := NewAuthorizer(answering{})
	a.decided = func(outcome string) { got = append(got, outcome) }

	res := authz.NewResource(authorizer.KindFile, "01J8R4", map[string]any{"owner": "https://issuer.example|9ab3"})
	if _, err := a.Decide(t.Context(), authorizer.ActionFileRead, res); err != nil {
		t.Errorf("the allow = %v", err)
	}
	if _, err := a.Decide(t.Context(), authorizer.ActionFileWrite, res); err == nil {
		t.Error("the deny was answered as an allow")
	}
	if _, err := a.Lookup(t.Context(), authorizer.ActionFileDelete, res); err == nil {
		t.Error("an endpoint that did not answer was answered as an allow")
	}
	if _, err := a.Decide(t.Context(), "file.teleport", res); err == nil {
		t.Error("an action outside the vocabulary was answered as an allow")
	}
	want := []string{OutcomeAllow, OutcomeDeny, OutcomeUnavailable}
	if !slices.Equal(got, want) {
		t.Errorf("the decisions recorded are %v, want %v", got, want)
	}
}

// TestAnAuthorizerWithNoSeamDecidesAnyway: the seam is optional, and a node
// that binds none still decides every request.
func TestAnAuthorizerWithNoSeamDecidesAnyway(t *testing.T) {
	a := NewAuthorizer(answering{})
	res := authz.NewResource(authorizer.KindFile, "01J8R4", map[string]any{"owner": "https://issuer.example|9ab3"})
	if _, err := a.Decide(t.Context(), authorizer.ActionFileRead, res); err != nil {
		t.Errorf("the allow = %v", err)
	}
}

// answering is an authorizer that gives one answer per action: an allow, a
// deny, an outage, and the refusal the shared client makes before the wire.
type answering struct{}

func (answering) Authorize(_ context.Context, req authz.Request) (authz.Decision, error) {
	switch req.Action {
	case authorizer.ActionFileRead:
		return authz.Decision{Allow: true, TTL: time.Minute}, nil
	case authorizer.ActionFileWrite:
		return authz.Decision{Reason: "no grant covers it"}, nil
	case authorizer.ActionFileDelete:
		return authz.Decision{}, errors.New("the endpoint did not answer")
	default:
		return authz.Decision{}, &authz.UnknownAction{Action: req.Action}
	}
}

// TestTheSourceIsFixedWhenTheNodeIsBuilt: which of the two decides is a
// property of the deployment and not of a request, so the label is settled
// once rather than carried through every call.
func TestTheSourceIsFixedWhenTheNodeIsBuilt(t *testing.T) {
	if source(nil, "authorizer") != nil {
		t.Error("a node that bound no seam was given a recorder anyway")
	}
	var got [2]string
	record := source(func(s, o string) { got = [2]string{s, o} }, "owner_policy")
	record(OutcomeAllow)
	if got != [2]string{"owner_policy", OutcomeAllow} {
		t.Errorf("the decision was recorded as %v", got)
	}
}
