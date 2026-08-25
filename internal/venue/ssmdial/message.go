package ssmdial

// The agent message binary format — the one thing on this wire that must be
// byte-exact, because the far end is the SSM agent and there is no
// negotiation to fall back on.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// headerLen is the size of every field before the payload length. A
// channel_closed message arrives with a 112-byte header (no payload type),
// which is why validation accepts a small range rather than one value.
const headerLen = 116

// messageTypeLen is the fixed, space-padded width of the type field.
const messageTypeLen = 32

// maxPayloadBytes bounds a payload read off the wire. The service does not
// send anything near this; the cap exists so a corrupt length field cannot
// make this end allocate whatever a uint32 can express.
const maxPayloadBytes = 1 << 24

// agentMessage is one framed message, in the field order the wire uses.
//
// https://github.com/aws/amazon-ssm-agent — agent/session/contracts/agentmessage.go
type agentMessage struct {
	headerLength   uint32
	messageType    messageType
	schemaVersion  uint32
	createdDate    time.Time
	sequenceNumber int64
	flags          messageFlag
	messageID      uuid.UUID
	payloadDigest  []byte
	payloadType    payloadType
	payloadLength  uint32
	payload        []byte
}

// newAgentMessage builds a message ready for a payload.
func newAgentMessage(now time.Time) *agentMessage {
	return &agentMessage{
		headerLength:  headerLen,
		schemaVersion: 1,
		createdDate:   now,
		messageID:     uuid.New(),
	}
}

var errMalformedMessage = errors.New("malformed agent message")

// marshal renders the message in wire format.
func (m *agentMessage) marshal() ([]byte, error) {
	digest := sha256.Sum256(m.payload)
	m.payloadDigest = digest[:]
	m.payloadLength = uint32(len(m.payload)) //nolint:gosec // a payload this end built, bounded by the caller's own writes

	buf := new(bytes.Buffer)

	fields := []any{
		m.headerLength,
		paddedType(m.messageType),
		m.schemaVersion,
		m.createdDate.UnixMilli(),
		m.sequenceNumber,
		uint64(m.flags),
		swapUUIDHalves(m.messageID[:]),
		m.payloadDigest,
		uint32(m.payloadType),
		m.payloadLength,
		m.payload,
	}

	for _, field := range fields {
		err := binary.Write(buf, binary.BigEndian, field)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errMalformedMessage, err)
		}
	}

	return buf.Bytes(), nil
}

// unmarshal reads a message off the wire.
//
// Every offset is bounds-checked before it is read: this is data from a
// network peer, and a truncated frame must be an error rather than a panic on
// a machine nobody can attach a debugger to.
func (m *agentMessage) unmarshal(data []byte) error {
	if len(data) < headerLen {
		return fmt.Errorf("%w: %d bytes is shorter than a header", errMalformedMessage, len(data))
	}

	m.headerLength = binary.BigEndian.Uint32(data)
	if m.headerLength > headerLen || m.headerLength < headerLen-4 {
		return fmt.Errorf("%w: header length %d", errMalformedMessage, m.headerLength)
	}

	m.messageType = messageType(bytes.TrimSpace(bytes.TrimRight(data[4:36], "\x00")))
	m.schemaVersion = binary.BigEndian.Uint32(data[36:40])
	m.createdDate = time.UnixMilli(int64(binary.BigEndian.Uint64(data[40:48]))) //nolint:gosec // a timestamp is descriptive here, never a bound
	m.sequenceNumber = int64(binary.BigEndian.Uint64(data[48:56]))              //nolint:gosec // as sent by the agent
	m.flags = messageFlag(binary.BigEndian.Uint64(data[56:64]))

	id, err := uuid.FromBytes(swapUUIDHalves(data[64:80]))
	if err != nil {
		return fmt.Errorf("%w: %w", errMalformedMessage, err)
	}

	m.messageID = id
	m.payloadDigest = data[80 : 80+sha256.Size]

	// A 112-byte header is a channel_closed message, which carries no payload
	// type — reading one from those four bytes would read the payload length.
	if m.headerLength == headerLen {
		m.payloadType = payloadType(binary.BigEndian.Uint32(data[112:headerLen]))
	}

	lengthEnd := int(m.headerLength) + 4
	if len(data) < lengthEnd {
		return fmt.Errorf("%w: truncated before the payload length", errMalformedMessage)
	}

	m.payloadLength = binary.BigEndian.Uint32(data[m.headerLength:lengthEnd])
	if m.payloadLength > maxPayloadBytes {
		return fmt.Errorf("%w: payload length %d", errMalformedMessage, m.payloadLength)
	}

	payloadEnd := lengthEnd + int(m.payloadLength)
	if len(data) < payloadEnd {
		return fmt.Errorf("%w: payload is %d bytes, header claims %d",
			errMalformedMessage, len(data)-lengthEnd, m.payloadLength)
	}

	m.payload = data[lengthEnd:payloadEnd]

	return nil
}

// paddedType renders a message type as the fixed-width, space-padded field
// the wire uses.
func paddedType(t messageType) []byte {
	field := make([]byte, messageTypeLen)
	for i := range field {
		field[i] = ' '
	}

	copy(field, t)

	return field
}

// swapUUIDHalves exchanges a UUID's two 8-byte halves.
//
// Not a byte-order quirk of this codec: the SSM agent stores a UUID as two
// little-endian longs, so the halves appear swapped relative to RFC 4122
// order. Its own inverse, which is why one function serves both directions.
func swapUUIDHalves(data []byte) []byte {
	swapped := make([]byte, len(data))
	copy(swapped, data[8:])
	copy(swapped[len(data)-8:], data[:8])

	return swapped
}
