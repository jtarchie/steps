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

// ServeListener serves one session per accepted connection until ctx ends.
//
// Connections are served concurrently, because two steps placed on one worker
// are two sessions — each names its own scratch (see Hello.Session), so they
// coexist the same way two SSH-execed shims already do. A session that fails
// is reported on stderr and costs nobody else's connection; a session is
// still running when ctx ends is waited for, so cleanup cannot race teardown.
func ServeListener(ctx context.Context, listener net.Listener, opts Options) error {
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

			serveErr := Serve(ctx, conn, conn, opts)
			if serveErr != nil {
				// Stderr, the same place stdio-mode diagnostics go: stdout is
				// protocol there and merely unused here, and consistency is
				// what lets an operator find either.
				fmt.Fprintf(os.Stderr, "shim: session ended badly: %v\n", serveErr)
			}
		}()
	}
}
