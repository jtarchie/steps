package main

import (
	"context"
	"fmt"
	"net"
	"os"

	"github.com/jtarchie/steps/internal/shim"
)

// ShimCmd is the remote half of a step placed on a worker.
//
// Nobody types it. The orchestrator pushes this binary over a venue, execs it
// as `steps _shim`, and this process's stdin and stdout ARE the protocol —
// which is why nothing on this path may write to stdout. Logging already goes
// to stderr (see initLogging), and a stray fmt.Print here would not look like
// a bug, it would look like a corrupt frame on a machine nobody can attach a
// debugger to.
//
// Hidden and underscore-named for two different reasons. Hidden keeps it out
// of --help, out of shell completion, and out of kong's "did you mean"
// suggestions, so it is not a command a reader can find and misuse. The
// underscore is for whoever reads a ps line on a worker: this is machinery,
// not a verb.
//
// It needs no special handling around RunCmd's default:"withargs" — kong
// matches a registered command name, hidden or not, before falling back to the
// default command.
//
// --listen is the one flag: the same protocol on a local TCP listener, for a
// venue that reaches the worker through a forwarded port (SSM) rather than an
// exec channel. It changes where sessions arrive and nothing about what they
// mean; identity stays self-computed either way.
type ShimCmd struct {
	Listen string `help:"serve the shim protocol on a TCP address instead of stdio, e.g. --listen 127.0.0.1:35207" name:"listen"`
	Once   bool   `help:"with --listen, serve one connection and exit"                                             name:"once"`
	Root   string `help:"with --listen, where session scratch directories are made"                                name:"root"`
}

func (s *ShimCmd) Run() error {
	// Build is the content hash of this binary, which the orchestrator uses to
	// prove the shim it reached is the one it pushed. Resolving it here rather
	// than accepting it as a flag means a shim cannot be talked into claiming
	// to be a binary it is not.
	build, err := shim.SelfBuild()
	if err != nil {
		return fmt.Errorf("identifying this binary: %w", err)
	}

	if s.Listen != "" {
		return s.listen(build)
	}

	err = shim.Serve(context.Background(), os.Stdin, os.Stdout, shim.Options{Build: build})
	if err != nil {
		return fmt.Errorf("shim: %w", err)
	}

	return nil
}

// listen serves sessions on a TCP address until the process is told to stop.
func (s *ShimCmd) listen(build string) error {
	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.Listen)
	if err != nil {
		return fmt.Errorf("shim: %w", err)
	}

	// The bound address, for whoever started this — a bootstrap script
	// grepping for the port, or a person checking it came up. Stdout is free
	// in this mode: the protocol lives on the connections.
	fmt.Printf("listening on %s\n", listener.Addr())

	err = shim.ServeListener(ctx, listener, shim.ListenOptions{
		Options: shim.Options{Build: build, Root: s.Root},
		Once:    s.Once,
	})
	if err != nil {
		return fmt.Errorf("shim: %w", err)
	}

	return nil
}
