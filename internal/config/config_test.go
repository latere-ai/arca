// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

func TestLoadAppliesEveryDefault(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{PublicAddr: ":8080", InternalAddr: ":8081"}
	if c != want {
		t.Fatalf("Load() = %+v, want %+v", c, want)
	}
}

func TestLoadReadsEveryVariable(t *testing.T) {
	c, err := Load(env(map[string]string{
		"ARCA_PUBLIC_ADDR":   "127.0.0.1:9000",
		"ARCA_INTERNAL_ADDR": "127.0.0.1:9001",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{PublicAddr: "127.0.0.1:9000", InternalAddr: "127.0.0.1:9001"}
	if c != want {
		t.Fatalf("Load() = %+v, want %+v", c, want)
	}
}

func TestLoadReportsEveryProblemInOneSortedMessage(t *testing.T) {
	_, err := Load(env(map[string]string{
		"ARCA_PUBLIC_ADDR":   "nope",
		"ARCA_INTERNAL_ADDR": "nope",
	}))
	if err == nil {
		t.Fatal("Load() accepted two bad addresses")
	}
	got := err.Error()
	for _, want := range []string{
		"configuration: ",
		`ARCA_INTERNAL_ADDR is "nope", not a host:port address`,
		`ARCA_PUBLIC_ADDR is "nope", not a host:port address`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q:\n%s", want, got)
		}
	}
	if i, j := strings.Index(got, "ARCA_INTERNAL_ADDR is"), strings.Index(got, "ARCA_PUBLIC_ADDR"); i > j {
		t.Errorf("problems are not sorted by name:\n%s", got)
	}
}

func TestLoadTreatsBlankAsUnset(t *testing.T) {
	c, err := Load(env(map[string]string{"ARCA_PUBLIC_ADDR": "  ", "ARCA_INTERNAL_ADDR": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicAddr != DefaultPublicAddr || c.InternalAddr != DefaultInternalAddr {
		t.Fatalf("blank values did not fall back to defaults: %+v", c)
	}
}

func TestLoadRefusesOneSocketForBothListeners(t *testing.T) {
	_, err := Load(env(map[string]string{"ARCA_PUBLIC_ADDR": "127.0.0.1:9000", "ARCA_INTERNAL_ADDR": "127.0.0.1:9000"}))
	if err == nil || !strings.Contains(err.Error(), "must differ from ARCA_PUBLIC_ADDR; both are 127.0.0.1:9000") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadAllowsPortZeroOnBothListeners(t *testing.T) {
	if _, err := Load(env(map[string]string{"ARCA_PUBLIC_ADDR": "127.0.0.1:0", "ARCA_INTERNAL_ADDR": "127.0.0.1:0"})); err != nil {
		t.Fatal(err)
	}
}
