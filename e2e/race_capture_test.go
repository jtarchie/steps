package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestEndToEndARaceLoserCannotCaptureOverTheWinner: a capture ignores cancellation, so a loser whose command finished after the winner captured wrote its own artifact over the winner's. Ordered with FIFOs and the run's own output, no clock: fast captures, its hook lets slow finish, and fast is held until slow has either captured (the bug: slow's hook says so) or been made to wait (the race says so).
func TestEndToEndARaceLoserCannotCaptureOverTheWinner(t *testing.T) {
	t.Parallel()

	for name, fast := range map[string]string{
		"a task wins": `      - task: fast
        outputs: [summary]
        run: echo from-fast > summary/text
`,
		"an agent wins": `      - agent: fast
        outputs: [summary]
        messages:
          - Summarize.
`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			model := newFakeLLM(t,
				callsTool("write_file", map[string]any{"path": "summary/text", "content": "from-fast\n"}),
				says("Done."),
			)

			raceLoserCannotCapture(t, model.URL, fast)
		})
	}
}

func raceLoserCannotCapture(t *testing.T, endpoint, fast string) {
	t.Helper()

	dir := t.TempDir()
	captured, release, never := filepath.Join(dir, "captured"), filepath.Join(dir, "release"), filepath.Join(dir, "never")
	seen := filepath.Join(dir, "seen.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: fast
  source:
    endpoint: %[5]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [write_file]

jobs:
- name: build
  plan:
  - task: fifos
    run: mkfifo %[1]s %[2]s %[3]s
  - race:
      steps:
%[6]s        on_success:
          task: hold
          run: |
            echo go > %[1]s
            cat %[2]s > /dev/null
      - task: slow
        outputs: [summary]
        run: |
          cat %[1]s > /dev/null
          echo from-slow > summary/text
        on_success:
          task: linger
          run: |
            echo slow-captured
            exec cat %[3]s
  - task: read
    inputs: [summary]
    run: cat summary/text >> %[4]s
`, captured, release, never, seen, endpoint, fast))

	out := &watchedOutput{want: []string{"slow-captured", "slow waits for its outcome"}, seen: make(chan string, 1)}
	done := make(chan error, 1)

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	go func() {
		done <- cli.RunContext(ctx, cli.Streams{Out: out, Err: os.Stderr}, []string{"run", path, "--job", "build", "--progress", "plain"})
	}()

	select {
	case matched := <-out.seen:
		if matched == "slow-captured" {
			t.Errorf("slow captured over the outputs fast had already written\n%s", out)
			stop()
			<-done

			return
		}
	case err := <-done:
		t.Fatalf("the run ended before slow finished its command: %v\n%s", err, out)
	}

	// Blocks until hold opens the FIFO to read, which it reaches once slow has been let go.
	holdOpen, err := os.OpenFile(release, os.O_WRONLY, 0) //nolint:gosec // the test's own FIFO
	if err != nil {
		t.Fatalf("release hold: %v", err)
	}

	_ = holdOpen.Close()

	err = <-done
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	if got := readFileString(t, seen); got != "from-fast\n" {
		t.Errorf("the step after the race read %q, want fast's artifact: fast won\n%s", got, out)
	}
}

// watchedOutput is a run's stdout that says which of want was written first.
type watchedOutput struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	want []string
	seen chan string
	once sync.Once
}

func (w *watchedOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf.Write(p)

	for _, want := range w.want {
		if strings.Contains(w.buf.String(), want) {
			w.once.Do(func() { w.seen <- want })
		}
	}

	return len(p), nil
}

func (w *watchedOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.buf.String()
}
