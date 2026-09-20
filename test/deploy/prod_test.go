// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package deploy

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// releaseTag is the version deploy/prod pins, which `lateregate release`
// rewrites through the release.stamp entry in .lateregate.yaml.
var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// TestTheTreeHoldsEveryOverlay names what spec 016's deploy tree is, so a
// directory that is deleted or renamed fails here rather than at the moment
// an operator follows the install document into a path that is gone.
func TestTheTreeHoldsEveryOverlay(t *testing.T) {
	for _, dir := range []string{
		"deploy/base",
		"deploy/bootstrap",
		"deploy/examples/kind",
		"deploy/examples/aws",
		"deploy/examples/digitalocean",
		"deploy/prod",
	} {
		if _, err := os.Stat(filepath.Join(root(t), filepath.FromSlash(dir))); err != nil {
			t.Errorf("%s is missing: %v", dir, err)
		}
	}
}

// TestProdPinsAReleasedImage keeps the operator's overlay pinned to a
// version rather than to a moving tag, and keeps the marker
// `lateregate release` rewrites in the shape its pattern matches. A cut
// that could not find the marker would refuse the release, which is the
// right failure but a late one.
func TestProdPinsAReleasedImage(t *testing.T) {
	k, ok := kustomizations(t)["deploy/prod"]
	if !ok {
		t.Fatal("no deploy/prod/kustomization.yaml")
	}
	if got := k.text("namespace"); got == "" {
		t.Error("deploy/prod declares no namespace")
	}
	if res := k.strings("resources"); !slices.Contains(res, "../base") {
		t.Errorf("deploy/prod resources = %v, want ../base among them", res)
	}
	images := k.items("images")
	if len(images) != 1 {
		t.Fatalf("deploy/prod pins %d images, want one: arcad and nothing beside it", len(images))
	}
	if got := imageName(images[0].text("name")); got != "arcad" {
		t.Errorf("deploy/prod pins the image %q, want arcad", images[0].text("name"))
	}
	if got := images[0].text("newTag"); !releaseTag.MatchString(got) {
		t.Errorf("deploy/prod newTag = %q, want a vX.Y.Z the release stamp rewrites", got)
	}
}

// TestProdRoutesWhatTheSmokeReads holds the Ingress to every path the
// release pipeline depends on. The deploy job smokes the origin the
// production environment names, so a rule dropped here leaves a green tree
// and a release that fails after the rollout, which is the worst moment to
// find it.
//
// The paths are read out of tools/smoke/release.sh rather than written
// here. A list in both places is a list that drifts: this one did, naming
// /readyz and /version while the script had grown to read /livez and
// /openapi.json as well, so the overlay routed neither and the deploy job
// would have taken a 404 at the origin after a rollout that worked.
//
// The catch-all is asserted absent for the opposite reason: the host is
// shared with several services, and a `/` rule would take the whole origin
// and answer 404 for every route another service adds.
//
// A smoked path that falls under a Prefix rule this Ingress claims is routed
// by that rule and needs none of its own, which is how the prefixed path the
// smoke reads for a 401 is covered. A rule of its own is required only of a
// smoked path outside every Prefix rule, which is what the four probes at the
// origin root are.
//
// The /v1 prefix this used to enumerate is asserted by
// TestProdClaimsOneV1PrefixAndItIsTheBasePathItServes, which derives it from
// the overlay. The prefixes were written here as a literal list, which is a
// third copy of the route table and drifted exactly as the smoke paths had.
func TestProdRoutesWhatTheSmokeReads(t *testing.T) {
	routed := prodRoutes(t)
	smoked := smokedPaths(t)
	if len(smoked) < 2 {
		t.Fatalf("read %d paths out of the release smoke, want the several it checks: the parse is wrong, not the overlay", len(smoked))
	}
	for _, path := range smoked {
		if under := coveringPrefix(routed, path); under != "" {
			continue
		}
		got, ok := routed[path]
		if !ok {
			t.Errorf("deploy/prod routes no %s, which the release smoke reads through the origin", path)
			continue
		}
		// Exact claims nothing beyond the path itself and is what these
		// rules want. The exception is a path holding a dot: the nginx
		// admission webhook refuses those under Exact or Prefix and
		// rejects the whole document, so the apply fails rather than the
		// rule, and ImplementationSpecific is the one type left.
		want := "Exact"
		if strings.Contains(path, ".") {
			want = "ImplementationSpecific"
		}
		if got != want {
			t.Errorf("deploy/prod routes %s as %q, want %q", path, got, want)
		}
	}
	if _, ok := routed["/"]; ok {
		t.Error("deploy/prod claims / at a shared origin; a catch-all takes the whole host")
	}
}

// coveringPrefix answers the Prefix rule that routes a path, or the empty
// string. It is Kubernetes' own rule, matched on whole segments: /v1/storage
// covers /v1/storage and everything below it, and covers /v1/storagefoo
// never.
func coveringPrefix(routed map[string]string, path string) string {
	for rule, kind := range routed {
		if kind != "Prefix" {
			continue
		}
		if path == rule || strings.HasPrefix(path, strings.TrimSuffix(rule, "/")+"/") {
			return rule
		}
	}
	return ""
}

// prodRoutes reads the path and the pathType of every rule of the
// production Ingress, and reports a rule that points anywhere but arcad.
// Two tests ask about that object, so it is read once here rather than
// walked twice with the second walk free to drift from the first.
func prodRoutes(t *testing.T) map[string]string {
	t.Helper()
	routed := map[string]string{}
	found := false
	for _, d := range read(t, "deploy/prod") {
		if d.kind() != "Ingress" {
			continue
		}
		found = true
		for _, rule := range d.items("spec", "rules") {
			for _, p := range rule.items("http", "paths") {
				routed[p.text("path")] = p.text("pathType")
				if backend := p.at("backend", "service").text("name"); backend != "arcad" {
					t.Errorf("deploy/prod routes %s to %q, want arcad", p.text("path"), backend)
				}
			}
		}
	}
	if !found {
		t.Fatal("deploy/prod holds no Ingress")
	}
	return routed
}

// TestProdClaimsOneV1PrefixAndItIsTheBasePathItServes is criterion 3 of spec
// 027: at an origin partitioned by capability this object claims one prefix
// and the server mounts its whole surface under it, so the claim and the
// mount are one value read twice rather than an enumeration that drifts.
//
// It drifted before. The Ingress used to enumerate the /v1 namespaces the
// document declares, `/v1/admin` was in the document and in no rule here, and
// the console asked api.latere.ai for `/v1/admin/overview` and took a 404
// from nginx with an HTML body while arcad served the route. One rule derived
// from ARCA_BASE_PATH cannot lose a namespace: a route the server grows
// answers under the same prefix.
//
// The document is held to the version as well, because the swap the server
// makes is of the leading `/v1`: a path declared outside it would be mounted
// nowhere the rule claims.
func TestProdClaimsOneV1PrefixAndItIsTheBasePathItServes(t *testing.T) {
	base := prodBasePath(t)
	if !strings.HasPrefix(base, "/v1") {
		t.Fatalf("deploy/prod sets ARCA_BASE_PATH to %q, which is not under the version spec 013 fixes", base)
	}
	served := servedPaths(t)
	if len(served) < 20 {
		t.Fatalf("read %d paths out of api/openapi.yaml, want the several dozen spec 013 registers: the parse is wrong, not the overlay", len(served))
	}
	for _, path := range served {
		if !strings.HasPrefix(path, "/v1/") {
			t.Errorf("the committed document declares %s, which is outside /v1 and would be mounted under no prefix this Ingress claims", path)
		}
	}

	var claimed []string
	for path, kind := range prodRoutes(t) {
		if kind == "Prefix" && strings.HasPrefix(path, "/v1") {
			claimed = append(claimed, path)
		}
	}
	slices.Sort(claimed)
	if len(claimed) != 1 {
		t.Fatalf("deploy/prod claims %v under /v1, want exactly one prefix: the origin is partitioned by capability", claimed)
	}
	if claimed[0] != base {
		t.Errorf("deploy/prod claims %s and serves under %s; the Ingress forwards without a rewrite, so the two are one value", claimed[0], base)
	}
}

// TestProdVerifiesTheOriginsAudienceBesideItsOwn: the overlay sits behind a
// shared origin, and a personal access token and a platform key are addressed
// to that origin rather than to a service behind it. The list is asserted
// here and not in the tree-wide audience test, because the base and the
// reaper name the core alone and this overlay is the one place a second name
// belongs.
//
// The first entry is held to `arca`: it is the primary, the name spec 001
// fixes for the core, what the verifier reports and what the start-up line
// prints.
func TestProdVerifiesTheOriginsAudienceBesideItsOwn(t *testing.T) {
	got, found := prodServerEnv(t, "ARCA_OIDC_AUDIENCE")
	if !found {
		t.Fatal("deploy/prod patches no ARCA_OIDC_AUDIENCE; a token addressed to the origin would be refused")
	}
	names := strings.Split(got, ",")
	for i := range names {
		names[i] = strings.TrimSpace(names[i])
	}
	if len(names) < 2 || names[0] != "arca" {
		t.Errorf("deploy/prod verifies %v, want arca first and the origin beside it", names)
	}
	if !slices.Contains(names, "api.latere.ai") {
		t.Errorf("deploy/prod verifies %v and not api.latere.ai, which is what a platform key is addressed to", names)
	}
	// The reaper verifies no token, and it still reads the same configuration
	// the server does. One value across the hosted deployment is what the
	// family's identity gate reads (ci-gate spec 026), and one value is one
	// fact to move.
	reaper, found := prodWorkloadEnv(t, "arcad-reaper", "ARCA_OIDC_AUDIENCE")
	if !found || reaper != got {
		t.Errorf("deploy/prod gives the reaper ARCA_OIDC_AUDIENCE %q (found %v), want the server's %q", reaper, found, got)
	}
}

// prodBasePath reads ARCA_BASE_PATH off the server this overlay deploys. The
// claim above is derived from it rather than written twice, so an operator
// who moves the prefix moves the Ingress with it or reads the failure here.
func prodBasePath(t *testing.T) string {
	t.Helper()
	base, found := prodServerEnv(t, "ARCA_BASE_PATH")
	if !found {
		t.Fatal("deploy/prod sets no ARCA_BASE_PATH; the server would serve at the root of the version and answer nothing the Ingress claims")
	}
	return base
}

// prodServerEnv reads one environment value of the arcad Deployment across
// the overlay's patches, which is where the server's own configuration is
// set. The reaper is not read: it serves no surface and verifies no token.
func prodServerEnv(t *testing.T, name string) (string, bool) {
	t.Helper()
	return prodWorkloadEnv(t, "arcad", name)
}

// prodWorkloadEnv reads one variable off the named Deployment of the overlay.
func prodWorkloadEnv(t *testing.T, workload, name string) (string, bool) {
	t.Helper()
	value, found := "", false
	for _, d := range read(t, "deploy/prod") {
		if d.kind() != "Deployment" || d.named() != workload {
			continue
		}
		for _, c := range d.containers() {
			for _, e := range c.items("env") {
				if e.text("name") == name {
					value, found = e.text("value"), true
				}
			}
		}
	}
	return value, found
}

// servedPaths returns every path the committed document declares, in the
// order it declares them.
//
// The document is read as text and not with the manifest reader beside it.
// That reader refuses a key holding a brace on purpose, and every path
// carrying a template parameter is one; the keys wanted here are the entries
// of the top-level `paths` mapping, which is a shape a line scan reads
// exactly.
func servedPaths(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root(t), filepath.FromSlash("api/openapi.yaml")))
	if err != nil {
		t.Fatalf("read the committed document: %v", err)
	}
	var out []string
	inPaths := false
	for line := range strings.SplitSeq(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if indent := len(line) - len(strings.TrimLeft(line, " ")); indent == 0 {
			inPaths = trimmed == "paths:"
			continue
		} else if !inPaths || indent != 2 {
			continue
		}
		path, ok := strings.CutSuffix(trimmed, ":")
		if !ok || !strings.HasPrefix(path, "/") {
			continue
		}
		if !slices.Contains(out, path) {
			out = append(out, path)
		}
	}
	return out
}

// smokedPaths reads the paths tools/smoke/release.sh asks the origin for,
// which is the list the production Ingress has to route. Each is a
// check_status call whose second argument is the path, so the path is taken
// from there rather than from the label beside it.
func smokedPaths(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root(t), filepath.FromSlash("tools/smoke/release.sh")))
	if err != nil {
		t.Fatalf("read the release smoke: %v", err)
	}
	var paths []string
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "check_status ") {
			continue
		}
		args := smokeArgs(line)
		if len(args) < 2 || !strings.HasPrefix(args[1], "/") {
			continue
		}
		if !slices.Contains(paths, args[1]) {
			paths = append(paths, args[1])
		}
	}
	return paths
}

// smokeArgs splits a shell call into its double-quoted arguments, which is
// all the smoke's own calls use.
func smokeArgs(line string) []string {
	var args []string
	for rest := line; ; {
		i := strings.Index(rest, `"`)
		if i < 0 {
			return args
		}
		rest = rest[i+1:]
		j := strings.Index(rest, `"`)
		if j < 0 {
			return args
		}
		args = append(args, rest[:j])
		rest = rest[j+1:]
	}
}

// TestProdAdmitsTheDatabasePortsThisInstallationUses: the base confines
// egress and admits 5432, which is where a Postgres an operator runs
// listens. This installation's database is a managed one on 25060, with its
// connection pool on 25061, and a port no policy names is a connection
// dropped rather than refused. The pool would wait out its own timeout, the
// database readiness check would fail, and the release's rollout would time
// out with the image already built and signed.
//
// The ports cannot be read off the manifests, because the database URL is a
// Secret this tree does not hold. They are written here instead, which is
// the only place the pairing can be asserted at all.
func TestProdAdmitsTheDatabasePortsThisInstallationUses(t *testing.T) {
	admitted := map[string]bool{}
	for _, d := range read(t, "deploy/prod") {
		if d.kind() != "NetworkPolicy" {
			continue
		}
		if !slices.Contains(d.at("spec").strings("policyTypes"), "Egress") {
			continue
		}
		for _, rule := range d.at("spec").items("egress") {
			for _, port := range rule.items("ports") {
				admitted[port.text("port")] = true
			}
		}
	}
	for _, c := range []struct{ port, why string }{
		{"25060", "the managed database listens there and the base names only 5432"},
		{"25061", "the database's connection pool listens there"},
		{"40318", "the telemetry collector injected into this namespace is reached there, not on the 4317 and 4318 the base admits"},
		{"8081", "platformd's internal container port answers the Arca decider; policy is evaluated on the pod port after the Service translation, so the base's 80 does not cover it"},
	} {
		if !admitted[c.port] {
			t.Errorf("deploy/prod admits no egress to %s; %s", c.port, c.why)
		}
	}
}

// TestProdKeepsCredentialsInSecrets is the gate's bearer rule over the
// overlay the gate itself does not read. deploy/prod is declared under
// identity.skip so the addresses it sets are allowed, and skipping it takes
// every other identity rule with it, so what the gate stops asserting is
// asserted here instead.
func TestProdKeepsCredentialsInSecrets(t *testing.T) {
	assertCredentialsAreReferences(t, "deploy/prod")
	for _, d := range read(t, "deploy/prod") {
		for _, c := range d.containers() {
			for _, e := range c.items("env") {
				if e.text("name") != "ARCA_AUTHORIZER_URL" {
					continue
				}
				// An endpoint is not a credential: it belongs in the
				// manifest a reviewer reads, not in a Secret.
				if e.text("value") == "" {
					t.Errorf("%s: ARCA_AUTHORIZER_URL is read from a Secret; only the bearer is one", d.rel)
				}
			}
		}
	}
}

// TestProdServesPublicObjectsThroughTheCDN: the server the cutover replaces
// redirected a public object to a CDN, and an installation that sets no base
// answers the ordinary presigned redirect instead. Nothing fails when the
// value is missing, which is why it is asserted: every public link that
// already exists would quietly start resolving somewhere else.
//
// Only the server is checked. The reaper serves no redirect, so a value there
// would be configuration nothing reads.
func TestProdServesPublicObjectsThroughTheCDN(t *testing.T) {
	found := false
	for _, d := range read(t, "deploy/prod") {
		if d.named() != "arcad" || d.kind() != "Deployment" {
			continue
		}
		for _, c := range d.containers() {
			for _, e := range c.items("env") {
				if e.text("name") != "ARCA_PUBLIC_CDN_URL" {
					continue
				}
				found = true
				if got := e.text("value"); !strings.HasPrefix(got, "https://") {
					t.Errorf("deploy/prod sets ARCA_PUBLIC_CDN_URL to %q, want an https base", got)
				}
			}
		}
	}
	if !found {
		t.Error("deploy/prod sets no ARCA_PUBLIC_CDN_URL; a public object would stop redirecting to the CDN the replaced service used")
	}
}

// TestProdIsDeclaredToTheGate proves the two entries that make the overlay
// legal are both present. Either one alone is wrong: skip without overlays
// stops the gate reading the addresses, so a document naming one is
// refused; overlays without skip leaves every other identity rule running
// over a directory written for one installation.
func TestProdIsDeclaredToTheGate(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(root(t), ".lateregate.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// The gate's own configuration is read as text rather than with the
	// manifest reader above: it carries folded scalars, which that reader
	// refuses on purpose. Two keys of one block is all this needs.
	listed := map[string][]string{}
	block, key := "", ""
	for line := range strings.SplitSeq(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		switch indent := len(line) - len(strings.TrimLeft(line, " ")); {
		case indent == 0:
			block, key = strings.TrimSuffix(trimmed, ":"), ""
		case indent == 2 && block == "identity":
			key = strings.TrimSuffix(trimmed, ":")
		case indent >= 4 && block == "identity" && strings.HasPrefix(trimmed, "- "):
			listed[key] = append(listed[key], strings.TrimSpace(trimmed[2:]))
		}
	}
	for _, want := range []string{"skip", "overlays"} {
		if got := listed[want]; !slices.Contains(got, "deploy/prod") {
			t.Errorf("identity.%s = %v, want deploy/prod among them (spec 016)", want, got)
		}
	}
}

// TestProdNamesOnlyAddressesTheFamilyAlreadyUses holds the one directory
// that may name an installation to the addresses the family's other
// manifests already carry. A hostname invented here would be one nobody
// serves, found at the first release rather than at review.
func TestProdNamesOnlyAddressesTheFamilyAlreadyUses(t *testing.T) {
	known := []string{
		// The platform origin, in front of several services.
		"api.latere.ai",
		// platformd's internal Service, which no ingress names.
		"platformd-internal.latere.svc.cluster.local",
		// The bucket's CDN edge, which a public object redirects to. No
		// manifest of the family serves it because DigitalOcean does: it is
		// the Spaces CDN in front of the same bucket this installation
		// reads, and it is the address the replaced service already
		// redirected to, read from its live configuration rather than
		// chosen here.
		"cdn.latere.ai",
	}
	seen := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(root(t), "deploy/prod"), func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		raw, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		rel := filepath.Base(p)
		for i, line := range strings.Split(string(raw), "\n") {
			for _, host := range hostsIn(line) {
				seen[host] = true
				if !slices.Contains(known, host) {
					t.Errorf("deploy/prod/%s:%d: names %s, which no manifest of the family serves; "+
						"an address here must be one that already exists", rel, i+1, host)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk deploy/prod: %v", err)
	}
	// Without this the test would pass on a reader that found no address at
	// all, which is the way a rule like this stops working.
	for _, want := range known {
		if !seen[want] {
			t.Errorf("deploy/prod names no %s; the overlay is the one place that must", want)
		}
	}
}

// host matches a name under the company's own domains, in either spelling:
// the public one and the cluster-internal one.
var host = regexp.MustCompile(`[A-Za-z0-9.-]*latere\.(ai|svc[A-Za-z0-9.-]*)`)

// hostsIn returns the company addresses one line names, dropping the
// registry namespace, which is written with a hyphen and is where images
// are published rather than an address anything is served at.
func hostsIn(line string) []string {
	var out []string
	for _, got := range host.FindAllString(line, -1) {
		got = strings.TrimSuffix(got, ".")
		if strings.HasSuffix(got, "-tls") || got == "latere.ai" || got == "latere.svc" {
			continue
		}
		out = append(out, got)
	}
	return out
}
