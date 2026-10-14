package venue

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/testsshd"
	"github.com/jtarchie/steps/internal/wire"
)

// tightenFetchLimits makes every fetched tree past four bytes too large.
// Not parallel: it swaps a package var, and a top-level test that does not
// call t.Parallel finishes before any test that does resumes.
func tightenFetchLimits(t *testing.T) {
	t.Helper()

	saved := fetchLimits
	fetchLimits = func() wire.UnpackOptions { return wire.UnpackOptions{MaxBytes: 4} }

	t.Cleanup(func() { fetchLimits = saved })
}

// TestFetchedTreesAreBounded: every path that brings a worker's tree home
// unpacks it under fetchLimits — a worker does not get to fill this disk —
// and the refusal leaves the local output as it was.
func TestFetchedTreesAreBounded(t *testing.T) { //nolint:paralleltest // swaps fetchLimits
	t.Run("ssh", func(t *testing.T) {
		cwd := t.TempDir()
		mustWrite(t, filepath.Join(cwd, "out", "kept.txt"), "kept\n")

		runner, err := NewRunner(sshSpec(t, newTestSSHD(t), cwd, "out"))
		if err != nil {
			t.Fatalf("NewRunner: %v", err)
		}

		t.Cleanup(func() { _ = runner.Close() })

		tightenFetchLimits(t)

		err = shell.Run(t.Context(), runner, "echo 12345678 > out/big.txt")
		assertTooLarge(t, err, cwd)
	})

	t.Run("docker+", func(t *testing.T) {
		cwd := t.TempDir()
		mustWrite(t, filepath.Join(cwd, "out", "kept.txt"), "kept\n")

		runner := plusRunnerFor(t, dockerPlusURL(testsshd.New(t), hostDockerSocket(t)), cwd, "out")

		tightenFetchLimits(t)

		err := shell.Run(t.Context(), runner, "echo 12345678 > out/big.txt")
		assertTooLarge(t, err, cwd)
	})

	t.Run("pull", func(t *testing.T) {
		worker := dockerPlusURL(testsshd.New(t), hostDockerSocket(t))
		digest := holdOnce(t, worker, "echo 12345678 > out/big.txt")

		tightenFetchLimits(t)

		_, err := Pull(t.Context(), shell.RunnerSpec{Worker: worker}, digest, t.TempDir())
		if !errors.Is(err, wire.ErrTreeTooLarge) {
			t.Fatalf("Pull = %v, want wire.ErrTreeTooLarge", err)
		}
	})
}

func assertTooLarge(t *testing.T, err error, cwd string) {
	t.Helper()

	if !errors.Is(err, wire.ErrTreeTooLarge) {
		t.Fatalf("Run = %v, want wire.ErrTreeTooLarge", err)
	}

	if got := mustRead(t, filepath.Join(cwd, "out", "kept.txt")); got != "kept\n" {
		t.Errorf("out/kept.txt = %q, want the local output untouched by a refused fetch", got)
	}

	_, statErr := os.Stat(filepath.Join(cwd, "out", "big.txt"))
	if !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("out/big.txt landed anyway: %v", statErr)
	}
}
