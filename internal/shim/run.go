package shim

// The shim as a process: what `steps _shim` and cmd/steps-shim both run, so
// either binary answers the same argv at every place a venue execs one.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Command is one invocation of the shim.
type Command struct {
	// Listen serves the protocol on a TCP address instead of stdio.
	Listen string
	// Once, with Listen, serves one connection and exits.
	Once bool
	// Root, with Listen, is where session scratch directories are made.
	Root string
	// Linger, with Listen, gives up if no connection arrives in this long.
	Linger time.Duration
}

// errNotShim is an argv that does not start with the one verb a venue sends.
var errNotShim = errors.New("usage: steps-shim _shim [--listen addr [--once] [--root dir] [--linger d]]")

// Run serves the shim's side of the protocol: over stdin and stdout, or on
// cmd.Listen with the bound address written to stdout.
func Run(ctx context.Context, cmd Command, stdin io.Reader, stdout io.Writer) error {
	// Resolved here rather than accepted as a flag, so a shim cannot be talked
	// into claiming to be a binary it is not.
	build, err := SelfBuild()
	if err != nil {
		return fmt.Errorf("identifying this binary: %w", err)
	}

	if cmd.Listen == "" {
		err = Serve(ctx, stdin, stdout, Options{Build: build})
		if err != nil {
			return fmt.Errorf("shim: %w", err)
		}

		return nil
	}

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cmd.Listen)
	if err != nil {
		return fmt.Errorf("shim: %w", err)
	}

	// The bound address, for whoever started this — a bootstrap script
	// grepping for the port, or a person checking it came up. Stdout is free
	// in this mode: the protocol lives on the connections.
	_, _ = fmt.Fprintf(stdout, "listening on %s\n", listener.Addr())

	err = ServeListener(ctx, listener, ListenOptions{
		Options: Options{Build: build, Root: cmd.Root},
		Once:    cmd.Once,
		Linger:  cmd.Linger,
	})
	if err != nil {
		return fmt.Errorf("shim: %w", err)
	}

	return nil
}

// Main is cmd/steps-shim: the argv `steps _shim` takes, parsed with the
// stdlib so the shim-only binary links nothing the protocol does not need.
func Main(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "_shim" {
		return errNotShim
	}

	var cmd Command

	flags := flag.NewFlagSet("steps-shim _shim", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cmd.Listen, "listen", "", "")
	flags.BoolVar(&cmd.Once, "once", false, "")
	flags.StringVar(&cmd.Root, "root", "", "")
	flags.DurationVar(&cmd.Linger, "linger", 0, "")

	err := flags.Parse(args[1:])
	if err != nil {
		return fmt.Errorf("%w: %w", errNotShim, err)
	}

	if flags.NArg() > 0 {
		return fmt.Errorf("%w: unexpected %q", errNotShim, flags.Arg(0))
	}

	ctx := context.Background()

	// Only a listener is told to stop by signal; in stdio mode the goodbye is
	// stdin closing, exactly as under `steps _shim`.
	if cmd.Listen != "" {
		var stop context.CancelFunc

		ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
	}

	return Run(ctx, cmd, stdin, stdout)
}
