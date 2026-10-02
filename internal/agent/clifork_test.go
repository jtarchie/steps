package agent

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// TestParseCLIStreamNestsAForkCutOffMidRun is the aborted review: the fork
// never reports finishing, and what it did through the bridge must still land
// in the step's transcript once the stream ends.
func TestParseCLIStreamNestsAForkCutOffMidRun(t *testing.T) {
	t.Parallel()

	rec := &transcriptRecorder{}
	reader, writer := io.Pipe()
	done := make(chan error, 1)

	go func() {
		_, err := parseCLIStream(reader, rec, nil)
		done <- err
	}()

	_, _ = fmt.Fprintln(writer, `{"type":"system","subtype":"task_started","task_id":"f1","description":"/code-review","prompt":"Review the diff."}`)

	fork := waitForFork(t, rec)
	fork.call("run_shell", map[string]any{"command": "git diff"})
	fork.result("run_shell", "diff --git a/main.go")

	_ = writer.Close()

	err := <-done
	if err != nil {
		t.Fatalf("parseCLIStream: %v", err)
	}

	recorded := rec.recorded()
	if len(recorded) != 1 || recorded[0].Type != "subagent" {
		t.Fatalf("want the fork nested as one delegation, got %+v", recorded)
	}

	nested := recorded[0]
	if nested.Agent != "/code-review" || nested.Request != "Review the diff." {
		t.Errorf("delegation = %q / %q", nested.Agent, nested.Request)
	}

	if len(nested.Events) != 2 || nested.Events[0].Name != "run_shell" || nested.Events[1].Type != "result" {
		t.Errorf("the fork's own call and result were not kept: %+v", nested.Events)
	}

	if rec.forkRecorder() != nil {
		t.Error("the fork is still open after its stream ended")
	}
}

// TestParseCLIStreamLeavesNarratedForksToTheStream holds the two forks whose
// calls the bridge must NOT record: either the stream already shows them, or
// the parent runs beside the fork and a call could be either's.
func TestParseCLIStreamLeavesNarratedForksToTheStream(t *testing.T) {
	t.Parallel()

	for name, line := range map[string]string{
		"started by a tool call": `{"type":"system","subtype":"task_started","task_id":"f1","tool_use_id":"toolu_1","description":"/code-review"}`,
		"backgrounded":           `{"type":"system","subtype":"task_started","task_id":"f1","is_backgrounded":true,"description":"/code-review"}`,
	} {
		rec := &transcriptRecorder{}
		reader, writer := io.Pipe()
		done := make(chan struct{})

		go func() {
			defer close(done)

			_, _ = parseCLIStream(reader, rec, nil)
		}()

		_, _ = fmt.Fprintln(writer, line)
		// A second line is only read once the first was dispatched.
		_, _ = fmt.Fprintln(writer, `{"type":"system","subtype":"status"}`)

		if rec.forkRecorder() != nil {
			t.Errorf("%s: opened a fork the bridge would double-record", name)
		}

		_ = writer.Close()
		<-done
	}
}

// TestParseCLIStreamClosesOnlyTheForkNamed keeps a notification for some other
// task from closing the open fork early.
func TestParseCLIStreamClosesOnlyTheForkNamed(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"type":"system","subtype":"task_started","task_id":"f1","description":"/code-review"}`,
		`{"type":"system","subtype":"task_notification","task_id":"other","status":"completed"}`,
	}, "\n")

	rec := &transcriptRecorder{}
	reader, writer := io.Pipe()
	done := make(chan struct{})

	go func() {
		defer close(done)

		_, _ = parseCLIStream(reader, rec, nil)
	}()

	_, _ = fmt.Fprintln(writer, stream)
	_, _ = fmt.Fprintln(writer, `{"type":"system","subtype":"status"}`)

	if rec.forkRecorder() == nil {
		t.Error("another task's notification closed the open fork")
	}

	_ = writer.Close()
	<-done
}

func waitForFork(t *testing.T, rec *transcriptRecorder) *transcriptRecorder {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		if fork := rec.forkRecorder(); fork != nil {
			return fork
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatal("the stream never opened the fork")

	return nil
}
