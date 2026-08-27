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
	"sync/atomic"
	"time"
)

// ListenOptions configure a listening shim: the session options every
// connection gets, plus how long the listener itself lives.
//
// The listener is unauthenticated, deliberately: it binds loopback on a
// machine whose local processes are already inside the trust boundary, the
// same standing stdio mode gives whoever can exec the binary. A multi-tenant
// worker is not a machine this contract fits.
type ListenOptions struct {
	Options
	// Once serves a single connection and then stops listening.
	//
	// It is how a shim started by a control plane cleans up after itself: an
	// aws:// venue bootstraps one shim per session and has no second channel
	// to tell it to stop, so the shim ends when its one conversation does
	// rather than lingering on somebody's instance.
	Once bool
	// Linger bounds how long to wait for the FIRST connection, and nothing
	// else. Zero waits forever.
	//
	// Once only ends a shim that somebody dialled. The aws:// bootstrap
	// starts the shim and THEN opens the forwarded session, so a failure in
	// between — SSM throttling, a websocket that will not dial — leaves a
	// root process sitting in Accept holding a port, with nothing on the
	// orchestrator still referring to it. On an instance that is never
	// stopped, every failed dial stranded another one.
	//
	// Deliberately not a bound on the session: a placed step legitimately
	// runs for hours, and a timer that could interrupt one would be a worse
	// bug than the leak it replaces.
	Linger time.Duration
}

// serveConn runs one accepted connection's session to completion.
func serveConn(ctx context.Context, conn net.Conn, opts Options) {
	defer func() { _ = conn.Close() }()

	// The session loop blocks in reads on the connection, which no context
	// can end — so a shutdown closes the connection out from under it, or a
	// shim told to stop would live for as long as its peer felt like staying
	// connected.
	sessionDone := make(chan struct{})
	defer close(sessionDone)

	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-sessionDone:
		}
	}()

	err := Serve(ctx, conn, conn, opts)
	if err != nil {
		// Stderr, the same place stdio-mode diagnostics go: stdout is
		// protocol there and merely unused here, and consistency is what lets
		// an operator find either.
		fmt.Fprintf(os.Stderr, "shim: session ended badly: %v\n", err)
	}
}

// watchLinger closes the listener if nothing dials it in time, and reports
// the two things the accept loop needs: a function to call on the first
// connection, and whether the close was this rather than a broken listener.
//
// A zero bound waits forever, which is what somebody running --listen by hand
// means; the venue passes one because its bootstrap and its dial are separate
// calls and only the first is guaranteed to happen.
func watchLinger(listener net.Listener, linger time.Duration, done <-chan struct{}) (func(), *atomic.Bool) {
	lingered := new(atomic.Bool)

	if linger <= 0 {
		return func() {}, lingered
	}

	dialled := make(chan struct{})

	go func() {
		timer := time.NewTimer(linger)
		defer timer.Stop()

		select {
		case <-timer.C:
			// Recorded before the close, so the Accept it unblocks reads as
			// this rather than as a listener that broke.
			lingered.Store(true)

			_ = listener.Close()
		case <-dialled:
		case <-done:
		}
	}()

	return sync.OnceFunc(func() { close(dialled) }), lingered
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

	first, lingered := watchLinger(listener, opts.Linger, done)

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || lingered.Load() {
				return nil
			}

			return fmt.Errorf("shim: accepting a connection: %w", err)
		}

		first()
		sessions.Add(1)

		go func() {
			defer sessions.Done()

			serveConn(ctx, conn, opts.Options)
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
