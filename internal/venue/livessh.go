package venue

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

// liveSSH is a session's ssh connection, dialled again when it drops: what a session made on the worker (a directory, volumes, a container) outlives the connection that made it.
type liveSSH struct {
	// mu guards client and ended alone: a daemon dialer reads them while its session holds its own lock.
	mu     sync.Mutex
	client *ssh.Client
	ended  <-chan struct{}
	// suspect is a command that failed without an exit, so the next one checks the connection first.
	suspect atomic.Bool
}

func (l *liveSSH) current() *ssh.Client {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.client
}

// adopt makes client the connection, and notes when it ends so the next command can dial again.
func (l *liveSSH) adopt(client *ssh.Client) {
	ended := make(chan struct{})

	go func() {
		_ = client.Wait()

		close(ended)
	}()

	keepAlive(client)

	l.mu.Lock()
	l.client, l.ended = client, ended
	l.mu.Unlock()
}

// redial dials again when the connection ended, reporting whether it did; a connection never adopted is left to the session's own first dial.
func (l *liveSSH) redial(ctx context.Context, dial func(context.Context) (*ssh.Client, error)) (bool, error) {
	l.mu.Lock()
	ended := l.ended
	l.mu.Unlock()

	if ended == nil || !l.gone(ended) {
		return false, nil
	}

	client, err := dial(ctx)
	if err != nil {
		return false, err
	}

	l.adopt(client)

	return true, nil
}

// gone is whether the connection ended. A suspect one is asked once: the failed command and the connection reporting its end come from one drop, and the retry can arrive between them.
func (l *liveSSH) gone(ended <-chan struct{}) bool {
	select {
	case <-ended:
		return true
	default:
	}

	if !l.suspect.Swap(false) {
		return false
	}

	answered := make(chan error, 1)

	go func() {
		_, _, err := l.current().SendRequest("keepalive@openssh.com", true, nil)
		answered <- err
	}()

	select {
	case err := <-answered:
		return err != nil
	case <-ended:
		return true
	case <-time.After(keepaliveTimeout):
		return true
	}
}
