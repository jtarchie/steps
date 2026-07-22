package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // path is a t.TempDir()-scoped file this test wrote
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

func TestSanitizeLabel(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"build":        "build",
		"a/b":          "a_b",
		"../evil":      ".._evil",
		"":             "_",
		"weird name!!": "weird_name_",
	}

	for in, want := range cases {
		got := sanitizeLabel(in)
		if got != want {
			t.Errorf("sanitizeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func ctxT() context.Context { return context.Background() }

func TestSharedProviderReturnsBuildRootForEveryStep(t *testing.T) {
	t.Parallel()

	p := &sharedProvider{}

	err := p.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	bw, err := p.NewBuild(ctxT(), "b1")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	defer CloseBuild(bw, "b1")

	resourceDir, err := bw.ResourceDir(ctxT(), "repo")
	if err != nil {
		t.Fatalf("ResourceDir: %v", err)
	}

	taskSpace, err := bw.TaskSpace(ctxT(), "01-build", []string{"repo"}, []string{"built"})
	if err != nil {
		t.Fatalf("TaskSpace: %v", err)
	}

	if taskSpace.Dir() != filepath.Dir(resourceDir) {
		t.Errorf("TaskSpace().Dir() = %q, want the build root %q", taskSpace.Dir(), filepath.Dir(resourceDir))
	}

	err = taskSpace.Capture(ctxT())
	if err != nil {
		t.Errorf("shared StepSpace.Capture should be a no-op, got %v", err)
	}

	err = taskSpace.Close()
	if err != nil {
		t.Errorf("shared StepSpace.Close should be a no-op, got %v", err)
	}

	putSpace, err := bw.PutSpace(ctxT(), "02-put", nil)
	if err != nil {
		t.Fatalf("PutSpace: %v", err)
	}

	if putSpace.Dir() != taskSpace.Dir() {
		t.Errorf("PutSpace().Dir() = %q, want the same shared root %q", putSpace.Dir(), taskSpace.Dir())
	}
}

// newTestCopyProvider builds a copy-strategy isolatingProvider rooted at a
// fresh t.TempDir(), so these tests exercise the same isolatingProvider/
// isolatingBuild/isolatingSpace lifecycle the btrfs backend shares, without
// requiring a btrfs filesystem to run.
func newTestCopyProvider(t *testing.T) Provider {
	t.Helper()

	p, err := newCopyProvider(&config.WorkspaceConfig{Strategy: "copy", Root: t.TempDir()})
	if err != nil {
		t.Fatalf("newCopyProvider: %v", err)
	}

	err = p.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	return p
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()

	err := os.WriteFile(path, []byte(contents), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

// The following testProvider* helpers exercise the Provider/
// BuildWorkspace/StepSpace contract that isolatingProvider implements once
// (workspace.go) over a pluggable treeBackend — copy or btrfs. Each is
// called both from the copy-backed tests below (which run everywhere) and
// from workspace_btrfs_linux_test.go's btrfs-backed tests (which only run
// when STEPS_TEST_BTRFS_ROOT points at a real btrfs filesystem), so a
// regression in either backend's materialize/capture/remove semantics is
// caught the same way regardless of which one hits it first.

func testProviderMaterializesDeclaredInputsAndOutputs(t *testing.T, newProvider func(t *testing.T) Provider) {
	t.Helper()

	p := newProvider(t)

	bw, err := p.NewBuild(ctxT(), "b1")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	defer CloseBuild(bw, "b1")

	repoDir, err := bw.ResourceDir(ctxT(), "repo")
	if err != nil {
		t.Fatalf("ResourceDir: %v", err)
	}

	writeFile(t, filepath.Join(repoDir, "file.txt"), "original")

	space, err := bw.TaskSpace(ctxT(), "01-build", []string{"repo"}, []string{"built"})
	if err != nil {
		t.Fatalf("TaskSpace: %v", err)
	}

	// The input is materialized as a copy (or snapshot) under its own name.
	if got := readFile(t, filepath.Join(space.Dir(), "repo", "file.txt")); got != "original" {
		t.Errorf("materialized input contents = %q, want %q", got, "original")
	}

	// The output directory exists, empty, ready for the step to populate.
	entries, err := os.ReadDir(filepath.Join(space.Dir(), "built"))
	if err != nil {
		t.Fatalf("reading output dir: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("output dir has %d entries, want 0 (freshly created)", len(entries))
	}

	// Nothing else besides the declared input/output exists in the space.
	rootEntries, err := os.ReadDir(space.Dir())
	if err != nil {
		t.Fatal(err)
	}

	if len(rootEntries) != 2 {
		t.Errorf("step space has %d entries, want exactly 2 (repo, built)", len(rootEntries))
	}
}

// testProviderMutatingInputDoesNotAffectArtifact is the isolation guarantee
// this whole feature exists for: a step mutating its materialized copy of
// an input must never affect the pristine artifact other steps draw from.
func testProviderMutatingInputDoesNotAffectArtifact(t *testing.T, newProvider func(t *testing.T) Provider) {
	t.Helper()

	p := newProvider(t)

	bw, err := p.NewBuild(ctxT(), "b1")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	defer CloseBuild(bw, "b1")

	repoDir, err := bw.ResourceDir(ctxT(), "repo")
	if err != nil {
		t.Fatalf("ResourceDir: %v", err)
	}

	writeFile(t, filepath.Join(repoDir, "file.txt"), "original")

	space, err := bw.TaskSpace(ctxT(), "01-build", []string{"repo"}, nil)
	if err != nil {
		t.Fatalf("TaskSpace: %v", err)
	}

	writeFile(t, filepath.Join(space.Dir(), "repo", "file.txt"), "mutated")

	if got := readFile(t, filepath.Join(repoDir, "file.txt")); got != "original" {
		t.Errorf("pristine artifact was mutated: got %q, want %q", got, "original")
	}
}

func testProviderCaptureAndDownstreamVisibility(t *testing.T, newProvider func(t *testing.T) Provider) {
	t.Helper()

	p := newProvider(t)

	bw, err := p.NewBuild(ctxT(), "b1")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	defer CloseBuild(bw, "b1")

	space, err := bw.TaskSpace(ctxT(), "01-build", nil, []string{"built"})
	if err != nil {
		t.Fatalf("TaskSpace: %v", err)
	}

	writeFile(t, filepath.Join(space.Dir(), "built", "out.txt"), "artifact")

	err = space.Capture(ctxT())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	err = space.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err = os.Stat(space.Dir())
	if !os.IsNotExist(err) {
		t.Errorf("step space %q still exists after Close", space.Dir())
	}

	// A later step naming "built" as an input sees the captured output.
	downstream, err := bw.TaskSpace(ctxT(), "02-use", []string{"built"}, nil)
	if err != nil {
		t.Fatalf("TaskSpace (downstream): %v", err)
	}

	if got := readFile(t, filepath.Join(downstream.Dir(), "built", "out.txt")); got != "artifact" {
		t.Errorf("captured output contents = %q, want %q", got, "artifact")
	}
}

func testProviderCaptureMissingOutputErrors(t *testing.T, newProvider func(t *testing.T) Provider) {
	t.Helper()

	p := newProvider(t)

	bw, err := p.NewBuild(ctxT(), "b1")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	defer CloseBuild(bw, "b1")

	space, err := bw.TaskSpace(ctxT(), "01-build", nil, []string{"built"})
	if err != nil {
		t.Fatalf("TaskSpace: %v", err)
	}

	// Simulate a step that deleted its own declared output directory.
	err = os.RemoveAll(filepath.Join(space.Dir(), "built"))
	if err != nil {
		t.Fatal(err)
	}

	err = space.Capture(ctxT())
	if err == nil {
		t.Error("Capture with a missing declared output should error")
	}
}

// testProviderCaptureSwappedOutputSymlinkRejected reproduces the
// container-escape-adjacent scenario from the security review: a step
// deletes its declared output directory and replaces it with a symlink to
// an arbitrary host path (here, a directory outside the step space
// entirely). Capture must refuse to follow it instead of copying the real
// target's contents into the pipeline's artifact store as if they were the
// legitimate output.
func testProviderCaptureSwappedOutputSymlinkRejected(t *testing.T, newProvider func(t *testing.T) Provider) {
	t.Helper()

	p := newProvider(t)

	bw, err := p.NewBuild(ctxT(), "b1")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	defer CloseBuild(bw, "b1")

	space, err := bw.TaskSpace(ctxT(), "01-build", nil, []string{"built"})
	if err != nil {
		t.Fatalf("TaskSpace: %v", err)
	}

	secret := t.TempDir()
	writeFile(t, filepath.Join(secret, "secret.txt"), "should not leak")

	// Simulate a step that deleted its own declared output directory and
	// replaced it with a symlink to a host path outside the step space.
	outPath := filepath.Join(space.Dir(), "built")

	err = os.RemoveAll(outPath)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Symlink(secret, outPath)
	if err != nil {
		t.Fatal(err)
	}

	err = space.Capture(ctxT())
	if err == nil {
		t.Fatal("Capture through a swapped output symlink should be rejected, not silently followed")
	}
}

func testProviderUnknownInputErrors(t *testing.T, newProvider func(t *testing.T) Provider) {
	t.Helper()

	p := newProvider(t)

	bw, err := p.NewBuild(ctxT(), "b1")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	defer CloseBuild(bw, "b1")

	_, err = bw.TaskSpace(ctxT(), "01-build", []string{"nonexistent"}, nil)
	if err == nil {
		t.Error("TaskSpace with an unmaterialized input should error")
	}
}

func testProviderSymlinkCopiedNotFollowed(t *testing.T, newProvider func(t *testing.T) Provider) {
	t.Helper()

	p := newProvider(t)

	bw, err := p.NewBuild(ctxT(), "b1")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	defer CloseBuild(bw, "b1")

	repoDir, err := bw.ResourceDir(ctxT(), "repo")
	if err != nil {
		t.Fatalf("ResourceDir: %v", err)
	}

	writeFile(t, filepath.Join(repoDir, "target.txt"), "hi")

	err = os.Symlink("target.txt", filepath.Join(repoDir, "link.txt"))
	if err != nil {
		t.Fatal(err)
	}

	space, err := bw.TaskSpace(ctxT(), "01-build", []string{"repo"}, nil)
	if err != nil {
		t.Fatalf("TaskSpace: %v", err)
	}

	linkPath := filepath.Join(space.Dir(), "repo", "link.txt")

	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("Lstat materialized link: %v", err)
	}

	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("materialized link.txt is not a symlink; it should be copied as a link, not followed")
	}
}

func TestCopyProviderMaterializesDeclaredInputsAndOutputs(t *testing.T) {
	t.Parallel()
	testProviderMaterializesDeclaredInputsAndOutputs(t, newTestCopyProvider)
}

func TestCopyProviderMutatingInputDoesNotAffectArtifact(t *testing.T) {
	t.Parallel()
	testProviderMutatingInputDoesNotAffectArtifact(t, newTestCopyProvider)
}

func TestCopyProviderCaptureAndDownstreamVisibility(t *testing.T) {
	t.Parallel()
	testProviderCaptureAndDownstreamVisibility(t, newTestCopyProvider)
}

func TestCopyProviderCaptureMissingOutputErrors(t *testing.T) {
	t.Parallel()
	testProviderCaptureMissingOutputErrors(t, newTestCopyProvider)
}

func TestCopyProviderCaptureSwappedOutputSymlinkRejected(t *testing.T) {
	t.Parallel()
	testProviderCaptureSwappedOutputSymlinkRejected(t, newTestCopyProvider)
}

func TestCopyProviderUnknownInputErrors(t *testing.T) {
	t.Parallel()
	testProviderUnknownInputErrors(t, newTestCopyProvider)
}

func TestCopyProviderSymlinkCopiedNotFollowed(t *testing.T) {
	t.Parallel()
	testProviderSymlinkCopiedNotFollowed(t, newTestCopyProvider)
}

func TestValidateArtifactFlowUnknownInputErrors(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Workspace: &config.WorkspaceConfig{Strategy: "copy"}}
	job := &config.Job{
		Name: "build",
		Plan: []config.Step{
			{Task: "unit", Run: "true", Inputs: []string{"repo"}},
		},
	}

	err := ValidateArtifactFlow(cfg, job)
	if err == nil {
		t.Fatal("expected an error for a task input naming nothing fetched or produced earlier")
	}
}

func TestValidateArtifactFlowChainedOutputsResolve(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Workspace: &config.WorkspaceConfig{Strategy: "copy"}}
	job := &config.Job{
		Name: "build",
		Plan: []config.Step{
			{Get: "repo"},
			{Task: "build", Run: "true", Inputs: []string{"repo"}, Outputs: []string{"built"}},
			{Task: "publish", Run: "true", Inputs: []string{"built"}},
			{Put: "results", Inputs: []string{"built"}},
		},
	}

	err := ValidateArtifactFlow(cfg, job)
	if err != nil {
		t.Errorf("expected the plan's declared inputs/outputs to resolve cleanly, got %v", err)
	}
}

// TestValidateArtifactFlowGetResetsAvailableArtifacts guards the fix for the
// build-boundary gap: a get starts a fresh triggered build whose artifact
// store is empty except for its own resource, so an input naming an artifact
// produced before a *later* get can't be satisfied at runtime and must be
// rejected at plan time, not accumulated as if all gets shared one store.
func TestValidateArtifactFlowGetResetsAvailableArtifacts(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Workspace: &config.WorkspaceConfig{Strategy: "copy"}}

	t.Run("output produced before a later get is not visible after it", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{
			Name: "build",
			Plan: []config.Step{
				{Get: "repo"},
				{Task: "build", Run: "true", Inputs: []string{"repo"}, Outputs: []string{"built"}},
				{Get: "other"},
				{Task: "test", Run: "true", Inputs: []string{"built"}},
			},
		}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil {
			t.Fatal("expected an error: `built` was produced before `get other`, whose fresh build can't see it")
		}
	})

	t.Run("an earlier get's resource is not visible after a later get", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{
			Name: "build",
			Plan: []config.Step{
				{Get: "repo"},
				{Get: "other"},
				{Task: "test", Run: "true", Inputs: []string{"repo"}},
			},
		}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil {
			t.Fatal("expected an error: `repo` belongs to a build superseded by `get other`")
		}
	})

	t.Run("the latest get's own resource is available", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{
			Name: "build",
			Plan: []config.Step{
				{Get: "repo"},
				{Get: "other"},
				{Task: "test", Run: "true", Inputs: []string{"other"}},
			},
		}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Errorf("expected `other` (the most recent get) to be available, got %v", err)
		}
	})
}

// TestValidateArtifactFlowStepHooks checks that a step hook's declared inputs
// are validated against the artifact view it will actually see: on_success
// sees the step's own outputs, failure-path hooks do not.
func TestValidateArtifactFlowStepHooks(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Workspace: &config.WorkspaceConfig{Strategy: "copy"}}

	t.Run("on_success hook may consume the step's own output", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{
			Name: "build",
			Plan: []config.Step{
				{Get: "repo"},
				{
					Task: "build", Run: "true", Inputs: []string{"repo"}, Outputs: []string{"built"},
					Hooks: config.Hooks{OnSuccess: &config.Step{Put: "results", Inputs: []string{"built"}}},
				},
			},
		}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Errorf("on_success hook consuming the step's output should validate, got %v", err)
		}
	})

	t.Run("ensure hook may not consume the step's own output", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{
			Name: "build",
			Plan: []config.Step{
				{Get: "repo"},
				{
					Task: "build", Run: "true", Inputs: []string{"repo"}, Outputs: []string{"built"},
					Hooks: config.Hooks{Ensure: &config.Step{Put: "results", Inputs: []string{"built"}}},
				},
			},
		}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil {
			t.Fatal("expected an error: ensure runs on the failure path, before `built` is guaranteed to exist")
		}
	})

	t.Run("a hook output is not visible to a later plan step", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{
			Name: "build",
			Plan: []config.Step{
				{Get: "repo"},
				{
					Task: "build", Run: "true", Inputs: []string{"repo"},
					Hooks: config.Hooks{OnSuccess: &config.Step{Task: "gen", Run: "true", Outputs: []string{"extra"}}},
				},
				{Task: "consume", Run: "true", Inputs: []string{"extra"}},
			},
		}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil {
			t.Fatal("expected an error: a conditional hook's output must not satisfy a later plan step's input")
		}
	})
}

func TestValidateArtifactFlowNoOpWithoutWorkspace(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	job := &config.Job{
		Name: "build",
		Plan: []config.Step{
			{Task: "unit", Run: "true"},
		},
	}

	err := ValidateArtifactFlow(cfg, job)
	if err != nil {
		t.Errorf("validateArtifactFlow with no workspace: block should always be a no-op, got %v", err)
	}
}
