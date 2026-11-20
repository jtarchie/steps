package shell

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	llsys "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

//nolint:gochecknoglobals // the one sandboxer this OS has
var hostSandbox sandboxer = landlockBox{}

// SandboxHelper is the hidden steps subcommand a linux fence re-executes steps as: Landlock restricts the process that asks, so steps cannot fence a child without becoming that child first.
const SandboxHelper = "__sandbox"

// landlockABI is the oldest ABI that restricts every way to change a file: ABI 1 and 2 leave truncate(2) open on any file the operator owns.
const landlockABI = 3

var errLandlockNetwork = errors.New("sandbox: network: none is not available on linux, where Landlock can cut TCP but not UDP or unix sockets; drop network: none, or run it in an image")

// landlockBox fences a command with Landlock, through steps itself as the helper (SandboxExec).
type landlockBox struct{}

func (landlockBox) command(policy sandboxPolicy, argv []string) ([]string, error) {
	if !policy.network {
		return nil, errLandlockNetwork
	}

	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("sandbox: cannot name the steps binary to re-execute: %w", err)
	}

	args := []string{self, SandboxHelper}

	for _, dir := range policy.writable {
		args = append(args, "--write="+dir)
	}

	for _, path := range policy.denyRead {
		args = append(args, "--hide="+path)
	}

	args = append(args, "--")

	return append(args, argv...), nil
}

// SandboxAvailable says why this machine cannot keep sandbox: true, or nil when it can.
func SandboxAvailable() error {
	abi, err := llsys.LandlockGetABIVersion()
	if err != nil {
		return fmt.Errorf("sandbox: Landlock is not enabled on this kernel (needs Linux 6.2+ with landlock in the lsm= list): %w", err)
	}

	if abi < landlockABI {
		return fmt.Errorf("sandbox: Landlock ABI %d is too old; the fence needs ABI %d (Linux 6.2+)", abi, landlockABI)
	}

	return nil
}

// SandboxExec restricts this process to the fence and replaces it with argv. Strict, never BestEffort: a kernel short of the ABI refuses here rather than running argv less fenced than the step asked.
func SandboxExec(writable, hidden, argv []string) error {
	if len(argv) == 0 {
		return errors.New("sandbox: no command to run")
	}

	err := SandboxAvailable()
	if err != nil {
		return err
	}

	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}

	grants, err := landlockGrants(writable, hidden)
	if err != nil {
		return err
	}

	rules := make([]landlock.Rule, 0, len(grants))

	for _, grant := range grants {
		rules = append(rules, landlockRule(grant))
	}

	err = landlock.V3.RestrictPaths(rules...)
	if err != nil {
		return fmt.Errorf("sandbox: apply Landlock: %w", err)
	}

	err = syscall.Exec(path, argv, os.Environ()) //nolint:gosec // argv is the step's own command, which this helper exists to run fenced
	if err != nil {
		return fmt.Errorf("sandbox: exec %s: %w", path, err)
	}

	return nil
}

func landlockRule(grant landlockGrant) landlock.Rule {
	switch grant.access {
	case listDir:
		return landlock.PathAccess(llsys.AccessFSReadDir, grant.path)
	case readTree:
		return landlock.RODirs(grant.path).IgnoreIfMissing()
	case readFile:
		return landlock.ROFiles(grant.path).IgnoreIfMissing()
	case writeTree:
		return landlock.RWDirs(grant.path).WithRefer().IgnoreIfMissing()
	case writeFile:
		return landlock.RWFiles(grant.path).IgnoreIfMissing()
	}

	return landlock.PathAccess(0, grant.path)
}
