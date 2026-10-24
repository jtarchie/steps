//go:build !darwin

package shell

import (
	"context"
	"errors"
	"testing"
)

func TestSandboxRefusesWhereNoImplementationExists(t *testing.T) {
	runner, err := NewRunner(RunnerSpec{Cwd: t.TempDir(), Sandbox: true})
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = runner.Close() }()

	_, err = RunCapture(context.Background(), runner, "true")
	if !errors.Is(err, errSandboxUnsupported) {
		t.Fatalf("RunCapture = %v; want errSandboxUnsupported rather than an unfenced command", err)
	}
}
