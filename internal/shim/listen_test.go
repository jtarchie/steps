package shim

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/wire"
)

// dialPeer is the frame-speaking half of one TCP connection to a listening
// shim, reusing nothing from the stdio peer on purpose: what these tests pin
// is that a connection IS a session, with no shared state between two.
type dialPeer struct {
	t       *testing.T
	conn    net.Conn
	encoder *wire.Encoder
	decoder *wire.Decoder
	op      uint32
}

func dialListener(t *testing.T, addr net.Addr) *dialPeer {
	t.Helper()

	conn, err := (&net.Dialer{}).DialContext(context.Background(), addr.Network(), addr.String())
	if err != nil {
		t.Fatalf("dialling the listening shim: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return &dialPeer{t: t, conn: conn, encoder: wire.NewEncoder(conn), decoder: wire.NewDecoder(conn)}
}

// hello opens the session and requires a healthy answer.
func (p *dialPeer) hello(session string) wire.HelloOK {
	p.t.Helper()

	p.op++

	err := p.encoder.WriteJSON(wire.FrameHello, p.op, wire.Hello{
		Protocol: wire.Protocol, Build: "test", Session: session,
	})
	if err != nil {
		p.t.Fatalf("sending the hello: %v", err)
	}

	frame, err := p.decoder.Read()
	if err != nil {
		p.t.Fatalf("reading the hello answer: %v", err)
	}

	if frame.Type == wire.FrameError {
		var wireErr wire.Error
		_ = wire.DecodeJSON(frame, &wireErr)
		p.t.Fatalf("the shim reported an error: %s", wireErr.Message)
	}

	var ok wire.HelloOK

	err = wire.DecodeJSON(frame, &ok)
	if err != nil {
		p.t.Fatalf("decoding the hello answer: %v", err)
	}

	return ok
}

// exec runs one command, returning its stdout.
func (p *dialPeer) exec(command string) string {
	p.t.Helper()

	p.op++

	err := p.encoder.WriteJSON(wire.FrameExec, p.op, wire.Exec{Command: command})
	if err != nil {
		p.t.Fatalf("sending the exec: %v", err)
	}

	var out []byte

	for {
		frame, err := p.decoder.Read()
		if err != nil {
			p.t.Fatalf("reading a frame: %v", err)
		}

		switch frame.Type { //nolint:exhaustive // anything else fails below
		case wire.FrameStdout:
			out = append(out, frame.Payload...)
		case wire.FrameStderr:
		case wire.FrameExit:
			return string(out)
		default:
			p.t.Fatalf("unexpected type %d frame while running a command", frame.Type)
		}
	}
}

// bye ends the session and waits for the far end to hang up.
func (p *dialPeer) bye() {
	p.t.Helper()

	p.op++
	_ = p.encoder.Write(wire.Frame{Type: wire.FrameBye, Op: p.op})
	_, _ = io.ReadAll(p.conn)
}

// serveTCP starts a listening shim on loopback and returns its address. The
// listener stops when the test ends.
func serveTCP(t *testing.T) net.Addr {
	t.Helper()

	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	served := make(chan error, 1)

	go func() {
		served <- ServeListener(ctx, listener, ListenOptions{Options: Options{Build: "test", Root: t.TempDir()}})
	}()

	t.Cleanup(func() {
		cancel()

		err := <-served
		if err != nil {
			t.Errorf("ServeListener: %v", err)
		}
	})

	return listener.Addr()
}

// TestShimListensOnTCP is the mode the SSM dialer forwards to: the framed
// protocol on an accepted connection instead of stdio, one session per
// connection, nothing else different.
func TestShimListensOnTCP(t *testing.T) {
	t.Parallel()

	addr := serveTCP(t)

	peer := dialListener(t, addr)

	ok := peer.hello("tcp-session-under-test")
	if ok.Protocol != wire.Protocol || ok.Workdir == "" {
		t.Fatalf("hello answer = %+v, want a healthy session", ok)
	}

	if got := peer.exec("echo over-tcp"); got != "over-tcp\n" {
		t.Errorf("stdout = %q, want %q", got, "over-tcp\n")
	}

	peer.bye()
}

// TestShimServesConcurrentConnections pins that two placed steps can share a
// worker: each connection is its own session with its own scratch, and
// neither can see or delete the other's.
func TestShimServesConcurrentConnections(t *testing.T) {
	t.Parallel()

	addr := serveTCP(t)

	first := dialListener(t, addr)
	second := dialListener(t, addr)

	okFirst := first.hello("tcp-concurrent-a")
	okSecond := second.hello("tcp-concurrent-b")

	if okFirst.Workdir == okSecond.Workdir {
		t.Fatalf("two sessions share a work directory %q", okFirst.Workdir)
	}

	var wg sync.WaitGroup

	wg.Add(2)

	go func() { defer wg.Done(); first.exec("echo a") }()
	go func() { defer wg.Done(); second.exec("echo b") }()

	wg.Wait()

	first.bye()
	second.bye()
}

// TestShimListenerShutsDownWithAClientConnected pins that a shim told to stop
// actually stops. The session loop blocks in reads on the connection, which
// no context can end — without closing the connection out from under it, a
// SIGTERM'd shim lived for as long as its peer felt like staying connected,
// half-cancelled, killing every command it accepted.
func TestShimListenerShutsDownWithAClientConnected(t *testing.T) {
	t.Parallel()

	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	served := make(chan error, 1)

	go func() {
		served <- ServeListener(ctx, listener, ListenOptions{Options: Options{Build: "test", Root: t.TempDir()}})
	}()

	peer := dialListener(t, listener.Addr())
	peer.hello("shutdown-under-test")

	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("ServeListener: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeListener did not return: the connected session outlived its shutdown")
	}
}
