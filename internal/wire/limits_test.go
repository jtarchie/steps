package wire

import (
	"bytes"
	"strings"
	"testing"
)

// roundTrip writes a frame and reads it back, reporting what either end
// refused.
func roundTrip(t *testing.T, frame Frame) error {
	t.Helper()

	var buf bytes.Buffer

	err := NewEncoder(&buf).Write(frame)
	if err != nil {
		return err
	}

	got, err := NewDecoder(&buf).Read()
	if err != nil {
		return err
	}

	if got.Type != frame.Type || got.Op != frame.Op || len(got.Payload) != len(frame.Payload) {
		t.Fatalf("round trip changed the frame: type %d op %d %d bytes", got.Type, got.Op, len(got.Payload))
	}

	return nil
}

// TestFrameLimitsAcceptTheValueItself: a frame AT each bound is legal. The
// mutation sweep found every one of these boundaries unpinned, so a `>` that
// became `>=` refused exactly the largest legal frame and no test noticed.
func TestFrameLimitsAcceptTheValueItself(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		frame Frame
	}{
		{name: "the largest payload", frame: Frame{Type: FrameData, Op: 1, Payload: make([]byte, MaxFrameBytes)}},
		{name: "the largest op", frame: Frame{Type: FrameEnd, Op: MaxOp}},
		{name: "the first frame type", frame: Frame{Type: FrameHello, Op: 1}},
		{name: "the last frame type", frame: Frame{Type: FramePush, Op: 1}},
	} {
		err := roundTrip(t, tc.frame)
		if err != nil {
			t.Errorf("%s was refused: %v", tc.name, err)
		}
	}
}

// TestFrameLimitsRefuseOnePast is the other side of each bound, on the end
// that owns it: the encoder refuses what this process built, the decoder what
// a peer wrote.
func TestFrameLimitsRefuseOnePast(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	err := NewEncoder(&buf).Write(Frame{Type: FrameData, Op: 1, Payload: make([]byte, MaxFrameBytes+1)})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("a payload one byte over the limit was written: %v", err)
	}

	err = NewEncoder(&buf).Write(Frame{Type: FrameEnd, Op: MaxOp + 1})
	if err == nil || !strings.Contains(err.Error(), "does not fit") {
		t.Errorf("an op one past the field was written: %v", err)
	}

	for _, tc := range []struct {
		name   string
		header []byte
		want   string
	}{
		{name: "a type past the last", header: []byte{byte(FramePush) + 1, 0, 0, 1, 0, 0, 0, 0}, want: "unknown frame type"},
		{name: "type zero", header: []byte{0, 0, 0, 1, 0, 0, 0, 0}, want: "unknown frame type"},
		{name: "a length over the limit", header: []byte{byte(FrameData), 0, 0, 1, 0, 0x10, 0, 1}, want: "over the"},
	} {
		_, err = NewDecoder(bytes.NewReader(tc.header)).Read()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s was decoded: %v", tc.name, err)
		}
	}
}
