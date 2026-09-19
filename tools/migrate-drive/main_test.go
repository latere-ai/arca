// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"strings"
	"testing"
)

// unreachable is a database URL that parses and answers nothing, so a case
// reaches the connection step without a store beside it.
const unreachable = "postgres://arca:arca@127.0.0.1:1/arca?sslmode=disable&connect_timeout=1"

func TestABadFlagIsATwo(t *testing.T) {
	var out, errs bytes.Buffer
	if code := cli(t.Context(), []string{"-nope"}, &out, &errs); code != exitUsage {
		t.Errorf("exit %d, want %d", code, exitUsage)
	}
	if code := cli(t.Context(), []string{"stray"}, &out, &errs); code != exitUsage {
		t.Errorf("exit %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errs.String(), "stray") {
		t.Errorf("the message names no argument: %s", errs.String())
	}
}

func TestTheFlagsThatHaveToHold(t *testing.T) {
	for _, c := range []struct {
		args  []string
		holds []string
	}{
		{nil, []string{"-source", "-target", "-issuer"}},
		{[]string{"-source", "postgres://a", "-target", "postgres://b"}, []string{"-issuer"}},
		{
			[]string{"-source", "postgres://a", "-target", "postgres://b", "-issuer", "not a url"},
			[]string{"not an absolute URL"},
		},
		{
			[]string{"-source", "postgres://a", "-target", "postgres://b",
				"-issuer", "https://issuer.example", "-prefix", "/"},
			[]string{"-prefix", "cannot be empty"},
		},
	} {
		var out, errs bytes.Buffer
		if code := cli(t.Context(), c.args, &out, &errs); code != exitRefused {
			t.Errorf("%v: exit %d, want %d", c.args, code, exitRefused)
		}
		for _, text := range c.holds {
			if !strings.Contains(errs.String(), text) {
				t.Errorf("%v: the refusal holds no %q: %s", c.args, text, errs.String())
			}
		}
	}
}

func TestThePrefixAndTheInstallationHaveToAgree(t *testing.T) {
	o := options{
		source: "postgres://a", target: "postgres://b",
		issuer: "https://issuer.example", prefix: "drive/", bucketPrefix: "arca/",
	}
	_, err := o.check()
	if err == nil {
		t.Fatal("a copy under one prefix for a server deployed with another is a refusal")
	}
	if !strings.Contains(err.Error(), BucketPrefixVar) {
		t.Errorf("the refusal names no variable: %v", err)
	}
	o.bucketPrefix = "drive"
	if _, err := o.check(); err != nil {
		t.Errorf("the same prefix written without its slash is a refusal: %v", err)
	}
	o.bucketPrefix = ""
	if prefix, err := o.check(); err != nil || prefix != "drive/" {
		t.Errorf("check = %q, %v", prefix, err)
	}
}

func TestNormalisePrefixEndsInOneSlash(t *testing.T) {
	for _, c := range []struct{ from, want string }{
		{"drive/", "drive/"},
		{"drive", "drive/"},
		{"/drive/", "drive/"},
		{"drive//", "drive/"},
		{"", ""},
		{"/", ""},
	} {
		if got := normalisePrefix(c.from); got != c.want {
			t.Errorf("normalisePrefix(%q) = %q, want %q", c.from, got, c.want)
		}
	}
}

func TestAMappingThatDoesNotReadStopsTheRunBeforeADatabaseIsOpened(t *testing.T) {
	var out, errs bytes.Buffer
	code := cli(t.Context(), []string{
		"-source", unreachable, "-target", unreachable,
		"-issuer", "https://issuer.example", "-org-subjects", "/nope/orgs.json",
	}, &out, &errs)
	if code != exitRefused {
		t.Errorf("exit %d, want %d", code, exitRefused)
	}
	if !strings.Contains(errs.String(), "organization mapping") {
		t.Errorf("the message is %q", errs.String())
	}
}

func TestADatabaseThatDoesNotAnswerIsARefusalAndNotAPanic(t *testing.T) {
	var out, errs bytes.Buffer
	code := cli(t.Context(), []string{
		"-source", unreachable, "-target", unreachable, "-issuer", "https://issuer.example",
	}, &out, &errs)
	if code != exitRefused {
		t.Errorf("exit %d, want %d", code, exitRefused)
	}
	if !strings.Contains(errs.String(), "reach") {
		t.Errorf("the message is %q", errs.String())
	}
	if strings.Contains(errs.String(), "arca:arca@") {
		t.Errorf("the message carries a password: %q", errs.String())
	}
}
