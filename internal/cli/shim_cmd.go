package cli

import (
	"context"
	"os"
	"time"

	"github.com/jtarchie/steps/internal/shim"
)

// ShimCmd is the remote half of a step placed on a worker.
//
// Nobody types it. The orchestrator pushes this binary over a venue, execs it
// as `steps _shim`, and this process's stdin and stdout ARE the protocol —
// which is why nothing on this path may write to stdout. Logging already goes
// to stderr (see InitLogging), and a stray fmt.Print here would not look like
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
	// Linger reaps a shim nobody dialled. The venue passes it on the
	// bootstrap; a shim started by hand waits forever, which is what somebody
	// at a terminal means by --listen.
	Linger time.Duration `help:"with --listen, give up if no connection arrives in this long (0 waits forever)" name:"linger"`
}

// Run serves one session as the pushed-to worker's half of the wire.
func (s *ShimCmd) Run() error {
	cmd := shim.Command{Listen: s.Listen, Once: s.Once, Root: s.Root, Linger: s.Linger}

	if s.Listen == "" {
		return shim.Run(context.Background(), cmd, os.Stdin, os.Stdout) //nolint:wrapcheck // shim.Run already names itself
	}

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	return shim.Run(ctx, cmd, os.Stdin, os.Stdout) //nolint:wrapcheck // as above
}
