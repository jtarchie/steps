package venue

// A placed step that also names an image.
//
// The tree travels the venue's way and the command runs in a container on the
// worker, driven by the same shell.DockerRunner every local containerized
// step uses — pointed at the daemon through the socket serveDockerSocket
// forwards, and mounting the copy of the tree the shim reported. Nothing here
// re-implements a container, and the shim never learns what one is.

import (
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/wire"
)

// sweepContainers is shell's orphan sweep, behind a variable so a test can
// prove WHICH daemon it was pointed at. That is the whole risk here: the two
// halves — resolving a worker's socket and sweeping a daemon — are each
// straightforward, and the bug is a socket that never crosses between them,
// leaving the sweep quietly tidying the orchestrator's own machine.
//
//nolint:gochecknoglobals // a test seam over one function, not state
var sweepContainers = shell.SweepOrphanedContainers

// containerRunner returns the runner that executes this step's commands in a
// container on the worker, opening the forwarded socket the first time.
//
// Memoized for the step, not per command: a container is started once and
// every command of the step runs inside it, which is the same promise a local
// image: step makes — an agent that pip-installs in one call finds it in the
// next.
func (s *session) containerRunner(ctx context.Context) (shell.Runner, error) {
	// Under the session mutex, because close() and abandon() read and clear
	// these same three fields: a cancelled step tears down while this
	// goroutine is between openDockerSocket returning and the assignment
	// below, and an unlocked write there left the teardown seeing nil for a
	// container that was about to exist — leaking it, its listener and its
	// socket directory, and racing under the detector besides.
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, errSessionClosed
	}

	if s.inner != nil {
		return s.inner, nil
	}

	socket, stop, err := s.openDockerSocket(ctx, false)
	if err != nil {
		return nil, err
	}

	dockerHost := "unix://" + socket

	// Reclaim what a killed run left on THIS worker, before starting anything
	// on it. The mirror of the sweep RunJob does for the local daemon, and the
	// reason it has to happen here is that a worker's daemon is reachable only
	// while a socket is forwarded to it: the labels already carry this
	// orchestrator's hostname and pid, so nothing about the question changes,
	// but until now nobody ever asked it of a machine other than this one, and
	// a container a placed step could not tear down ran there until somebody
	// noticed.
	//
	// Per socket rather than per job, which is the finer grain: it is one
	// `docker ps` with two label filters over a connection already open, and
	// the alternative is job-level state about workers that only sessions can
	// reach.
	sweepContainers(ctx, dockerHost)

	spec := s.containerSpec(dockerHost)
	// The session's resolved environment, which is where STEPS_WORKER lives.
	// The exec path sends it in the frame; a container has to be told, or a
	// script that branches on placement takes the local branch on the very
	// machine the tag was written to select.
	spec.EnvValues = s.env

	inner, err := shell.NewRunner(spec)
	if err != nil {
		stop()

		return nil, fmt.Errorf("%w %q: %w", ErrWorker, s.worker.URL, err)
	}

	s.inner, s.dockerStop = inner, stop

	return inner, nil
}

// containerSpec is the spec the worker's container is built from.
//
// Separated from the dialling so the decisions in it can be read — and
// tested — without a live session, because every one of them is about
// answering with the WORKER's facts rather than this process's.
func (s *session) containerSpec(dockerHost string) shell.RunnerSpec {
	spec := s.container

	// The worker is already resolved; what is wanted here is a plain
	// container runner, aimed elsewhere.
	spec.Worker = ""
	spec.WorkerTag = ""
	spec.DockerHost = dockerHost
	// The daemon resolves -v against ITS filesystem, so the mount is the
	// tree the shim unpacked, not this machine's copy of anything.
	spec.MountPath = s.workdir

	// An explicit user: crosses verbatim — the same contract Concourse has,
	// where the value is a name the far end resolves. Only the DEFAULT is
	// decided here, and it has to be decided from the worker: the tree this
	// container writes into lives there, so the ownership mismatch the
	// default exists to prevent happens there too, against the identity the
	// shim runs as. Asking this process would answer about the wrong machine.
	if spec.User == "" {
		spec.User = shell.DefaultContainerUserFor(s.goos, idOrUnknown(s.uid), idOrUnknown(s.gid))
	}

	return spec
}

// idOrUnknown reads a reported identity, answering -1 when the shim did not
// send one — an older shim, or a platform with no answer. That defers to the
// image, which is what an unset user means everywhere else.
func idOrUnknown(id *int) int {
	if id == nil {
		return -1
	}

	return *id
}

// runContained runs one command in the worker's container, answering in the
// same shape session.run does so exchange does not care which ran it.
func (r runner) runContained(ctx context.Context, command string, p plan) (string, string, wire.Exit, error) {
	inner, err := r.containerFor(ctx)
	if err != nil {
		return "", "", wire.Exit{}, err
	}

	var (
		stdout, stderr string
		code           int
	)

	err = r.session.withDockerRouting(ctx, func() error {
		var runErr error

		// Either flag streams BOTH, because shell's container runner has one
		// streamed variant and streams both through it.
		//
		// ponytail: RunCapture DOES ask for one and not the other —
		// plan{streamStderr: true, capture: true} — so a placed image: step
		// reached through it would stream the stdout its caller asked to have
		// captured quietly. p.capture is ignored here for the same reason:
		// this path always captures. Neither has a production caller today
		// (nothing calls RunCapture on a placed image step), and the honest
		// fix is a shell API that can split the two, not a cleverer branch
		// here.
		if p.streamStdout || p.streamStderr {
			stdout, stderr, code, runErr = inner.RunCaptureFullLimitedStreamed(ctx, command, p.maxBytes, p.spillDir)
		} else {
			stdout, stderr, code, runErr = inner.RunCaptureFullLimited(ctx, command, p.maxBytes, p.spillDir)
		}

		if runErr != nil {
			return fmt.Errorf("%w", runErr)
		}

		return nil
	})

	if err != nil {
		return stdout, stderr, wire.Exit{}, err
	}

	// Started is true because the runner answered with an exit code at all:
	// shell reports a command that never ran as an error, which is the same
	// distinction wire.Exit carries.
	return stdout, stderr, wire.Exit{Code: code, Started: true}, nil
}

// containerFor labels the inner runner the way this one is labelled, so a
// placed containerized step's output reads like every other step's.
func (r runner) containerFor(ctx context.Context) (shell.Runner, error) {
	inner, err := r.session.containerRunner(ctx)
	if err != nil {
		return nil, err
	}

	if r.label != "" {
		inner = inner.WithLabel(r.label)
	}

	return inner, nil
}
