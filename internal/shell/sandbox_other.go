//go:build !darwin

package shell

import (
	"errors"
	"fmt"
	"runtime"
)

//nolint:gochecknoglobals // the one sandboxer this OS has
var hostSandbox sandboxer = unsupportedSandbox{}

var errSandboxUnsupported = errors.New("sandbox: is implemented only on darwin")

// unsupportedSandbox refuses: running the command unfenced would make sandbox: true a promise nothing keeps. A Linux implementation (bubblewrap or Landlock) replaces this file for its OS.
type unsupportedSandbox struct{}

func (unsupportedSandbox) command(sandboxPolicy, []string) ([]string, error) {
	return nil, fmt.Errorf("%w (running on %s)", errSandboxUnsupported, runtime.GOOS)
}
