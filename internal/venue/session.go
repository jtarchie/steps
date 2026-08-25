package venue

// One conversation with one worker, for one step.
//
// The shape is dockerSession's, deliberately and almost line for line: a lazy
// connection made on first use, a failure that sticks so a broken worker is
// not re-dialled once per command, and a teardown that builds its own context
// rather than taking the caller's. Those three decisions were each paid for
// once already by the container path, and a venue gets them wrong in exactly
// the same ways.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/wire"
)

// closeTimeout bounds a teardown. Mirrors dockerCleanupTimeout, for the same
// reason: Close runs from deferred paths whose context is routinely already
// cancelled — a timed-out step, a Ctrl-C — and those are precisely the cases
// where leaving scratch on somebody else's machine would be worst.
const closeTimeout = 30 * time.Second

// helloTimeout bounds the handshake.
//
// Without it a worker that accepted the connection but could not run the
// binary — the shape every architecture mismatch takes — leaves this end
// blocked forever on a frame that is never coming, and a step hangs instead of
// failing. Generous, because it also covers a cold machine paging in a 50MB
// binary.
const helloTimeout = 60 * time.Second

// transport is a byte pipe to a shim, plus whatever has to be torn down to
// release it. The venue does not care whether that is a child process or an
// SSH channel, which is what lets a second scheme land without touching
// anything below.
type transport struct {
	in    io.ReadCloser
	out   io.WriteCloser
	close func(context.Context) error
	// diagnostics is whatever the far end wrote outside the protocol. It is
	// the only account of a shim that never started — a binary built for
	// another architecture, a loader that refused it — because such a shim
	// never says anything the protocol can carry.
	diagnostics func() string
	// exited is closed when the far end's process ends, and is the reliable
	// way to learn that: an SSH channel's reader does not dependably return
	// EOF when the remote command dies, so a handshake waiting only on bytes
	// would wait out its whole timeout for a shim that was never going to
	// speak. Closed rather than sent to, so both the handshake and the
	// teardown can read it. nil when a transport has no separate notion of the
	// process ending.
	exited <-chan struct{}
	// build is the content hash of the binary this transport actually
	// started. Not SelfBuild(): a worker reached with ?binary= runs a binary
	// the operator built, whose hash is not this process's, and greet compares
	// what came back against what went out.
	build string
}

// session owns the conversation. Runners hold it BY POINTER so a WithLabel
// copy shares one worker rather than dialling a second — the same reason
// DockerRunner holds its session by pointer.
type session struct {
	worker Worker
	// cwd is the local tree that goes out and results come back into.
	cwd string
	// outputs names what to bring back after each command.
	outputs []string
	// env carries the values the pipeline's env: opted into, resolved here.
	env map[string]string
	// keep leaves the worker's scratch behind, following --keep-workspace.
	keep bool

	mu        sync.Mutex
	attempted bool
	startErr  error
	closed    bool
	transport *transport
	encoder   *wire.Encoder
	decoder   *wire.Decoder
	workdir   string
	op        uint32
}

var (
	// errSessionClosed is a command on a session whose step already finished.
	errSessionClosed = errors.New("the step's worker session has been closed")
	// errNoWorkdir is a shim that answered a hello without naming where it put
	// the tree, which no shim this repo built can do.
	errNoWorkdir = errors.New("the worker did not report a work directory")
	// errWrongBuild is a worker running a steps binary this run did not push.
	errWrongBuild = errors.New("the worker is not running the binary that was pushed to it")
	// errLossyWorker is a worker whose filesystem cannot represent something
	// the step cache treats as content. See lossyGOOS.
	errLossyWorker = errors.New("the worker's filesystem cannot hold what the step's tree carries")
)

// lossyGOOS names the operating systems whose filesystem cannot store a file's
// executable bit, which internal/workspace's digestTree hashes as content.
//
// Windows has nowhere to put it: os.Chmod there consults only the write bit,
// setting or clearing FILE_ATTRIBUTE_READONLY, and returns no error; os.Stat
// synthesizes 0444 or 0666 for every regular file and ORs in 0111 only for a
// directory. So a tree unpacks without its executable bits, the repack on the
// way home reads that back off the filesystem, and the tree that returns is
// not the tree that went out — silently, since the step cache cannot tell a
// stripped bit from an edit and no layer raised an error.
//
// A map rather than a comparison so a second such platform is a line rather
// than a rewrite, and js/wasip1 are deliberately absent: neither can run a
// step's shell in the first place, so they fail earlier and for a plainer
// reason.
//
//nolint:gochecknoglobals // a fact about operating systems, not state
var lossyGOOS = map[string]string{
	"windows": "an executable bit",
}

// short is a build hash cut to something a human can compare in an error.
func short(build string) string {
	if len(build) > 12 {
		return build[:12]
	}

	return build
}

// ensure connects, greets, and sends the step's tree, once.
//
// A failure sticks. An unreachable host, a rejected key or a failed binary
// push must not be retried once per run_shell in a conversation: the first
// answer is the true one and every retry costs another timeout.
func (s *session) ensure(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return errSessionClosed
	}

	if s.attempted {
		return s.startErr
	}

	s.attempted = true
	s.startErr = s.connect(ctx)

	return s.startErr
}

func (s *session) connect(ctx context.Context) error {
	transport, err := dial(ctx, s.worker)
	if err != nil {
		return fmt.Errorf("worker %q: %w", s.worker, err)
	}

	s.transport = transport
	s.encoder = wire.NewEncoder(transport.out)
	s.decoder = wire.NewDecoder(transport.in)

	err = s.greet()
	if err != nil {
		// The transport is already up, so tearing it down here is what keeps a
		// failed handshake from stranding a child process or an SSH channel.
		//nolint:contextcheck // deliberately not the caller's context; see abandon
		s.abandon()

		return fmt.Errorf("worker %q: %w", s.worker, err)
	}

	err = s.upload(ctx)
	if err != nil {
		//nolint:contextcheck // deliberately not the caller's context; see abandon
		s.abandon()

		return fmt.Errorf("worker %q: %w", s.worker, err)
	}

	return nil
}

// abandon tears down a transport whose session never opened.
//
// Under its own bounded context, never the caller's: the caller's may have no
// deadline at all, and a worker that has already stopped answering would then
// hold the step open forever on the cleanup rather than the work.
func (s *session) abandon() {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()

	_ = s.transport.close(ctx)
	s.transport = nil
}

func (s *session) greet() error {
	build := s.transport.build

	// The session name has to be unique across every session that could ever
	// share a worker, because it names a scratch directory the shim removes on
	// its way out: two sessions agreeing on a name means one deletes the
	// other's tree mid-step. Step directory and pid are the traceable part —
	// they say which run to blame for a leftover — and the random suffix is
	// what makes the guarantee, since two builds can produce the same step
	// directory name and two orchestrators can share a pid.
	name, err := sessionName(s.cwd)
	if err != nil {
		return err
	}

	err = s.write(wire.Frame{Type: wire.FrameHello, Op: s.nextOp()}, wire.Hello{
		Protocol: wire.Protocol,
		Build:    build,
		Session:  name,
		Keep:     s.keep,
		Root:     s.worker.Root,
	})
	if err != nil {
		return err
	}

	frame, err := s.readHello()
	if err != nil {
		return err
	}

	var ok wire.HelloOK

	err = decode(frame, &ok)
	if err != nil {
		return err
	}

	if ok.Protocol != wire.Protocol {
		return fmt.Errorf("%w: this steps speaks protocol %d and the worker's shim speaks %d — the binary on the worker is not this one",
			wire.ErrProtocol, wire.Protocol, ok.Protocol)
	}

	// The check wire.Hello.Build has always described and nobody performed.
	// A pushed binary is reused when a file of the right SIZE is already at
	// its content-keyed path, which is a guess about bytes; this is the
	// answer. It is also the only thing that catches a protocol-compatible
	// shim that is nonetheless not the build this run pushed — an older steps
	// left at that path by hand, or a truncation a matching size hid.
	if build != "" && ok.Build != "" && ok.Build != build {
		return fmt.Errorf("%w: the worker is running build %s and this steps pushed %s — remove %s on the worker and run again",
			errWrongBuild, short(ok.Build), short(build), remoteShimPath(s.worker, build))
	}

	// Refused rather than warned, matching what the codec does one package
	// over: wire.PackTree refuses to ship a fifo because dropping an entry
	// would change what digestTree computes over the extracted copy, and a
	// cache that quietly disagrees with itself is worse than a step that
	// refuses to ship one. This is that hazard with a machine in the middle.
	//
	// On silence, not refused: an empty GOOS is a shim that said nothing about
	// its filesystem — one an operator started by hand over a bare ssh
	// command, say — and rejecting a worker for answering a shorter hello
	// would break machines that are fine. The build check above takes the
	// same view of an empty Build.
	if lost, lossy := lossyGOOS[ok.GOOS]; lossy {
		return fmt.Errorf("%w: %s runs %s, which has nowhere to store %s — a tree sent there comes back without one, and nothing reports it",
			errLossyWorker, s.worker.URL, ok.GOOS, lost)
	}

	if ok.Workdir == "" {
		return errNoWorkdir
	}

	s.workdir = ok.Workdir

	return nil
}

// readHello reads the handshake under a deadline, so a shim that never got as
// far as speaking is reported rather than waited on.
func (s *session) readHello() (wire.Frame, error) {
	type result struct {
		frame wire.Frame
		err   error
	}

	// Buffered, so the read goroutine can always finish and exit even after
	// this function has given up on it. Closing the transport is what
	// unblocks it.
	answered := make(chan result, 1)

	go func() {
		frame, err := s.read()
		answered <- result{frame: frame, err: err}
	}()

	timer := time.NewTimer(helloTimeout)
	defer timer.Stop()

	var ended <-chan struct{}
	if s.transport != nil {
		ended = s.transport.exited
	}

	select {
	case answer := <-answered:
		if answer.err != nil {
			return wire.Frame{}, s.startupError(answer.err)
		}

		return answer.frame, nil
	case <-ended:
		// The shim is gone and said nothing. Whatever it wrote on the way out
		// is the whole explanation, and waiting for the timeout would only
		// delay reporting it.
		return wire.Frame{}, s.startupError(errShimExited)
	case <-timer.C:
		return wire.Frame{}, s.startupError(errNoHello)
	}
}

// errShimExited is a worker whose shim died before the handshake.
var errShimExited = errors.New("the shim exited before answering")

// errNoHello is a worker that accepted a connection and then said nothing.
var errNoHello = errors.New("the worker did not answer within the handshake timeout")

// startupError explains a handshake that did not happen, using whatever the
// far end said outside the protocol.
func (s *session) startupError(cause error) error {
	note := ""
	if s.transport != nil && s.transport.diagnostics != nil {
		note = s.transport.diagnostics()
	}

	if note == "" {
		return fmt.Errorf("%w: %w", errShimDidNotStart, cause)
	}

	// The worker's own words first: "cannot execute binary file" says more
	// than any wrapper this end could write.
	return fmt.Errorf("%w: %w (worker said: %s) — build a binary for that machine and name it with ?binary=",
		errShimDidNotStart, cause, note)
}

// close tears the session down, letting the shim remove its own scratch.
//
// It builds its own context rather than taking a caller's, because the caller's
// is routinely already cancelled by the time cleanup runs, and a cancelled
// context here would skip the goodbye that frees the worker.
func (s *session) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}

	s.closed = true

	if s.transport == nil {
		// Never dialled: nothing to release, which is the ordinary case for a
		// step that was skipped or failed before its first command.
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()

	// Best effort: a worker that already vanished cannot be told goodbye, and
	// saying so would replace the real error with a cleanup one.
	_ = s.encoder.Write(wire.Frame{Type: wire.FrameBye, Op: s.nextOp()})

	err := s.transport.close(ctx)
	s.transport = nil

	if err != nil {
		return fmt.Errorf("worker %q: %w", s.worker, err)
	}

	return nil
}

func (s *session) nextOp() uint32 {
	s.op++

	return s.op
}

func (s *session) write(frame wire.Frame, payload any) error {
	err := s.encoder.WriteJSON(frame.Type, frame.Op, payload)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// read returns the next frame, turning an error frame from the shim into a Go
// error so callers never have to check for it.
func (s *session) read() (wire.Frame, error) {
	frame, err := s.decoder.Read()
	if err != nil {
		// A transport that died mid-step is infrastructure, and saying so
		// explicitly is what keeps it from being read as a command's verdict.
		return wire.Frame{}, fmt.Errorf("the connection to the worker was lost: %w", err)
	}

	if frame.Type == wire.FrameError {
		var wireErr wire.Error

		decodeErr := decode(frame, &wireErr)
		if decodeErr != nil {
			return wire.Frame{}, decodeErr
		}

		return wire.Frame{}, errors.New(wireErr.Message)
	}

	return frame, nil
}

// sessionName is a worker-unique name for one step's scratch.
func sessionName(cwd string) (string, error) {
	suffix := make([]byte, 8)

	_, err := rand.Read(suffix)
	if err != nil {
		return "", fmt.Errorf("naming the session: %w", err)
	}

	return fmt.Sprintf("%s-%d-%s", filepath.Base(cwd), os.Getpid(), hex.EncodeToString(suffix)), nil
}

func decode(frame wire.Frame, v any) error {
	err := wire.DecodeJSON(frame, v)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}
