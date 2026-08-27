// Package shim is the remote half of a tagged step.
//
// Nobody types it. The orchestrator pushes this binary to a worker, execs it
// as `steps _shim`, and the process's stdin and stdout ARE the protocol — so
// nothing on this path may write to stdout except frames.
//
// It knows nothing about pipelines: no config, no store, no workspace, no
// merkle. That ignorance is the contract. A worker runs a binary and a
// command in a directory; every decision about what the command means, what
// its output is worth, and whether the result may be cached stays on the
// orchestrator, where the pipeline is.
package shim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/jtarchie/steps/internal/compress"
	"github.com/jtarchie/steps/internal/wire"
)

// Options configure one served session.
type Options struct {
	// Build is this binary's content hash, echoed back so the orchestrator can
	// prove the shim it reached is the one it pushed.
	Build string
	// Root is where session scratch directories are made. Empty takes the
	// system temp dir.
	Root string
	// DockerSocket is the daemon this worker forwards to for a placed step
	// that names an image. Empty takes /var/run/docker.sock.
	//
	// This end's own setting, never the peer's: a socket path arriving on the
	// wire would let whoever reaches the listener pick which of the worker's
	// services to be connected to.
	DockerSocket string
}

// Serve runs one session over a byte pipe until the peer says goodbye or goes
// away.
//
// It takes a reader and a writer rather than a connection because the transport
// is not its business: today that is this process's stdin and stdout at the far
// end of an SSH exec channel, and a later venue that hands it an accepted
// socket does not change a line of what happens here.
func Serve(ctx context.Context, in io.Reader, out io.Writer, opts Options) (err error) {
	session := &session{
		decoder: wire.NewDecoder(in),
		encoder: wire.NewEncoder(out),
		opts:    opts,
		done:    make(chan struct{}),
	}

	// A forwarded socket outliving the session that opened it is the same
	// leak as a shim outliving its dial, one layer down.
	defer session.docker.closeAll()

	// The one thing this end says unasked: that the machine under it is going
	// away. Started before the first frame, because an eviction notice can
	// arrive at any moment and the orchestrator can only learn it from here.
	//
	// Its own cancellable context, not the session's: closing a channel
	// cannot abort a metadata request already in flight, so teardown would
	// wait out the HTTP timeout on every step.
	watchCtx, stopWatching := context.WithCancel(ctx)

	session.drains.Add(1)

	go session.watchForDrain(watchCtx)

	defer func() {
		stopWatching()
		close(session.done)
		session.drains.Wait()

		cleanupErr := session.cleanup()
		if err == nil {
			err = cleanupErr
		}
	}()

	return session.run(ctx)
}

type session struct {
	decoder *wire.Decoder
	// mu guards encoder: a command's output is pumped from its own goroutines
	// while the main loop may answer a cancel, and two writers interleaving
	// mid-frame would corrupt the stream rather than merely reorder it.
	mu      sync.Mutex
	encoder *wire.Encoder

	opts    Options
	workdir string
	keep    bool
	// compression is what the hello negotiated for tree transfers: the token
	// this shim echoed back, or empty for raw.
	compression string
	// dataplane is how tree bytes travel: wire.DataPlaneURLs when negotiated,
	// empty for the tunnel.
	dataplane string

	// cancels stops each command still in flight, keyed by its operation and
	// held under mu. A MAP rather than one registration, because the frame
	// loop deliberately keeps reading while a command runs — endCommand's own
	// comment says a second exec can register before the first finishes, and
	// with a single slot that second registration silently deregistered the
	// first, so a cancel aimed at it found nothing and timeout:, fail_fast
	// and race: ended nothing while the step ran to completion on the worker.
	cancels map[uint32]context.CancelFunc
	// running tracks commands still in flight, so a session cannot tear its
	// scratch out from under one on the way out.
	running sync.WaitGroup

	// done closes when the session ends, stopping the drain watcher; drains
	// waits for it, so a poll in flight cannot outlive Serve and write to a
	// closed connection.
	done   chan struct{}
	drains sync.WaitGroup
	// docker holds the forwarded docker-socket streams, by operation.
	docker dockerStreams
}

func (s *session) run(ctx context.Context) error {
	// A command outlives the frame that asked for it (see handle), so the
	// session must not return — and cleanup must not run — while one is still
	// writing to the tree it is about to remove.
	defer s.running.Wait()

	for {
		frame, err := s.decoder.Read()
		if errors.Is(err, io.EOF) {
			// The orchestrator is gone. This is the ordinary end of a session
			// — and, when it arrives mid-step, the only cancellation that
			// reliably works: sshd does not forward signals to an exec
			// channel, so a killed or disconnected orchestrator is heard here
			// as a closed stdin and nowhere else. The deferred cleanup takes
			// the scratch directory with it.
			return nil
		}

		if err != nil {
			return fmt.Errorf("shim: %w", err)
		}

		done, err := s.handle(ctx, frame)
		if err != nil {
			// An operation that failed is reported and the session continues:
			// the orchestrator decides whether one bad step ends the run. A
			// transport error, by contrast, has already returned above.
			sendErr := s.send(wire.FrameError, frame.Op, wire.Error{Message: err.Error()})
			if sendErr != nil {
				return sendErr
			}

			continue
		}

		if done {
			return nil
		}
	}
}

func (s *session) handle(ctx context.Context, frame wire.Frame) (bool, error) {
	switch frame.Type {
	case wire.FrameHello:
		return false, s.hello(frame)
	case wire.FrameUpload:
		return false, s.upload(ctx, frame)
	case wire.FrameExec:
		return false, s.startExec(ctx, frame)
	case wire.FrameFetch:
		return false, s.fetch(ctx, frame)
	case wire.FrameDockerOpen, wire.FrameDockerData, wire.FrameDockerClose:
		return false, s.handleDocker(ctx, frame)
	case wire.FrameCancel:
		s.cancelRunning(frame.Op)

		return false, nil
	case wire.FrameBye:
		return true, nil
	case wire.FrameHelloOK, wire.FrameStdout, wire.FrameStderr, wire.FrameExit,
		wire.FrameData, wire.FrameEnd, wire.FrameError, wire.FrameDraining:
		// Frames the shim sends, never receives.
		return false, fmt.Errorf("%w: a shim cannot answer a type %d frame", wire.ErrProtocol, frame.Type)
	default:
		return false, fmt.Errorf("%w: unknown frame type %d", wire.ErrProtocol, frame.Type)
	}
}

func (s *session) hello(frame wire.Frame) error {
	var hello wire.Hello

	err := wire.DecodeJSON(frame, &hello)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	// One hello per session. A second would rewrite the workdir a running
	// command's goroutine is reading — see startExec, which deliberately
	// leaves the frame loop free — launching the next command somewhere else
	// and pointing cleanup's RemoveAll at a directory this session never made.
	if s.workdir != "" {
		return errReopened
	}

	err = checkSessionName(hello.Session)
	if err != nil {
		return err
	}

	if hello.Protocol != wire.Protocol {
		return fmt.Errorf("%w: orchestrator speaks protocol %d, this shim speaks %d — the pushed binary is not the one that pushed it",
			wire.ErrProtocol, hello.Protocol, wire.Protocol)
	}

	s.keep = hello.Keep

	// Accepted only when understood: an unknown token is an orchestrator
	// newer than this binary, and the honest answer is silence — raw is the
	// floor both ends always share.
	if hello.Compression == wire.CompressionZstd {
		s.compression = wire.CompressionZstd
	}

	if hello.DataPlane == wire.DataPlaneURLs {
		s.dataplane = wire.DataPlaneURLs
	}

	// The orchestrator's mapping wins: it is the operator naming a disk on
	// THIS machine, and it is the same path the binary was pushed under.
	root := hello.Root
	if root == "" {
		root = s.opts.Root
	}

	if root == "" {
		root = os.TempDir()
	}

	// Named after the session rather than randomly, so a scratch directory
	// left behind by a crash says which run to blame.
	s.workdir = filepath.Join(root, "steps-shim", hello.Session, "work")

	err = os.MkdirAll(s.workdir, 0o700)
	if err != nil {
		return fmt.Errorf("making the work directory: %w", err)
	}

	// After MkdirAll, so this describes the filesystem the tree will really
	// land on rather than whichever ancestor happened to exist.
	fstype, free := fsInfo(s.workdir)

	return s.send(wire.FrameHelloOK, frame.Op, wire.HelloOK{
		Protocol:    wire.Protocol,
		Build:       s.opts.Build,
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
		Workdir:     s.workdir,
		Compression: s.compression,
		DataPlane:   s.dataplane,
		FSType:      fstype,
		FSFree:      free,
	})
}

// upload lands the step's tree in the work directory: fetched from the URL
// the frame carries under DataPlaneURLs, or read from FrameData frames until
// FrameEnd on the tunnel — unpacking as the bytes arrive rather than staging
// them, because a step's tree can be larger than the worker's memory.
func (s *session) upload(ctx context.Context, frame wire.Frame) error {
	if s.workdir == "" {
		return errUnopened
	}

	if s.dataplane == wire.DataPlaneURLs {
		err := s.downloadTree(ctx, frame)
		if err != nil {
			return fmt.Errorf("fetching the step tree: %w", err)
		}

		return s.sendEnd(frame.Op)
	}

	reader, done := s.dataReader(frame.Op)

	err := s.unpack(reader)

	// Drained BEFORE the acknowledgement, not after. The far end sends its
	// next operation as soon as it hears this one landed, so a drain that ran
	// later would read that operation's opening frame and throw it away as
	// leftovers — the step would then wait forever for a reply to a command
	// nobody kept.
	drainErr := done()

	if err != nil {
		return fmt.Errorf("unpacking the step tree: %w", err)
	}

	if drainErr != nil {
		return drainErr
	}

	// Acknowledged, because silence is indistinguishable from a refusal that
	// has not arrived yet. Without this the orchestrator could only check its
	// own writes, and would go on to run the step's command against a tree
	// this end never accepted.
	return s.sendEnd(frame.Op)
}

func (s *session) fetch(ctx context.Context, frame wire.Frame) error {
	if s.workdir == "" {
		return errUnopened
	}

	var fetch wire.Fetch

	err := wire.DecodeJSON(frame, &fetch)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	if s.dataplane == wire.DataPlaneURLs {
		err = s.uploadOutputs(ctx, fetch)
		if err != nil {
			return fmt.Errorf("shipping the step outputs: %w", err)
		}

		return s.sendEnd(frame.Op)
	}

	writer := s.dataWriter(frame.Op)

	err = s.pack(writer, fetch.Paths)
	if err != nil {
		return fmt.Errorf("packing the step outputs: %w", err)
	}

	err = writer.Flush()
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return s.sendEnd(frame.Op)
}

// unpack reads one tar stream into the work directory, through the negotiated
// compression. Strict rather than sniffing: after agreeing to zstd, raw bytes
// are a peer that forgot to compress, and accepting them would leave that bug
// shipping trees that only sometimes unpack.
func (s *session) unpack(reader io.Reader) error {
	//nolint:wrapcheck // both callers wrap with the operation's own context
	return compress.Unpack(reader, s.compression == wire.CompressionZstd, func(r io.Reader) error {
		return wire.UnpackTree(r, s.workdir) //nolint:wrapcheck // both callers wrap with the operation's own context
	})
}

// pack writes the named outputs as one tar stream, through the negotiated
// compression.
func (s *session) pack(writer io.Writer, paths []string) error {
	//nolint:wrapcheck // the caller wraps with the operation's own context
	return compress.Pack(writer, s.compression == wire.CompressionZstd, func(w io.Writer) error {
		return wire.PackPaths(w, s.workdir, paths) //nolint:wrapcheck // the caller wraps with the operation's own context
	})
}

// errUnopened is any operation arriving before a hello. It cannot happen with
// a client this repo wrote, which is exactly why it is worth answering
// explicitly rather than dereferencing an empty path.
var errUnopened = errors.New("no session: the orchestrator sent an operation before its hello")

// errReopened is a second hello on a session that already answered one.
var errReopened = errors.New("this session already had its hello")

// errBadSession is a session name that is not a single directory name.
var errBadSession = errors.New("the session name must be one directory name")

// checkSessionName refuses a name that would leave the root it is joined to.
//
// The name lands in the scratch path and cleanup removes that path's PARENT,
// so "../.." makes the shim delete a tree outside the root it was given.
// Validated rather than trusted because --listen serves whatever connects: the
// listener is unauthenticated by design, and under the aws:// bootstrap this
// process is root.
func checkSessionName(name string) error {
	if name == "" || name == "." || name == ".." || name != filepath.Base(name) {
		return fmt.Errorf("%w: %q", errBadSession, name)
	}

	return nil
}

func (s *session) send(frameType wire.FrameType, op uint32, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.encoder.WriteJSON(frameType, op, payload)
	if err != nil {
		return fmt.Errorf("shim: %w", err)
	}

	return nil
}

// sendEnd acknowledges one finished operation — the only payloadless frame
// this end ever initiates.
func (s *session) sendEnd(op uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.encoder.Write(wire.Frame{Type: wire.FrameEnd, Op: op})
	if err != nil {
		return fmt.Errorf("shim: %w", err)
	}

	return nil
}

func (s *session) sendData(frameType wire.FrameType, op uint32, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.encoder.Write(wire.Frame{Type: frameType, Op: op, Payload: payload})
	if err != nil {
		return fmt.Errorf("shim: %w", err)
	}

	return nil
}

// cleanup removes the session's scratch. It runs on every exit path, including
// the orchestrator vanishing mid-step, because a directory left on someone
// else's machine is the one failure nobody local ever sees.
func (s *session) cleanup() error {
	if s.workdir == "" || s.keep {
		return nil
	}

	// The parent, not the work directory: the session owns
	// <root>/steps-shim/<session>, and leaving the empty shell behind would
	// accumulate one directory per run forever.
	err := os.RemoveAll(filepath.Dir(s.workdir))
	if err != nil {
		return fmt.Errorf("removing the session scratch: %w", err)
	}

	return nil
}
