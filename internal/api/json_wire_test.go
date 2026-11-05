// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"errors"
	"strings"
	"testing"
)

func TestASecondJSONValueInTheBodyIsRefusedAsBadRequest(t *testing.T) {
	_, err := DecodeBody[star](bodied(starred+` `+starred, JSONMediaType))
	if code := refusalOf(t, err); code != CodeBadRequest {
		t.Fatalf("two documents = %q", code)
	}
	if !strings.Contains(err.Error(), "more than one JSON value") {
		t.Errorf("the refusal reads %q", err)
	}
	if _, err := DecodeBody[star](bodied(starred+"\n  \n", JSONMediaType)); err != nil {
		t.Errorf("trailing whitespace is refused: %v", err)
	}
}

func TestAnEmptyBodyIsRefusedAsBadRequest(t *testing.T) {
	if code := refusalOf(t, second(DecodeBody[star](bodied("", JSONMediaType)))); code != CodeBadRequest {
		t.Fatalf("an empty body = %q", code)
	}
}

func TestAnUnknownMemberUnderANestedObjectNamesTheMember(t *testing.T) {
	type outer struct {
		Inner star `json:"inner"`
	}
	_, err := DecodeBody[outer](bodied(`{"inner":{"owner":"me","kind":"pin"}}`, JSONMediaType))
	r, ok := errors.AsType[*Refusal](err)
	if !ok || len(r.Fields) != 1 || r.Fields[0] != "kind" {
		t.Fatalf("the nested refusal read %v", err)
	}
}
