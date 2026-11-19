package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/jtarchie/steps/internal/dockerapi"
)

// Attached is a command in a container spoken to over its stdin and stdout, for a server that answers there (a stdio mcp_servers: entry with image:).
type Attached struct {
	// Stdout is what the command writes; closing it ends the command and removes its container.
	Stdout io.ReadCloser
	// Stdin is what the command reads.
	Stdin io.WriteCloser
}

// Attach starts argv in a container from spec.Image, in workdir when one is given.
func Attach(ctx context.Context, spec RunnerSpec, argv []string, workdir string, stderr io.Writer) (Attached, error) {
	if spec.Image == "" {
		return Attached{}, errors.New("shell: attach needs an image")
	}

	runner, err := NewRunner(spec)
	if err != nil {
		return Attached{}, err
	}

	docker, _ := runner.(DockerRunner)

	client, id, startOut, err := docker.session.ensure(ctx)
	if err != nil {
		_ = runner.Close()

		return Attached{}, fmt.Errorf("starting %q in image %q: %w: %s", argv[0], spec.Image, err, startOut)
	}

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	done := make(chan struct{})

	go func() {
		defer close(done)

		code, err := client.Exec(ctx, id, dockerapi.ExecOptions{Cmd: argv, Stdin: stdinR, Stdout: stdoutW, Stderr: stderr, WorkingDir: workdir})
		if err == nil {
			err = fmt.Errorf("%q in image %q exited with status %d", argv[0], spec.Image, code)
		}

		_ = stdoutW.CloseWithError(err)
	}()

	var once sync.Once

	end := func() error {
		once.Do(func() {
			// Unblocks a copy writing into a reader nobody reads, before the container's removal ends the exec.
			_ = stdinW.Close()
			_ = stdoutR.Close()
			_ = runner.Close()

			<-done
		})

		return nil
	}

	return Attached{Stdout: closer{Reader: stdoutR, close: end}, Stdin: stdinW}, nil
}

type closer struct {
	io.Reader

	close func() error
}

func (c closer) Close() error { return c.close() }
