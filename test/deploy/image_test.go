// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runtimeStage returns the block between the shared runtime markers of a
// Dockerfile, which is the stage the developer image and the release image
// both run.
func runtimeStage(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root(t), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	const opening, closing = "# >>> shared runtime base <<<\n", "# <<< shared runtime base >>>"
	_, after, ok := strings.Cut(string(raw), opening)
	if !ok {
		t.Fatalf("%s has no opening runtime marker", name)
	}
	stage, _, ok := strings.Cut(after, closing)
	if !ok {
		t.Fatalf("%s has no closing runtime marker", name)
	}
	return stage
}

// TestRuntimeStagesMatch is spec 016's third criterion. The image an
// operator pulls and the image a contributor builds must run on the same
// base, as the same user, with the same ports and the same entry point, or
// a release is proved against something other than what it ships. Copying
// the block is what makes them the same; this is what keeps them so.
func TestRuntimeStagesMatch(t *testing.T) {
	developer := runtimeStage(t, "Dockerfile")
	release := runtimeStage(t, "Dockerfile.ci")
	if developer != release {
		t.Errorf("the runtime stages differ.\nDockerfile:\n%s\nDockerfile.ci:\n%s", developer, release)
	}
	// A stage that had lost its contents would match itself and prove
	// nothing, so what must be in it is named here.
	for _, want := range []string{
		"FROM gcr.io/distroless/static-debian12:nonroot",
		"USER nonroot:nonroot",
		"EXPOSE 8080 8081",
		`ENTRYPOINT ["/usr/local/bin/arcad"]`,
	} {
		if !strings.Contains(developer, want) {
			t.Errorf("the shared runtime stage does not carry %q", want)
		}
	}
	// The binary arrives from a different place in each image, which is the
	// one difference between them and therefore belongs outside the block.
	if strings.Contains(developer, "COPY") {
		t.Error("the shared runtime stage copies the binary; the two images get it from different places, " +
			"so the COPY belongs outside the markers")
	}
}

// TestTheReleaseImageCopiesWhatThePipelineBuilt keeps Dockerfile.ci reading
// the archive layout the release workflow writes. The two are in different
// files and a path that drifted would fail at the push, after the archives
// were built and before anything was signed.
func TestTheReleaseImageCopiesWhatThePipelineBuilt(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(root(t), "Dockerfile.ci"))
	if err != nil {
		t.Fatal(err)
	}
	const want = "COPY out/release/bin/${TARGETOS}_${TARGETARCH}/arcad /usr/local/bin/arcad"
	if !strings.Contains(string(raw), want) {
		t.Errorf("Dockerfile.ci does not copy %q", want)
	}
	// An ARG declared before the first FROM is in scope for FROM lines
	// only; a COPY inside the stage reads it as empty and the path becomes
	// bin/_/arcad, which is what the first tag's build died on. The two
	// must be declared inside the stage, after its FROM.
	lastFrom := strings.LastIndex(string(raw), "\nFROM ")
	for _, arg := range []string{"ARG TARGETOS", "ARG TARGETARCH"} {
		at := strings.LastIndex(string(raw), arg)
		if at < 0 {
			t.Errorf("Dockerfile.ci does not declare %s, so buildx cannot fill the path per platform", arg)
			continue
		}
		if at < lastFrom {
			t.Errorf("Dockerfile.ci declares %s before the runtime stage's FROM, where the stage's COPY cannot read it", arg)
		}
	}
	workflow, err := os.ReadFile(filepath.Join(root(t), ".github/workflows/release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workflow), "out/release/bin/") {
		t.Error("release.yml does not write out/release/bin/, which Dockerfile.ci copies from")
	}
}

// TestTheStubsImageBuildsTheStubsCommand keeps Dockerfile.stubs, which the
// release workflow builds and the kind example runs, in the tree and
// building the one command the stubs package exports. The first tag was
// cut with the workflow naming a file that did not exist.
func TestTheStubsImageBuildsTheStubsCommand(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(root(t), "Dockerfile.stubs"))
	if err != nil {
		t.Fatalf("the release workflow builds Dockerfile.stubs: %v", err)
	}
	text := string(raw)
	for _, want := range []string{
		"./test/stubs/cmd/arca-stubs",
		"EXPOSE 8081 8082",
		`ENTRYPOINT ["/usr/local/bin/arca-stubs"]`,
		"USER nonroot:nonroot",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Dockerfile.stubs lacks %q", want)
		}
	}
	workflow, err := os.ReadFile(filepath.Join(root(t), ".github/workflows/release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workflow), "-f Dockerfile.stubs") {
		t.Error("release.yml no longer builds Dockerfile.stubs; the kind example and the conformance job run that image")
	}
}
