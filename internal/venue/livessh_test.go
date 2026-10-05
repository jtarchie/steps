package venue

import (
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/jtarchie/steps/internal/testsshd"
)

// The retry after a dropped connection can arrive before the connection reports its end; a suspect one is asked, and a dead one answers by failing.
func TestLiveSSHAsksASuspectConnectionWhetherItIsAlive(t *testing.T) {
	t.Parallel()

	live := testsshd.New(t).Dial(t)
	dead := testsshd.New(t).Dial(t)
	_ = dead.Close()

	notYetEnded := make(chan struct{})

	for name, c := range map[string]struct {
		client         *ssh.Client
		suspect, ended bool
	}{
		"dead, suspect":     {client: dead, suspect: true, ended: true},
		"dead, not suspect": {client: dead},
		"live, suspect":     {client: live, suspect: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			l := &liveSSH{client: c.client}
			l.suspect.Store(c.suspect)

			if got := l.gone(notYetEnded); got != c.ended {
				t.Errorf("gone = %v, want %v", got, c.ended)
			}
		})
	}
}

// A connection replaced is closed: one judged gone may only be slow, and nothing else would ever close it or the drain loop it carries.
func TestLiveSSHClosesTheConnectionItReplaces(t *testing.T) {
	t.Parallel()

	server := testsshd.New(t)
	first := server.Dial(t)

	var l liveSSH

	l.adopt(first)
	l.adopt(server.Dial(t))

	t.Cleanup(func() { _ = l.current().Close() })

	ended := make(chan struct{})

	go func() {
		_ = first.Wait()

		close(ended)
	}()

	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Error("the replaced connection is still open")
	}
}

// dropAndWait closes the connection and waits until it reports its end, as a keepalive that gave up between commands leaves it.
func dropAndWait(t *testing.T, l *liveSSH) {
	t.Helper()

	l.mu.Lock()
	client, ended := l.client, l.ended
	l.mu.Unlock()

	_ = client.Close()

	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the closed connection never reported its end")
	}
}

// A paused tunnel is not a dead one: a probe that waited out one keepalive timeout proves only slowness, and redialling then would close a connection still carrying a transfer. Death is keepalive's call, after its misses.
func TestLiveSSHDoesNotCallAPausedConnectionDead(t *testing.T) {
	runner, frozen := proxiedSSHRunner(t)

	previous := keepaliveTimeout
	keepaliveTimeout = 200 * time.Millisecond

	t.Cleanup(func() { keepaliveTimeout = previous })

	bare, ok := runner.(bareRunner)
	if !ok {
		t.Fatalf("runner is %T, want an ssh:// runner", runner)
	}

	l := &bare.s.conn

	l.mu.Lock()
	ended := l.ended
	l.mu.Unlock()

	frozen.Store(true)
	l.suspect.Store(true)

	if l.gone(ended) {
		t.Error("gone = true for a tunnel that only paused; the redial would close it under whatever it carries")
	}

	frozen.Store(false)
}
