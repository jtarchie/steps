package venue

// Getting the wire back from the docker router, when the worker will not
// give it back.

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/wire"
)

// shrinkDockerHandoff makes the bound testable. The real one is measured in
// tens of seconds because an ordinary handoff is a round trip to a machine
// that may be busy; what is worth proving is that it ends at all.
func shrinkDockerHandoff(t *testing.T) {
	t.Helper()

	previousBound, previousGrace := dockerHandoffTimeout, dockerHandoffGrace
	dockerHandoffTimeout, dockerHandoffGrace = 150*time.Millisecond, 20*time.Millisecond

	t.Cleanup(func() { dockerHandoffTimeout, dockerHandoffGrace = previousBound, previousGrace })
}

// deafSession is a session whose worker never answers: reads block forever,
// which is what a wedged machine looks like from here.
func deafSession(t *testing.T) *session {
	t.Helper()

	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })

	session := &session{
		decoder: wire.NewDecoder(reader),
		encoder: wire.NewEncoder(io.Discard),
	}
	session.relay = &dockerRelay{session: session, conns: map[uint32]net.Conn{}, doneOp: 1}

	return session
}

// TestDockerHandoffEndsWhenTheWorkerDoesNot pins that the wire is reclaimed
// on a bound rather than on the worker's cooperation.
//
// The router owns the connection while a containerized command runs, and it
// is handed back by asking the shim to answer a close. A worker that has
// stopped answering — the case this whole eviction machinery exists for —
// then left the step waiting forever, including a step that was already being
// cancelled. The command it was cancelling had finished; the wait had not.
func TestDockerHandoffEndsWhenTheWorkerDoesNot(t *testing.T) {
	shrinkDockerHandoff(t)

	session := deafSession(t)

	started := time.Now()

	err := session.withDockerRouting(context.Background(), func() error { return nil })
	if err == nil {
		t.Error("a handoff the worker never completed reported success")
	}

	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("withDockerRouting took %s — it waited on the worker rather than on a bound", elapsed)
	}

	// The connection is desynced: whatever the router did or did not read,
	// this session cannot be trusted with the next command.
	if !session.broken.Load() {
		t.Error("the session was left usable after a handoff that never completed")
	}
}

// TestDockerHandoffGivesUpSoonerWhenCancelled pins the cancellation half: a
// caller that has already given up should not wait out the full bound, which
// is the difference between a cancel that lands and one that is merely
// eventual.
func TestDockerHandoffGivesUpSoonerWhenCancelled(t *testing.T) {
	shrinkDockerHandoff(t)

	session := deafSession(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()

	_ = session.withDockerRouting(ctx, func() error { return nil })

	if elapsed := time.Since(started); elapsed >= dockerHandoffTimeout {
		t.Errorf("a cancelled handoff waited %s, the full bound — cancellation bought nothing", elapsed)
	}
}
