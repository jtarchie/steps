package ssmdial

// The data channel: a websocket carrying agent messages, presented to a
// caller as an ordinary byte pipe.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Channel is one SSM session, as a byte pipe. Reads deliver what the far end
// wrote to the forwarded port; writes are delivered to it.
type Channel struct {
	ws *websocket.Conn

	// writeMu serializes websocket writes. A websocket connection permits one
	// writer at a time, and this end has three: the caller's Write, the read
	// loop's acknowledgements, and the ping and resend timers.
	writeMu sync.Mutex

	// mu guards the sequence and retransmission state below.
	mu       sync.Mutex
	seq      int64
	synSent  bool
	unacked  map[int64]*agentMessage
	closed   bool
	closeErr error

	// delivered carries reassembled payload bytes to Read.
	delivered chan []byte
	// pending is what a partially-consumed Read left over.
	pending []byte

	// handshaked closes when the agent reports the handshake complete, which
	// is when the pipe may carry data.
	handshaked  chan struct{}
	handshakeOK sync.Once

	// inOrder reassembles the inbound stream: the agent may retransmit or
	// deliver out of order, and a client that dropped anything not strictly
	// increasing would silently lose bytes under load.
	inSeq        int64
	inSeqSeeded  bool
	inDelivered  bool
	inBuf        map[int64][]byte
	agentVersion string

	stop     chan struct{}
	stopOnce sync.Once
	loops    sync.WaitGroup

	// now is the clock, injectable so a test can hold message timestamps
	// still. Nothing in the protocol compares them.
	now func() time.Time
}

// ErrChannelClosed is a session the far end ended. Its message carries the
// agent's own account, which is the only explanation of a refused port or a
// terminated session that ever reaches this end.
var ErrChannelClosed = errors.New("the SSM session was closed")

// errHandshake is a session that could not be established.
var errHandshake = errors.New("the SSM session handshake failed")

// Open dials a session's stream URL and completes the handshake, returning a
// channel ready to carry bytes.
func Open(ctx context.Context, streamURL, token string) (*Channel, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second}

	conn, response, err := dialer.DialContext(ctx, streamURL, http.Header{})
	if err != nil {
		return nil, fmt.Errorf("dialling the SSM data channel: %w", err)
	}

	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}

	channel := &Channel{
		ws:         conn,
		unacked:    map[int64]*agentMessage{},
		delivered:  make(chan []byte, 64),
		handshaked: make(chan struct{}),
		inBuf:      map[int64][]byte{},
		stop:       make(chan struct{}),
		now:        time.Now,
	}

	err = channel.openDataChannel(token)
	if err != nil {
		_ = channel.Close()

		return nil, err
	}

	channel.loops.Add(3)

	go channel.readLoop()
	go channel.tick(resendInterval, channel.resendUnacked)
	go channel.tick(pingInterval, channel.ping)

	err = channel.awaitHandshake(ctx)
	if err != nil {
		_ = channel.Close()

		return nil, err
	}

	return channel, nil
}

// openDataChannel presents the session token, which is what authorizes this
// websocket to carry the session.
func (c *Channel) openDataChannel(token string) error {
	request := openChannelRequest{
		MessageSchemaVersion: "1.0",
		RequestID:            uuid.NewString(),
		TokenValue:           token,
	}

	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("opening the SSM data channel: %w", err)
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	err = c.ws.WriteMessage(websocket.TextMessage, payload)
	if err != nil {
		return fmt.Errorf("opening the SSM data channel: %w", err)
	}

	return nil
}

// awaitHandshake blocks until the agent says the session is established, the
// session ends, or the caller gives up.
func (c *Channel) awaitHandshake(ctx context.Context) error {
	select {
	case <-c.handshaked:
		return nil
	case <-c.stop:
		return fmt.Errorf("%w: %w", errHandshake, c.err())
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", errHandshake, ctx.Err())
	}
}

// AgentVersion is what the agent reported during the handshake, for an error
// message that has to explain a version-dependent behavior.
func (c *Channel) AgentVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.agentVersion
}

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
			// Drain anything already reassembled before reporting the end: a
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

// Write sends bytes to the forwarded port.
func (c *Channel) Write(p []byte) (int, error) {
	message := newAgentMessage(c.now())
	message.messageType = msgInputStreamData
	message.payloadType = payloadOutput
	message.payload = append([]byte(nil), p...)

	err := c.send(message, true)
	if err != nil {
		return 0, err
	}

	return len(p), nil
}

// send stamps sequence and flags, retains the message for retransmission when
// it is one the agent acknowledges, and writes it.
func (c *Channel) send(message *agentMessage, retain bool) error {
	c.mu.Lock()

	if c.closed {
		c.mu.Unlock()

		return c.closeErr
	}

	if message.messageType == msgAcknowledge {
		// An acknowledgement carries the sequence number it answers, is never
		// itself acknowledged, and must not claim the channel's opening SYN —
		// a Windows agent rejects a SYN-flagged ack and the handshake stalls.
		message.flags = flagAck
	} else {
		if !c.synSent {
			c.synSent = true
			c.seq = 0
			message.flags = flagSyn
		} else {
			c.seq++
			message.flags = flagData
		}

		message.sequenceNumber = c.seq

		if retain {
			c.unacked[message.sequenceNumber] = message
		}
	}

	c.mu.Unlock()

	return c.writeMessage(message)
}

// writeMessage renders and writes one message.
func (c *Channel) writeMessage(message *agentMessage) error {
	data, err := message.marshal()
	if err != nil {
		return err
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	err = c.ws.WriteMessage(websocket.BinaryMessage, data)
	if err != nil {
		return fmt.Errorf("writing to the SSM data channel: %w", err)
	}

	return nil
}

// readLoop is the only reader of the websocket.
func (c *Channel) readLoop() {
	defer c.loops.Done()

	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				err = io.EOF
			}

			c.fail(fmt.Errorf("the SSM data channel ended: %w", err))

			return
		}

		err = c.handle(data)
		if err != nil {
			c.fail(err)

			return
		}
	}
}

// handle routes one inbound message.
func (c *Channel) handle(data []byte) error {
	message := new(agentMessage)

	err := message.unmarshal(data)
	if err != nil {
		return err
	}

	switch message.messageType {
	case msgAcknowledge:
		c.clearAcked(message)

		return nil
	case msgPausePublication, msgStartPublication:
		// Ignored, as the official client ignores them: reliability here is
		// acknowledgement and retransmission, and withholding writes on a
		// pause loses data rather than pacing it.
		return nil
	case msgChannelClosed:
		return c.channelClosed(message)
	case msgOutputStreamData:
		return c.outputStream(message)
	case msgInputStreamData:
		// Sent by this end only.
		return nil
	default:
		return nil
	}
}

// outputStream handles the agent's data and control payloads.
func (c *Channel) outputStream(message *agentMessage) error {
	switch message.payloadType {
	case payloadOutput:
		return c.deliver(message)
	case payloadHandshakeRequest:
		return c.answerHandshake(message)
	case payloadHandshakeComplete:
		c.handshakeOK.Do(func() { close(c.handshaked) })

		return c.acknowledge(message)
	case payloadEncChallengeRequest:
		// Reachable only if the handshake reported KMS encryption supported,
		// which this client never does — so a challenge arriving means the
		// session is encrypted and this end cannot read it. Saying so beats
		// answering with plaintext the agent will reject.
		return fmt.Errorf("%w: the session requires KMS encryption, which this client does not implement", errHandshake)
	case payloadUndefined, payloadHandshakeResponse:
		return c.acknowledge(message)
	default:
		return c.acknowledge(message)
	}
}

// answerHandshake replies to the agent's opening request.
func (c *Channel) answerHandshake(message *agentMessage) error {
	err := c.acknowledge(message)
	if err != nil {
		return err
	}

	request := new(handshakeRequest)

	err = json.Unmarshal(message.payload, request)
	if err != nil {
		return fmt.Errorf("%w: %w", errHandshake, err)
	}

	c.mu.Lock()
	c.agentVersion = request.AgentVersion
	c.mu.Unlock()

	response := handshakeResponse{
		ClientVersion:          clientVersion,
		ProcessedClientActions: make([]processedClientAction, 0, len(request.RequestedClientActions)),
	}

	for _, action := range request.RequestedClientActions {
		processed := processedClientAction{ActionType: action.ActionType}

		switch action.ActionType {
		case actionSessionType:
			processed.ActionStatus = actionSuccess
		case actionKMSEncryption:
			// Honest refusal: see the package doc. The agent treats this as a
			// failed handshake and closes, which is the outcome an operator
			// can act on.
			processed.ActionStatus = actionUnsupported
			processed.Error = "steps does not implement KMS session encryption"
		default:
			processed.ActionStatus = actionUnsupported
		}

		response.ProcessedClientActions = append(response.ProcessedClientActions, processed)
	}

	payload, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("%w: %w", errHandshake, err)
	}

	reply := newAgentMessage(c.now())
	reply.messageType = msgInputStreamData
	reply.payloadType = payloadHandshakeResponse
	reply.payload = payload

	// Not retained for retransmission: the agent answers a handshake response
	// with handshake_complete rather than an acknowledgement, so a retained
	// copy would be re-sent forever.
	return c.send(reply, false)
}

// deliver acknowledges one data message and hands its payload on in sequence.
func (c *Channel) deliver(message *agentMessage) error {
	err := c.acknowledge(message)
	if err != nil {
		return err
	}

	for _, chunk := range c.reassemble(message) {
		if len(chunk) == 0 {
			continue
		}

		select {
		case c.delivered <- chunk:
		case <-c.stop:
			return c.err()
		}
	}

	return nil
}

// reassemble files one payload by sequence number and returns whatever
// contiguous run that completed — nothing, when it filled no gap.
func (c *Channel) reassemble(message *agentMessage) [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Seed the expected sequence from the first frame seen. Before anything
	// has been delivered, a lower number is an earlier frame that overtook
	// this one rather than a duplicate, so the watermark moves down to it;
	// once delivery has begun it only moves forward.
	switch {
	case !c.inSeqSeeded:
		c.inSeq = message.sequenceNumber
		c.inSeqSeeded = true
	case message.sequenceNumber < c.inSeq && !c.inDelivered:
		c.inSeq = message.sequenceNumber
	case message.sequenceNumber < c.inSeq:
		// Already delivered: a retransmission this end has seen. Acknowledged
		// by the caller so the agent stops, and dropped here.
		return nil
	}

	c.inBuf[message.sequenceNumber] = append([]byte(nil), message.payload...)

	var run [][]byte

	for {
		next, ok := c.inBuf[c.inSeq]
		if !ok {
			return run
		}

		run = append(run, next)
		delete(c.inBuf, c.inSeq)
		c.inSeq++
		c.inDelivered = true
	}
}

// acknowledge tells the agent a message landed, so it stops retransmitting.
func (c *Channel) acknowledge(message *agentMessage) error {
	content := acknowledgeContent{
		MessageType:         message.messageType,
		MessageID:           message.messageID.String(),
		SequenceNumber:      message.sequenceNumber,
		IsSequentialMessage: true,
	}

	payload, err := json.Marshal(content)
	if err != nil {
		return fmt.Errorf("acknowledging an SSM message: %w", err)
	}

	ack := newAgentMessage(c.now())
	ack.messageType = msgAcknowledge
	ack.payloadType = payloadUndefined
	ack.sequenceNumber = message.sequenceNumber
	ack.payload = payload

	return c.send(ack, false)
}

// clearAcked drops a message the agent confirmed. The acknowledged number is
// in the payload; the header's is not guaranteed to match it.
func (c *Channel) clearAcked(message *agentMessage) {
	sequence := message.sequenceNumber

	var content acknowledgeContent

	err := json.Unmarshal(message.payload, &content)
	if err == nil && content.MessageType != "" {
		sequence = content.SequenceNumber
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.unacked, sequence)
}

// channelClosed ends the session, carrying the agent's own explanation.
func (c *Channel) channelClosed(message *agentMessage) error {
	payload := new(channelClosedPayload)

	err := json.Unmarshal(message.payload, payload)
	if err != nil || payload.Output == "" {
		return ErrChannelClosed
	}

	return fmt.Errorf("%w: %s", ErrChannelClosed, payload.Output)
}

// resendUnacked re-sends everything the agent has not confirmed.
func (c *Channel) resendUnacked() {
	c.mu.Lock()

	pending := make([]*agentMessage, 0, len(c.unacked))
	for _, message := range c.unacked {
		pending = append(pending, message)
	}

	c.mu.Unlock()

	for _, message := range pending {
		err := c.writeMessage(message)
		if err != nil {
			c.fail(err)

			return
		}
	}
}

// ping holds idle timers open. Server-side keepalives are one-directional and
// do not keep a NAT or load-balancer entry alive on this side.
func (c *Channel) ping() {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	_ = c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
}

// tick runs fn on an interval until the channel stops.
func (c *Channel) tick(every time.Duration, fn func()) {
	defer c.loops.Done()

	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			fn()
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
	c.fail(io.EOF)

	c.writeMu.Lock()
	// Best effort: a service that already hung up cannot be told goodbye, and
	// saying so would replace the real error with a cleanup one.
	_ = c.ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second))
	c.writeMu.Unlock()

	err := c.ws.Close()

	c.loops.Wait()

	if err != nil {
		return fmt.Errorf("closing the SSM data channel: %w", err)
	}

	return nil
}
