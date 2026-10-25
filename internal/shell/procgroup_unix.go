//go:build unix

package shell

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	"github.com/jtarchie/steps/internal/events"
)

// live is what KillAll reaches: sessions Start made and the caller has not released.
var live = struct { //nolint:gochecknoglobals // the process's own children are process-wide
	sync.Mutex
	pids map[int]struct{}
}{pids: map[int]struct{}{}}

// Start isolates a headless child in its own session, so a /dev/tty open fails instead of SIGTTIN-stopping the daemon, and a cancel reaps grandchildren; interactive children stay foreground so their prompts work.
func Start(ctx context.Context, cmd *exec.Cmd) (release func(), err error) {
	if !events.Headless(ctx) {
		return func() {}, cmd.Start() //nolint:wrapcheck // the caller names the command
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return killGroup(cmd.Process.Pid) }

	// Held across Start, so a KillAll racing it cannot miss a session that is about to exist.
	live.Lock()
	defer live.Unlock()

	err = cmd.Start()
	if err != nil {
		return func() {}, err //nolint:wrapcheck // the caller names the command
	}

	pid := cmd.Process.Pid
	live.pids[pid] = struct{}{}

	return func() {
		live.Lock()
		delete(live.pids, pid)
		live.Unlock()
	}, nil
}

// KillAll SIGKILLs every session Start has running: the second ^C, which must not wait out a step's grace.
func KillAll() {
	live.Lock()
	defer live.Unlock()

	for pid := range live.pids {
		_ = killGroup(pid)
	}
}

// IgnoreTerminalStops makes a tty read or write from a background group fail with EIO instead of stopping this process, and children inherit it across exec.
func IgnoreTerminalStops() {
	signal.Ignore(syscall.SIGTTIN, syscall.SIGTTOU)
}

// killGroup kills a session leader's group; ESRCH means it is already gone, which exec.Cmd wants spelled os.ErrProcessDone.
func killGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}

	return err //nolint:wrapcheck // exec.Cmd reports it as the cancel's error
}
