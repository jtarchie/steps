package shell

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/testdocker"
)

// execCase is one point of Command's matrix and what every runner must answer
// for it: the same Result, the same live bytes, the same error class.
type execCase struct {
	name    string
	cmd     Command
	want    Result
	exitErr bool
	// streamed is what went out live, stdout then stderr: each has its own writer, so their interleaving is the OS's.
	streamed [2]string
}

//nolint:gochecknoglobals // a read-only table shared by the host and docker runs
var execCases = []execCase{
	{
		name: "exit as data keeps both streams",
		cmd:  Command{Script: "echo out; echo err >&2; exit 3", Capture: true, ExitAsData: true},
		want: Result{Stdout: "out\n", Stderr: "err\n", Code: 3},
	},
	{
		name:     "exit as error still hands back what was captured",
		cmd:      Command{Script: "echo out; echo err >&2; exit 4", Stream: StreamStderr, Capture: true},
		want:     Result{Stdout: "out\n", Stderr: "err\n", Code: 4},
		exitErr:  true,
		streamed: [2]string{"", "[lbl] err\n"},
	},
	{
		name:     "streamed without capture keeps nothing",
		cmd:      Command{Script: "echo out; echo err >&2", Stream: StreamBoth},
		want:     Result{},
		streamed: [2]string{"[lbl] out\n", "[lbl] err\n"},
	},
	{
		name: "limit bounds each stream while it runs",
		cmd:  Command{Script: "printf 0123456789", Capture: true, Limit: 4, ExitAsData: true},
		want: Result{Stdout: "0123\n... [truncated 6 bytes]"},
	},
	{
		name: "stdin off reads end of file",
		cmd:  Command{Script: "cat; echo done", Capture: true, ExitAsData: true},
		want: Result{Stdout: "done\n"},
	},
}

// TestContractExecMatrix holds the host and the container to one answer for
// every combination of Command's settings, through Exec itself rather than a
// helper that picks one combination.
func TestContractExecMatrix(t *testing.T) {
	runners := map[string]func(t *testing.T) Runner{
		"host": func(t *testing.T) Runner {
			t.Helper()

			return newTestRunner(t, RunnerSpec{Cwd: t.TempDir()})
		},
		"docker": func(t *testing.T) Runner {
			t.Helper()
			testdocker.Require(t)

			return newTestRunner(t, RunnerSpec{Image: testImage, Cwd: testdocker.MountableDir(t)})
		},
	}

	for name, newRunner := range runners {
		t.Run(name, func(t *testing.T) {
			runner := newRunner(t).WithLabel("lbl")

			for _, c := range execCases {
				t.Run(c.name, func(t *testing.T) {
					assertExecCase(t, runner, c)
				})
			}
		})
	}
}

func assertExecCase(t *testing.T, runner Runner, c execCase) {
	t.Helper()

	var liveOut, liveErr bytes.Buffer

	ctx := events.WithOutput(context.Background(), events.Output{Stdout: &liveOut, Stderr: &liveErr})

	res, err := runner.Exec(ctx, c.cmd)

	if c.exitErr {
		if !IsExitError(err) {
			t.Fatalf("err = %v, want an exit error", err)
		}

		if !strings.Contains(err.Error(), "failed") {
			t.Errorf("err = %v, want it to name the failed command", err)
		}
	} else if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if res != c.want {
		t.Errorf("Result = %+v, want %+v", res, c.want)
	}

	if got := [2]string{liveOut.String(), liveErr.String()}; got != c.streamed {
		t.Errorf("streamed %q, want %q", got, c.streamed)
	}
}

// A command that never started is an error under ExitAsData, with nothing
// captured and the -1 no exit status could produce, on every runner.
func TestContractExecNeverStarted(t *testing.T) {
	runners := map[string]func(t *testing.T) Runner{
		"host": func(t *testing.T) Runner {
			t.Helper()

			return HostRunner{cwd: "/definitely/not/a/dir"}
		},
		"docker": func(t *testing.T) Runner {
			t.Helper()
			testdocker.Require(t)

			runner := newTestRunner(t, RunnerSpec{Image: testImage, Cwd: testdocker.MountableDir(t)})
			CloseRunner(runner, "test")

			return runner
		},
	}

	for name, newRunner := range runners {
		t.Run(name, func(t *testing.T) {
			res, err := newRunner(t).Exec(context.Background(), Command{Script: "true", Capture: true, ExitAsData: true})
			if err == nil || IsExitError(err) {
				t.Fatalf("err = %v, want an infrastructure error", err)
			}

			if res != (Result{Code: -1}) {
				t.Errorf("Result = %+v, want only the -1 no exit status could produce", res)
			}
		})
	}
}
