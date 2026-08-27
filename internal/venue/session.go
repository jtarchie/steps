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
	"sync/atomic"
	"time"

	"github.com/jtarchie/steps/internal/blobstore"
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
	// interrupt cuts the conversation NOW, from another goroutine, in a way
	// that unblocks both a blocked read and a blocked write. It exists
	// because closing in alone is not that: an SSH stdout is a plain Reader
	// wrapped in a NopCloser, so "close the reader" enforced nothing there —
	// timeout:, fail_fast, race: and Ctrl-C were all inert against a wedged
	// ssh:// worker, the exact failure the watchdog exists to end. And a
	// worker that stopped READING blocks the encoder on backpressure, which
	// no reader-side close can ever unstick.
	interrupt func()
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
	// broken records that the conversation died after it opened — a read or a
	// write hit a dead transport — so the next ensure redials instead of
	// pushing frames into a pipe nobody holds. Atomic rather than under mu,
	// because reads and writes run outside the mutex while ensure consults it
	// under one.
	broken    atomic.Bool
	transport *transport
	encoder   *wire.Encoder
	decoder   *wire.Decoder
	workdir   string
	// fstype and fsfree describe the filesystem workdir sits on, as the shim
	// reported it. Empty fstype means the shim could not say — an older one,
	// or a platform with no answer — and never "an ordinary disk".
	fstype string
	fsfree uint64
	// compression is what the handshake negotiated for tree transfers: the
	// token the shim echoed back, or empty for raw against an older shim.
	compression string
	// blobs is the artifact store, nil unless --artifact-store was given.
	// With it, greet offers the URL data plane; without it, or against a shim
	// that stays silent, trees ride the tunnel — the floor both ends share.
	blobs *blobstore.Store
	// dataplane is what the handshake settled: wire.DataPlaneURLs or empty.
	dataplane string
	// drain holds the worker's own announcement of its end, or nil.
	//
	// Deliberately NOT under mu, and this is load-bearing: ensure() holds mu
	// across connect, which uploads the tree and reads its acknowledgement —
	// and read absorbs drain notices, so a noteDrain reaching for mu would
	// deadlock the session against itself on exactly the frame it exists to
	// handle.
	drain atomic.Pointer[wire.Draining]
	// op mints operation ids. Atomic because close() takes s.mu and every
	// other minting path takes nothing, so a teardown racing a transfer was
	// an unsynchronized read-modify-write that could hand two operations the
	// same id.
	op atomic.Uint32
}

// ErrEvicted is a step whose worker was taken away underneath it.
//
// Distinct from every other failure on purpose, and the reason is a
// deliberate divergence from Concourse, which errors a build when a worker
// vanishes. Infrastructure reclaiming a machine is not the step saying no and
// not the step being flaky: an author's attempts: budget is their statement
// about their own work, and spending it on the cloud taking a spot instance
// would charge them for something they neither caused nor can fix.
var ErrEvicted = errors.New("the worker was reclaimed while the step was running")

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

// ensure connects, greets, and sends the step's tree, once per conversation.
//
// The stickiness boundary is the HANDSHAKE. A failure to dial or to greet
// sticks: an unreachable host, a rejected key, a failed binary push or a shim
// that never answers gives the same answer every time, and each re-ask costs
// another timeout. A transport that died any time AFTER a successful hello —
// mid-upload, mid-command, mid-fetch — is the opposite case: the worker was
// reachable and then went away, a crash or a dropped tunnel, and without a
// fresh dial every retry an attempts: budget pays for fails against the same
// dead pipe, for a reason the pipeline author can neither see nor fix. The
// redial re-sends the step's LOCAL tree, which already holds everything
// earlier commands fetched back; whatever the dead worker held beyond that
// died with it.
func (s *session) ensure(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return errSessionClosed
	}

	if s.attempted && !s.broken.Load() {
		return s.startErr
	}

	if s.transport != nil {
		// The transport under the dead conversation is dead weight, and
		// redialling without releasing it would leak a child process or an
		// SSH connection per retry.
		//nolint:contextcheck // deliberately not the caller's context; see abandon
		s.abandon()
	}

	s.attempted = true
	// Cleared before the dial rather than after connect: everything that can
	// mark it again is scoped to the conversation — the handshake's own reads
	// and writes go through the raw, non-marking paths — so the flag standing
	// after connect fails means the transport died on a worker that had
	// already answered its hello, and the next command redials it. A failure
	// to dial or to greet leaves the flag clear, and sticks.
	s.broken.Store(false)
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

	err = s.writeRaw(wire.Frame{Type: wire.FrameHello, Op: s.nextOp()}, wire.Hello{
		Protocol: wire.Protocol,
		Build:    build,
		Session:  name,
		Keep:     s.keep,
		Root:     s.worker.Root,
		// Always offered, never required: a shim that echoes it speaks zstd
		// for the tree transfers, and one that answers with silence — an
		// older binary — gets raw, the floor both ends always share.
		Compression: wire.CompressionZstd,
		DataPlane:   s.offerDataPlane(),
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

	err = s.checkHello(ok, build)
	if err != nil {
		return err
	}

	if ok.Workdir == "" {
		return errNoWorkdir
	}

	s.workdir = ok.Workdir
	s.compression = ok.Compression
	s.dataplane = ok.DataPlane
	s.fstype = ok.FSType
	s.fsfree = ok.FSFree

	notice := volatileWorkdirNotice(s.worker, ok)
	if notice != "" {
		fmt.Println(notice)
	}

	return nil
}

// volatileWorkdirNotice is what to say about a workdir that is memory, and
// empty about one that is not.
//
// It earns a warning because the cost is invisible from this end and lands on
// the step rather than on the operator: an aws:// worker whose URL names no
// path defaults to the shim's temp directory, which on Amazon Linux 2023 —
// and Fedora, and recent Debian and Ubuntu — is tmpfs at half the machine's
// RAM. The pushed binary and the step's tree are then spending the memory the
// build wanted, and a stop/start loses the content-hash cache entirely.
//
// Silence is never warned about: an older shim that cannot say is a different
// fact from a disk, and a warning nobody can act on is one operators learn to
// scroll past.
func volatileWorkdirNotice(worker Worker, ok wire.HelloOK) string {
	switch ok.FSType {
	case "tmpfs", "ramfs":
	default:
		return ""
	}

	return fmt.Sprintf(
		"worker %s: %s is on %s (%d MiB free) — that is memory, not disk: the pushed binary and this step's tree spend it, and a reboot loses both. Name a path on a real disk in the worker URL, as in %s/var/tmp/steps",
		worker.Address(), ok.Workdir, ok.FSType, ok.FSFree>>20, worker.Address())
}

// offerDataPlane is the plane greet proposes: URLs when a blob store is
// configured, nothing otherwise — an offer the venue could not honor would
// be a lie the first upload exposes.
func (s *session) offerDataPlane() string {
	if s.blobs == nil {
		return ""
	}

	return wire.DataPlaneURLs
}

// checkHello decides whether the shim that answered is one this run can use:
// the right protocol, the binary this run pushed, and a machine that can hold
// what a step's tree carries.
//
// Split out of greet because the three are one list of the same shape, while
// greet's own job is the exchange around them.
func (s *session) checkHello(ok wire.HelloOK, build string) error {
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

	// The shim may accept the offered compression or stay silent; a third
	// answer is a peer this end cannot decode, said now rather than as a
	// garbled transfer later.
	if ok.Compression != "" && ok.Compression != wire.CompressionZstd {
		return fmt.Errorf("%w: the worker's shim answered with compression %q, which this steps never offered",
			wire.ErrProtocol, ok.Compression)
	}

	// Same rule for the data plane: an echo is only valid for what was
	// offered, and nothing is offered without a store to mint URLs from.
	if ok.DataPlane != "" && ok.DataPlane != s.offerDataPlane() {
		return fmt.Errorf("%w: the worker's shim answered with data plane %q, which this steps never offered",
			wire.ErrProtocol, ok.DataPlane)
	}

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
		// Through the non-marking reader, and absorbing drains: a worker
		// already under a reclamation notice — the ordinary case on a redial,
		// since the eviction is why the last transport died — would otherwise
		// have its notice decoded as a zero HelloOK and be reported as
		// running the wrong binary.
		frame, err := s.awaitHandshakeFrame()
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
	//
	// Bounded, and off this goroutine, because it is the one frame write in
	// the package with no watchdog over it. A worker that stopped READING
	// blocks the encoder on backpressure, which no reader-side close can
	// unstick -- so writing it inline parked teardown forever, holding s.mu,
	// never reaching the transport.close that closeTimeout was built to
	// bound. interrupt is what unblocks a wedged write; see transport.
	byeOp := s.nextOp()
	sent := make(chan struct{})

	go func() {
		defer close(sent)

		_ = s.encoder.Write(wire.Frame{Type: wire.FrameBye, Op: byeOp})
	}()

	select {
	case <-sent:
	case <-ctx.Done():
		if s.transport.interrupt != nil {
			s.transport.interrupt()
		}

		<-sent
	}

	err := s.transport.close(ctx)
	s.transport = nil

	if err != nil {
		return fmt.Errorf("worker %q: %w", s.worker, err)
	}

	return nil
}

func (s *session) nextOp() uint32 {
	return s.op.Add(1)
}

// write sends one control frame, marking the conversation broken on a
// transport failure so ensure redials — see read.
func (s *session) write(frame wire.Frame, payload any) error {
	err := s.writeRaw(frame, payload)
	if err != nil {
		s.broken.Store(true)
	}

	return err
}

// writeRaw is write without the broken marking, for the handshake: a failure
// before the hello has been answered is an open failure, which sticks, and
// must not queue a redial.
func (s *session) writeRaw(frame wire.Frame, payload any) error {
	err := s.encoder.WriteJSON(frame.Type, frame.Op, payload)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// writeFrame sends a frame exactly as given — its Payload crosses verbatim,
// which is what the raw-byte frames (tree data, docker streams) need and what
// write() cannot do, since that one JSON-encodes its argument and ignores the
// frame's own payload.
func (s *session) writeFrame(frame wire.Frame) error {
	err := s.encoder.Write(frame)
	if err != nil {
		s.broken.Store(true)

		return fmt.Errorf("%w", err)
	}

	return nil
}

// reclaimedBy reports whether the worker said it is DEFINITELY going away,
// and what it said. An advisory rebalance recommendation answers false: it is
// worth printing and worth not sending new work to, but acting on it the way
// a reclamation is acted on would destroy a healthy machine.
func (s *session) reclaimedBy() (string, bool) {
	notice := s.drain.Load()
	if notice == nil || !notice.Terminal {
		return "", false
	}

	return notice.Reason, true
}

// noteDrain records a worker's announcement of its own end.
//
// Advisory in itself: it fails nothing. A spot notice gives about two
// minutes, so the command in flight often finishes, and a step that succeeded
// on a machine that later went away succeeded. What it changes is the
// CLASSIFICATION of a failure that follows.
//
// A terminal notice is never overwritten by a later advisory one: the machine
// does not become healthy again because a second, weaker message arrived.
func (s *session) noteDrain(frame wire.Frame) {
	notice := new(wire.Draining)

	err := decode(frame, notice)
	if err != nil {
		// A notice this end cannot read is still a worker saying something is
		// wrong, but not evidence of a reclamation — treating an unparseable
		// frame as one would arm the eviction path on a decode bug.
		notice = &wire.Draining{Reason: "an unreadable draining notice"}
	}

	// Compare-and-swap rather than load-then-store: two goroutines reach here
	// — the operation reader and readHello's detached one, which outlives the
	// handshake it was started for — so a plain store lets an advisory notice
	// clobber a terminal one that landed between the load and the store, and
	// the reclamation is then billed to the author's attempts: budget.
	for {
		previous := s.drain.Load()
		if previous != nil && previous.Terminal && !notice.Terminal {
			return
		}

		if s.drain.CompareAndSwap(previous, notice) {
			break
		}
	}

	reason := notice.Reason
	if reason == "" {
		reason = "no reason given"
	}

	kind := "is draining"
	if notice.Terminal {
		kind = "is being reclaimed"
	}

	// The deadline is said, not just carried. It is the one fact that tells an
	// operator whether the command in flight can finish inside the grace, and
	// the field's own contract is that it reaches them with the reason.
	when := ""
	if notice.Deadline != "" {
		when = " (expected gone by " + notice.Deadline + ")"
	}

	fmt.Printf("worker %s %s: %s%s\n", s.worker.Address(), kind, reason, when)
}

// read is readFrame, marking the conversation broken on a transport failure so
// ensure redials. Only a transport failure: an error frame is the shim
// ANSWERING — an operation that failed over a healthy pipe, which the shim's
// own loop survives — and redialling on it would abandon the worker's scratch
// and re-ship the tree for an error a fresh dial cannot fix. Every reader goes
// through here except readHello's goroutine, which can outlive an abandoned
// handshake: its late error must not un-stick an open failure ensure has
// already recorded.
func (s *session) read() (wire.Frame, error) {
	frame, err := s.readFrame()
	if errors.Is(err, errWorkerLost) {
		s.broken.Store(true)
	}

	return frame, err
}

// awaitHandshakeFrame is awaitOperationFrame for the handshake, reading
// through readFrame so a late answer cannot un-stick an open failure ensure
// has already recorded — see read.
func (s *session) awaitHandshakeFrame() (wire.Frame, error) {
	for {
		frame, err := s.readFrame()
		if err != nil || frame.Type != wire.FrameDraining {
			return frame, err
		}

		if frame.Op != wire.DrainOp {
			return wire.Frame{}, fmt.Errorf("%w: a draining notice arrived for operation %d rather than %d",
				wire.ErrProtocol, frame.Op, wire.DrainOp)
		}

		s.noteDrain(frame)
	}
}

// awaitOperationFrame returns the next frame that belongs to an operation,
// noting and swallowing any unsolicited draining notices before it.
//
// One place, for the same reason readFrame turns an error frame into a Go
// error here rather than at every call site: a drain notice arrives whenever
// the worker learns of its own end, which is to say during whatever happens
// to be in flight — a tree upload, a fetch acknowledgement, a command's
// output. Handled per call site, every reader that forgot it would report the
// notice as a protocol violation and poison the session with an invented
// error, on exactly the machines this feature exists for.
func (s *session) awaitOperationFrame() (wire.Frame, error) {
	for {
		frame, err := s.read()
		if err != nil {
			return frame, err
		}

		if frame.Type != wire.FrameDraining {
			return frame, nil
		}

		if frame.Op != wire.DrainOp {
			// A notice claiming an operation is a peer this end does not
			// understand; the op is what says "about the session" rather than
			// "about the thing you asked for".
			return wire.Frame{}, fmt.Errorf("%w: a draining notice arrived for operation %d rather than %d",
				wire.ErrProtocol, frame.Op, wire.DrainOp)
		}

		s.noteDrain(frame)
	}
}

// errWorkerLost is a transport that died mid-conversation, as opposed to an
// error frame a live shim sent over it.
var errWorkerLost = errors.New("the connection to the worker was lost")

// readFrame returns the next frame, turning an error frame from the shim into
// a Go error so callers never have to check for it.
func (s *session) readFrame() (wire.Frame, error) {
	frame, err := s.decoder.Read()
	if err != nil {
		// A transport that died mid-step is infrastructure, and saying so
		// explicitly is what keeps it from being read as a command's verdict.
		return wire.Frame{}, fmt.Errorf("%w: %w", errWorkerLost, err)
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
