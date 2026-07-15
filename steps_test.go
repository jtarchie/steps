package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunFlagParsing(t *testing.T) {
	t.Parallel()

	const pipeline = `
jobs:
- name: build
  plan:
  - task: build
    run: echo hi
- name: test
  plan:
  - task: test
    run: echo hi
`

	path := filepath.Join(t.TempDir(), "pipeline.yml")

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("job flag before positional path", func(t *testing.T) {
		t.Parallel()

		err := run([]string{"--job", "build", path})
		if err != nil {
			t.Errorf("run: %v", err)
		}
	})

	t.Run("job flag after positional path", func(t *testing.T) {
		t.Parallel()

		err := run([]string{path, "--job", "test"})
		if err != nil {
			t.Errorf("run: %v", err)
		}
	})

	t.Run("job flag attached value", func(t *testing.T) {
		t.Parallel()

		err := run([]string{"--job=build", path})
		if err != nil {
			t.Errorf("run: %v", err)
		}
	})

	t.Run("missing job on ambiguous pipeline", func(t *testing.T) {
		t.Parallel()

		err := run([]string{path})
		if err == nil {
			t.Error("expected error when --job is omitted and multiple jobs exist")
		}
	})

	t.Run("missing pipeline path", func(t *testing.T) {
		t.Parallel()

		err := run(nil)
		if err == nil {
			t.Error("expected error for missing positional pipeline path")
		}
	})
}
