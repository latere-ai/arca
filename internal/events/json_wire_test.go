// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package events

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestADetailIsStoredWithSortedKeys(t *testing.T) {
	got, err := marshalDetail(map[string]any{"z": 1, "a": "x", "m": true, "b": []string{"q"}})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"a":"x","b":["q"],"m":true,"z":1}`
	if string(got) != want {
		t.Errorf("the stored detail is %s, want %s", got, want)
	}
}

func TestADetailWithInvalidUTF8IsStoredWithTheReplacementCharacter(t *testing.T) {
	got, err := marshalDetail(map[string]any{"path": "a\xffb"})
	if err != nil {
		t.Fatalf("a path with a stray byte refused the append: %v", err)
	}
	if want := "{\"path\":\"a�b\"}"; string(got) != want {
		t.Errorf("the stored detail is %q, want %q", got, want)
	}
	if out := detailJSON(map[string]any{"path": "a\xffb"}); string(out) != "{\"path\":\"a�b\"}" {
		t.Errorf("the served detail is %q", out)
	}
}

func TestAnEntryWithoutADetailCarriesNone(t *testing.T) {
	w := httptest.NewRecorder()
	writePage(w, Page{Entries: []Event{{ID: 1, Owner: "o", Detail: map[string]any{"q": "a<b&c"}}, {ID: 2, Owner: "o"}}})
	body := w.Body.String()
	if !strings.Contains(body, `"detail":{"q":`) {
		t.Errorf("the page reads %s", body)
	}
	if strings.Contains(body, `"detail":null`) || strings.Count(body, `"detail"`) != 1 {
		t.Errorf("an entry without a detail carries one: %s", body)
	}
}
