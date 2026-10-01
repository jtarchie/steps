package cli

// The small fixtures this package's tests share. They are deliberately
// duplicated from ./e2e rather than exported from it: a helper that captures
// os.Stdout or writes a pipeline file is three lines of policy, and a shared
// one would have to be a non-test package compiled into every build.

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
//
// It assigns the os.Stdout GLOBAL, which every fmt.Printf in the code under
// test reads — so a test using it must NOT be parallel. TestNoParallelTestRedirectsStdout
// in ./e2e enforces that across the module, this package included.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	orig := os.Stdout
	os.Stdout = w

	// Deferred because a t.Fatal inside fn exits through runtime.Goexit, which would leave every later test writing into this pipe.
	func() {
		defer func() {
			_ = w.Close()
			os.Stdout = orig
		}()

		fn()
	}()

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}

	return string(data)
}

// captureLog installs a text logger for the rest of the test and returns what it has logged so far. Not for a parallel test: the default logger is process-wide.
func captureLog(t *testing.T) func() string {
	t.Helper()

	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return buf.String
}

// lockedBuffer is written by whatever goroutine logs and read by the test.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p) //nolint:wrapcheck // a bytes.Buffer write never fails
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// writePipelineFile writes a pipeline fixture to path.
func writePipelineFile(t *testing.T, path, pipeline string) {
	t.Helper()

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// flagFixture writes the smallest pipeline a command can be pointed at: one
// job, one task, nothing to fetch. What the tests using it assert happens
// before any of it executes.
func flagFixture(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "pipeline.yml")

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run: "true"
`)

	return path
}
