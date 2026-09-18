// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package deploy

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// kustomizations returns every kustomization.yaml of the deploy tree, keyed
// by the directory it is in.
func kustomizations(t *testing.T) map[string]document {
	t.Helper()
	out := map[string]document{}
	for _, d := range read(t, "deploy") {
		if filepath.Base(d.rel) == "kustomization.yaml" {
			out[filepath.Dir(d.rel)] = d
		}
	}
	return out
}

// overlays are the kustomizations that are not the base: each declares a
// namespace and each names ../../base or ../base.
func overlays(t *testing.T) map[string]document {
	t.Helper()
	out := map[string]document{}
	for dir, k := range kustomizations(t) {
		if dir != "deploy/base" {
			out[dir] = k
		}
	}
	return out
}

// TestOverlaysResolve proves that every path a kustomize render would read
// exists. It is not the render itself: kustomize is not on PATH under the
// gate's hermetic run and the module takes no test-only dependency for it,
// so `kubectl kustomize` over the base and every overlay runs in CI, in the
// render step of the release workflow. What this catches is the failure
// that actually happens, which is a file renamed or added without the
// kustomization that names it.
func TestOverlaysResolve(t *testing.T) {
	base := root(t)
	found := kustomizations(t)
	if _, ok := found["deploy/base"]; !ok {
		t.Fatal("no deploy/base/kustomization.yaml")
	}
	for dir, k := range found {
		var referenced []string
		referenced = append(referenced, k.strings("resources")...)
		referenced = append(referenced, k.strings("components")...)
		for _, p := range k.items("patches") {
			if path := p.text("path"); path != "" {
				referenced = append(referenced, path)
			}
		}
		if len(referenced) == 0 {
			t.Errorf("%s/kustomization.yaml names nothing", dir)
		}
		for _, ref := range referenced {
			if _, err := os.Stat(filepath.Join(base, dir, filepath.FromSlash(ref))); err != nil {
				t.Errorf("%s/kustomization.yaml names %s, which the tree does not hold", dir, ref)
			}
		}
	}
	for dir, k := range overlays(t) {
		if k.text("namespace") == "" {
			t.Errorf("%s declares no namespace; the base declares none, so an overlay must", dir)
		}
		names := k.strings("resources")
		if !containsAny(names, "../base", "../../base") {
			t.Errorf("%s resources = %v, want the base among them", dir, names)
		}
	}
}

func containsAny(have []string, want ...string) bool {
	for _, w := range want {
		if slices.Contains(have, w) {
			return true
		}
	}
	return false
}

// TestEveryOverlaySetsThePublicURL holds each overlay to the one value the
// base cannot carry. ARCA_PUBLIC_URL has no default and every URL the
// server writes is built from it, so an overlay that omits it deploys a
// server that refuses to start.
func TestEveryOverlaySetsThePublicURL(t *testing.T) {
	for dir := range overlays(t) {
		set := ""
		for _, d := range read(t, dir) {
			for _, c := range d.containers() {
				for _, e := range c.items("env") {
					if e.text("name") == "ARCA_PUBLIC_URL" {
						set = e.text("value")
					}
				}
			}
		}
		switch {
		case set == "":
			t.Errorf("%s sets no ARCA_PUBLIC_URL; the origin clients reach is the one value no base can guess", dir)
		case !strings.HasPrefix(set, "http://") && !strings.HasPrefix(set, "https://"):
			t.Errorf("%s sets ARCA_PUBLIC_URL to %q, which is not an origin", dir, set)
		}
	}
}

// installations are the two spellings of one company's own deployment: the
// public host and the cluster-internal one.
var installations = []string{"latere.ai", "latere.svc"}

// TestOnlyProdNamesAnInstallation is spec 016's rule that the tree an
// operator copies carries no address of the company that hosts Arca.
// deploy/prod is the one directory that may, and it is declared in
// .lateregate.yaml under both identity.skip and identity.overlays so the
// gate reads the addresses it sets rather than refusing them. Everything
// else names a host under example.com or under localhost.
//
// The registry namespace ghcr.io/latere-ai is written with a hyphen and is
// not one of these spellings: it is where the images are published, which
// a fork overrides in one place, and not an address anything is served at.
func TestOnlyProdNamesAnInstallation(t *testing.T) {
	base := root(t)
	err := filepath.WalkDir(filepath.Join(base, "deploy"), func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(base, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "deploy/prod/") {
			return nil
		}
		raw, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(raw), "\n") {
			for _, name := range installations {
				if strings.Contains(line, name) {
					t.Errorf("%s:%d: names %s; only deploy/prod may name the installation it serves (spec 016)",
						rel, i+1, name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk deploy: %v", err)
	}
}

// TestTheKindStackPublishesWhatATestReaches keeps the host ports of the
// cluster file and the node ports of the overlay in step. They are written
// in two files and a mismatch is invisible until a smoke run times out
// against a port nothing listens on.
func TestTheKindStackPublishesWhatATestReaches(t *testing.T) {
	published := map[string]bool{}
	for _, d := range read(t, "deploy/examples/kind") {
		if d.kind() != "Cluster" {
			continue
		}
		for _, n := range d.items("nodes") {
			for _, m := range n.items("extraPortMappings") {
				if got, want := m.text("hostPort"), m.text("containerPort"); got != want {
					t.Errorf("the kind cluster maps containerPort %s to hostPort %s; a test reaches localhost", want, got)
				}
				published[m.text("containerPort")] = true
			}
		}
	}
	if len(published) == 0 {
		t.Fatal("the kind cluster publishes no port")
	}
	for _, d := range read(t, "deploy/examples/kind") {
		if d.kind() != "Service" {
			continue
		}
		for _, p := range d.items("spec", "ports") {
			port := p.text("nodePort")
			if port == "" {
				continue
			}
			if !published[port] {
				t.Errorf("%s publishes nodePort %s, which kind.yaml does not map to a host port", d.rel, port)
			}
		}
	}
}

// TestTheStackScriptsAreExecutable keeps up.sh and down.sh runnable from a
// clone. A script the install document tells an operator to run, that they
// must chmod first, is a step the document does not have.
func TestTheStackScriptsAreExecutable(t *testing.T) {
	for _, name := range []string{"up.sh", "down.sh"} {
		info, err := os.Stat(filepath.Join(root(t), "deploy/examples/kind", name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("deploy/examples/kind/%s is not executable", name)
		}
	}
}
