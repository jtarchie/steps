package venue

import (
	"context"
	"fmt"
	"sync"

	"golang.org/x/crypto/ssh"
)

// maxSessionOutput bounds what a worker's command can make this process hold: the worker is a machine steps does not trust with its memory, as ssmdial's backlog caps say.
const maxSessionOutput = 64 << 10

// tailWriter keeps the last maxSessionOutput bytes written, the end being where every caller's marker or notice is.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, p...)
	if len(w.buf) > 2*maxSessionOutput {
		w.buf = append(w.buf[:0], w.buf[len(w.buf)-maxSessionOutput:]...)
	}

	return len(p), nil
}

func (w *tailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return string(w.buf[max(0, len(w.buf)-maxSessionOutput):])
}

// cappedOutput is session.Output (or CombinedOutput, with combined) keeping only the tail.
func cappedOutput(session *ssh.Session, command string, combined bool) (string, error) {
	out := &tailWriter{}

	session.Stdout = out
	if combined {
		session.Stderr = out
	}

	err := session.Run(command)

	return out.String(), err
}

// sessionOutput runs command in one session of client; ssh exec takes no context, so ctx ending closes the client, which is what ends the wait.
func sessionOutput(ctx context.Context, client *ssh.Client, command string, combined bool) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("opening a session: %w", err)
	}

	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	out, err := cappedOutput(session, command, combined)
	_ = session.Close()

	stop()

	if ctx.Err() != nil {
		return out, fmt.Errorf("the wait was abandoned: %w", context.Cause(ctx))
	}

	return out, err
}
