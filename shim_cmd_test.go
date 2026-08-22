package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alecthomas/kong"
)

// TestShimCommandIsHiddenButReachable pins both halves of how `steps _shim` is
// wired, neither of which is observable from --help (which exits the process).
//
// Reachable matters because RunCmd is the default command with withargs: if
// kong ever stopped matching a registered name first, `steps _shim` would be
// parsed as `steps run _shim` and a worker would try to load a pipeline called
// _shim. Hidden matters because this is machinery, not a verb somebody should
// find by reading the help and try out.
func TestShimCommandIsHiddenButReachable(t *testing.T) {
	t.Parallel()

	var cli CLI

	parser, err := kong.New(&cli, kong.Name("steps"), kong.Vars{"version": buildVersion})
	if err != nil {
		t.Fatalf("building the CLI model: %v", err)
	}

	var found *kong.Node

	for _, node := range parser.Model.Children {
		if node.Name == "_shim" {
			found = node

			break
		}
	}

	if found == nil {
		t.Fatal("no _shim command is registered; a pushed binary would have nothing to run")
	}

	if !found.Hidden {
		t.Error("_shim is not hidden; it would show up in --help and completion as though it were a verb")
	}
}

// TestShimWritesNothingButFramesToStdout is the one rule this command cannot
// break. The process's stdout IS the protocol, so a stray print — a debug
// line, a banner, a warning that logging could not be configured — does not
// look like a bug at the far end. It looks like a corrupt frame, on the
// machine that is hardest to attach a debugger to.
//
// An empty stdin is an orchestrator that hung up before saying anything, which
// the shim treats as an ordinary end of session.
func TestShimWritesNothingButFramesToStdout(t *testing.T) {
	// Not parallel: it replaces the process's stdin and stdout.
	captured := filepath.Join(t.TempDir(), "stdout")

	out, err := os.Create(captured) //nolint:gosec // a path inside this test's own temp dir
	if err != nil {
		t.Fatalf("creating the capture file: %v", err)
	}

	empty, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("opening %s: %v", os.DevNull, err)
	}

	realIn, realOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = empty, out

	t.Cleanup(func() {
		os.Stdin, os.Stdout = realIn, realOut
		_ = empty.Close()
		_ = out.Close()
	})

	err = run([]string{"_shim"})
	if err != nil {
		t.Fatalf("steps _shim: %v", err)
	}

	_ = out.Close()

	written, err := os.ReadFile(captured) //nolint:gosec // a path this test just built
	if err != nil {
		t.Fatalf("reading what the shim wrote: %v", err)
	}

	if len(written) != 0 {
		t.Errorf("the shim wrote %d bytes to stdout before any frame was asked of it: %q", len(written), written)
	}
}
