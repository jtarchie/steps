package agent

// The container half of the file tools, against real images: they run whenever a daemon is reachable and skip cleanly when one is not, because a test that only runs when somebody opts in is how a shipped feature stays broken without anybody noticing.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/agent/tree"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/testdocker"
)

// An image: agent with no tools: grant gets the read-only file tools, and they read the container's copy of the tree, not this machine's: the default grant reaches the tree like any other.
func TestADefaultGrantImageAgentReadsTheContainer(t *testing.T) {
	t.Parallel()
	testdocker.Require(t)

	dir := t.TempDir()
	writeBytes(t, dir, "where.txt", []byte("from-the-volume"))
	volume := testdocker.Tree(t, dir)
	writeBytes(t, dir, "where.txt", []byte("from-this-machine"))

	runner, err := shell.NewRunner(shell.RunnerSpec{Image: "alpine:3", MountPath: testdocker.TreePath, Volumes: volume.Volumes})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { shell.CloseRunner(runner, "test") })

	prepared := preparedAgentStep{
		dir: dir, runner: runner, lost: &tree.Lost{},
		resolvedAgent: resolvedAgent{ri: config.ResolvedInvocation{AgentName: "hand", Image: "alpine:3"}},
		space:         testStepSpace(t),
	}
	prepared.conv.env.dir = dir

	prepared, err = prepared.openTree(t.Context())
	if err != nil {
		t.Fatalf("openTree: %v", err)
	}

	got := fmt.Sprint(execReadFile(t.Context(), map[string]any{"path": "where.txt"}, prepared.conv.env))
	if !strings.Contains(got, "from-the-volume") {
		t.Errorf("read_file(where.txt) = %s, want the container's copy", got)
	}
}

// TestContainerTreeReadsAnImageWhole is the binary half of the container path:
// the file crosses as a script's stdout, so a byte the shell or the capture
// mangled — a NUL, a CR, a high byte — would reach the model as a corrupt
// image the provider refuses. A ranged read goes through it too, since a ranged
// read of an image is answered with the whole image.
func TestContainerTreeReadsAnImageWhole(t *testing.T) {
	t.Parallel()
	testdocker.Require(t)

	for _, image := range []string{"alpine:3", "debian:13-slim"} {
		t.Run(image, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			data := encodedImage(t, "png", 64, 48)
			writeBytes(t, dir, "shot.png", data)

			runner, err := shell.NewRunner(shell.RunnerSpec{Image: image, MountPath: testdocker.TreePath, Volumes: testdocker.Tree(t, dir).Volumes})
			if err != nil {
				t.Fatalf("NewRunner: %v", err)
			}

			t.Cleanup(func() { shell.CloseRunner(runner, "test") })

			lost := &tree.Lost{}

			contained, root, err := tree.ForStep(t.Context(), runner, image, dir, []config.ToolSpec{{Builtin: "read_file"}}, lost)
			if err != nil {
				t.Fatalf("tree.ForStep(%s): %v", image, err)
			}

			env := toolEnv{dir: root, tree: contained, lost: lost, sight: hostedSight}

			for _, args := range []map[string]any{
				{"path": "shot.png"},
				{"path": "shot.png", "start_line": float64(1), "end_line": float64(1)},
			} {
				got := execReadFile(t.Context(), args, env)

				img, ok := got[imageKey].(toolImage)
				if !ok {
					t.Fatalf("read_file(%v) in %s = %v, want the image", args, image, got)
				}

				if !bytes.Equal(img.data, data) || img.width != 64 || img.height != 48 {
					t.Errorf("read_file(%v) in %s returned %d bytes (%dx%d), want the file's own %d (64x48)",
						args, image, len(img.data), img.width, img.height, len(data))
				}
			}
		})
	}
}
