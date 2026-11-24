package shell

// A reattachable command (steps#306): run as the main process of a container of its own, on the session's volumes, so the runtime keeps its output and exit code while nobody is listening. An exec keeps neither: its output is gone the moment nobody reads it, and its exit code for about ten minutes.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/events"
)

// dockerRunLabel names the run a command container belongs to, which is what a restarted daemon finds it by.
const dockerRunLabel = "steps.run"

// errCommandGone is a command whose container vanished while steps could not watch it: removed, or a worker rebooted.
var errCommandGone = errors.New("the command's container is gone: it was removed, or its worker restarted")

type commandKeyKey struct{}

// WithCommandKey adds part to where ctx's commands sit in their run, so a reattachable command's container is named the same by any process that reaches the same place: a step's plan position, its pass through it, its attempt.
func WithCommandKey(ctx context.Context, part string) context.Context {
	if prior, _ := ctx.Value(commandKeyKey{}).(string); prior != "" {
		part = prior + "/" + part
	}

	return context.WithValue(ctx, commandKeyKey{}, part)
}

func commandKey(ctx context.Context) string {
	key, _ := ctx.Value(commandKeyKey{}).(string)

	return key
}

// reattachRetry is how long a reattach waits between tries to reach the daemon again.
var reattachRetry = time.Second

// reattachGrace is how long a cancelled command is given to stop on TERM before it is removed by force.
const reattachGrace = 10 * time.Second

// commandName is the container name for the session's n-th reattachable command under ctx: derived, so it can be found again without storing anything. Random without a run and a place in it, since two steps of one run would otherwise share a name and the second would adopt the first's command.
func (s *dockerSession) commandName(ctx context.Context, n int64) (string, error) {
	runID, key := events.RunID(ctx), commandKey(ctx)
	if runID == "" || key == "" {
		var buf [8]byte

		_, err := rand.Read(buf[:])
		if err != nil {
			return "", fmt.Errorf("naming a command container: %w", err)
		}

		return "steps-cmd-" + hex.EncodeToString(buf[:]), nil
	}

	sum := sha256.Sum256([]byte(runID + "\x00" + key + "\x00" + strconv.FormatInt(n, 10)))

	return "steps-cmd-" + hex.EncodeToString(sum[:12]), nil
}

// countedStream delivers each byte of one stream once across every read of the log: a read starts from the beginning, and what an earlier read already delivered is skipped.
type countedStream struct {
	dst       io.Writer
	delivered *atomic.Int64
	pos       int64
}

func (c *countedStream) Write(p []byte) (int, error) {
	start := c.pos
	c.pos += int64(len(p))

	seen := c.delivered.Load()
	if c.pos <= seen {
		return len(p), nil
	}

	fresh := p
	if start < seen {
		fresh = p[seen-start:]
	}

	_, err := c.dst.Write(fresh)
	if err != nil {
		return 0, err //nolint:wrapcheck // the sink's own error
	}

	c.delivered.Add(int64(len(fresh)))

	return len(p), nil
}

// commandRun is one reattachable command's progress: its container and the bytes of each stream shown so far.
type commandRun struct {
	id             string
	stdout, stderr atomic.Int64
}

// ponytail: every read is the whole log from its first byte, the counted bytes discarded, one extra transfer of the log per reattach; past megabytes, Docker's engine can start at `since` (exact there, one timestamp per line), which Podman cannot (thousands of lines share one).
func (r *commandRun) read(ctx context.Context, client *dockerapi.Client, follow bool, out *Output) error {
	err := client.Logs(ctx, r.id, follow,
		&countedStream{dst: out.Stdout, delivered: &r.stdout},
		&countedStream{dst: out.Stderr, delivered: &r.stderr})
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// runCommandContainer runs cmd as the main process of a container of its own and follows it to its exit code, reattaching through any drop of the connection while ctx lives.
func (d DockerRunner) runCommandContainer(ctx context.Context, client *dockerapi.Client, cmd Command, out *Output) (int, error) {
	s := d.session

	name, err := s.commandName(ctx, s.commands.Add(1))
	if err != nil {
		return 0, err
	}

	run := &commandRun{}

	run.id, err = d.createCommand(ctx, client, cmd, name)
	if err != nil {
		return 0, err
	}

	for {
		code, readErr := run.follow(ctx, client, out)
		if readErr == nil {
			reclaimCommand(ctx, client, run.id)

			return code, nil
		}

		if ctx.Err() == nil {
			slog.WarnContext(ctx, "shell.docker.reattach", "container", name, "error", readErr)

			next, err := s.reattach(ctx, run.id)
			if errors.Is(err, errCommandGone) {
				return 0, err
			}

			if next != nil {
				client = next
			}
		}

		if ctx.Err() != nil {
			s.stopCommand(ctx, run.id)

			return dockerapi.SignalledExitCode, nil //nolint:nilerr // a caller that gave up cut the command off: data, as an exec's is
		}
	}
}

// follow shows the output live, takes the exit code, then reads once more without follow: follow decides nothing about completeness, since Podman ends one early on an exited container.
func (r *commandRun) follow(ctx context.Context, client *dockerapi.Client, out *Output) (int, error) {
	err := r.read(ctx, client, true, out)
	if err != nil {
		return 0, err
	}

	code, err := client.Wait(ctx, r.id)
	if err != nil {
		return 0, fmt.Errorf("%w", err)
	}

	err = r.read(ctx, client, false, out)
	if err != nil {
		return 0, err
	}

	return code, nil
}

// createCommand makes and starts the command's container, or adopts the one already under name: what a process that lost track of it left running.
func (d DockerRunner) createCommand(ctx context.Context, client *dockerapi.Client, cmd Command, name string) (string, error) {
	s := d.session

	status, _, err := client.ContainerStatus(ctx, name)
	if err == nil && status != "created" {
		slog.InfoContext(ctx, "shell.docker.reattach_adopted", "container", name, "status", status)

		return name, nil
	}

	if err == nil {
		// Made but never started: nothing ran, so starting afresh repeats nothing.
		reclaimCommand(ctx, client, name)
	}

	labels := OwnershipLabels()
	if runID := events.RunID(ctx); runID != "" {
		labels[dockerRunLabel] = runID
	}

	workdir := s.workingDir()
	if d.subdir != "" {
		workdir = path.Join(workdir, d.subdir)
	}

	spec := dockerapi.ContainerSpec{
		Image: s.image,
		// The script as an exec runs it, past the image's ENTRYPOINT: moby/buildkit's would take it as arguments to buildkitd and never exit.
		Entrypoint:  []string{"sh", "-c"},
		Cmd:         []string{cmd.Script},
		Name:        name,
		WorkingDir:  workdir,
		Volumes:     s.volumes,
		Env:         append(s.containerEnv(), buildEnvPairs(ctx)...),
		Labels:      labels,
		User:        s.user,
		Network:     s.network,
		ExtraHosts:  s.extraHosts,
		Privileged:  s.privileged,
		CPUShares:   int64(s.cpuShares),
		MemoryBytes: s.memoryBytes,
		Init:        true,
		OpenStdin:   cmd.Input != "",
		LogDriver:   dockerapi.JSONFileLogs,
	}

	id, err := client.CreateContainer(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	var input io.Reader
	if cmd.Input != "" {
		input = strings.NewReader(cmd.Input)
	}

	err = client.StartWithInput(ctx, id, input)
	if err != nil {
		reclaimCommand(ctx, client, id)

		return "", fmt.Errorf("%w", err)
	}

	return id, nil
}

// reattach reaches the daemon again and asks whether the command's container is still there, trying until it answers or ctx ends: timeout: is the bound, and a step without one waits for its worker.
func (s *dockerSession) reattach(ctx context.Context, id string) (*dockerapi.Client, error) {
	for {
		client, err := s.reconnect(ctx)
		if err == nil {
			_, _, err = client.ContainerStatus(ctx, id)
			if dockerapi.IsNotFound(err) {
				return nil, fmt.Errorf("%w (container %s)", errCommandGone, id)
			}

			if err == nil {
				return client, nil
			}
		}

		slog.DebugContext(ctx, "shell.docker.reattach_unanswered", "container", id, "error", err)

		select {
		case <-ctx.Done():
			return nil, ctx.Err() //nolint:wrapcheck // the caller asks ctx, not this
		case <-time.After(reattachRetry):
		}
	}
}

// reconnect is the session's connection, dialled again through the venue when it can do that.
func (s *dockerSession) reconnect(ctx context.Context) (*dockerapi.Client, error) {
	if s.redial != nil {
		err := s.redial(ctx)
		if err != nil {
			return nil, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client == nil {
		return nil, errSessionClosed
	}

	return s.client, nil
}

// stopCommand ends a cancelled command now rather than at teardown: TERM, a grace, then removal by force. Under its own bounded context, reaching the daemon again if it has to, since the likeliest reason it runs is that the caller's context just ended.
func (s *dockerSession) stopCommand(ctx context.Context, id string) {
	stopCtx, cancel := removalContext(ctx)
	defer cancel()

	for {
		client, err := s.reconnect(stopCtx)
		if err == nil {
			err = client.Kill(stopCtx, id, "TERM")
			if err == nil {
				graceCtx, graceCancel := context.WithTimeout(stopCtx, reattachGrace)
				_, _ = client.Wait(graceCtx, id)

				graceCancel()

				err = client.RemoveContainer(stopCtx, id)
			}
		}

		if err == nil {
			return
		}

		select {
		case <-stopCtx.Done():
			slog.WarnContext(ctx, "shell.docker.stop_failed", "container", id, "error", err)

			return
		case <-time.After(reattachRetry):
		}
	}
}

// reclaimCommand removes a command's container once its output and exit code are taken.
func reclaimCommand(ctx context.Context, client *dockerapi.Client, id string) {
	removeCtx, cancel := removalContext(ctx)
	defer cancel()

	reclaim(removeCtx, client, id)
}
