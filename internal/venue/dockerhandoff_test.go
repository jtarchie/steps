package venue

// Getting the wire back from the docker router, when the worker will not
// give it back.

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/shell"
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
	session.relay = newTestRelay(session)

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

// countingReader records how many goroutines are inside Read at once. Two is
// the bug: wire.Decoder stamps a shared header, so a second reader tears the
// bytes the first one is mid-way through.
type countingReader struct {
	inner io.Reader

	mu   sync.Mutex
	now  int
	peak int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.mu.Lock()
	c.now++

	if c.now > c.peak {
		c.peak = c.now
	}

	c.mu.Unlock()

	read, err := c.inner.Read(p)

	c.mu.Lock()
	c.now--
	c.mu.Unlock()

	return read, err //nolint:wrapcheck // a pass-through reader
}

func (c *countingReader) counts() (inFlight, peak int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now, c.peak
}

// waitUntil polls until want reports true, failing rather than hanging.
func waitUntil(t *testing.T, bound time.Duration, what string, want func() bool) {
	t.Helper()

	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		if want() {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatalf("waited %s for %s", bound, what)
}

// relayClosed reports the relay having ended every stream, which route() does
// on its way out of a dead transport. It is the one signal available before
// and after this fix, so a test can wait for the router to be DONE rather
// than sleep and hope.
func relayClosed(relay *dockerRelay) bool {
	relay.mu.Lock()
	defer relay.mu.Unlock()

	return relay.closed
}

// stalledSession is deafSession with a reader that counts its readers.
func stalledSession(t *testing.T) (*session, *countingReader, *io.PipeWriter) {
	t.Helper()

	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })

	counter := &countingReader{inner: reader}

	session := &session{
		decoder: wire.NewDecoder(counter),
		encoder: wire.NewEncoder(io.Discard),
	}
	session.relay = newTestRelay(session)

	return session, counter, writer
}

// TestAStalledBracketRefusesASecondRouter is the worst of the cluster.
//
// A handoff that timed out returns with its router STILL blocked in the read,
// and nothing marked the relay as already-routing — so teardownContainer's own
// bracket, which runs while s.relay is still set, started a SECOND router on
// the same decoder. Measured: two goroutines on one wire.Decoder, and -race
// reporting the torn shared header directly — one router's io.ReadFull writing
// d.header while the other read d.header[0] to decide the frame type. Only one
// of the two could ever consume stopRouting's single echo, so the bracket that
// lost returned claiming the wire was handed back while a router still owned
// it.
func TestAStalledBracketRefusesASecondRouter(t *testing.T) {
	shrinkDockerHandoff(t)

	session, counter, writer := stalledSession(t)

	// The bracket a worker never answers: it gives up on its bound and leaves
	// its router in the read.
	_ = session.withDockerRouting(context.Background(), func() error { return nil })

	ran := false

	// The teardown's bracket — `docker rm -f`, over a wire the first one never
	// gave back.
	err := session.withDockerRouting(context.Background(), func() error {
		ran = true

		return nil
	})
	if err == nil {
		t.Error("the teardown's bracket reported success on a wire an earlier one never returned")
	}

	if ran {
		t.Error("the teardown ran its docker call over a wire it could not get, which no router was there to carry")
	}

	// Released so the orphan can end, and waited for, so nothing is still in
	// the read when the count below is taken.
	_ = writer.Close()

	waitUntil(t, 5*time.Second, "the router to leave the wire", func() bool {
		inFlight, _ := counter.counts()

		return inFlight == 0
	})

	if _, peak := counter.counts(); peak > 1 {
		t.Errorf("%d goroutines were on one decoder at once: they tear the header they share, and only one of them can consume the single echo that ends either", peak)
	}
}

// TestSettleWaitsForAStalledRouter pins wg's own stated contract — "wg covers
// every goroutine this relay starts".
//
// It did not cover a router: the three other goroutine sites each take a
// wg.Add and withDockerRouting's did not, and in the SHIPPED path (a
// containerized placed step, which opens the socket without a router) that is
// the ONLY router there is — so the counter never covered one at all. settle()
// then returned with a reader still on the wire, and stop() went on to remove
// the socket directory under it.
func TestSettleWaitsForAStalledRouter(t *testing.T) {
	shrinkDockerHandoff(t)

	session, _, writer := stalledSession(t)

	_ = session.withDockerRouting(context.Background(), func() error { return nil })

	settled := make(chan struct{})

	go func() { defer close(settled); session.relay.settle() }()

	select {
	case <-settled:
		t.Fatal("settle() returned with a router still on the wire — the socket directory goes next, and this reader's frames are still arriving")
	case <-time.After(200 * time.Millisecond):
	}

	_ = writer.Close()

	select {
	case <-settled:
	case <-time.After(5 * time.Second):
		t.Fatal("settle() never returned once the router ended")
	}
}

// TestAnOrphanedRouterDoesNotBreakTheNextConversation is the quietest of the
// three and the most expensive.
//
// The orphan's read eventually fails, and it marked the SESSION broken —
// after ensure() had already cleared that flag for the redial. Measured on a
// real dial: ensure() returned nil with broken=false, and the orphan then
// flipped it true on a healthy, freshly dialled session. Every later command
// redials from there, abandoning the worker's scratch and re-uploading the
// tree, which breaks the promise a placed image: step makes — that an agent
// which pip-installs in one call finds it in the next. session.read() already
// guards this exact hazard for readHello's detached goroutine; the router got
// no such treatment.
func TestAnOrphanedRouterDoesNotBreakTheNextConversation(t *testing.T) {
	shrinkDockerHandoff(t)

	session, _, writer := stalledSession(t)

	_ = session.withDockerRouting(context.Background(), func() error { return nil })

	if !session.broken.Load() {
		t.Fatal("a stalled handoff left the session usable — the wire is desynced either way")
	}

	// The redial, in the two moves ensure() and connect() make: a clean flag
	// and a new conversation. The orphan is still blocked on the OLD one.
	fresh, freshWriter := io.Pipe()
	t.Cleanup(func() { _ = freshWriter.Close() })

	session.mu.Lock()
	session.decoder = wire.NewDecoder(fresh)
	session.mu.Unlock()

	session.broken.Store(false)

	// And now the worker the orphan was waiting on goes away.
	_ = writer.Close()

	waitUntil(t, 5*time.Second, "the orphaned router to finish", func() bool {
		return relayClosed(session.relay)
	})

	if session.broken.Load() {
		t.Error("a router left over from a dead conversation marked the fresh one broken: every later command redials, abandoning the worker's scratch and re-shipping the whole tree")
	}
}

// newTestRelay is the relay a test drives directly, built the way
// openDockerSocket builds one — its decoder captured from the session it
// belongs to, not read back out of it later.
func newTestRelay(s *session) *dockerRelay {
	return &dockerRelay{session: s, decoder: s.decoder, conns: map[uint32]net.Conn{}, doneOp: 1}
}

// countingRunner is a container runner that records only whether its teardown
// was attempted.
type countingRunner struct {
	shell.Runner

	closes atomic.Int64
}

func (c *countingRunner) Close() error {
	c.closes.Add(1)

	return nil
}

// TestTeardownDoesNotRouteOverADeadConversation is the other end of the same
// bug.
//
// `docker rm -f` travels the forwarded socket like every other docker call,
// which is why the removal runs inside a routing bracket at all. On a
// conversation that has already died there is no bracket to be had: the wire
// answers nothing, and an earlier router may still own it — so trying puts a
// second reader on one decoder to run a command whose reply nobody can carry.
// Nothing sweeps a worker, so the container is said out loud instead.
func TestTeardownDoesNotRouteOverADeadConversation(t *testing.T) {
	shrinkDockerHandoff(t)

	session, counter, _ := stalledSession(t)

	inner := &countingRunner{}
	session.inner = inner
	session.broken.Store(true)

	session.teardownContainer()

	if inner.closes.Load() != 0 {
		t.Error("the teardown sent `docker rm -f` over a wire whose conversation had already died")
	}

	if _, peak := counter.counts(); peak > 0 {
		t.Errorf("%d goroutines went onto the decoder to tear down a session that was already broken", peak)
	}
}

// TestARedialDoesNotRaceTheGoroutinesItOutlives covers both halves of the
// codec seam, because both are crossed by goroutines that outlive their
// conversation.
//
// Writing: a relay's accept goroutine and one pump per open stream write
// through the session until the relay settles, while a redial replaces the
// encoder under them. The replacement was made under s.mu and the writes under
// writeMu, which is no lock in common.
//
// Reading: readHello's goroutine reads on past the handshake it was started
// for, and reached for s.decoder each time round rather than holding the one
// its own conversation opened — the same field connect() was replacing, under
// no lock at all.
func TestARedialDoesNotRaceTheGoroutinesItOutlives(t *testing.T) {
	t.Parallel()

	session := &session{
		encoder: wire.NewEncoder(io.Discard),
		decoder: wire.NewDecoder(strings.NewReader("")),
	}

	// The decoder the handshake's goroutine was started on, captured the way
	// readHello captures it.
	handshake := session.decoder

	stop := make(chan struct{})
	started := make(chan struct{})

	var (
		wg   sync.WaitGroup
		once sync.Once
	)

	for range 4 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for {
				select {
				case <-stop:
					return
				default:
				}

				_ = session.writeFrame(wire.Frame{Type: wire.FrameDockerData, Op: 1, Payload: []byte("x")})
				_, _ = session.awaitHandshakeFrame(handshake)

				once.Do(func() { close(started) })
			}
		}()
	}

	<-started

	// The redials those goroutines are outliving.
	for range 50 {
		session.adoptCodec(strings.NewReader(""), io.Discard)
	}

	close(stop)
	wg.Wait()
}
