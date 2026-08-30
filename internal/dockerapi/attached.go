package dockerapi

// A container this process drives in the foreground, reading its output as it
// arrives.
//
// Separate from the session container, which is a long-lived box that commands
// are exec'd INTO. This is the other shape: the container's lifetime IS the
// process's, its stdout is a stream the caller parses as it streams rather
// than a result collected at the end, and the caller owns the decision to stop
// waiting. internal/agent's containerized CLI is the case — a long-running
// child whose transcript is read turn by turn, so that a step which times out
// mid-conversation still has what it managed to do.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Attached is a running container whose streams this process holds.
type Attached struct {
	// Stdout is the container's standard output, readable as it arrives. It
	// ends when the container's output does, so a caller reading to EOF stops
	// on its own rather than waiting out a timeout.
	//
	// Wait closes it, so every read must happen before Wait is called — the
	// same contract exec.Cmd's StdoutPipe has, and for the same reason.
	Stdout io.Reader

	client *Client
	id     string
	// stdout is Stdout's own end, kept so Wait can close it.
	stdout *io.PipeReader
	hijack client.HijackedResponse
	// pumped is closed once the demultiplexer has finished, so Wait can
	// promise that everything the container said has been delivered before it
	// reports the status.
	pumped chan struct{}
	closer sync.Once
}

// StartAttached creates, attaches to, and starts a container in one step.
//
// Attached BEFORE started, which is the whole reason this is one call: a
// container that is started first can produce — and finish producing — output
// before anything is listening, and the first thing the CLI this was written
// for emits is the line that says the session began.
func (c *Client) StartAttached(
	ctx context.Context, spec ContainerSpec, stdin io.Reader, stderr io.Writer,
) (*Attached, error) {
	id, err := c.CreateContainer(ctx, spec)
	if err != nil {
		return nil, err
	}

	hijack, err := c.api.ContainerAttach(ctx, id, client.ContainerAttachOptions{
		Stream: true,
		Stdin:  stdin != nil,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		reclaimAttached(ctx, c, id)

		return nil, fmt.Errorf("attaching to container %s: %w", id, err)
	}

	err = c.StartContainer(ctx, id)
	if err != nil {
		hijack.Close()
		reclaimAttached(ctx, c, id)

		return nil, err
	}

	reader, writer := io.Pipe()

	attached := &Attached{
		Stdout: reader, stdout: reader,
		client: c, id: id, hijack: hijack.HijackedResponse, pumped: make(chan struct{}),
	}

	if stdin != nil {
		go func() {
			_, _ = io.Copy(attached.hijack.Conn, stdin)
			_ = attached.hijack.CloseWrite()
		}()
	}

	go func() {
		defer close(attached.pumped)

		// Demultiplexed: both streams share one connection with a frame
		// header, so a plain copy would splice the container's log lines into
		// the transcript a parser is reading.
		_, copyErr := stdcopy.StdCopy(writer, stderr, attached.hijack.Reader)
		_ = writer.CloseWithError(copyErr)
	}()

	return attached, nil
}

// reclaimAttached takes away a container that was created but never got to
// run, on a context that a cancelled caller cannot abort.
// Unconditionally, including for a self-removing one. AutoRemove is honoured
// when a STARTED container exits; a container still in Created because the
// attach or the start failed never exits, so the daemon never reaps it. The
// ownership labels mean a later run's sweep would eventually find it, but that
// is a later PROCESS — this one's pid is still alive, so the sweep skips it —
// and it would sit on the daemon for the rest of the run.
func reclaimAttached(ctx context.Context, c *Client, id string) {
	_ = c.RemoveContainer(context.WithoutCancel(ctx), id)
}

// Wait blocks until the container exits and returns its status.
//
// The status is DATA, like every other exit in this package: a CLI that
// reports a task failure exits nonzero while having spoken perfectly well for
// itself, and treating the status as the verdict would call every such run an
// infrastructure error.
//
// Stdout is CLOSED here, before the demultiplexer is waited on, and that
// ordering is the whole of it. A caller that stopped reading early — a parse
// that gave up on an over-long line, a step that timed out — leaves the
// demultiplexer blocked writing into a pipe nobody drains; waiting for it
// then would deadlock against a caller who has done nothing wrong. Closing
// unblocks it. It is the same reason exec.Cmd.Wait closes the pipe it handed
// out, and it carries the same obligation: finish reading before calling this.
func (a *Attached) Wait(ctx context.Context) (int, error) {
	result := a.client.api.ContainerWait(ctx, a.id, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})

	select {
	case status := <-result.Result:
		a.settle(false)

		return int(status.StatusCode), nil
	case err := <-result.Error:
		a.settle(true)

		return 0, fmt.Errorf("waiting on container %s: %w", a.id, err)
	case <-ctx.Done():
		a.settle(true)

		return 0, fmt.Errorf("waiting on container %s: %w", a.id, ctx.Err())
	}
}

// settle ends both streams and waits for the demultiplexer, so that when Wait
// returns nothing is still writing to the writers the caller handed in.
//
// Every path, not just the successful one, which is the bug this had. A caller
// passes its own stderr buffer here and reads it the moment Wait returns; a
// strings.Builder still being written to by the pump is a data race, and the
// timeout path — where the caller most wants to read what was said — was
// exactly the one that skipped the wait.
//
// interrupt closes the connection FIRST, and only when the container did not
// finish: the demultiplexer is then blocked in a read nothing else will end.
// On the ordinary path the daemon has already closed its side, and cutting the
// connection there could truncate the last bytes instead of delivering them.
func (a *Attached) settle(interrupt bool) {
	if interrupt {
		a.close()
	}

	_ = a.stdout.CloseWithError(errAttachClosed)
	<-a.pumped
	a.close()
}

// close releases the hijacked connection, once however many times it is asked.
func (a *Attached) close() {
	a.closer.Do(func() { a.hijack.Close() })
}

// errAttachClosed is what a read after Wait reports, so a stream the caller
// has already finished with is not mistaken for a container that failed.
var errAttachClosed = errors.New("the container's output stream is closed")
