// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package deploy tests what a release ships beside the binary: the
// Kubernetes manifests under deploy/, which are the only description of how
// an installation runs arcad and which nothing else in the build reads. A
// lost security field, a credential written as a literal, or an overlay that
// names a file it does not hold is invisible until someone applies it, so
// spec 016 asserts them here.
//
// The tests walk each kustomization's own resource and patch lists rather
// than naming files, so a manifest that exists but is not listed is one
// these tests cannot see, exactly as kustomize would not see it.
package deploy

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// root is the repository root.
func root(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// document is one YAML document of one file, with the path it came from.
type document struct {
	rel string
	*node
}

func (d document) kind() string  { return d.text("kind") }
func (d document) named() string { return d.text("metadata", "name") }

// containers returns every container of a workload, init containers
// included. A document that is not a workload has none.
func (d document) containers() []*node {
	spec := d.at("spec", "template", "spec")
	var out []*node
	out = append(out, spec.items("initContainers")...)
	out = append(out, spec.items("containers")...)
	return out
}

// read parses every manifest under a directory of the deploy tree, failing
// the test on a file the reader cannot read: a manifest nothing could read
// is a manifest nothing checked.
func read(t *testing.T, dir string) []document {
	t.Helper()
	var out []document
	base := root(t)
	err := filepath.WalkDir(filepath.Join(base, dir), func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		if ext := filepath.Ext(p); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		rel, relErr := filepath.Rel(base, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		raw, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		docs, parseErr := parse(string(raw))
		if parseErr != nil {
			t.Errorf("%s: %v", rel, parseErr)
			return nil
		}
		for _, d := range docs {
			out = append(out, document{rel: rel, node: d})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s holds no manifest", dir)
	}
	return out
}

// imageName is the name an image was built under: the last path segment,
// without the tag and without the digest. It is what tells this
// repository's own container from MinIO, Postgres, or the stub image
// running beside it.
func imageName(image string) string {
	if i := strings.IndexByte(image, '@'); i >= 0 {
		image = image[:i]
	}
	image = image[strings.LastIndexByte(image, '/')+1:]
	if i := strings.IndexByte(image, ':'); i >= 0 {
		image = image[:i]
	}
	return image
}

// credentialNames match a variable whose value is a secret. A manifest that
// wrote one as a literal would put it in git and in every `kubectl get` a
// reader can run.
var credentialNames = regexp.MustCompile(`_(TOKEN|SECRET_KEY|ACCESS_KEY|PASSWORD|DATABASE_URL)$`)

// assertCredentialsAreReferences is the gate's bearer rule over one
// directory: every credential arrives from a Secret, and no two variables
// of one container read the same key, because a credential between two
// endpoints is per endpoint.
func assertCredentialsAreReferences(t *testing.T, dir string) {
	t.Helper()
	for _, d := range read(t, dir) {
		for _, c := range d.containers() {
			refs := map[string]string{}
			for _, e := range c.items("env") {
				name := e.text("name")
				if !credentialNames.MatchString(name) {
					continue
				}
				if e.text("value") != "" {
					t.Errorf("%s: %s is written as a literal; read it from a Secret", d.rel, name)
					continue
				}
				ref := e.at("valueFrom", "secretKeyRef")
				if ref == nil {
					t.Errorf("%s: %s has no value and no secretKeyRef", d.rel, name)
					continue
				}
				key := ref.text("name") + "/" + ref.text("key")
				if other, ok := refs[key]; ok {
					t.Errorf("%s: %s and %s both read %s; every credential is per endpoint",
						d.rel, other, name, key)
				}
				refs[key] = name
			}
		}
	}
}

// TestBaseIsConfined holds the base to the Pod security the threat model
// asks of it: a workload that cannot become root, cannot write its own
// filesystem, holds no capability, and is given the drain the server needs
// to finish a read in flight.
func TestBaseIsConfined(t *testing.T) {
	docs := read(t, "deploy/base")
	workloads := 0
	for _, d := range docs {
		if d.kind() != "Deployment" {
			continue
		}
		workloads++
		pod := d.at("spec", "template", "spec")
		if got := pod.text("terminationGracePeriodSeconds"); got != "90" {
			t.Errorf("%s/%s: terminationGracePeriodSeconds = %q, want 90, which covers the drain delay and the grace period of spec 002",
				d.rel, d.named(), got)
		}
		if got := pod.text("automountServiceAccountToken"); got != "false" {
			t.Errorf("%s/%s: automountServiceAccountToken = %q, want false; arcad speaks to no API server (spec 001, invariant 9)",
				d.rel, d.named(), got)
		}
		sc := pod.at("securityContext")
		if got := sc.text("runAsNonRoot"); got != "true" {
			t.Errorf("%s/%s: runAsNonRoot = %q, want true", d.rel, d.named(), got)
		}
		if got := sc.text("seccompProfile", "type"); got != "RuntimeDefault" {
			t.Errorf("%s/%s: seccompProfile.type = %q, want RuntimeDefault", d.rel, d.named(), got)
		}
		for _, c := range d.containers() {
			csc := c.at("securityContext")
			if got := csc.text("readOnlyRootFilesystem"); got != "true" {
				t.Errorf("%s/%s/%s: readOnlyRootFilesystem = %q, want true; arcad keeps nothing on local disk",
					d.rel, d.named(), c.text("name"), got)
			}
			if got := csc.text("allowPrivilegeEscalation"); got != "false" {
				t.Errorf("%s/%s/%s: allowPrivilegeEscalation = %q, want false",
					d.rel, d.named(), c.text("name"), got)
			}
			if dropped := csc.strings("capabilities", "drop"); !slices.Contains(dropped, "ALL") {
				t.Errorf("%s/%s/%s: capabilities.drop = %v, want ALL",
					d.rel, d.named(), c.text("name"), dropped)
			}
			if c.at("resources", "requests") == nil {
				t.Errorf("%s/%s/%s: no resource request; an unbounded pod is scheduled anywhere and evicted first",
					d.rel, d.named(), c.text("name"))
			}
		}
	}
	if workloads != 2 {
		t.Errorf("the base holds %d Deployments, want the API replicas and the reconciler of spec 010", workloads)
	}
}

// TestTheBaseServesBothListeners keeps the two listeners of spec 002 in the
// base: the public one an ingress routes, and the internal one the probes
// and the scrape reach. A probe pointed at the public listener would report
// ready while the internal one was dead.
func TestTheBaseServesBothListeners(t *testing.T) {
	var api *node
	for _, d := range read(t, "deploy/base") {
		if d.kind() == "Deployment" && d.named() == "arcad" {
			api = d.containers()[0]
		}
	}
	if api == nil {
		t.Fatal("no Deployment named arcad in the base")
	}
	ports := map[string]string{}
	for _, p := range api.items("ports") {
		ports[p.text("name")] = p.text("containerPort")
	}
	for name, want := range map[string]string{"http": "8080", "internal": "8081"} {
		if ports[name] != want {
			t.Errorf("the container's %s port is %q, want %s (spec 002)", name, ports[name], want)
		}
	}
	for _, probe := range []string{"livenessProbe", "readinessProbe", "startupProbe"} {
		got := api.at(probe, "httpGet")
		if got == nil {
			t.Errorf("the container has no %s", probe)
			continue
		}
		if port := got.text("port"); port != "internal" {
			t.Errorf("%s reaches port %q, want the internal listener", probe, port)
		}
	}
	if got := api.at("readinessProbe", "httpGet").text("path"); got != "/readyz" {
		t.Errorf("readinessProbe path = %q, want /readyz", got)
	}
}

// TestTheBaseKeepsCredentialsInSecrets is the gate's bearer rule over the
// base, which is the file set every installation inherits.
func TestTheBaseKeepsCredentialsInSecrets(t *testing.T) {
	assertCredentialsAreReferences(t, "deploy/base")
}

// TestTheBaseLeavesThePrometheusRuleOut keeps the alert rules out of the
// kustomization. A PrometheusRule needs the Prometheus operator's
// CustomResourceDefinition, which Arca does not require and the kind
// example does not install, so a base that named it would fail to apply on
// every installation without that operator (spec 016).
func TestTheBaseLeavesThePrometheusRuleOut(t *testing.T) {
	var k document
	for _, d := range read(t, "deploy/base") {
		if filepath.Base(d.rel) == "kustomization.yaml" {
			k = d
		}
	}
	if k.node == nil {
		t.Fatal("no deploy/base/kustomization.yaml")
	}
	if slices.Contains(k.strings("resources"), "prometheusrule.yaml") {
		t.Error("deploy/base names prometheusrule.yaml as a resource; it must be applied beside the base")
	}
	if _, err := os.Stat(filepath.Join(root(t), "deploy/base/prometheusrule.yaml")); err != nil {
		t.Errorf("the base holds no prometheusrule.yaml for spec 018 to fill: %v", err)
	}
}

// TestEveryArcadContainerNamesTheAudience is the gate's audience rule
// written as a test, so a manifest that lost the value fails with the file
// and the container named rather than at the end of a gate run. Spec 001
// fixes the audience of the core; a container that does not name it accepts
// whatever the binary's default is that release, which is a decision by
// coincidence.
//
// It reads the containers that name an image, which is every container the
// tree declares whole: the base, the reaper and the bootstrap job. A patch
// adds environment to one of those containers and declares no image, so what
// an overlay sets is asserted where that overlay is read.
// TestProdVerifiesTheOriginsAudienceBesideItsOwn is the one that matters
// here: ARCA_OIDC_AUDIENCE is a comma list from spec 027, the primary is the
// first entry, and the production overlay adds the origin in front of the
// core to it.
func TestEveryArcadContainerNamesTheAudience(t *testing.T) {
	found := 0
	for _, d := range read(t, "deploy") {
		for _, c := range d.containers() {
			if imageName(c.text("image")) != "arcad" {
				continue
			}
			found++
			var audience string
			for _, e := range c.items("env") {
				if e.text("name") == "ARCA_OIDC_AUDIENCE" {
					audience = e.text("value")
				}
			}
			if audience != "arca" {
				t.Errorf("%s: container %q sets ARCA_OIDC_AUDIENCE to %q, want arca (spec 001)",
					d.rel, c.text("name"), audience)
			}
		}
	}
	if found == 0 {
		t.Fatal("no container of the tree runs arcad")
	}
}

// TestTheReaderRefusesWhatItCannotRead proves the manifest reader stops on
// the constructs the deploy tree is not written in, rather than reading
// them wrongly and passing a check it should fail.
func TestTheReaderRefusesWhatItCannotRead(t *testing.T) {
	for name, text := range map[string]string{
		"an anchor":        "spec: &a\n  x: 1\n",
		"a merge key":      "spec:\n  <<: *a\n",
		"a block scalar":   "script: |\n  echo hi\n",
		"a flow mapping":   "spec: {x: 1}\n",
		"a tab":            "spec:\n\tx: 1\n",
		"an unclosed flow": "args: [a, b\n",
	} {
		if _, err := parse(text); err == nil {
			t.Errorf("the reader accepted %s; it must refuse what it cannot read", name)
		}
	}
	docs, err := parse("kind: Deployment\nspec:\n  items:\n    - name: a\n      value: \"1\"\n    - plain\n---\nkind: Service\n")
	if err != nil {
		t.Fatalf("the reader refused what the tree is written in: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("%d documents, want 2", len(docs))
	}
	d := document{rel: "t", node: docs[0]}
	if d.kind() != "Deployment" {
		t.Errorf("kind = %q", d.kind())
	}
	items := d.items("spec", "items")
	if len(items) != 2 {
		t.Fatalf("%d items, want 2", len(items))
	}
	if got := items[0].text("value"); got != "1" {
		t.Errorf("the first item's value = %q, want 1", got)
	}
	if got := items[1].value; got != "plain" {
		t.Errorf("the second item = %q, want the scalar plain", got)
	}
	second := document{rel: "t", node: docs[1]}
	if got := second.kind(); got != "Service" {
		t.Errorf("the second document's kind = %q, want Service", got)
	}
}
