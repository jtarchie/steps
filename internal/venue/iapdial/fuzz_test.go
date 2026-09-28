package iapdial

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// FuzzLengthPrefixed pins that an accepted string is exactly the bytes its prefix counts, and that a prefix claiming more than arrived is refused rather than read short.
func FuzzLengthPrefixed(f *testing.F) {
	f.Add([]byte{0, 0, 0, 3, 'a', 'b', 'c'})
	f.Add([]byte{0, 0, 0, 3, 'a', 'b', 'c', 'x'})
	f.Add([]byte{0, 0, 0, 4, 'a'})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0, 0})

	f.Fuzz(func(t *testing.T, body []byte) {
		got, err := lengthPrefixed(body)

		fits := len(body) >= 4 && int64(binary.BigEndian.Uint32(body)) <= int64(len(body)-4)
		if (err == nil) != fits {
			t.Fatalf("lengthPrefixed(%x): err = %v, want accepted = %v", body, err, fits)
		}

		if err == nil && !bytes.Equal(got, body[4:4+binary.BigEndian.Uint32(body)]) {
			t.Fatalf("lengthPrefixed(%x) = %x", body, got)
		}
	})
}

// FuzzChannelHandle drives the read loop's frame handler with whatever a relay could send, and holds both ledgers: outbound confirmations only ever rise and never pass what was sent, and inbound bytes reach Read exactly as the data frames carried them.
func FuzzChannelHandle(f *testing.F) {
	frame := func(tag uint16, body ...byte) []byte {
		return append(binary.BigEndian.AppendUint16(nil, tag), body...)
	}

	sid := frame(tagConnectSuccessSID, 0, 0, 0, 2, 's', 'd')
	data := frame(tagData, 0, 0, 0, 3, 'a', 'b', 'c')
	ack := frame(tagAck, 0, 0, 0, 0, 0, 0, 0, 5)

	f.Add(uint64(10), messages(sid, data, ack, data))
	f.Add(uint64(3), messages(sid, ack))
	f.Add(uint64(10), messages(data))
	f.Add(uint64(10), messages(sid, sid))
	f.Add(uint64(0), messages(sid, frame(0x7777), frame(tagReconnectSuccessAck)))

	f.Fuzz(func(t *testing.T, sent uint64, stream []byte) {
		// Past the ack window data would trigger a websocket write, and this channel has no socket.
		if len(stream) > ackWindow {
			return
		}

		channel := &Channel{
			delivered: make(chan []byte, len(stream)+1),
			connected: make(chan struct{}),
			stop:      make(chan struct{}),
			totalSent: sent,
		}

		var want, got []byte

		for len(stream) > 0 {
			size := min(int(stream[0]), len(stream)-1)
			message := stream[1 : 1+size]
			stream = stream[1+size:]

			sent := carried(channel, message)
			delivered, err := handleOne(t, channel, message)
			got = append(got, delivered...)
			want = append(want, sent...)

			if err != nil {
				// The read loop ends the session on the first error.
				break
			}
		}

		if !bytes.Equal(got, want) {
			t.Fatalf("Read would see %q, the relay sent %q", got, want)
		}

		if channel.totalReceived != uint64(len(got)) {
			t.Fatalf("inbound ledger %d, delivered %d bytes", channel.totalReceived, len(got))
		}
	})
}

// messages packs frames as FuzzChannelHandle splits them: a length byte, then the frame.
func messages(frames ...[]byte) []byte {
	var out []byte

	for _, frame := range frames {
		out = append(out, byte(len(frame))) //nolint:gosec // seed frames are a few bytes
		out = append(out, frame...)
	}

	return out
}

// carried is what a data frame puts on the stream, judged before handle runs: nothing, unless the connection is already confirmed and the frame parses.
func carried(channel *Channel, message []byte) []byte {
	if !channel.isConnected() || len(message) < tagLen || binary.BigEndian.Uint16(message) != tagData {
		return nil
	}

	payload, err := lengthPrefixed(message[tagLen:])
	if err != nil {
		return nil
	}

	return payload
}

// handleOne runs one frame through the channel, returning what reached Read and checking the outbound ledger across it.
func handleOne(t *testing.T, channel *Channel, message []byte) ([]byte, error) {
	t.Helper()

	confirmedBefore := channel.totalConfirmed

	err := channel.handle(message)
	if err != nil && !errors.Is(err, errProtocol) {
		t.Fatalf("handle(%x): %v is not a protocol error", message, err)
	}

	if channel.totalConfirmed < confirmedBefore || channel.totalConfirmed > channel.totalSent {
		t.Fatalf("ledger confirmed %d (was %d) of %d sent", channel.totalConfirmed, confirmedBefore, channel.totalSent)
	}

	var delivered []byte

	for {
		select {
		case payload := <-channel.delivered:
			delivered = append(delivered, payload...)
		default:
			return delivered, err
		}
	}
}
