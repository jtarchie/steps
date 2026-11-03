package cli

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jtarchie/steps/internal/shell"
)

// withSignalCancel derives a context from parent that is canceled on
// SIGINT/SIGTERM, and returns it along with its cancel func. A second signal
// is the operator saying "now": see exitNow.
func withSignalCancel(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	stopped := make(chan struct{})

	go watchSignals(ctx, sigs, stopped, cancel, exitNow)

	// Once, since callers defer this and may also call it early themselves.
	return ctx, sync.OnceFunc(func() {
		signal.Stop(sigs)
		close(stopped)
		cancel()
	})
}

// watchSignals cancels on the first signal and hands the second to now. The
// shutdown the first one starts can wait a long time (an abort hook's grace, a
// non-interruptible job's), and a second ^C swallowed meanwhile left SIGKILL
// as the only way out, which orphans every child.
func watchSignals(ctx context.Context, sigs <-chan os.Signal, stopped <-chan struct{}, cancel func(), now func(os.Signal)) {
	select {
	case sig := <-sigs:
		slog.WarnContext(ctx, "signal.received", "signal", sig.String())
		cancel()
	case <-stopped:
		return
	}

	select {
	case sig := <-sigs:
		slog.WarnContext(ctx, "signal.received_again", "signal", sig.String(), "action", "killing every child and exiting now")
		now(sig)
	case <-stopped:
	}
}

// exitNow kills every child session and dies of sig, so a supervisor reads the
// exit as the signal it sent rather than as an error code this process chose.
func exitNow(sig os.Signal) {
	shell.KillAll()
	signal.Reset(sig)

	self, err := os.FindProcess(os.Getpid())
	if err == nil {
		_ = self.Signal(sig)
	}

	// Unreachable where sig's default action is to terminate; a backstop elsewhere.
	time.Sleep(time.Second)
	os.Exit(1)
}
