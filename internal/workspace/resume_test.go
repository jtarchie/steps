package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// The tree a restart resumes is a stale build by every sign the sweep reads, so only being named spares it — by a path that may be spelled relative to where the root was.
func TestTheSweepSparesATreeARestartResumes(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	resumed := filepath.Join(root, "b-deadbeef-1-build")
	stale := filepath.Join(root, "b-deadbeef-2-build")

	for _, dir := range []string{resumed, stale} {
		err := os.MkdirAll(dir, 0o750)
		if err != nil {
			t.Fatal(err)
		}
	}

	provider, err := NewProvider(&config.WorkspaceConfig{Strategy: "copy", Root: root}, false)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = provider.Close() }()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	relative, err := filepath.Rel(cwd, resumed)
	if err != nil {
		t.Fatal(err)
	}

	provider.Spare([]string{relative})

	err = provider.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	_, statErr := os.Stat(resumed)
	if statErr != nil {
		t.Errorf("the sweep took the tree a restart resumes: %v", statErr)
	}

	_, statErr = os.Stat(stale)
	if !os.IsNotExist(statErr) {
		t.Errorf("sparing one tree spared %q too", stale)
	}
}

// A daemon resumes one run while building others beside it, so continuing in the old tree must not leak into the provider every other run shares.
func TestResumingContinuesOneRunAndLeavesTheProviderAlone(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	kept := filepath.Join(root, "b-deadbeef-1-build")

	err := os.MkdirAll(filepath.Join(kept, "artifacts", "out"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	provider, err := NewProvider(&config.WorkspaceConfig{Strategy: "copy", Root: root}, false)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = provider.Close() }()

	resumed, err := provider.Resuming(kept).NewBuild(t.Context(), "build")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}

	if resumed.Root() != kept || !resumed.HasArtifact("out") {
		t.Errorf("the resumed build is at %q, want the kept tree %q with its artifact", resumed.Root(), kept)
	}

	assertClosingKeeps(t, resumed)

	other, err := provider.NewBuild(t.Context(), "build")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}

	defer func() { _ = other.Close() }()

	if other.Root() == kept || !strings.HasPrefix(filepath.Base(other.Root()), buildDirPrefix) {
		t.Errorf("another run's build is at %q, want a fresh tree of its own", other.Root())
	}
}

func assertClosingKeeps(t *testing.T, build BuildWorkspace) {
	t.Helper()

	err := build.Close()
	if err != nil {
		t.Fatal(err)
	}

	_, err = os.Stat(build.Root())
	if err != nil {
		t.Errorf("closing a resumed build removed the tree it continued in: %v", err)
	}
}
