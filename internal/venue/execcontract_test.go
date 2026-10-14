package venue

import (
	"bytes"
	"context"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
)

// TestPlacedExecMatrix is internal/shell's TestContractExecMatrix asked of the
// placed runners: a step that moves to a worker must answer every combination
// of Command's settings the way it did on this machine.
func TestPlacedExecMatrix(t *testing.T) {
	cases := []struct {
		name     string
		cmd      shell.Command
		want     shell.Result
		exitErr  bool
		streamed [2]string
	}{
		{
			name: "exit as data keeps both streams",
			cmd:  shell.Command{Script: "echo out; echo err >&2; exit 3", Capture: true, ExitAsData: true},
			want: shell.Result{Stdout: "out\n", Stderr: "err\n", Code: 3},
		},
		{
			name:     "exit as error still hands back what was captured",
			cmd:      shell.Command{Script: "echo out; echo err >&2; exit 4", Stream: shell.StreamStderr, Capture: true},
			want:     shell.Result{Stdout: "out\n", Stderr: "err\n", Code: 4},
			exitErr:  true,
			streamed: [2]string{"", "[lbl] err\n"},
		},
		{
			name:     "streamed without capture keeps nothing",
			cmd:      shell.Command{Script: "echo out; echo err >&2", Stream: shell.StreamBoth},
			streamed: [2]string{"[lbl] out\n", "[lbl] err\n"},
		},
		{
			name: "limit bounds each stream while it runs",
			cmd:  shell.Command{Script: "printf 0123456789", Capture: true, Limit: 4, ExitAsData: true},
			want: shell.Result{Stdout: "0123\n... [truncated 6 bytes]"},
		},
	}

	runners := map[string]func(t *testing.T) shell.Runner{
		"ssh": func(t *testing.T) shell.Runner {
			t.Helper()

			return newLocalRunner(t, sshSpec(t, newTestSSHD(t), t.TempDir()))
		},
		"docker+": func(t *testing.T) shell.Runner {
			t.Helper()

			return newLocalRunner(t, localWorker(t, t.TempDir()))
		},
	}

	for name, newRunner := range runners {
		t.Run(name, func(t *testing.T) {
			runner := newRunner(t).WithLabel("lbl")

			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					var liveOut, liveErr bytes.Buffer

					ctx := events.WithOutput(context.Background(), events.Output{Stdout: &liveOut, Stderr: &liveErr})

					res, err := runner.Exec(ctx, c.cmd)
					if c.exitErr != shell.IsExitError(err) || (!c.exitErr && err != nil) {
						t.Fatalf("err = %v, want exit error %v", err, c.exitErr)
					}

					if res != c.want {
						t.Errorf("Result = %+v, want %+v", res, c.want)
					}

					if got := [2]string{liveOut.String(), liveErr.String()}; got != c.streamed {
						t.Errorf("streamed %q, want %q", got, c.streamed)
					}
				})
			}
		})
	}
}
