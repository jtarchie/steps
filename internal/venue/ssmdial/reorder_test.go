package ssmdial

// A frame that arrives after the stream has moved past it.

import (
	"errors"
	"testing"
)

// TestReassembleRefusesAFrameItAlreadyPassed pins that bytes the client
// cannot place are reported rather than dropped.
//
// The watermark is seeded from the FIRST frame seen and delivered
// immediately, so a frame that overtook an earlier one leaves that earlier
// one below the watermark — where it was indistinguishable from a
// retransmission and silently discarded. Silence is the wrong answer: the
// step's own output stream loses bytes with nothing anywhere saying so, and a
// truncated stream is read downstream as content.
//
// Reported instead, which kills the session and lets the step redial. The
// alternative — assuming the stream starts at a known sequence and buffering
// from there — would be better still, and is not something this port can
// assert: AWS has never specified it, and guessing a start that is wrong
// stalls every session forever rather than losing a rare frame.
func TestReassembleRefusesAFrameItAlreadyPassed(t *testing.T) {
	t.Parallel()

	channel := &Channel{inBuf: map[int64][]byte{}}

	// The stream opens at 7 as far as this client can tell, and delivers.
	run, err := channel.reassemble(&agentMessage{sequenceNumber: 7, payload: []byte("seven")})
	if err != nil {
		t.Fatalf("the first frame was refused: %v", err)
	}

	if len(run) != 1 || string(run[0]) != "seven" {
		t.Fatalf("run = %q, want the first frame delivered", run)
	}

	// A retransmission of what was just delivered is ordinary, and dropped.
	run, err = channel.reassemble(&agentMessage{sequenceNumber: 7, payload: []byte("seven")})
	if err != nil || len(run) != 0 {
		t.Errorf("a retransmission gave run=%q err=%v, want it dropped quietly", run, err)
	}

	// Frame 6 overtook: it belongs BEFORE everything delivered, and there is
	// no honest way to place it now.
	_, err = channel.reassemble(&agentMessage{sequenceNumber: 6, payload: []byte("six")})
	if !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("error = %v, want ErrOutOfOrder for a frame the stream has passed", err)
	}
}

// TestReassembleStillOrdersWithinTheStream pins the ordinary case the guard
// must not disturb: frames that arrive out of order but ahead of the
// watermark are buffered and released as a contiguous run.
func TestReassembleStillOrdersWithinTheStream(t *testing.T) {
	t.Parallel()

	channel := &Channel{inBuf: map[int64][]byte{}}

	run, err := channel.reassemble(&agentMessage{sequenceNumber: 0, payload: []byte("a")})
	if err != nil || len(run) != 1 {
		t.Fatalf("first frame: run=%q err=%v", run, err)
	}

	// 2 before 1: held, then both released when 1 lands.
	run, err = channel.reassemble(&agentMessage{sequenceNumber: 2, payload: []byte("c")})
	if err != nil || len(run) != 0 {
		t.Fatalf("out-of-order frame was not held: run=%q err=%v", run, err)
	}

	run, err = channel.reassemble(&agentMessage{sequenceNumber: 1, payload: []byte("b")})
	if err != nil {
		t.Fatalf("reassemble: %v", err)
	}

	if len(run) != 2 || string(run[0]) != "b" || string(run[1]) != "c" {
		t.Errorf("run = %q, want b then c released together", run)
	}
}
