package main

import (
	"context"
	"fmt"
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
type ShimCmd struct{}

func (s *ShimCmd) Run() error {
	// Build is the content hash of this binary, which the orchestrator uses to
	// prove the shim it reached is the one it pushed. Resolving it here rather
	// than accepting it as a flag means a shim cannot be talked into claiming
	// to be a binary it is not.
	build, err := shim.SelfBuild()
	if err != nil {
		return fmt.Errorf("identifying this binary: %w", err)
	}

	err = shim.Serve(context.Background(), os.Stdin, os.Stdout, shim.Options{Build: build})
	if err != nil {
		return fmt.Errorf("shim: %w", err)
	}

	return nil
}
