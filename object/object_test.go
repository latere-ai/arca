// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package object

import (
	"strings"
	"testing"
)

// theID is one minted id, written out so the shard and the key in the
// assertions below are read from the text rather than computed by the code
// under test.
const theID = ID("0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a1f")

func TestNewIDMintsACanonicalVersionSevenID(t *testing.T) {
	seen := map[ID]bool{}
	var previous ID
	for range 100 {
		id := NewID()
		if _, err := ParseID(string(id)); err != nil {
			t.Fatalf("NewID() = %q, which ParseID refuses: %v", id, err)
		}
		if got := string(id)[14]; got != '7' {
			t.Fatalf("NewID() = %q, version %c, want 7", id, got)
		}
		if seen[id] {
			t.Fatalf("NewID() repeated %q", id)
		}
		seen[id] = true
		if id < previous {
			t.Fatalf("NewID() = %q after %q; version 7 ids sort by the order they were minted in", id, previous)
		}
		previous = id
	}
}

func TestParseIDRefusesEverySpellingButTheCanonicalOne(t *testing.T) {
	if got, err := ParseID(string(theID)); err != nil || got != theID {
		t.Fatalf("ParseID(%q) = %q, %v", theID, got, err)
	}
	for _, text := range []string{
		"",
		"not a uuid",
		"0192F0C3-6C1A-7B3E-9A2E-6B7C8D9E0A1F",
		"{0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a1f}",
		"urn:uuid:0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a1f",
		"0192f0c36c1a7b3e9a2e6b7c8d9e0a1f",
	} {
		if got, err := ParseID(text); err == nil {
			t.Errorf("ParseID(%q) = %q, want an error", text, got)
		}
	}
}

func TestKeyPutsTheShardFromTheTailUnderThePrefix(t *testing.T) {
	if got, want := theID.Shard(), "1f"; got != want {
		t.Fatalf("Shard() = %q, want %q", got, want)
	}
	if got, want := theID.Key("arca/"), "arca/1f/0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a1f"; got != want {
		t.Fatalf("Key() = %q, want %q", got, want)
	}
	if got, want := theID.Key(""), "1f/0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a1f"; got != want {
		t.Fatalf("Key() with no prefix = %q, want %q", got, want)
	}
}

func TestAnIDThatIsNoIDHasNoShardAndNoKey(t *testing.T) {
	broken := ID("nope")
	if got := broken.Shard(); got != "" {
		t.Errorf("Shard() = %q, want the empty string", got)
	}
	if got := broken.Key("arca/"); got != "" {
		t.Errorf("Key() = %q, want the empty string", got)
	}
}

func TestParseKeyReadsBackTheIDAndRefusesTheRest(t *testing.T) {
	id, err := ParseKey("arca/", theID.Key("arca/"))
	if err != nil || id != theID {
		t.Fatalf("ParseKey() = %q, %v", id, err)
	}
	for name, key := range map[string]string{
		"another prefix":      "other/1f/0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a1f",
		"no shard":            "arca/0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a1f",
		"no id":               "arca/1f/hello",
		"a disagreeing shard": "arca/ab/0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a1f",
	} {
		if got, err := ParseKey("arca/", key); err == nil {
			t.Errorf("%s: ParseKey(%q) = %q, want an error", name, key, got)
		}
	}
}

// FuzzKeyRoundTrip is criterion 1 of spec 003: whatever the prefix, a key
// built from an id reads back as that id.
func FuzzKeyRoundTrip(f *testing.F) {
	f.Add("arca/", string(theID))
	f.Add("", "0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a00")
	f.Add("a/b/", "ffffffff-ffff-7fff-bfff-ffffffffffff")
	f.Fuzz(func(t *testing.T, prefix, text string) {
		id, err := ParseID(text)
		if err != nil {
			t.Skip()
		}
		key := id.Key(prefix)
		if !strings.HasPrefix(key, prefix) {
			t.Fatalf("Key(%q) = %q, which is not under the prefix", prefix, key)
		}
		got, err := ParseKey(prefix, key)
		if err != nil {
			t.Fatalf("ParseKey(%q, %q): %v", prefix, key, err)
		}
		if got != id {
			t.Fatalf("round trip of %q under %q gave %q", id, prefix, got)
		}
	})
}
