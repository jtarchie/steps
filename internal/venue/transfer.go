package venue

// Moving the step's tree, in both directions.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/jtarchie/steps/internal/compress"
	"github.com/jtarchie/steps/internal/wire"
)

// upload sends the step's whole materialized tree once, when the session opens.
//
// Whole rather than declared-inputs-only: the tree is already exactly what the
// step is supposed to see — internal/workspace built it from the declared
// inputs and nothing else — so filtering again here would be a second, drifting
// opinion about the same question.
func (s *session) upload(ctx context.Context) error {
	if s.cwd == "" {
		return nil
	}

	stop := s.watchTransfer(ctx)
	defer stop()

	if s.dataplane == wire.DataPlaneURLs {
		return s.uploadViaStore(ctx)
	}

	names, err := treeArtifacts(s.cwd)
	if err != nil {
		return err
	}

	for _, name := range names {
		err = s.uploadArtifactOnTunnel(name)
		if err != nil {
			return err
		}
	}

	return nil
}

// uploadArtifactOnTunnel offers one artifact and sends it only if asked.
//
// The same grain the store plane uses, for the same measured reason: two
// steps of one job share their inputs and differ only in their outputs, so a
// whole-tree key never repeats and the worker pulls the same payload down
// again for every step. The tunnel is the slower of the two planes, which
// makes re-sending worse here, not better.
//
// Staged to a file first because the digest has to be known before anything
// is offered, and packing twice to avoid a temp file would read the whole
// tree twice instead.
func (s *session) uploadArtifactOnTunnel(name string) error {
	// The negotiated encoding, not always zstd: the shim unpacks with
	// whatever the hello agreed, and a legacy one agreed raw.
	digest, staged, err := s.packArtifactToFile(name, s.compression == wire.CompressionZstd)
	if staged != "" {
		defer func() { _ = os.Remove(staged) }()
	}

	if err != nil {
		return err
	}

	op := s.nextOp()

	err = s.write(wire.Frame{Type: wire.FrameUpload, Op: op},
		wire.Upload{Artifacts: []wire.UploadArtifact{{Name: name, Digest: digest}}})
	if err != nil {
		return err
	}

	answer, err := s.awaitOperationFrame()
	if err != nil {
		return err
	}

	if answer.Op != op {
		return s.desync("a type %d frame for operation %d answered an upload offer for %d",
			answer.Type, answer.Op, op)
	}

	// Already held: nothing crosses, which is the whole point.
	if answer.Type == wire.FrameEnd {
		return nil
	}

	if answer.Type != wire.FrameNeed {
		return s.desync("the worker answered a type %d frame to an upload offer", answer.Type)
	}

	return s.sendArtifact(op, staged)
}

// sendArtifact streams one staged artifact and waits for the acknowledgement.
func (s *session) sendArtifact(op uint32, staged string) error {
	file, err := os.Open(staged) //nolint:gosec // a temp file this process just wrote
	if err != nil {
		return fmt.Errorf("reading a staged artifact: %w", err)
	}

	defer func() { _ = file.Close() }()

	writer := &chunkWriter{
		send: s.writeFrame,
		op:   op,
		buf:  make([]byte, 0, wire.DataChunkBytes),
	}

	sent, err := io.Copy(writer, file)
	if err != nil {
		return fmt.Errorf("sending an artifact: %w", err)
	}

	s.sentArtifactBytes.Add(sent)

	err = writer.flush()
	if err != nil {
		return err
	}

	err = s.writeFrame(wire.Frame{Type: wire.FrameEnd, Op: op})
	if err != nil {
		return err
	}

	// Waited for, as the tree upload always was: a refusal — a full disk, a
	// read-only scratch — arrives as an error frame here rather than as a
	// failure in the first command, which has real side effects.
	return s.awaitEnd(op, "the artifact")
}

// fetch brings the named directories back into the local tree.
//
// It runs after EVERY command rather than once at the end of the step, and
// that timing is the whole point. A task's assert: files: is checked against
// the local step directory inside the retry loop, long before the workspace
// captures anything — so a design that synced only at capture time would make
// every assertion on a placed step fail with "does not exist". After a Run*
// method returns, the local tree reflects the worker, exactly as it would have
// if the command had run here.
func (s *session) fetch(ctx context.Context) error {
	if s.cwd == "" || (len(s.outputs) == 0 && !s.fetchAll) {
		return nil
	}

	stop := s.watchTransfer(ctx)
	defer stop()

	if s.dataplane == wire.DataPlaneURLs {
		return s.fetchViaStore(ctx)
	}

	// Staged, then swapped. Removing the destinations first and unpacking over
	// them would destroy the step's outputs the moment a transfer died — a
	// dropped connection, a truncated stream — and the retry that follows
	// would then re-upload the emptied tree and fail for an unrelated reason.
	// Local execution has no such window, and placement must not add one.
	//
	// Inside cwd deliberately: the swap below is a rename, which needs to stay
	// on one filesystem.
	//
	// BEFORE the ask, not after: a full disk here used to return with the
	// fetch frame already sent, so the shim answered an operation nobody read
	// and every later command met the leftovers as "a frame for operation N
	// arrived during operation N+1" — a poisoned session, for a local failure
	// the worker had nothing to do with.
	staging, err := os.MkdirTemp(s.cwd, ".steps-fetch-")
	if err != nil {
		return fmt.Errorf("staging the fetched outputs: %w", err)
	}

	defer func() { _ = os.RemoveAll(staging) }()

	op := s.nextOp()

	// Empty Paths asks the shim for the whole tree, which is what fetchAll
	// means; s.outputs is nil exactly then.
	err = s.write(wire.Frame{Type: wire.FrameFetch, Op: op}, wire.Fetch{Paths: s.outputs})
	if err != nil {
		return err
	}

	err = s.receive(op, staging)
	if err != nil {
		return err
	}

	return s.swapFetched(staging)
}

// swapFetched moves each fetched output into place, replacing what was there.
//
// Per output rather than all at once: an output the worker sent nothing for is
// left alone rather than emptied, which is what a step that declared an output
// and produced nothing already means everywhere else — a fact reported where
// outputs are checked, not a reason to delete the previous one here.
func (s *session) swapFetched(staging string) error {
	names := s.outputs

	if s.fetchAll {
		// Whatever the worker sent — the tree IS the output, so what the
		// worker no longer has goes too, or a retried in: would leave the
		// local tree a union of every attempt and the resource cache would
		// keep it under the version. The staging directory sits inside cwd
		// and is skipped by name.
		var err error

		names, err = treeArtifacts(staging)
		if err != nil {
			return err
		}

		err = s.removeAbsent(names, filepath.Base(staging))
		if err != nil {
			return err
		}
	}

	for _, name := range names {
		src := filepath.Join(staging, name)

		_, err := os.Lstat(src)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return fmt.Errorf("fetching %q back: %w", name, err)
		}

		dst := filepath.Join(s.cwd, name)

		err = os.RemoveAll(dst)
		if err != nil {
			return fmt.Errorf("clearing %q before replacing it: %w", name, err)
		}

		err = os.Rename(src, dst)
		if err != nil {
			return fmt.Errorf("replacing %q with what the worker sent: %w", name, err)
		}
	}

	return nil
}

// removeAbsent deletes the local top-level entries the worker did not send,
// bar the staging directory itself.
func (s *session) removeAbsent(sent []string, staging string) error {
	keep := map[string]bool{staging: true}
	for _, name := range sent {
		keep[name] = true
	}

	local, err := treeArtifacts(s.cwd)
	if err != nil {
		return err
	}

	for _, name := range local {
		if keep[name] {
			continue
		}

		err := os.RemoveAll(filepath.Join(s.cwd, name))
		if err != nil {
			return fmt.Errorf("removing %q, which the worker no longer has: %w", name, err)
		}
	}

	return nil
}

// receive unpacks one operation's data frames into the local tree.
func (s *session) receive(op uint32, into string) error {
	reader, writer := io.Pipe()

	var (
		wg        sync.WaitGroup
		unpackErr error
	)

	wg.Add(1)

	go func() {
		defer wg.Done()

		unpackErr = s.unpackTree(reader, into)
		// Closing with the error stops the frame loop below from blocking on a
		// pipe nobody is reading any more.
		_ = reader.CloseWithError(unpackErr)
	}()

	readErr := s.pump(op, writer)

	_ = writer.Close()

	wg.Wait()

	if readErr != nil {
		return readErr
	}

	if unpackErr != nil {
		return fmt.Errorf("unpacking what the worker sent back: %w", unpackErr)
	}

	return nil
}

// unpackTree reads one tar stream into a directory, through the negotiated
// compression.
func (s *session) unpackTree(r io.Reader, into string) error {
	//nolint:wrapcheck // receive wraps with the transfer's own context
	return compress.Unpack(r, s.compression == wire.CompressionZstd, func(r io.Reader) error {
		return wire.UnpackFetchedTree(r, into) //nolint:wrapcheck // receive wraps with the transfer's own context
	})
}

// pump forwards this operation's data frames into w until the transfer ends.
//
// A write that fails — the unpacker refusing an entry, a full disk — does not
// end the READING. The shim is still sending this operation's frames and will
// resume its loop after them, so a pump that returned early would leave them
// in the stream for the next operation to read as a protocol violation: the
// session would be poisoned by its own tidiness, every retry desynced, while
// the shim executed each retried command and had its answers misread. So the
// remaining frames are drained and discarded, exactly as the shim's own
// frameReader drains an upload it rejected, and the write error is returned
// once the stream is back on a frame boundary.
func (s *session) pump(op uint32, w io.Writer) error {
	var failed error

	for {
		frame, err := s.awaitOperationFrame()
		if err != nil {
			return err
		}

		if isDockerFrame(frame.Type) {
			continue
		}

		if frame.Op != op {
			return s.desync("a type %d frame for operation %d arrived during operation %d",
				frame.Type, frame.Op, op)
		}

		switch frame.Type {
		case wire.FrameEnd:
			return failed
		case wire.FrameData:
			if failed != nil {
				continue
			}

			_, err = w.Write(frame.Payload)
			if err != nil {
				failed = fmt.Errorf("%w", err)
			}
		case wire.FrameHello, wire.FrameHelloOK, wire.FrameUpload, wire.FrameExec,
			wire.FrameStdout, wire.FrameStderr, wire.FrameExit, wire.FrameFetch,
			wire.FrameCancel, wire.FrameError, wire.FrameBye, wire.FrameDraining,
			wire.FrameDockerOpen, wire.FrameDockerData, wire.FrameDockerClose, wire.FrameNeed:
			return fmt.Errorf("%w: a type %d frame interrupted a transfer", wire.ErrProtocol, frame.Type)
		}
	}
}

// chunkWriter turns writes into bounded data frames, so a cancel can be heard
// between chunks rather than queueing behind a whole tree.
type chunkWriter struct {
	// send is the session's serialized frame writer, which is also what marks
	// the conversation broken: a write that died mid-upload is the transport
	// gone, not the tree unreadable. The session's own writer rather than the
	// raw encoder because the docker relay put other goroutines on it, and
	// wire.Encoder stamps a shared header and payload buffer per frame.
	send func(wire.Frame) error
	op   uint32
	buf  []byte
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	written := len(p)

	for len(p) > 0 {
		room := wire.DataChunkBytes - len(w.buf)
		if room > len(p) {
			room = len(p)
		}

		w.buf = append(w.buf, p[:room]...)
		p = p[room:]

		if len(w.buf) == wire.DataChunkBytes {
			err := w.flush()
			if err != nil {
				return 0, err
			}
		}
	}

	return written, nil
}

func (w *chunkWriter) flush() error {
	if len(w.buf) == 0 {
		return nil
	}

	err := w.send(wire.Frame{Type: wire.FrameData, Op: w.op, Payload: w.buf})
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	w.buf = w.buf[:0]

	return nil
}
