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
	for _, arg := range []string{"ARG TARGETOS", "ARG TARGETARCH"} {
		if !strings.Contains(string(raw), arg) {
			t.Errorf("Dockerfile.ci does not declare %s, so buildx cannot fill the path per platform", arg)
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
