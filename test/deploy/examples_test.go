// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package deploy

import (
	"fmt"
	"net/url"
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

// dialled are the variables whose value is an address arcad opens a
// connection to. ARCA_PUBLIC_URL is not one of them: it is the origin
// clients reach this installation at, and nothing in the process dials it.
//
// The collector is here under both of its spellings, because internal/config
// reads both: the table's own row, and the standard name an operator that
// instruments a whole namespace injects. An overlay that writes either into a
// manifest is held to the same egress rule as the bucket and the database.
//
// An injected endpoint reaches no manifest, so this test cannot see it and
// nothing here holds deploy/prod's collector port: 40318 is asserted by
// TestProdAdmitsTheDatabasePortsThisInstallationUses in prod_test.go, beside
// the database ports, which is the only place the pairing of a port with an
// endpoint no file in this tree carries can be asserted at all.
var dialled = []string{
	"ARCA_OIDC_ISSUERS",
	"ARCA_AUTHORIZER_URL",
	"ARCA_BUCKET_ENDPOINT",
	"ARCA_DATABASE_URL",
	"ARCA_OTEL_EXPORTER_OTLP_ENDPOINT",
	"OTEL_EXPORTER_OTLP_ENDPOINT",
}

// endpoints is every address an overlay configures arcad to dial, mapped to
// the variable that carries it: the container environment, and the Secrets
// the overlay writes for itself. An overlay whose Secrets an operator fills
// in by hand declares none, and there is then nothing here to hold it to.
func endpoints(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	keep := func(name, value string) {
		if !slices.Contains(dialled, name) || value == "" {
			return
		}
		// ARCA_OIDC_ISSUERS is a list, read the way internal/config reads it.
		for part := range strings.SplitSeq(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out[part] = name
			}
		}
	}
	for _, d := range read(t, dir) {
		for _, c := range d.containers() {
			for _, e := range c.items("env") {
				keep(e.text("name"), e.text("value"))
			}
		}
		if d.kind() != "Secret" {
			continue
		}
		if data := d.at("stringData"); data != nil {
			for _, e := range data.children {
				keep(e.key, e.value)
			}
		}
	}
	return out
}

// egressPorts is the union of the TCP ports the NetworkPolicies of these
// documents admit to a pod labelled app.kubernetes.io/name: arcad. Policies
// are additive, so the union is what a replica may reach.
func egressPorts(sets ...[]document) map[string]bool {
	out := map[string]bool{}
	for _, docs := range sets {
		for _, d := range docs {
			if d.kind() != "NetworkPolicy" ||
				d.text("spec", "podSelector", "matchLabels", "app.kubernetes.io/name") != "arcad" {
				continue
			}
			for _, rule := range d.items("spec", "egress") {
				for _, p := range rule.items("ports") {
					// The default protocol is TCP, which is what every
					// endpoint below is reached over.
					if proto := p.text("protocol"); proto == "" || proto == "TCP" {
						out[p.text("port")] = true
					}
				}
			}
		}
	}
	return out
}

// dialPort is the TCP port an endpoint is reached on: the one it names, or
// its scheme's.
func dialPort(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%s is not a URL", endpoint)
	}
	if port := u.Port(); port != "" {
		return port, nil
	}
	switch u.Scheme {
	case "https":
		return "443", nil
	case "http":
		return "80", nil
	case "postgres", "postgresql":
		return "5432", nil
	}
	return "", fmt.Errorf("%s names the scheme %q, whose port this test does not know", endpoint, u.Scheme)
}

// TestEveryOverlayAdmitsTheEgressItsEndpointsNeed: egress is an allow-list
// of ports, and every CNI that enforces policy drops what it does not
// admit rather than refusing it, so an endpoint on a port no policy names
// is a replica waiting out its own timeout against a dependency that is up
// and answering. That is how the kind stack of spec 016 crash-looped: the
// stub issuer on 8081, MinIO on 9000 and the stub authorizer on 8082 are
// none of the base's 53, 80, 443, 5432, 4317 and 4318.
func TestEveryOverlayAdmitsTheEgressItsEndpointsNeed(t *testing.T) {
	base := read(t, "deploy/base")
	for dir := range overlays(t) {
		admitted := egressPorts(base, read(t, dir))
		for endpoint, variable := range endpoints(t, dir) {
			port, err := dialPort(endpoint)
			if err != nil {
				t.Errorf("%s sets %s to %s: %v", dir, variable, endpoint, err)
				continue
			}
			if !admitted[port] {
				t.Errorf("%s sets %s to %s, and no NetworkPolicy admits egress to TCP %s; a cluster that enforces policy drops the connection and the replica waits out its own timeout",
					dir, variable, endpoint, port)
			}
		}
	}
}

// TestTheKindStubExpectsTheBearerArcadSends: the stub authorizer refuses
// every question whose bearer is not the one it was started with, and an
// authorizer that refuses the probe is one arcad's readiness reports as
// unavailable, silently, for as long as the pod lives. v0.1.2's release run
// was lost exactly so: arcad sent the Secret's ARCA_AUTHORIZER_TOKEN, the
// stub had been started with no -authorizer-token and so expected the
// package default, and /readyz answered 503 for five minutes with nothing
// in any log. The e2e tier passes because its harness hands both sides one
// value; the overlay had two.
//
// The assertion reads both out of the manifests. The stub's expectation is
// its -authorizer-token argument, or the package default when the argument
// is absent; arcad's is the Secret key its deployment reads. They must be
// one value, and the way the overlay makes them one is by having the stub
// read the same Secret key, so this test also accepts a $(VAR) expansion
// whose env entry points at that key.
func TestTheKindStubExpectsTheBearerArcadSends(t *testing.T) {
	var arcadToken, stubToken string
	var stubEnvFromSecretKey string
	for _, d := range read(t, "deploy/examples/kind") {
		switch {
		case d.kind() == "Secret" && d.named() == "arcad-auth":
			arcadToken = d.at("stringData").text("ARCA_AUTHORIZER_TOKEN")
		case d.kind() == "Deployment" && d.named() == "arca-stubs":
			for _, c := range d.containers() {
				args := c.strings("args")
				for i, a := range args {
					switch {
					case strings.HasPrefix(a, "-authorizer-token="):
						stubToken = strings.TrimPrefix(a, "-authorizer-token=")
					case a == "-authorizer-token" && i+1 < len(args):
						stubToken = args[i+1]
					}
				}
				for _, e := range c.items("env") {
					if ref := e.at("valueFrom", "secretKeyRef"); ref.text("name") == "arcad-auth" && ref.text("key") == "ARCA_AUTHORIZER_TOKEN" {
						stubEnvFromSecretKey = "$(" + e.text("name") + ")"
					}
				}
			}
		}
	}
	if arcadToken == "" {
		t.Fatal("deploy/examples/kind gives arcad no ARCA_AUTHORIZER_TOKEN")
	}
	const packageDefault = "stub-authorizer-token"
	switch {
	case stubToken == "":
		if arcadToken != packageDefault {
			t.Fatalf("the stub is started with no -authorizer-token and so expects %q; arcad sends %q; every probe is refused and readiness never passes", packageDefault, arcadToken)
		}
	case stubEnvFromSecretKey != "" && stubToken == stubEnvFromSecretKey:
		// One source: the stub reads the Secret key arcad reads.
	case stubToken != arcadToken:
		t.Fatalf("the stub expects %q and arcad sends %q", stubToken, arcadToken)
	}
}
