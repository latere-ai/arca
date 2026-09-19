// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package events

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/auth"
)

// Criterion 9 of spec 012, the reading half. The decision path marks a
// request that was allowed on a space its caller neither owns nor holds a
// covering grant on; the append puts that on the row. No writer passes the
// key and no writer can, which is what "wherever it happened" means: a route
// added later is recorded without being told to be.

// aStranger is a subject that owns nothing in aSpace.
const aStranger = "https://issuer.example|11c4f0a9"

// deciding drives one real allow through the decision path and answers the
// context the request carries afterwards. It is the real seam and not a flag
// set by hand: what this test is about is that the two ends agree.
func deciding(t *testing.T, caller, owner string) context.Context {
	t.Helper()
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	client, err := auth.NewClient(auth.ClientOptions{
		URL: s.URL(), Token: s.Token(), HTTP: &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("the client would not build: %v", err)
	}
	issuer, sub, _ := strings.Cut(caller, "|")
	ctx := auth.WithMarks(auth.WithCaller(t.Context(),
		auth.Caller{Subject: caller, Issuer: issuer, Sub: sub, Claims: map[string]any{}}))
	res := authorizer.File{
		ID: "01J8R4", Owner: owner, Path: "files/reports/q3.pdf", Plane: "files",
	}.Resource()
	if _, err := auth.NewAuthorizer(client).Lookup(ctx, authorizer.ActionFileDelete, res); err != nil {
		t.Fatalf("the allow was refused: %v", err)
	}
	return ctx
}

// appended answers the detail the append bound, decoded off the statement's
// arguments, which is the bytes the column holds.
func appended(t *testing.T, q *fakeQuerier) map[string]any {
	t.Helper()
	raw, ok := q.args[0][4].([]byte)
	if !ok || len(raw) == 0 {
		return nil
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("the detail is not JSON: %v", err)
	}
	return detail
}

func TestAnAdministrativeAllowMarksTheEventTheMutationAppends(t *testing.T) {
	q := &fakeQuerier{row: values(int64(8841))}
	e := anEvent(aSpace, ActionDelete)
	written := map[string]any{"trashed": true}
	e.Detail = written
	e.Actor = aStranger

	if _, err := NewLog().Append(deciding(t, aStranger, aSpace), q, e); err != nil {
		t.Fatal(err)
	}
	detail := appended(t, q)
	if detail[DetailAdmin] != true {
		t.Errorf("the moderation delete appended %v and carries no admin mark", detail)
	}
	if detail["trashed"] != true {
		t.Errorf("the mark replaced what the writer passed: %v", detail)
	}
	if _, marked := written[DetailAdmin]; marked {
		t.Error("the append wrote the mark into the map the writer handed it")
	}
}

// TestAnOwnersOwnMutationMarksNothing: the mark says neither ownership nor a
// grant explains the allow, so a space acting in itself carries none. A
// reader that saw admin on every event would learn nothing from it.
func TestAnOwnersOwnMutationMarksNothing(t *testing.T) {
	q := &fakeQuerier{row: values(int64(8842))}
	e := anEvent(aSpace, ActionDelete)
	e.Detail = map[string]any{"trashed": true}

	if _, err := NewLog().Append(deciding(t, aSpace, aSpace), q, e); err != nil {
		t.Fatal(err)
	}
	if detail := appended(t, q); detail[DetailAdmin] != nil {
		t.Errorf("an owner's own delete appended %v", detail)
	}
}

// TestAnEventWithNoDetailStillCarriesTheMark: the detail of a hard delete is
// empty, and an administrative one has exactly one key. An append that only
// added the mark to a detail that already existed would leave the plainest
// moderation unrecorded.
func TestAnEventWithNoDetailStillCarriesTheMark(t *testing.T) {
	q := &fakeQuerier{row: values(int64(8843))}
	e := anEvent(aSpace, ActionDelete)
	e.Actor = aStranger

	if _, err := NewLog().Append(deciding(t, aStranger, aSpace), q, e); err != nil {
		t.Fatal(err)
	}
	detail := appended(t, q)
	if len(detail) != 1 || detail[DetailAdmin] != true {
		t.Errorf("a moderation delete with no detail of its own appended %v", detail)
	}
}

// TestTheReapersEventsCarryNoMark: the reaper appends on a context no
// middleware touched and no caller is on, so the mark is neither set nor
// read. An installation reading admin on a purge would be reading the
// server's own housekeeping as somebody's touch.
func TestTheReapersEventsCarryNoMark(t *testing.T) {
	q := &fakeQuerier{row: values(int64(8844))}
	e := Event{Owner: aSpace, Action: ActionReap, Detail: map[string]any{"trash_purged": 3}}

	if _, err := NewLog().Append(context.Background(), q, e); err != nil {
		t.Fatal(err)
	}
	if detail := appended(t, q); detail[DetailAdmin] != nil {
		t.Errorf("a reap appended %v", detail)
	}
}
