package ssmdial

// The protocol, against a fake agent.
//
// What these tests can prove and what they cannot is worth stating plainly.
// They pin the wire FORMAT (a message this end writes is one the fake parses,
// and vice versa), the handshake sequence, acknowledgement and
// retransmission, in-order reassembly, and the shapes of failure. They cannot
// prove agreement with the real SSM agent, because the fake is this repo's
// own reading of the protocol — only a session against AWS can settle that.
// The fake is a regression net, not a conformance one.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// fakeAgent is the far end of a data channel: it performs the handshake, then
// echoes what it is sent, and records what it saw.
type fakeAgent struct {
	t *testing.T

	// refuseEncryption makes the handshake demand KMS encryption, which this
	// client must refuse rather than fake.
	demandEncryption bool
	// dropFirstData ignores the first data message without acknowledging it,
	// so a test can require the retransmission that follows.
	dropFirstData bool
	// scramble delivers the agent's own data messages out of order.
	scramble bool

	mu         sync.Mutex
	conn       *websocket.Conn
	seq        int64
	token      string
	received   [][]byte
	acked      []int64
	dropped    bool
	terminated bool
}

func (a *fakeAgent) SawTerminate() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.terminated
}

func (a *fakeAgent) Received() [][]byte {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([][]byte(nil), a.received...)
}

func (a *fakeAgent) Acked() []int64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]int64(nil), a.acked...)
}

// serve runs the fake until the client hangs up.
func (a *fakeAgent) serve(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		a.t.Errorf("upgrading: %v", err)

		return
	}

	defer func() { _ = conn.Close() }()

	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()

	// The first message is the token that authorizes the channel.
	_, opening, err := conn.ReadMessage()
	if err != nil {
		return
	}

	var open openChannelRequest

	err = json.Unmarshal(opening, &open)
	if err != nil {
		a.t.Errorf("the client did not open the channel with json: %v", err)

		return
	}

	a.mu.Lock()
	a.token = open.TokenValue
	a.mu.Unlock()

	a.handshake()
	a.pump()
}

// handshake asks for the actions this fake wants and waits for the answer.
func (a *fakeAgent) handshake() {
	actions := []requestedClientAction{{ActionType: actionSessionType, ActionParameters: map[string]string{"SessionType": "Port"}}}
	if a.demandEncryption {
		actions = append(actions, requestedClientAction{ActionType: actionKMSEncryption})
	}

	payload, err := json.Marshal(handshakeRequest{AgentVersion: "3.3.1000.0", RequestedClientActions: actions})
	if err != nil {
		a.t.Errorf("marshalling the handshake request: %v", err)

		return
	}

	a.write(msgOutputStreamData, payloadHandshakeRequest, payload)
}

// pump handles everything after the handshake request.
func (a *fakeAgent) pump() {
	for {
		_, data, err := a.conn.ReadMessage()
		if err != nil {
			return
		}

		message := new(agentMessage)

		err = message.unmarshal(data)
		if err != nil {
			a.t.Errorf("the client sent a message this end cannot parse: %v", err)

			return
		}

		switch {
		case message.messageType == msgAcknowledge:
			var content acknowledgeContent

			_ = json.Unmarshal(message.payload, &content)

			a.mu.Lock()
			a.acked = append(a.acked, content.SequenceNumber)
			a.mu.Unlock()
		case message.payloadType == payloadHandshakeResponse:
			a.onHandshakeResponse(message)
		case message.payloadType == payloadOutput:
			a.onData(message)
		case message.payloadType == payloadFlag:
			a.mu.Lock()
			a.terminated = a.terminated ||
				(len(message.payload) == 4 && binary.BigEndian.Uint32(message.payload) == flagTerminateSession)
			a.mu.Unlock()
		}
	}
}

// onHandshakeResponse completes the handshake, or closes the channel when the
// client refused an action — which is what a real agent does.
func (a *fakeAgent) onHandshakeResponse(message *agentMessage) {
	var response handshakeResponse

	err := json.Unmarshal(message.payload, &response)
	if err != nil {
		a.t.Errorf("the client sent an unparseable handshake response: %v", err)

		return
	}

	for _, action := range response.ProcessedClientActions {
		if action.ActionStatus != actionSuccess {
			payload, marshalErr := json.Marshal(channelClosedPayload{
				Output: "client reported action " + string(action.ActionType) + " unsupported",
			})
			if marshalErr != nil {
				a.t.Errorf("marshalling the close payload: %v", marshalErr)

				return
			}

			a.write(msgChannelClosed, payloadUndefined, payload)

			return
		}
	}

	a.write(msgOutputStreamData, payloadHandshakeComplete, nil)
}

// onData acknowledges and echoes one data message, exercising the drop and
// reorder behaviors a real channel has to survive.
func (a *fakeAgent) onData(message *agentMessage) {
	a.mu.Lock()
	drop := a.dropFirstData && !a.dropped

	if drop {
		a.dropped = true
	}

	a.mu.Unlock()

	if drop {
		// Neither acknowledged nor echoed: the client must retransmit.
		return
	}

	a.ack(message)

	a.mu.Lock()
	a.received = append(a.received, append([]byte(nil), message.payload...))
	a.mu.Unlock()

	a.write(msgOutputStreamData, payloadOutput, message.payload)

	if !a.scramble {
		return
	}

	// A gap, opened AFTER the stream is already flowing — the case that
	// actually happens, where a retransmitted or delayed frame arrives behind
	// a later one. The client must hold "|B" until "|A" fills the gap and
	// then deliver both in sequence.
	a.mu.Lock()
	gapSeq := a.seq
	a.seq++
	laterSeq := a.seq
	a.seq++
	a.mu.Unlock()

	a.writeSeq(msgOutputStreamData, payloadOutput, []byte("|B"), laterSeq)
	a.writeSeq(msgOutputStreamData, payloadOutput, []byte("|A"), gapSeq)
}

func (a *fakeAgent) ack(message *agentMessage) {
	payload, err := json.Marshal(acknowledgeContent{
		MessageType:         message.messageType,
		MessageID:           message.messageID.String(),
		SequenceNumber:      message.sequenceNumber,
		IsSequentialMessage: true,
	})
	if err != nil {
		a.t.Errorf("marshalling an acknowledgement: %v", err)

		return
	}

	out := newAgentMessage(time.Now())
	out.messageType = msgAcknowledge
	out.payloadType = payloadUndefined
	out.sequenceNumber = message.sequenceNumber
	out.flags = flagAck
	out.payload = payload

	a.send(out)
}

// write sends one message under the fake's own increasing sequence.
func (a *fakeAgent) write(kind messageType, payload payloadType, body []byte) {
	a.mu.Lock()
	seq := a.seq
	a.seq++
	a.mu.Unlock()

	a.writeSeq(kind, payload, body, seq)
}

func (a *fakeAgent) writeSeq(kind messageType, payload payloadType, body []byte, seq int64) {
	out := newAgentMessage(time.Now())
	out.messageType = kind
	out.payloadType = payload
	out.sequenceNumber = seq
	out.messageID = uuid.New()
	out.payload = body

	a.send(out)
}

func (a *fakeAgent) send(message *agentMessage) {
	data, err := message.marshal()
	if err != nil {
		a.t.Errorf("marshalling: %v", err)

		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	_ = a.conn.WriteMessage(websocket.BinaryMessage, data)
}

// openFake starts a fake agent and opens a channel to it.
func openFake(t *testing.T, agent *fakeAgent) (*Channel, *fakeAgent) {
	t.Helper()

	agent.t = t

	server := httptest.NewServer(http.HandlerFunc(agent.serve))
	t.Cleanup(server.Close)

	url := "ws" + strings.TrimPrefix(server.URL, "http")

	channel, err := Open(t.Context(), url, "test-token")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = channel.Close() })

	return channel, agent
}

// readAtLeast reads until it has n bytes or the channel ends.
func readAtLeast(t *testing.T, channel *Channel, n int) string {
	t.Helper()

	var got []byte

	buf := make([]byte, 4096)

	for len(got) < n {
		read, err := channel.Read(buf)
		if read > 0 {
			got = append(got, buf[:read]...)
		}

		if err != nil {
			t.Fatalf("Read after %q: %v", got, err)
		}
	}

	return string(got)
}

// TestChannelCarriesBytes is the contract: after a handshake, what goes in
// one end comes out the other, and the token that authorized the channel is
// the one StartSession minted.
func TestChannelCarriesBytes(t *testing.T) {
	t.Parallel()

	channel, agent := openFake(t, &fakeAgent{})

	_, err := channel.Write([]byte("hello agent"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := readAtLeast(t, channel, len("hello agent")); got != "hello agent" {
		t.Errorf("read %q, want %q", got, "hello agent")
	}

	agent.mu.Lock()
	token := agent.token
	agent.mu.Unlock()

	if token != "test-token" {
		t.Errorf("the channel opened with token %q, want the one the session minted", token)
	}

	if channel.AgentVersion() != "3.3.1000.0" {
		t.Errorf("AgentVersion = %q, want what the agent reported", channel.AgentVersion())
	}
}

// TestChannelAcknowledgesWhatItReceives pins the half of reliability the
// agent depends on: an unacknowledged message is retransmitted forever, so a
// client that does not acknowledge stalls a real session under load.
func TestChannelAcknowledgesWhatItReceives(t *testing.T) {
	t.Parallel()

	channel, agent := openFake(t, &fakeAgent{})

	_, err := channel.Write([]byte("ping"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	readAtLeast(t, channel, len("ping"))

	// The handshake request and the echoed data both had to be acknowledged.
	if len(agent.Acked()) < 2 {
		t.Errorf("the client acknowledged %d messages, want at least the handshake and the data", len(agent.Acked()))
	}
}

// TestChannelRetransmitsWhatWasNotAcknowledged is the other half: a message
// the agent never confirms has to be sent again, or a lost frame stalls the
// session forever.
func TestChannelRetransmitsWhatWasNotAcknowledged(t *testing.T) {
	t.Parallel()

	channel, agent := openFake(t, &fakeAgent{dropFirstData: true})

	_, err := channel.Write([]byte("resend me"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// The first copy is dropped; the retransmission is what arrives.
	if got := readAtLeast(t, channel, len("resend me")); got != "resend me" {
		t.Errorf("read %q, want the retransmitted payload", got)
	}

	if len(agent.Received()) == 0 {
		t.Error("the agent received nothing, so nothing was retransmitted")
	}
}

// TestChannelReordersWhatArrivesOutOfOrder pins in-sequence delivery: a
// delayed or retransmitted frame can arrive behind a later one, and a client
// that delivered frames as they landed would corrupt a tar stream silently —
// the failure mode nothing downstream could attribute.
//
// The gap is opened after the stream is already flowing, which is the case
// that occurs: the watermark is seeded from the first frame seen, so nothing
// can arrive "before the beginning".
func TestChannelReordersWhatArrivesOutOfOrder(t *testing.T) {
	t.Parallel()

	channel, _ := openFake(t, &fakeAgent{scramble: true})

	_, err := channel.Write([]byte("first"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := readAtLeast(t, channel, len("first|A|B")); got != "first|A|B" {
		t.Errorf("read %q, want the frames in sequence order", got)
	}
}

// TestChannelRefusesAnEncryptedSession pins the honest refusal: an account
// that requires KMS session encryption gets a handshake failure naming it,
// not a session that silently sends plaintext.
func TestChannelRefusesAnEncryptedSession(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{t: t, demandEncryption: true}

	server := httptest.NewServer(http.HandlerFunc(agent.serve))
	t.Cleanup(server.Close)

	_, err := Open(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), "test-token")
	if err == nil {
		t.Fatal("a session demanding KMS encryption was established")
	}

	if !errors.Is(err, ErrChannelClosed) && !errors.Is(err, errHandshake) {
		t.Errorf("error = %v, want the handshake or the channel close naming the refusal", err)
	}
}

// TestChannelReportsTheAgentsOwnGoodbye pins the one explanation an operator
// ever gets for a refused port: the agent's channel_closed output.
func TestChannelReportsTheAgentsOwnGoodbye(t *testing.T) {
	t.Parallel()

	channel, agent := openFake(t, &fakeAgent{})

	payload, err := json.Marshal(channelClosedPayload{Output: "Connection refused: 127.0.0.1:35207"})
	if err != nil {
		t.Fatal(err)
	}

	agent.write(msgChannelClosed, payloadUndefined, payload)

	buf := make([]byte, 64)

	for {
		_, err = channel.Read(buf)
		if err != nil {
			break
		}
	}

	if !strings.Contains(err.Error(), "Connection refused") {
		t.Errorf("error = %v, want the agent's own account of the close", err)
	}
}

// TestMessageRoundTripsItsWireFormat is the format itself, held still: every
// field a peer reads has to survive marshal and unmarshal unchanged. The
// UUID halves are the trap — the agent stores them swapped, and a codec that
// got that wrong would produce acknowledgements naming a message id no agent
// recognizes.
func TestMessageRoundTripsItsWireFormat(t *testing.T) {
	t.Parallel()

	original := newAgentMessage(time.UnixMilli(1_700_000_000_000))
	original.messageType = msgInputStreamData
	original.payloadType = payloadOutput
	original.sequenceNumber = 42
	original.flags = flagData
	original.payload = []byte("some payload")

	data, err := original.marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	decoded := new(agentMessage)

	err = decoded.unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.messageType != original.messageType ||
		decoded.payloadType != original.payloadType ||
		decoded.sequenceNumber != original.sequenceNumber ||
		decoded.flags != original.flags ||
		decoded.messageID != original.messageID ||
		string(decoded.payload) != string(original.payload) ||
		!decoded.createdDate.Equal(original.createdDate) {
		t.Errorf("round trip changed the message:\n before %+v\n after  %+v", original, decoded)
	}
}

// TestMessageRefusesATruncatedFrame pins that network data is treated as
// hostile: every offset is bounds-checked, so a short frame is an error
// rather than a panic on a machine nobody can attach a debugger to.
func TestMessageRefusesATruncatedFrame(t *testing.T) {
	t.Parallel()

	full := newAgentMessage(time.Now())
	full.messageType = msgOutputStreamData
	full.payloadType = payloadOutput
	full.payload = []byte("a payload that will be cut short")

	data, err := full.marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, cut := range []int{0, 1, headerLen - 1, headerLen, headerLen + 2, len(data) - 1} {
		decoded := new(agentMessage)

		err = decoded.unmarshal(data[:cut])
		if err == nil {
			t.Errorf("a frame truncated to %d bytes was accepted", cut)
		}
	}
}

// TestChannelIsAByteStream pins that a caller may read a stream in whatever
// sizes it likes: the channel is an io.Reader, not a message queue, and a
// short buffer must leave the rest pending rather than dropping it.
func TestChannelIsAByteStream(t *testing.T) {
	t.Parallel()

	channel, _ := openFake(t, &fakeAgent{})

	_, err := channel.Write([]byte("abcdefghij"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	var got []byte

	small := make([]byte, 3)

	for len(got) < len("abcdefghij") {
		n, readErr := channel.Read(small)
		got = append(got, small[:n]...)

		if readErr != nil {
			t.Fatalf("Read: %v", readErr)
		}
	}

	if string(got) != "abcdefghij" {
		t.Errorf("read %q in 3-byte reads, want the whole stream", got)
	}
}

var _ io.ReadWriteCloser = (*Channel)(nil)

// TestOpenFailsWhenTheServiceIsNotAWebsocket pins the shape of a stream URL
// that answers something other than a session.
func TestOpenFailsWhenTheServiceIsNotAWebsocket(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	_, err := Open(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), "test-token")
	if err == nil {
		t.Fatal("Open succeeded against a service that refused the upgrade")
	}
}

// TestOpenRespectsItsContext pins that a stream URL that accepts the socket
// and then says nothing does not hang a step forever.
func TestOpenRespectsItsContext(t *testing.T) {
	t.Parallel()

	// The handler blocks until the test lets it go: an upgraded connection is
	// hijacked, so its request context is never cancelled and the server
	// cannot end the handler on its own.
	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}

		defer func() { _ = conn.Close() }()

		// Read the open request and then answer nothing at all.
		_, _, _ = conn.ReadMessage()

		<-release
	}))

	// Registered before the release below, so cleanup runs in the order that
	// works: let the handler go, then stop the server.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	_, err := Open(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), "test-token")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the context's deadline", err)
	}
}

// TestChannelChunksLargeWrites pins the payload cap: every known client — the
// official plugin and the agent itself — stays at or under 1024 bytes per
// message, and the messaging service's own frame limit is undocumented. A
// larger frame would work against this fake and die only against production,
// which is exactly the class of failure these tests otherwise cannot see.
func TestChannelChunksLargeWrites(t *testing.T) {
	t.Parallel()

	channel, agent := openFake(t, &fakeAgent{})

	payload := bytes.Repeat([]byte("x"), 5*maxOutboundPayload+7)

	n, err := channel.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("Write = %d, %v; want the whole payload accepted", n, err)
	}

	if got := readAtLeast(t, channel, len(payload)); got != string(payload) {
		t.Fatal("the chunked payload did not reassemble")
	}

	received := agent.Received()
	if len(received) < 6 {
		t.Fatalf("the agent saw %d messages for a %d-byte write, want it chunked", len(received), len(payload))
	}

	for i, message := range received {
		if len(message) > maxOutboundPayload {
			t.Fatalf("message %d carries %d bytes, over every known client's cap", i, len(message))
		}
	}
}

// TestChannelSaysGoodbye pins the terminate flag: without it the session
// lingers server-side against the instance's concurrent-session limit, and
// the agent's port session blocks awaiting a reconnect that is never coming.
func TestChannelSaysGoodbye(t *testing.T) {
	t.Parallel()

	channel, agent := openFake(t, &fakeAgent{})

	_, err := channel.Write([]byte("work"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	readAtLeast(t, channel, len("work"))

	err = channel.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)

	for {
		if agent.SawTerminate() {
			return
		}

		if time.Now().After(deadline) {
			t.Fatal("the agent never saw the terminate flag; the session lingers server-side")
		}

		time.Sleep(10 * time.Millisecond)
	}
}
