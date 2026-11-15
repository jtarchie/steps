package e2e

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// lockedBuffer is a buffer two goroutines may write: a run's stdout and stderr copiers are separate goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p) //nolint:wrapcheck // an io.Writer passes its writer's error through
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// TestTwoInvocationsAtOnceEachWriteToTheirOwnStreams is the seam that lets this suite run in one process: an invocation writes what a person reads to the streams it was handed, never to the process's. Each of several runs at once must find its own step output, its own listing, its own log lines and nobody else's — a print left on os.Stdout, or a log line that lost its context, shows up as a missing line here.
func TestTwoInvocationsAtOnceEachWriteToTheirOwnStreams(t *testing.T) {
	t.Parallel()

	const invocations = 4

	outputs := make([]*lockedBuffer, invocations)
	logs := make([]*lockedBuffer, invocations)
	paths := make([]string, invocations)

	var group sync.WaitGroup

	for i := range invocations {
		outputs[i] = &lockedBuffer{}
		logs[i] = &lockedBuffer{}
		dir := t.TempDir()
		path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: say
    inputs: []
    run: echo marker-%d-of-a-step
`, i))

		paths[i] = path

		group.Go(func() {
			streams := cli.Streams{Out: outputs[i], Err: logs[i]}

			for _, args := range [][]string{
				{"validate", path},
				{"run", path, "--job", "build", "--progress", "plain"},
				append([]string{"runs", "list"}, readArgs(path)...),
			} {
				err := cli.RunWith(streams, args)
				if err != nil {
					t.Errorf("invocation %d: %v: %v", i, args, err)
				}
			}
		})
	}

	group.Wait()

	for i, output := range outputs {
		got := output.String()

		for _, want := range []string{"ok: ", fmt.Sprintf("marker-%d-of-a-step", i), "build"} {
			if !strings.Contains(got, want) {
				t.Errorf("invocation %d did not write %q to its own stream:\n%s", i, want, got)
			}
		}

		assertOnlyItsOwn(t, i, got, logs[i].String(), paths)
	}
}

// assertOnlyItsOwn: invocation i's stderr holds its own log lines, and neither of its streams holds another invocation's. slog has one handler per process, so a log line reaches an invocation's own stderr only by the context it was logged with.
func assertOnlyItsOwn(t *testing.T, i int, got, logged string, paths []string) {
	t.Helper()

	if !strings.Contains(logged, " pipeline.run ") || !strings.Contains(logged, "pipeline="+paths[i]) {
		t.Errorf("invocation %d's stderr does not carry its own log lines:\n%s", i, logged)
	}

	for other := range paths {
		if other == i {
			continue
		}

		if strings.Contains(got, fmt.Sprintf("marker-%d-of-a-step", other)) {
			t.Errorf("invocation %d's stream carries invocation %d's output:\n%s", i, other, got)
		}

		if strings.Contains(logged, "pipeline="+paths[other]) {
			t.Errorf("invocation %d's stderr carries invocation %d's log lines:\n%s", i, other, logged)
		}
	}
}
