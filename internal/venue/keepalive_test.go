package venue

import (
	"context"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/jtarchie/steps/internal/shell"
)

// silentProxy forwards to target, holding every byte while frozen: frozen for good is a tunnel whose machine is gone, frozen for a moment is a busy one.
func silentProxy(t *testing.T, target string) (string, *atomic.Bool) {
	t.Helper()

	var (
		frozen       atomic.Bool
		listenConfig net.ListenConfig
		open         sync.WaitGroup
	)

	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	// Held, not dropped, while frozen: a busy tunnel delivers late, it does not lose bytes.
	pipe := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)

		for {
			n, readErr := src.Read(buf)
			for frozen.Load() {
				time.Sleep(10 * time.Millisecond)
			}

			if n > 0 {
				_, _ = dst.Write(buf[:n])
			}

			if readErr != nil {
				_ = dst.Close()

				return
			}
		}
	}

	go func() {
		for {
			client, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}

			server, dialErr := (&net.Dialer{}).DialContext(context.Background(), "tcp", target)
			if dialErr != nil {
				_ = client.Close()

				continue
			}

			open.Add(2)

			go func() { defer open.Done(); pipe(server, client) }()
			go func() { defer open.Done(); pipe(client, server) }()
		}
	}()

	t.Cleanup(func() {
		frozen.Store(false)
		_ = listener.Close()
		open.Wait()
	})

	return listener.Addr().String(), &frozen
}

func shortKeepalive(t *testing.T) {
	t.Helper()

	previousInterval, previousTimeout := keepaliveInterval, keepaliveTimeout
	keepaliveInterval, keepaliveTimeout = 200*time.Millisecond, 200*time.Millisecond

	t.Cleanup(func() { keepaliveInterval, keepaliveTimeout = previousInterval, previousTimeout })
}

func proxiedSSHRunner(t *testing.T) (shell.Runner, *atomic.Bool) {
	t.Helper()

	server := newTestSSHD(t)
	address, frozen := silentProxy(t, server.Addr())

	worker := "ssh://" + address + server.Root + "?" + url.Values{
		"identity":   {server.Identity},
		"hostkey":    {ssh.FingerprintSHA256(server.HostKey)},
		"ssh_config": {"none"},
	}.Encode()

	runner := newLocalRunner(t, shell.RunnerSpec{Cwd: t.TempDir(), Worker: worker})

	err := runner.Run(context.Background(), "true")
	if err != nil {
		t.Fatal(err)
	}

	return runner, frozen
}

// A worker whose tunnel goes silent mid-command must fail the command as infrastructure within the keepalive window, not wait for a reply that is never coming: the real spot test sat forty minutes on exactly this.
func TestAWorkerThatGoesSilentFailsTheCommand(t *testing.T) {
	shortKeepalive(t)

	runner, frozen := proxiedSSHRunner(t)

	go func() {
		time.Sleep(500 * time.Millisecond)
		frozen.Store(true)
	}()

	started := time.Now()
	err := runner.Run(context.Background(), "sleep 5")

	if err == nil || shell.IsExitError(err) {
		t.Fatalf("Run over a silent tunnel: %v, want an infrastructure failure", err)
	}

	// Well inside the command's own five seconds, so it was the keepalive that ended it.
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("took %s to notice the tunnel was gone", elapsed)
	}
}

// One late answer is a busy tunnel, not a dead one: a pause shorter than the misses allowed must not end a healthy session mid-transfer.
func TestAWorkerThatPausesKeepsItsSession(t *testing.T) {
	shortKeepalive(t)

	runner, frozen := proxiedSSHRunner(t)

	go func() {
		time.Sleep(300 * time.Millisecond)
		frozen.Store(true)
		time.Sleep(350 * time.Millisecond)
		frozen.Store(false)
	}()

	err := runner.Run(context.Background(), "sleep 2")
	if err != nil {
		t.Fatalf("Run across a brief pause: %v", err)
	}
}
