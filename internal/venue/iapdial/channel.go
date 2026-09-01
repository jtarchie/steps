package iapdial

// The relay channel: a websocket carrying framed bytes, presented to a caller
// as a net.Conn so it can sit directly under an SSH client connection.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Channel is one relay session, as a byte pipe. Reads deliver what the far
// end wrote to the forwarded port; writes are delivered to it.
type Channel struct {
	ws *websocket.Conn

	// writeMu serializes websocket writes. A websocket connection permits one
	// writer at a time, and this end has three: the caller's Write, the read
	// loop's acknowledgements, and the ping timer.
	writeMu sync.Mutex

	// mu guards the closed flag and the byte accounting below.
	mu       sync.Mutex
	closed   bool
	closeErr error
	// totalSent and totalConfirmed are the outbound ledger: what Write has
	// framed, and what the relay's cumulative acks have confirmed. Kept only
	// to catch a relay confirming bytes that were never sent, or confirming
	// backwards — either means the two ends disagree about the stream, which
	// is a dead session rather than a shrug.
	totalSent      uint64
	totalConfirmed uint64

	// totalReceived and lastAcked are the inbound ledger, touched only by the
	// read loop: how much the relay has delivered, and how much of that this
	// end has acknowledged.
	totalReceived uint64
	lastAcked     uint64

	// sid is the relay's session id, from the CONNECT_SUCCESS_SID frame. Held
	// for diagnostics only — reconnect-by-sid is deliberately not implemented
	// (see the package doc).
	sid []byte

	// delivered carries payload bytes to Read.
	delivered chan []byte
	// pending is what a partially-consumed Read left over.
	pending []byte

	// connected closes when the relay confirms the backend connection, which
	// is when the pipe may carry data.
	connected   chan struct{}
	connectedOK sync.Once

	stop     chan struct{}
	stopOnce sync.Once
	loops    sync.WaitGroup

	// closeOnce makes Close idempotent, and that is a contract rather than a
	// nicety: the venue transport wires this one method to BOTH interrupt and
	// close, so every cancelled step calls it twice.
	closeOnce   sync.Once
	closeResult error
}

// errProtocol is a frame sequence the canonical client would refuse too.
var errProtocol = errors.New("the IAP relay broke protocol")

// errRelayClosed is a session the relay ended with an explanation.
var errRelayClosed = errors.New("the IAP relay closed the session")

// ErrBackendNotReached is the relay reporting it could not connect to the
// forwarded port. Exported because a caller that just created the instance
// knows something this package does not: sshd is probably still booting, and
// the dial is worth retrying.
var ErrBackendNotReached = errors.New("the IAP relay could not reach the forwarded port")

// Read returns bytes the far end wrote to the forwarded port.
func (c *Channel) Read(p []byte) (int, error) {
	if len(c.pending) == 0 {
		select {
		case chunk, ok := <-c.delivered:
			if !ok {
				return 0, c.err()
			}

			c.pending = chunk
		case <-c.stop:
			// Drain anything already delivered before reporting the end: a
			// session that closed immediately after its last write still owes
			// those bytes to the caller.
			select {
			case chunk := <-c.delivered:
				c.pending = chunk
			default:
				return 0, c.err()
			}
		}
	}

	n := copy(p, c.pending)
	c.pending = c.pending[n:]

	return n, nil
}

// Write sends bytes to the forwarded port, in frames no larger than the
// relay's window unit.
func (c *Channel) Write(p []byte) (int, error) {
	for offset := 0; offset < len(p); offset += maxDataFrame {
		end := min(offset+maxDataFrame, len(p))
		chunk := p[offset:end]

		c.mu.Lock()

		if c.closed {
			cause := c.closeErr
			c.mu.Unlock()

			// net.ErrClosed, never io.EOF. EOF is the READER's answer, and
			// Close records it as the cause — handing it back from a WRITE
			// makes a dead tunnel read as an orderly goodbye at every layer
			// above that checks the sentinel.
			if cause == nil || errors.Is(cause, io.EOF) {
				return offset, fmt.Errorf("writing to the IAP relay: %w", net.ErrClosed)
			}

			return offset, cause
		}

		// Counted before the write reaches the wire, so an ack racing the
		// write's return can never look like a confirmation of unsent bytes.
		c.totalSent += uint64(len(chunk))
		c.mu.Unlock()

		frame := make([]byte, headerLen+len(chunk))
		binary.BigEndian.PutUint16(frame, tagData)
		binary.BigEndian.PutUint32(frame[tagLen:], uint32(len(chunk))) //nolint:gosec // at most maxDataFrame by the chunking above
		copy(frame[headerLen:], chunk)

		err := c.writeFrame(frame)
		if err != nil {
			return offset, err
		}
	}

	return len(p), nil
}

// writeFrame writes one frame, bounded — see writeTimeout.
func (c *Channel) writeFrame(frame []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))

	err := c.ws.WriteMessage(websocket.BinaryMessage, frame)
	if err != nil {
		return fmt.Errorf("writing to the IAP relay: %w", err)
	}

	return nil
}

// readLoop is the only reader of the websocket.
func (c *Channel) readLoop() {
	defer c.loops.Done()

	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			c.fail(closeExplanation(err))

			return
		}

		err = c.handle(data)
		if err != nil {
			c.fail(err)

			return
		}
	}
}

// closeExplanation turns the relay's websocket close code into something an
// operator can act on. The codes are not documented; these two are what the
// canonical client special-cases, and everything else keeps the relay's own
// words.
func closeExplanation(err error) error {
	var closeErr *websocket.CloseError

	if !errors.As(err, &closeErr) {
		return fmt.Errorf("the IAP relay connection ended: %w", err)
	}

	switch closeErr.Code {
	case websocket.CloseNormalClosure, websocket.CloseGoingAway:
		return io.EOF
	case 4003:
		// The relay reached the instance's network and nothing answered on
		// the port — or a firewall dropped it, which looks identical from
		// here. The firewall is the likelier miss on a fresh project.
		return fmt.Errorf("%w: nothing listening there, or no firewall rule allows the IAP range 35.235.240.0/20 to reach it (%w)", ErrBackendNotReached, err)
	case 4047:
		// "Failed to lookup instance": the relay's own directory has not
		// caught up with an instance created moments ago — observed against
		// the real relay on a machine that was RUNNING with its host keys
		// already published. Retryable for the same reason 4003 is; for an
		// instance that genuinely does not exist, the caller's retry window
		// bounds the wait and this text is the explanation it reports.
		return fmt.Errorf("%w: the relay could not find the instance — for a machine created moments ago this means not yet (%w)", ErrBackendNotReached, err)
	case 4004:
		return fmt.Errorf("%w: the access token needs reauthentication (%w)", errRelayClosed, err)
	default:
		return fmt.Errorf("%w: %w", errRelayClosed, err)
	}
}

// handle routes one inbound message. One frame per message; bytes trailing a
// parsed frame are discarded, as the canonical client discards them.
func (c *Channel) handle(data []byte) error {
	if len(data) < tagLen {
		return fmt.Errorf("%w: a %d-byte message is too short to carry a tag", errProtocol, len(data))
	}

	tag := binary.BigEndian.Uint16(data)
	body := data[tagLen:]

	switch tag {
	case tagConnectSuccessSID:
		return c.connectSuccess(body)
	case tagData:
		return c.data(body)
	case tagAck:
		return c.ack(body)
	case tagReconnectSuccessAck:
		// This end never dials the reconnect endpoint, so the relay
		// confirming one means the two ends disagree about what session this
		// is.
		return fmt.Errorf("%w: a RECONNECT_SUCCESS_ACK arrived on a connection that never asked to reconnect", errProtocol)
	default:
		// Unknown tags are discarded, as the canonical client discards them:
		// the relay may grow frame types, and a client that died on novelty
		// would break on the relay's schedule rather than its own.
		return nil
	}
}

// connectSuccess records the session id and opens the pipe.
//
// The body is a uint32-length-prefixed byte string, NOT a bare integer — the
// community Go clients misread this one, which only fails to matter because
// they never use the sid either. See the package doc.
func (c *Channel) connectSuccess(body []byte) error {
	select {
	case <-c.connected:
		return fmt.Errorf("%w: a second CONNECT_SUCCESS_SID arrived", errProtocol)
	default:
	}

	sid, err := lengthPrefixed(body)
	if err != nil {
		return fmt.Errorf("%w: reading the session id: %w", errProtocol, err)
	}

	c.mu.Lock()
	c.sid = append([]byte(nil), sid...)
	c.mu.Unlock()

	c.connectedOK.Do(func() { close(c.connected) })

	return nil
}

// data delivers one payload and acknowledges when enough has accumulated.
func (c *Channel) data(body []byte) error {
	if !c.isConnected() {
		return fmt.Errorf("%w: data arrived before the connection was confirmed", errProtocol)
	}

	payload, err := lengthPrefixed(body)
	if err != nil {
		return fmt.Errorf("%w: reading a data frame: %w", errProtocol, err)
	}

	if len(payload) == 0 {
		return nil
	}

	chunk := append([]byte(nil), payload...)

	select {
	case c.delivered <- chunk:
	case <-c.stop:
		return c.err()
	}

	// Only the read loop touches this ledger, so no lock guards it.
	c.totalReceived += uint64(len(payload))
	if c.totalReceived-c.lastAcked > ackWindow {
		frame := make([]byte, tagLen+8)
		binary.BigEndian.PutUint16(frame, tagAck)
		binary.BigEndian.PutUint64(frame[tagLen:], c.totalReceived)

		err := c.writeFrame(frame)
		if err != nil {
			return err
		}

		c.lastAcked = c.totalReceived
	}

	return nil
}

// ack checks the relay's cumulative confirmation against the outbound ledger.
func (c *Channel) ack(body []byte) error {
	if !c.isConnected() {
		return fmt.Errorf("%w: an ack arrived before the connection was confirmed", errProtocol)
	}

	if len(body) < 8 {
		return fmt.Errorf("%w: a %d-byte ack body is too short", errProtocol, len(body))
	}

	confirmed := binary.BigEndian.Uint64(body)

	c.mu.Lock()
	defer c.mu.Unlock()

	switch {
	case confirmed < c.totalConfirmed:
		return fmt.Errorf("%w: it confirmed %d bytes after already confirming %d", errProtocol, confirmed, c.totalConfirmed)
	case confirmed > c.totalSent:
		return fmt.Errorf("%w: it confirmed %d bytes when only %d were sent", errProtocol, confirmed, c.totalSent)
	}

	c.totalConfirmed = confirmed

	return nil
}

// lengthPrefixed reads a uint32-length-prefixed byte string, discarding
// whatever trails it.
func lengthPrefixed(body []byte) ([]byte, error) {
	if len(body) < 4 {
		return nil, fmt.Errorf("%d bytes is too short for a length prefix", len(body))
	}

	length := binary.BigEndian.Uint32(body)
	if int64(len(body)-4) < int64(length) {
		return nil, fmt.Errorf("the length prefix says %d bytes and %d follow", length, len(body)-4)
	}

	return body[4 : 4+length], nil
}

// isConnected reports whether the relay has confirmed the backend connection.
func (c *Channel) isConnected() bool {
	select {
	case <-c.connected:
		return true
	default:
		return false
	}
}

// SessionID is the relay's identifier for this session, for an error message
// that has to name it.
func (c *Channel) SessionID() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.sid
}

// ping holds idle timers open. The relay's own liveness signals are
// one-directional and do not keep a NAT or load-balancer entry alive on this
// side.
func (c *Channel) ping() {
	defer c.loops.Done()

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.writeMu.Lock()
			_ = c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			c.writeMu.Unlock()
		case <-c.stop:
			return
		}
	}
}

// fail records why the channel ended and wakes everything waiting on it.
func (c *Channel) fail(err error) {
	c.mu.Lock()

	if c.closeErr == nil {
		c.closeErr = err
	}

	c.closed = true
	c.mu.Unlock()

	c.stopOnce.Do(func() { close(c.stop) })
}

// err is why the channel ended, or io.EOF for an orderly finish.
func (c *Channel) err() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closeErr != nil {
		return c.closeErr
	}

	return io.EOF
}

// Close ends the session and waits for its goroutines.
func (c *Channel) Close() error {
	c.closeOnce.Do(func() { c.closeResult = c.shutdown() })

	return c.closeResult
}

func (c *Channel) shutdown() error {
	c.writeMu.Lock()
	// Best effort: a relay that already hung up cannot be told goodbye, and
	// saying so would replace the real error with a cleanup one.
	_ = c.ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second))
	c.writeMu.Unlock()

	c.fail(io.EOF)

	err := c.ws.Close()

	c.loops.Wait()

	if err != nil {
		return fmt.Errorf("closing the IAP relay connection: %w", err)
	}

	return nil
}

// The net.Conn remainder. An SSH client connection needs a net.Conn, and
// these are the parts of one a relay session does not really have.

// iapAddr names an end of the tunnel for net.Conn's address methods.
type iapAddr string

func (a iapAddr) Network() string { return "iap" }
func (a iapAddr) String() string  { return string(a) }

// LocalAddr names this end.
func (c *Channel) LocalAddr() net.Addr { return iapAddr("iap-client") }

// RemoteAddr names the relay end.
func (c *Channel) RemoteAddr() net.Addr { return iapAddr(subprotocol) }

// SetDeadline is accepted and ignored: the SSH client this conn exists to
// carry never sets deadlines, and the venue interrupts a session by Close,
// which unblocks both directions. Individual writes are already bounded by
// writeTimeout.
func (c *Channel) SetDeadline(time.Time) error { return nil }

// SetReadDeadline is accepted and ignored — see SetDeadline.
func (c *Channel) SetReadDeadline(time.Time) error { return nil }

// SetWriteDeadline is accepted and ignored — see SetDeadline.
func (c *Channel) SetWriteDeadline(time.Time) error { return nil }
