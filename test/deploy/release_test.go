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

// workflow returns the release workflow as text. It is read as text and
// never with the manifest reader above: a workflow carries expressions,
// folded scalars and quoted flow, which that reader refuses on purpose, and
// what is asserted here is which names appear in which job and step.
func workflow(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root(t), ".github/workflows/release.yml"))
	if err != nil {
		t.Fatalf("read the release workflow: %v", err)
	}
	return string(raw)
}

// job returns the block of one job, from its key under `jobs:` to the next
// line at the jobs' own indentation: the next job, or the comment above it.
func job(t *testing.T, text, name string) string {
	t.Helper()
	_, after, ok := strings.Cut(text, "\n  "+name+":\n")
	if !ok {
		t.Fatalf("the workflow has no job named %q", name)
	}
	lines := strings.Split(after, "\n")
	for i, line := range lines {
		if len(line) > 2 && strings.HasPrefix(line, "  ") && line[2] != ' ' {
			lines = lines[:i]
			break
		}
	}
	// The block keeps the newline before its first line, so a key is found
	// as "\n    key:" wherever it stands in the job.
	return "\n" + strings.Join(lines, "\n")
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

// stage is where the shared image release stages live. Each is called at
// one version, a commit or a release tag of latere-ai/ci.
const stage = "latere-ai/ci/.github/workflows/"

// TestTheReleaseRunsThroughTheSharedStagesInOrder holds the pipeline's
// shape. The shared build stage builds, signs and attests both images; the
// conformance run and the deploy, which are Arca's own, run against what it
// pushed; only then does the shared publish stage tag the digests and
// create the release, and the verify stage read it back. Signing, attesting
// and tagging are the stages' alone: this file calls none of those tools.
func TestTheReleaseRunsThroughTheSharedStagesInOrder(t *testing.T) {
	text := workflow(t)
	needs := regexp.MustCompile(`(?m)^    needs: (.+)$`)
	for _, want := range []struct {
		job, uses, needs string
	}{
		{"build", "image-build.yml", ""},
		{"conformance", "", "build"},
		{"deploy", "", "[build, conformance]"},
		{"publish", "image-publish.yml", "[build, conformance, deploy]"},
		{"verify", "release-verify.yml", "publish"},
	} {
		block := job(t, text, want.job)
		got := ""
		if m := needs.FindStringSubmatch(block); m != nil {
			got = m[1]
		}
		if got != want.needs {
			t.Errorf("%s needs %q, want %q", want.job, got, want.needs)
		}
		calls := strings.Contains(block, "\n    uses: "+stage)
		switch {
		case want.uses != "" && !strings.Contains(block, "\n    uses: "+stage+want.uses+"@"):
			t.Errorf("%s does not call the shared stage %s", want.job, want.uses)
		case want.uses == "" && calls:
			t.Errorf("%s calls a shared stage; it is Arca's own job", want.job)
		}
	}
	for _, direct := range []string{"cosign", "anchore/sbom-action", "actions/attest-", "imagetools"} {
		if strings.Contains(text, direct) {
			t.Errorf("release.yml names %s; the shared stages sign, attest and tag", direct)
		}
	}
}

// TestReleasePublishesUnderTheOwnersNamespace is spec 016's second
// criterion. The workflow fixes no registry namespace: the build stage
// derives one from the repository that runs it, so a tag on a fork
// publishes to the fork's own packages and reaches none of Latere's. A
// namespace handed to the stage, or a literal in a job that builds,
// publishes or checks an image, is what would break that; the jobs between
// the stages reach the images only through what the build outputs.
func TestReleasePublishesUnderTheOwnersNamespace(t *testing.T) {
	text := workflow(t)
	for _, name := range []string{"build", "conformance", "publish", "verify"} {
		block := job(t, text, name)
		if strings.Contains(block, "ghcr.io/latere-ai") {
			t.Errorf("the job %q names ghcr.io/latere-ai; it must use the derived namespace, "+
				"or a tag on a fork publishes to Latere's packages", name)
		}
		if strings.Contains(block, "namespace:") {
			t.Errorf("the job %q sets a namespace; the build stage derives it from the repository owner", name)
		}
	}
	for name, outputs := range map[string][]string{
		"conformance": {"needs.build.outputs.ref_1", "needs.build.outputs.digest_1", "needs.build.outputs.ref_2", "needs.build.outputs.digest_2"},
		"deploy":      {"needs.build.outputs.ref_1", "needs.build.outputs.digest_1"},
		"publish":     {"needs.build.outputs.images"},
	} {
		block := job(t, text, name)
		for _, out := range outputs {
			if !strings.Contains(block, out) {
				t.Errorf("the job %q does not read %s, so it reaches an image the build did not push", name, out)
			}
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

// TestTheDeployRollsTheDigestTheBuildPushed: the build stage pushes each
// image by digest under no tag, and the publish stage gives the tag only
// after the deploy has passed. A deploy that named the image by the tag
// would pull a reference that does not exist yet, so both the overlay and
// the set image pin the digest the build output.
func TestTheDeployRollsTheDigestTheBuildPushed(t *testing.T) {
	apply := step(t, workflow(t), "Apply deploy/prod and roll the new image")
	for _, want := range []string{
		"ARCAD_DIGEST: ${{ needs.build.outputs.digest_1 }}",
		`\1digest: ${ARCAD_DIGEST}|`,
		`"arcad=${ARCAD_REF}@${ARCAD_DIGEST}"`,
	} {
		if !strings.Contains(apply, want) {
			t.Errorf("the deploy does not carry %q", want)
		}
	}
	if strings.Contains(apply, ":${TAG}") {
		t.Error("the deploy names the image by the tag, which the image does not carry until publish")
	}
}

// TestEveryThirdPartyActionIsPinned keeps every `uses:` on a commit. A
// floating tag is a name somebody else can move, and this workflow signs
// artifacts and holds a kubeconfig. The shared stages are latere-ai/ci's,
// the same organization's, and are called at a commit or a release tag of
// it, all three at the same one, so the build, publish and verify stages
// agree on what passes between them.
func TestEveryThirdPartyActionIsPinned(t *testing.T) {
	commit := regexp.MustCompile(`^[0-9a-f]{40}$`)
	release := regexp.MustCompile(`^v\d+(\.\d+\.\d+(-[0-9A-Za-z.]+)?)?$`)
	pinned := 0
	var stages []string
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
		name, version, found := strings.Cut(ref, "@")
		switch {
		case !found:
			t.Errorf("release.yml:%d: %s names no version", i+1, ref)
		case strings.HasPrefix(name, stage):
			if !commit.MatchString(version) && !release.MatchString(version) {
				t.Errorf("release.yml:%d: %s is called at %q, which is neither a commit nor a release tag", i+1, name, version)
			}
			stages = append(stages, version)
		case !commit.MatchString(version):
			t.Errorf("release.yml:%d: %s is pinned to %q, which is not a commit", i+1, ref, version)
		default:
			pinned++
		}
	}
	if pinned == 0 {
		t.Fatal("the workflow uses no action, so this test proves nothing")
	}
	if len(stages) != 3 {
		t.Errorf("the workflow calls %d shared stages, want build, publish and verify", len(stages))
	}
	if versions := slices.Compact(slices.Sorted(slices.Values(stages))); len(versions) > 1 {
		t.Errorf("the shared stages are called at %v, want one version of them", versions)
	}
}

// TestTheReleaseNotesComeFromTheChangelog is spec 016's tenth criterion at
// the workflow's end: a tag with no CHANGELOG section is refused before
// anything is built, and the release body is that section. The shared
// build stage refuses the tag and the shared publish stage writes the body,
// both with the same reader the pre-push hook runs, so no job here creates
// or edits a release of its own.
func TestTheReleaseNotesComeFromTheChangelog(t *testing.T) {
	text := workflow(t)
	if !strings.Contains(job(t, text, "build"), "uses: "+stage+"image-build.yml@") {
		t.Error("the build does not run through the shared build stage, which refuses a tag with no section")
	}
	if !strings.Contains(job(t, text, "publish"), "uses: "+stage+"image-publish.yml@") {
		t.Error("the release is not published by the shared publish stage, which uses the section as the body")
	}
	if strings.Contains(text, "gh release") {
		t.Error("release.yml runs gh release itself; the publish stage creates the release with the section as its body")
	}
}
