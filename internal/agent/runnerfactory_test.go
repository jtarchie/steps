package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

// TestRunnerFactoryBuildsThroughThePlacement: a factory is how a placed step's tools reach the machine it was placed on, so build must go through it, and only a nil one means this machine.
func TestRunnerFactoryBuildsThroughThePlacement(t *testing.T) {
	t.Parallel()

	placed := errors.New("placed elsewhere")

	_, err := RunnerFactory(func(context.Context, shell.RunnerSpec) (shell.Runner, error) { return nil, placed }).build(t.Context(), shell.RunnerSpec{})
	if !errors.Is(err, placed) {
		t.Errorf("build = %v, want the factory's own answer", err)
	}

	local, err := RunnerFactory(nil).build(t.Context(), shell.RunnerSpec{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("build with no factory: %v", err)
	}

	shell.CloseRunner(local, "test")
}
