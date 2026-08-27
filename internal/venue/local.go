package venue

// The local: venue: a shim in a child process on this machine.
//
// It exists for three reasons, in ascending order of how much they matter. It
// is how somebody runs a tagged pipeline on a laptop with no worker to reach.
// It is how the shim gets debugged by hand. And it is how everything above the
// transport — framing, the tree round trip, exit semantics, cancellation —
// gets exercised without a network, credentials, or a second machine, which is
// what lets the documented example be one that actually runs.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// dial opens a transport to a worker.
func dial(ctx context.Context, worker Worker) (*transport, error) {
	switch worker.Scheme {
	case SchemeLocal:
		return dialLocal(worker)
	case SchemeSSH:
		return dialSSH(ctx, worker)
	case SchemeAWS:
		return dialSSM(ctx, worker)
	default:
		return nil, fmt.Errorf("%w: unknown scheme %q", ErrWorker, worker.Scheme)
	}
}

// errNoBinary is a shim that cannot be started because this process cannot say
// where its own binary is — which on any ordinary system it can.
var errNoBinary = errors.New("cannot locate this steps binary to run a shim from")

func dialLocal(worker Worker) (*transport, error) {
	binary := worker.Binary
	if binary == "" {
		var err error

		binary, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errNoBinary, err)
		}
	}

	// Deliberately NOT CommandContext, and not tied to any step's context:
	// cancelling a step must cancel the COMMAND the shim is running, not the
	// shim itself, or a merely-interrupted step loses the outputs the session
	// was about to hand back. The shim exits on its own when its stdin closes,
	// which is what close below does.
	build, err := buildOf(worker)
	if err != nil {
		return nil, err
	}

	//nolint:noctx,gosec // see above: the lifetime is the session's, not a command's, and the binary is this process or one the operator named
	command := exec.Command(binary, "_shim")

	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("opening a pipe to the shim: %w", err)
	}

	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("opening a pipe from the shim: %w", err)
	}

	// The shim's stderr is diagnostics, never protocol, so it goes where every
	// other diagnostic goes. This is also the only place a shim that failed to
	// start — a wrong architecture, a missing interpreter — gets to say so.
	command.Stderr = os.Stderr

	err = command.Start()
	if err != nil {
		return nil, fmt.Errorf("starting a shim: %w", err)
	}

	return &transport{
		in:    stdout,
		out:   stdin,
		build: build,
		// Both pipes, not just the reader: a blocked write into a shim that
		// stopped reading unsticks only when its stdin goes away.
		interrupt: func() {
			_ = stdout.Close()
			_ = stdin.Close()
		},
		close: func(ctx context.Context) error {
			// Closing stdin is the goodbye: the shim sees EOF, removes its
			// scratch, and exits. Waiting for that is what makes a finished
			// step mean a finished cleanup.
			_ = stdin.Close()

			return waitFor(ctx, command)
		},
	}, nil
}

// waitFor reaps the shim, killing it if it overstays the teardown budget.
func waitFor(ctx context.Context, command *exec.Cmd) error {
	done := make(chan error, 1)

	go func() { done <- command.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("the shim exited badly: %w", err)
		}

		return nil
	case <-ctx.Done():
		_ = command.Process.Kill()
		// Still reaped, so the process cannot outlive this one as a zombie.
		<-done

		return fmt.Errorf("the shim did not exit: %w", ctx.Err())
	}
}
