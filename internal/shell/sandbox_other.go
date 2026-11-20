//go:build !darwin && !linux

package shell

import (
	"errors"
	"fmt"
	"runtime"
)

//nolint:gochecknoglobals // the one sandboxer this OS has
var hostSandbox sandboxer = unsupportedSandbox{}

var errSandboxUnsupported = errors.New("sandbox: is implemented only on darwin and linux")

// unsupportedSandbox refuses: running the command unfenced would make sandbox: true a promise nothing keeps.
type unsupportedSandbox struct{}

func (unsupportedSandbox) command(sandboxPolicy, []string) ([]string, error) {
	return nil, SandboxAvailable()
}

// SandboxAvailable says why this machine cannot keep sandbox: true.
func SandboxAvailable() error {
	return fmt.Errorf("%w (running on %s)", errSandboxUnsupported, runtime.GOOS)
}

// SandboxExec refuses, as the fence does here.
func SandboxExec(_, _, _ []string) error { return SandboxAvailable() }
