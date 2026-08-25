package shim

// The listening shim: the same framed protocol, served on accepted TCP
// connections instead of stdio. This is what an SSM port-forwarding tunnel
// terminates at — a venue that cannot exec a process on the worker dials a
// port instead, and nothing above the accept changes.

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
)

// ListenOptions configure a listening shim: the session options every
// connection gets, plus how long the listener itself lives.
type ListenOptions struct {
	Options
	// Once serves a single connection and then stops listening.
	//
	// It is how a shim started by a control plane cleans up after itself: an
	// aws:// venue bootstraps one shim per session and has no second channel
	// to tell it to stop, so the shim ends when its one conversation does
	// rather than lingering on somebody's instance.
	Once bool
}

// ServeListener serves one session per accepted connection until ctx ends, or
// until the first connection finishes when Once is set.
//
// Connections are served concurrently, because two steps placed on one worker
// are two sessions — each names its own scratch (see Hello.Session), so they
// coexist the same way two SSH-execed shims already do. A session that fails
// is reported on stderr and costs nobody else's connection; a session still
// running when ctx ends is waited for, so cleanup cannot race teardown.
func ServeListener(ctx context.Context, listener net.Listener, opts ListenOptions) error {
	// Closing the listener is what unblocks Accept; done keeps the closer
	// from outliving a return caused by an Accept error rather than ctx.
	done := make(chan struct{})
	defer close(done)

	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}

		_ = listener.Close()
	}()

	var sessions sync.WaitGroup
	defer sessions.Wait()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			return fmt.Errorf("shim: accepting a connection: %w", err)
		}

		sessions.Add(1)

		go func() {
			defer sessions.Done()
			defer func() { _ = conn.Close() }()

			serveErr := Serve(ctx, conn, conn, opts.Options)
			if serveErr != nil {
				// Stderr, the same place stdio-mode diagnostics go: stdout is
				// protocol there and merely unused here, and consistency is
				// what lets an operator find either.
				fmt.Fprintf(os.Stderr, "shim: session ended badly: %v\n", serveErr)
			}
		}()

		if opts.Once {
			// Waited for, not abandoned: the session is still running, and
			// returning here would close the listener and run the caller's
			// cleanup out from under it.
			sessions.Wait()

			return nil
		}
	}
}
