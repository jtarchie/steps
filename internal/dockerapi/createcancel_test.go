package dockerapi

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// cancelAfterCreate is a connection that, once a create has gone out, waits for the daemon's answer (so the container exists) and only then cancels the caller, holding the answer back long enough for a client riding that context to give up on it.
type cancelAfterCreate struct {
	net.Conn

	cancel context.CancelFunc
	mu     sync.Mutex
	armed  bool
}

func (c *cancelAfterCreate) Write(p []byte) (int, error) {
	c.mu.Lock()
	if bytes.Contains(p, []byte("/containers/create")) {
		c.armed = true
	}
	c.mu.Unlock()

	return c.Conn.Write(p) //nolint:wrapcheck // a pass-through
}

func (c *cancelAfterCreate) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)

	c.mu.Lock()
	armed := c.armed
	c.armed = false
	c.mu.Unlock()

	if armed {
		c.cancel()
		time.Sleep(200 * time.Millisecond)
	}

	return n, err //nolint:wrapcheck // a pass-through
}

// A create the daemon carried out after its caller gave up used to leave the container behind with nobody holding its id: measured as a cancelled step's container, and its holder, sitting Created for as long as the process lived.
func TestACancelledCreateLeavesNoContainer(t *testing.T) {
	requireDaemon(t)

	host, err := ResolveHost()
	if err != nil {
		t.Skip(err)
	}

	network, address, ok := strings.Cut(host, "://")
	if !ok || network != "unix" {
		t.Skipf("docker host %q is not a unix socket", host)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	client, err := NewDialer(host, func(dialCtx context.Context) (net.Conn, error) {
		conn, dialErr := (&net.Dialer{}).DialContext(dialCtx, network, address)
		if dialErr != nil {
			return nil, dialErr //nolint:wrapcheck // a test dialer
		}

		return &cancelAfterCreate{Conn: conn, cancel: cancel}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = client.Close() }()

	name := "steps-test-" + strings.ReplaceAll(t.Name(), "/", "-")
	removeFixture(name)
	t.Cleanup(func() { removeFixture(name) })

	_, err = client.CreateHolder(ctx, name, testImage, map[string]string{"steps.test.owner": "createcancel"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateHolder = %v, want the caller's cancellation back", err)
	}

	//nolint:gosec // a name this test generated
	if exec.CommandContext(context.Background(), "docker", "inspect", "--type", "container", name).Run() == nil {
		t.Error("the container the daemon made after its caller left is still there, and nothing holds its id")
	}
}
