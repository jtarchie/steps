package ssmdial

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// FuzzAgentMessageUnmarshal holds the network parser to what its callers assume of an accepted frame, and a full-header frame to surviving a trip back through marshal.
func FuzzAgentMessageUnmarshal(f *testing.F) {
	message := newAgentMessage(time.UnixMilli(1_700_000_000_000))
	message.messageType = msgOutputStreamData
	message.sequenceNumber = 7
	message.payloadType = payloadOutput
	message.payload = []byte("hello")

	wire, err := message.marshal()
	if err != nil {
		f.Fatal(err)
	}

	f.Add(wire)
	f.Add(wire[:headerLen])
	f.Add(append(bytes.Clone(wire), "trailing"...))

	closed := bytes.Clone(wire[:headerLen-4])
	binary.BigEndian.PutUint32(closed, headerLen-4)
	f.Add(append(closed, 0, 0, 0, 0))

	f.Fuzz(func(t *testing.T, data []byte) {
		var got agentMessage

		err := got.unmarshal(data)
		if err != nil {
			if !errors.Is(err, errMalformedMessage) {
				t.Fatalf("error %v does not wrap errMalformedMessage", err)
			}

			return
		}

		checkAcceptedMessage(t, data, &got)

		// A shorter header leaves payloadType unread, and marshal always writes one, so only the full header can round-trip.
		if got.headerLength == headerLen {
			checkMessageRoundTrip(t, data, &got)
		}
	})
}

func checkAcceptedMessage(t *testing.T, data []byte, got *agentMessage) {
	t.Helper()

	if got.headerLength < headerLen-4 || got.headerLength > headerLen {
		t.Fatalf("accepted header length %d", got.headerLength)
	}

	if int(got.payloadLength) != len(got.payload) || got.payloadLength > maxPayloadBytes {
		t.Fatalf("payload length %d, payload %d bytes", got.payloadLength, len(got.payload))
	}

	start := int(got.headerLength) + 4
	if !bytes.Equal(got.payload, data[start:start+len(got.payload)]) {
		t.Fatal("payload is not the bytes after the length field")
	}
}

func checkMessageRoundTrip(t *testing.T, data []byte, got *agentMessage) {
	t.Helper()

	again, err := got.marshal()
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}

	var back agentMessage

	err = back.unmarshal(again)
	if err != nil {
		t.Fatalf("a marshalled message does not parse: %v", err)
	}

	// The digest is recomputed from the payload, so it is the one field allowed to change.
	back.payloadDigest = got.payloadDigest
	if !reflect.DeepEqual(back, *got) {
		t.Fatalf("round trip changed the message:\n got %+v\nback %+v", got, back)
	}

	// Everything but the type padding and the digest is byte-exact: marshal rewrites both.
	end := headerLen + 4 + len(got.payload)
	for _, span := range [][2]int{{0, 4}, {36, 80}, {112, end}} {
		if !bytes.Equal(again[span[0]:span[1]], data[span[0]:span[1]]) {
			t.Fatalf("bytes %d..%d changed on the round trip", span[0], span[1])
		}
	}
}

// FuzzSwapUUIDHalves pins that one function serves both directions, and that the result is the wire order of the id.
func FuzzSwapUUIDHalves(f *testing.F) {
	f.Add(bytes.Repeat([]byte{0xab}, 16))
	f.Add([]byte("0123456789abcdef"))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) != 16 {
			return
		}

		swapped := swapUUIDHalves(data)
		if !bytes.Equal(swapUUIDHalves(swapped), data) {
			t.Fatalf("not an involution: %x", data)
		}

		if !bytes.Equal(swapped[:8], data[8:]) || !bytes.Equal(swapped[8:], data[:8]) {
			t.Fatalf("halves not exchanged: %x -> %x", data, swapped)
		}

		id := uuid.UUID(data)
		message := &agentMessage{headerLength: headerLen, messageID: id}

		out, err := message.marshal()
		if err != nil {
			t.Fatal(err)
		}

		var back agentMessage

		err = back.unmarshal(out)
		if err != nil || back.messageID != id {
			t.Fatalf("id %s came back as %s (%v)", id, back.messageID, err)
		}
	})
}

// FuzzReassemble feeds frames in any order and holds the stream to what a reader needs: consecutive sequence numbers from the first frame, each delivered once, and nothing buffered that could have been released.
func FuzzReassemble(f *testing.F) {
	f.Add(int64(0), []byte{4, 5, 6})
	f.Add(int64(100), []byte{6, 4, 5, 5, 3})
	f.Add(int64(-9), []byte{10, 8, 9, 7})

	f.Fuzz(func(t *testing.T, base int64, order []byte) {
		// Sequence numbers are the agent's counter from zero; near the int64 edges is not a stream anybody sends.
		if len(order) == 0 || !streamable(base) {
			return
		}

		channel := &Channel{inBuf: map[int64][]byte{}}
		first := base + int64(order[0]%32)
		seen := map[int64]bool{}
		next := first

		for _, b := range order {
			sequence := base + int64(b%32)
			payload := binary.BigEndian.AppendUint64(nil, uint64(sequence)) //nolint:gosec // a label, compared as bytes

			run, err := channel.reassemble(&agentMessage{sequenceNumber: sequence, payload: payload})
			if sequence < first {
				if !errors.Is(err, ErrOutOfOrder) {
					t.Fatalf("frame %d below the stream start %d: err = %v", sequence, first, err)
				}

				continue
			}

			if err != nil {
				t.Fatalf("frame %d: %v", sequence, err)
			}

			seen[sequence] = true
			next = checkRun(t, run, next)

			if seen[next] {
				t.Fatalf("frame %d is held although everything before it was delivered", next)
			}
		}

		for sequence := range channel.inBuf {
			if sequence < next {
				t.Fatalf("frame %d still buffered after the stream passed it", sequence)
			}
		}
	})
}

// checkRun holds one released run to consecutive frames from next, returning where the stream stands after it.
func checkRun(t *testing.T, run [][]byte, next int64) int64 {
	t.Helper()

	for _, chunk := range run {
		if got := int64(binary.BigEndian.Uint64(chunk)); got != next { //nolint:gosec // the label FuzzReassemble wrote
			t.Fatalf("delivered frame %d, want %d", got, next)
		}

		next++
	}

	return next
}

func streamable(base int64) bool { return base <= 1<<62 && base >= -(1<<62) }
