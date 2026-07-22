//go:build linux

package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestValidateBtrfsRootRejectsNonBtrfsFilesystem needs the btrfs CLI but not
// an actual btrfs filesystem, so it runs on any Linux box that has
// btrfs-progs installed — unlike the tests below, it needs no
// STEPS_TEST_BTRFS_ROOT.
func TestValidateBtrfsRootRejectsNonBtrfsFilesystem(t *testing.T) {
	t.Parallel()

	_, err := exec.LookPath("btrfs")
	if err != nil {
		t.Skip("btrfs CLI not installed")
	}

	err = validateBtrfsRoot(t.TempDir())
	if err == nil {
		t.Fatal("expected an error for a root not on a btrfs filesystem")
	}

	if !strings.Contains(err.Error(), "not on a btrfs filesystem") {
		t.Errorf("error = %v, want it to mention the root is not on a btrfs filesystem", err)
	}
}

func TestValidateBtrfsRootRequiresRoot(t *testing.T) {
	t.Parallel()

	err := validateBtrfsRoot("")
	if err == nil {
		t.Fatal("expected an error for an empty root")
	}
}

// realBtrfsRoot skips unless STEPS_TEST_BTRFS_ROOT names a writable
// directory on an actual btrfs filesystem — see this repo's docs for how to
// provision one (e.g. a loopback-mounted btrfs image inside a privileged
// Linux container; a plain CI runner's /tmp is almost never btrfs).
func realBtrfsRoot(t *testing.T) string {
	t.Helper()

	root := os.Getenv("STEPS_TEST_BTRFS_ROOT")
	if root == "" {
		t.Skip("set STEPS_TEST_BTRFS_ROOT to a writable directory on a real btrfs filesystem to run btrfs backend tests")
	}

	ok, err := isBtrfs(root)
	if err != nil {
		t.Fatalf("isBtrfs(%q): %v", root, err)
	}

	if !ok {
		t.Skipf("STEPS_TEST_BTRFS_ROOT=%q is not on a btrfs filesystem", root)
	}

	return root
}

// newTestBtrfsProvider builds a btrfs-strategy isolatingProvider rooted at
// a fresh subvolume-capable directory under STEPS_TEST_BTRFS_ROOT, run
// through the same isolatingProvider/Build/Space lifecycle the copy backend
// tests (workspace_test.go) exercise — only the treeBackend differs.
func newTestBtrfsProvider(t *testing.T) Provider {
	t.Helper()

	root := realBtrfsRoot(t)

	dir, err := os.MkdirTemp(root, "steps-btrfs-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp under btrfs root: %v", err)
	}

	t.Cleanup(func() {
		removeErr := os.RemoveAll(dir)
		if removeErr != nil {
			t.Logf("cleanup: RemoveAll(%q): %v", dir, removeErr)
		}
	})

	p := newBtrfsProvider(&config.WorkspaceConfig{Strategy: "btrfs", Root: dir, Options: config.WorkspaceOptions{Compression: "zstd"}})

	err = p.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	return p
}

func TestBtrfsProviderMaterializesDeclaredInputsAndOutputs(t *testing.T) {
	t.Parallel()
	testProviderMaterializesDeclaredInputsAndOutputs(t, newTestBtrfsProvider)
}

func TestBtrfsProviderMutatingInputDoesNotAffectArtifact(t *testing.T) {
	t.Parallel()
	testProviderMutatingInputDoesNotAffectArtifact(t, newTestBtrfsProvider)
}

func TestBtrfsProviderCaptureAndDownstreamVisibility(t *testing.T) {
	t.Parallel()
	testProviderCaptureAndDownstreamVisibility(t, newTestBtrfsProvider)
}

func TestBtrfsProviderCaptureMissingOutputErrors(t *testing.T) {
	t.Parallel()
	testProviderCaptureMissingOutputErrors(t, newTestBtrfsProvider)
}

func TestBtrfsProviderCaptureSwappedOutputSymlinkRejected(t *testing.T) {
	t.Parallel()
	testProviderCaptureSwappedOutputSymlinkRejected(t, newTestBtrfsProvider)
}

func TestBtrfsProviderUnknownInputErrors(t *testing.T) {
	t.Parallel()
	testProviderUnknownInputErrors(t, newTestBtrfsProvider)
}

func TestBtrfsProviderSymlinkCopiedNotFollowed(t *testing.T) {
	t.Parallel()
	testProviderSymlinkCopiedNotFollowed(t, newTestBtrfsProvider)
}

// TestBtrfsProviderCreatesRealSubvolumes checks the backend actually took
// the CoW-subvolume path (not, say, silently falling back to plain
// directories) and that the compression property was applied — behavior
// specific to this backend, so it isn't part of the shared contract tests
// above.
func TestBtrfsProviderCreatesRealSubvolumes(t *testing.T) {
	t.Parallel()

	p := newTestBtrfsProvider(t)

	bw, err := p.NewBuild(ctxT(), "b1")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	defer CloseBuild(bw, "b1")

	repoDir, err := bw.ResourceDir(ctxT(), "repo")
	if err != nil {
		t.Fatalf("ResourceDir: %v", err)
	}

	out, err := exec.Command("btrfs", "subvolume", "show", repoDir).CombinedOutput() //nolint:gosec // repoDir is a workspace-internal path this test just created
	if err != nil {
		t.Fatalf("btrfs subvolume show %q: %v: %s", repoDir, err, out)
	}

	prop, err := exec.Command("btrfs", "property", "get", repoDir, "compression").CombinedOutput() //nolint:gosec // repoDir is a workspace-internal path this test just created
	if err != nil {
		t.Fatalf("btrfs property get %q compression: %v: %s", repoDir, err, prop)
	}

	if !strings.Contains(string(prop), "zstd") {
		t.Errorf("compression property = %q, want it to mention zstd", prop)
	}
}

func TestBtrfsProviderCleansUpSubvolumesOnClose(t *testing.T) {
	t.Parallel()

	p := newTestBtrfsProvider(t)

	bw, err := p.NewBuild(ctxT(), "b1")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}

	repoDir, err := bw.ResourceDir(ctxT(), "repo")
	if err != nil {
		t.Fatalf("ResourceDir: %v", err)
	}

	CloseBuild(bw, "b1")

	_, err = os.Stat(repoDir)
	if !os.IsNotExist(err) {
		t.Errorf("resource subvolume %q still exists after the build's Close", repoDir)
	}

	_, err = os.Stat(filepath.Dir(repoDir))
	if !os.IsNotExist(err) {
		t.Errorf("build root %q still exists after Close", filepath.Dir(repoDir))
	}
}
