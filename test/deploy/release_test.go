// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workflow returns the release workflow as text. It is read as text and
// never with the manifest reader above: a workflow carries expressions,
// folded scalars and quoted flow, which that reader refuses on purpose, and
// what is asserted here is which names appear in which step.
func workflow(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root(t), ".github/workflows/release.yml"))
	if err != nil {
		t.Fatalf("read the release workflow: %v", err)
	}
	return string(raw)
}

// step returns the block of one named step, from its `- name:` line to the
// next step at the same indentation.
func step(t *testing.T, text, name string) string {
	t.Helper()
	const marker = "\n      - name: "
	_, after, ok := strings.Cut(text, marker+name+"\n")
	if !ok {
		t.Fatalf("the workflow has no step named %q", name)
	}
	before, _, _ := strings.Cut(after, marker)
	if next := strings.Index(after, "\n      - uses: "); next >= 0 && next < len(before) {
		before = after[:next]
	}
	return before
}

// TestReleasePublishesUnderTheOwnersNamespace is spec 016's second
// criterion. The workflow fixes no registry namespace: it derives one from
// the repository that runs it, so a tag on a fork publishes to the fork's
// own packages and reaches none of Latere's. A literal in a build, a push
// or a signature is what would break that, so those steps are named here
// and held to the variables.
func TestReleasePublishesUnderTheOwnersNamespace(t *testing.T) {
	text := workflow(t)
	if !strings.Contains(text, "format('ghcr.io/{0}', github.repository_owner)") {
		t.Error("the workflow does not derive the image namespace from the repository owner")
	}
	if !strings.Contains(text, "vars.ARCA_RELEASE_IMAGE_NAMESPACE") {
		t.Error("the workflow offers no override for an installation that publishes elsewhere")
	}
	for _, name := range []string{
		"Build the release archives",
		"Build and push arcad",
		"Build and push arca-stubs",
		"Assert both architectures are published",
		"Sign the images and checksums.txt",
	} {
		if got := step(t, text, name); strings.Contains(got, "ghcr.io/latere-ai") {
			t.Errorf("the step %q names ghcr.io/latere-ai; it must use the derived namespace, "+
				"or a tag on a fork publishes to Latere's packages", name)
		}
	}
}

// TestTheDeployJobIsGatedAndNamesTheEnvironment holds the one job that
// reaches a cluster to the two things that keep it off every other
// repository, and to the environment that makes a release a deployment
// GitHub records rather than a step in a log.
func TestTheDeployJobIsGatedAndNamesTheEnvironment(t *testing.T) {
	text := workflow(t)
	for _, want := range []string{
		// The repository variable and the secret of spec 016. A fork sets
		// neither, so a tag there publishes every artifact and skips this.
		"vars.ARCA_RELEASE_DEPLOY != ''",
		"secrets.ARCA_KUBECONFIG",
		"      name: production",
		"kubectl apply -k deploy/prod/",
		"rollout status deployment/arcad",
		"tools/smoke/release.sh",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the release workflow does not carry %q", want)
		}
	}
	// The smoke must read the origin the environment records, because a
	// rollout returns as soon as the new replicas are ready and only a
	// request through the ingress proves what answers there.
	deploy := step(t, text, "Smoke the live service")
	if !strings.Contains(deploy, "BASE_URL: https://api.latere.ai") {
		t.Error("the smoke does not read the origin the production environment names")
	}
	if !strings.Contains(deploy, "TAG: ${{ github.ref_name }}") {
		t.Error("the smoke is given no TAG, so it cannot fail a rollout that still serves the old version")
	}
}

// TestEveryThirdPartyActionIsPinned keeps every `uses:` on a commit. A
// floating tag is a name somebody else can move, and this workflow signs
// artifacts and holds a kubeconfig.
func TestEveryThirdPartyActionIsPinned(t *testing.T) {
	pinned := 0
	for i, line := range strings.Split(workflow(t), "\n") {
		trimmed := strings.TrimSpace(line)
		ref, ok := strings.CutPrefix(trimmed, "- uses: ")
		if !ok {
			if r, inner := strings.CutPrefix(trimmed, "uses: "); inner {
				ref, ok = r, true
			}
		}
		if !ok {
			continue
		}
		ref, _, _ = strings.Cut(ref, " ")
		_, version, found := strings.Cut(ref, "@")
		switch {
		case !found:
			t.Errorf("release.yml:%d: %s names no version", i+1, ref)
		case len(version) != 40:
			t.Errorf("release.yml:%d: %s is pinned to %q, which is not a commit", i+1, ref, version)
		default:
			pinned++
		}
	}
	if pinned == 0 {
		t.Fatal("the workflow uses no action, so this test proves nothing")
	}
}

// TestTheReleaseNotesComeFromTheChangelog is spec 016's tenth criterion at
// the workflow's end: a tag with no CHANGELOG section is refused before a
// release exists, by the same reader the pre-push hook runs.
func TestTheReleaseNotesComeFromTheChangelog(t *testing.T) {
	text := workflow(t)
	if !strings.Contains(text, "go tool lateregate release-notes") {
		t.Error("the publish job does not read the release note from CHANGELOG.md")
	}
	if !strings.Contains(text, "--notes-file body.md") {
		t.Error("the publish job does not use the changelog section as the release body")
	}
}
