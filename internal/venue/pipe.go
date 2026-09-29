package venue

// Piping a tree one worker holds into another worker's upload (steps#143).
//
// With no store to carry it, a remote input still does not land here: the
// consumer is offered it by digest, and only when it asks does this end open
// a session to the holder, send FrameGet, and relay the holder's data frames
// into the consumer's operation as they arrive — Concourse's web node piping
// one worker's stream into another's. Nothing is written down or decompressed
// on this machine; the consumer verifies the digest before it places a byte.

import (
	"context"
	"fmt"
	"io"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/wire"
)

// pipeRemoteArtifact offers one remote input and, if the consumer does not
// already hold it, pipes it from its holder. holders keeps one session per
// holder for the whole upload, so several inputs from one worker cost one
// dial — on aws:// that is an SSM bootstrap each.
//
// A failure mid-pipe fails the step, naming the input and both machines; the
// producing step is not re-run, as a lost holder is not in ensureLocal and a
// lost volume's worker is not in Concourse.
//
// ponytail: a consumer that took a tree is not recorded as a second holder,
// so N consumers on N workers each read it from the one holder, and a local
// reader after a piped consumer pulls it again. The upgrade is a list of
// holders in workspace.RemoteArtifact.
func (s *session) pipeRemoteArtifact(ctx context.Context, name string, input shell.RemoteInput, holders map[string]*session) error {
	// Already held — its own output, or an earlier pipe's: nothing crosses,
	// and the holder is never dialled.
	op, need, err := s.offerOnTunnel(wire.UploadArtifact{Name: name, Digest: input.Digest, Foreign: true})
	if err != nil || !need {
		return err
	}

	// Said, because the whole transfer runs before the step's first command
	// and a large one is otherwise a silent pause at step start.
	events.Note(ctx, events.NoteInfo, fmt.Sprintf("piping %q from worker %s to worker %s", name, holderAddress(input.Holder), s.worker.Address()))

	holdErr, sendErr := s.relayFromHolder(ctx, name, input, holders, op)
	if sendErr != nil {
		return fmt.Errorf("worker %s stopped receiving input %q from worker %s: %w", s.worker.Address(), name, holderAddress(input.Holder), sendErr)
	}

	// Ended whatever the holder did: the consumer is committed and reading,
	// and only an end brings its verdict back and keeps the conversation in
	// step. After a holder failure that verdict is the truncation this end
	// caused, and is dropped for the holder's own error.
	endErr := s.writeFrame(wire.Frame{Type: wire.FrameEnd, Op: op})
	if endErr == nil {
		endErr = s.awaitEnd(op, "the piped artifact")
	}

	if holdErr != nil {
		return fmt.Errorf("input %q is held by worker %s and could not be piped to worker %s: %w", name, holderAddress(input.Holder), s.worker.Address(), holdErr)
	}

	if endErr != nil {
		return fmt.Errorf("worker %s refused input %q piped from worker %s: %w", s.worker.Address(), name, holderAddress(input.Holder), endErr)
	}

	return nil
}

// relayFromHolder streams one held tree from its holder into the consumer's
// operation op, answering what the holder did wrong and what the consumer did
// separately, because they are different failures to name.
//
// ponytail: no buffer between the two. Backpressure runs end to end — a slow
// consumer slows the holder — and a bounded spill to disk would decouple them
// at the cost of the property this exists for; measure before adding one.
func (s *session) relayFromHolder(ctx context.Context, name string, input shell.RemoteInput, holders map[string]*session, op uint32) (holdErr, sendErr error) {
	holder, err := s.holderFor(ctx, input.Holder, holders)
	if err != nil {
		return err, nil
	}

	// The bytes are relayed as they are, never transcoded, so both ends have
	// to have agreed the same encoding — which an exact protocol makes them.
	if holder.compression != s.compression {
		return fmt.Errorf("%w: the holder speaks compression %q and the consumer %q", wire.ErrProtocol, holder.compression, s.compression), nil
	}

	stop := holder.watchTransfer(ctx)
	defer stop()

	hop := holder.nextOp()

	err = holder.write(wire.Frame{Type: wire.FrameGet, Op: hop}, wire.Get{Name: name, Digest: input.Digest})
	if err != nil {
		return err, nil
	}

	writer := &chunkWriter{send: s.writeFrame, op: op, buf: make([]byte, 0, wire.DataChunkBytes)}
	relay := &relayWriter{w: &tallyWriter{w: writer, n: &s.sentArtifactBytes}, holder: holder.transport}

	err = holder.pump(hop, relay)
	if relay.failed != nil {
		return nil, relay.failed
	}

	if err != nil {
		return err, nil
	}

	return nil, writer.flush()
}

// holderFor is the session to holder, dialled on first use.
func (s *session) holderFor(ctx context.Context, holder string, holders map[string]*session) (*session, error) {
	if open, ok := holders[holder]; ok {
		return open, nil
	}

	open, err := dialHolder(ctx, shell.RunnerSpec{Worker: holder, ArtifactStore: s.worker.ArtifactStore})
	if err != nil {
		return nil, err
	}

	holders[holder] = open

	return open, nil
}

// holderAddress is a holder URL without its credentials, for a note or an
// error that lands in the run record; as written if it does not parse, which dialling it
// will then say.
func holderAddress(holder string) string {
	worker, err := ParseWorker(holder)
	if err != nil {
		return holder
	}

	return worker.Address()
}

// relayWriter is the consumer's side of a pipe. Its first failure cuts the
// holder's transport, because pump drains a failed operation to its end to
// keep a reusable session in step — and a holder session is one-shot, so on
// a slow link that drain would hold a dead consumer's failure back for the
// rest of the tree.
type relayWriter struct {
	w      io.Writer
	holder *transport
	failed error
}

func (r *relayWriter) Write(p []byte) (int, error) {
	if r.failed != nil {
		return 0, r.failed
	}

	n, err := r.w.Write(p)
	if err != nil {
		r.failed = err

		if r.holder != nil && r.holder.interrupt != nil {
			r.holder.interrupt()
		}
	}

	return n, err //nolint:wrapcheck // a pass-through; the caller names both machines
}
