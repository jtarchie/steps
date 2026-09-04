package iapdial

// The fake relay: a real websocket server performing the connect handshake
// and the frame protocol, so every test here exercises the same reading of
// the wire format the client uses. The same-misreading risk that carries is
// why the real-relay conformance test exists (gcp_test.go).

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// relayConn is one accepted websocket, with helpers speaking the relay's side
// of the protocol.
type relayConn struct {
	t  *testing.T
	ws *websocket.Conn

	// received accumulates DATA payloads the client sent; acks holds the ack
	// values the client sent.
	mu       sync.Mutex
	received []byte
	acks     []uint64
}

func (r *relayConn) sendSID(sid []byte) {
	r.t.Helper()

	frame := make([]byte, tagLen+4+len(sid))
	binary.BigEndian.PutUint16(frame, tagConnectSuccessSID)
	binary.BigEndian.PutUint32(frame[tagLen:], uint32(len(sid))) //nolint:gosec // test sids are tiny
	copy(frame[tagLen+4:], sid)

	err := r.ws.WriteMessage(websocket.BinaryMessage, frame)
	if err != nil {
		r.t.Errorf("fake relay: sending the SID: %v", err)
	}
}

func (r *relayConn) sendData(payload []byte) {
	r.t.Helper()

	frame := make([]byte, headerLen+len(payload))
	binary.BigEndian.PutUint16(frame, tagData)
	binary.BigEndian.PutUint32(frame[tagLen:], uint32(len(payload))) //nolint:gosec // test payloads fit a frame
	copy(frame[headerLen:], payload)

	err := r.ws.WriteMessage(websocket.BinaryMessage, frame)
	if err != nil {
		r.t.Errorf("fake relay: sending data: %v", err)
	}
}

func (r *relayConn) sendAck(confirmed uint64) {
	r.t.Helper()

	frame := make([]byte, tagLen+8)
	binary.BigEndian.PutUint16(frame, tagAck)
	binary.BigEndian.PutUint64(frame[tagLen:], confirmed)

	err := r.ws.WriteMessage(websocket.BinaryMessage, frame)
	if err != nil {
		r.t.Errorf("fake relay: sending an ack: %v", err)
	}
}

func (r *relayConn) sendRaw(frame []byte) {
	r.t.Helper()

	err := r.ws.WriteMessage(websocket.BinaryMessage, frame)
	if err != nil {
		r.t.Errorf("fake relay: sending a raw frame: %v", err)
	}
}

// readFrames consumes client messages, filing DATA payloads and acks, until
// the connection ends.
func (r *relayConn) readFrames() {
	for {
		_, msg, err := r.ws.ReadMessage()
		if err != nil {
			return
		}

		if len(msg) < tagLen {
			continue
		}

		tag := binary.BigEndian.Uint16(msg)
		body := msg[tagLen:]

		r.mu.Lock()

		switch tag {
		case tagData:
			payload, err := lengthPrefixed(body)
			if err == nil {
				r.received = append(r.received, payload...)
			}
		case tagAck:
			if len(body) >= 8 {
				r.acks = append(r.acks, binary.BigEndian.Uint64(body))
			}
		}

		r.mu.Unlock()
	}
}

func (r *relayConn) close(code int, reason string) {
	r.t.Helper()

	_ = r.ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
}

// serveRelay starts a fake relay whose handler runs once per connection, and
// returns a connect URL for it. The handler owns the connection's script; the
// server closes what the handler leaves open.
func serveRelay(t *testing.T, handler func(*relayConn)) string {
	t.Helper()

	upgrader := websocket.Upgrader{
		Subprotocols: []string{subprotocol},
		// The relay's origin is bot:iap-tuneler by design, which the default
		// same-host check would refuse.
		CheckOrigin: func(*http.Request) bool { return true },
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if got := req.Header.Get("Origin"); got != relayOrigin {
			t.Errorf("Origin = %q, want %q", got, relayOrigin)
		}

		if got := req.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want a bearer test-token", got)
		}

		ws, err := upgrader.Upgrade(w, req, nil)
		if err != nil {
			t.Errorf("fake relay: upgrade: %v", err)

			return
		}

		defer func() { _ = ws.Close() }()

		handler(&relayConn{t: t, ws: ws})
	}))

	// Close waits for in-flight handlers. A WaitGroup the handler Add(1)s
	// to races its own Wait when a dial times out mid-handshake.
	t.Cleanup(server.Close)

	return "ws" + strings.TrimPrefix(server.URL, "http") + "/v4/connect?stub=true"
}

// dialFake opens a channel against a fake relay.
func dialFake(t *testing.T, handler func(*relayConn)) *Channel {
	t.Helper()

	connectURL := serveRelay(t, handler)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	channel, err := Open(ctx, connectURL, "test-token")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = channel.Close() })

	return channel
}

func TestBytesAreCarriedBothWays(t *testing.T) {
	t.Parallel()

	relay := make(chan *relayConn, 1)
	channel := dialFake(t, func(conn *relayConn) {
		conn.sendSID([]byte("session-1"))
		conn.sendData([]byte("from the far end"))
		relay <- conn
		conn.readFrames()
	})

	got := make([]byte, 16)

	_, err := io.ReadFull(channel, got)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if string(got) != "from the far end" {
		t.Fatalf("Read = %q, want %q", got, "from the far end")
	}

	_, err = channel.Write([]byte("from this end"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	conn := <-relay

	deadline := time.Now().Add(5 * time.Second)

	for {
		conn.mu.Lock()
		received := string(conn.received)
		conn.mu.Unlock()

		if received == "from this end" {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("relay received %q, want %q", received, "from this end")
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func TestReadsAreByteStreamNotMessages(t *testing.T) {
	t.Parallel()

	channel := dialFake(t, func(conn *relayConn) {
		conn.sendSID([]byte("sid"))
		conn.sendData([]byte("abc"))
		conn.sendData([]byte("defgh"))
		conn.readFrames()
	})

	// A 2-byte buffer forces partial consumption across frame boundaries.
	var assembled []byte

	buf := make([]byte, 2)

	for len(assembled) < 8 {
		n, err := channel.Read(buf)
		if err != nil {
			t.Fatalf("Read after %q: %v", assembled, err)
		}

		assembled = append(assembled, buf[:n]...)
	}

	if string(assembled) != "abcdefgh" {
		t.Fatalf("assembled %q, want %q", assembled, "abcdefgh")
	}
}

func TestWritesAreChunkedToTheFrameLimit(t *testing.T) {
	t.Parallel()

	frames := make(chan int, 16)
	channel := dialFake(t, func(conn *relayConn) {
		conn.sendSID([]byte("sid"))

		for {
			_, msg, err := conn.ws.ReadMessage()
			if err != nil {
				return
			}

			if len(msg) >= tagLen && binary.BigEndian.Uint16(msg) == tagData {
				payload, err := lengthPrefixed(msg[tagLen:])
				if err != nil {
					t.Errorf("fake relay: bad data frame: %v", err)

					return
				}

				frames <- len(payload)
			}
		}
	})

	payload := bytes.Repeat([]byte("x"), maxDataFrame+maxDataFrame/2)

	n, err := channel.Write(payload)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if n != len(payload) {
		t.Fatalf("Write = %d, want %d", n, len(payload))
	}

	first, second := <-frames, <-frames
	if first != maxDataFrame || second != maxDataFrame/2 {
		t.Fatalf("frame sizes = %d, %d — want %d, %d", first, second, maxDataFrame, maxDataFrame/2)
	}
}

func TestSessionIDIsLengthPrefixedNotAnInteger(t *testing.T) {
	t.Parallel()

	// 27 bytes: a sid a client misreading the frame as a uint64 cannot
	// survive — it would consume 8 bytes and complain about the rest, or
	// deliver a number instead of the id.
	sid := []byte("abcdefghijklmnopqrstuvwxyz!")

	channel := dialFake(t, func(conn *relayConn) {
		conn.sendSID(sid)
		conn.readFrames()
	})

	if !bytes.Equal(channel.sessionID(), sid) {
		t.Fatalf("sessionID = %q, want %q", channel.sessionID(), sid)
	}
}

func TestDataBeforeConnectSuccessFailsTheDial(t *testing.T) {
	t.Parallel()

	connectURL := serveRelay(t, func(conn *relayConn) {
		conn.sendData([]byte("too early"))
		conn.readFrames()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	_, err := Open(ctx, connectURL, "test-token")
	if err == nil {
		t.Fatal("Open succeeded against a relay that sent data before confirming")
	}

	if !strings.Contains(err.Error(), "before the connection was confirmed") {
		t.Fatalf("err = %v, want it to name the early data", err)
	}
}

func TestASecondConnectSuccessKillsTheSession(t *testing.T) {
	t.Parallel()

	proceed := make(chan struct{})
	channel := dialFake(t, func(conn *relayConn) {
		conn.sendSID([]byte("one"))
		// Held until the dial has returned, so the duplicate cannot race the
		// first frame into failing Open instead of the established session.
		<-proceed
		conn.sendSID([]byte("two"))
		conn.readFrames()
	})

	close(proceed)

	_, err := channel.Read(make([]byte, 1))
	if err == nil || !strings.Contains(err.Error(), "second CONNECT_SUCCESS_SID") {
		t.Fatalf("Read = %v, want the duplicate connect refused", err)
	}
}

func TestReceivedBytesAreAcknowledgedCumulatively(t *testing.T) {
	t.Parallel()

	relay := make(chan *relayConn, 1)
	channel := dialFake(t, func(conn *relayConn) {
		conn.sendSID([]byte("sid"))
		relay <- conn

		// Three full frames: 49152 bytes, crossing the 2-window threshold.
		for range 3 {
			conn.sendData(bytes.Repeat([]byte("y"), maxDataFrame))
		}

		conn.readFrames()
	})

	discard := make([]byte, 4096)

	total := 0
	for total < 3*maxDataFrame {
		n, err := channel.Read(discard)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}

		total += n
	}

	conn := <-relay

	deadline := time.Now().Add(5 * time.Second)

	for {
		conn.mu.Lock()
		acks := append([]uint64(nil), conn.acks...)
		conn.mu.Unlock()

		if len(acks) > 0 {
			// Cumulative: the value is a running total, not a delta.
			if acks[0] <= ackWindow || acks[0] > 3*maxDataFrame {
				t.Fatalf("first ack = %d, want a cumulative count above %d", acks[0], ackWindow)
			}

			return
		}

		if time.Now().After(deadline) {
			t.Fatal("the client never acknowledged received bytes")
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func TestAckAboveBytesSentKillsTheSession(t *testing.T) {
	t.Parallel()

	channel := dialFake(t, func(conn *relayConn) {
		conn.sendSID([]byte("sid"))
		conn.sendAck(999999)
		conn.readFrames()
	})

	_, err := channel.Read(make([]byte, 1))
	if err == nil || !strings.Contains(err.Error(), "confirmed 999999 bytes") {
		t.Fatalf("Read = %v, want the impossible ack refused", err)
	}
}

func TestAckMovingBackwardsKillsTheSession(t *testing.T) {
	t.Parallel()

	proceed := make(chan struct{})
	channel := dialFake(t, func(conn *relayConn) {
		conn.sendSID([]byte("sid"))
		<-proceed
		conn.sendAck(100)
		conn.sendAck(50)
		conn.readFrames()
	})

	_, err := channel.Write(bytes.Repeat([]byte("z"), 100))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	close(proceed)

	_, err = channel.Read(make([]byte, 1))
	if err == nil || !strings.Contains(err.Error(), "after already confirming") {
		t.Fatalf("Read = %v, want the backwards ack refused", err)
	}
}

func TestUnknownTagsAreDiscarded(t *testing.T) {
	t.Parallel()

	channel := dialFake(t, func(conn *relayConn) {
		conn.sendSID([]byte("sid"))
		conn.sendRaw([]byte{0x00, 0xFF, 1, 2, 3})
		conn.sendData([]byte("still here"))
		conn.readFrames()
	})

	got := make([]byte, 10)

	_, err := io.ReadFull(channel, got)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if string(got) != "still here" {
		t.Fatalf("Read = %q, want %q", got, "still here")
	}
}

func TestBytesTrailingAFrameAreDiscarded(t *testing.T) {
	t.Parallel()

	channel := dialFake(t, func(conn *relayConn) {
		conn.sendSID([]byte("sid"))

		payload := []byte("kept")
		trailer := []byte("dropped")
		frame := make([]byte, headerLen+len(payload), headerLen+len(payload)+len(trailer))
		binary.BigEndian.PutUint16(frame, tagData)
		binary.BigEndian.PutUint32(frame[tagLen:], uint32(len(payload))) //nolint:gosec // a 4-byte payload
		copy(frame[headerLen:], payload)
		frame = append(frame, trailer...)
		conn.sendRaw(frame)

		conn.sendData([]byte(" and alive"))
		conn.readFrames()
	})

	got := make([]byte, 14)

	_, err := io.ReadFull(channel, got)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if string(got) != "kept and alive" {
		t.Fatalf("Read = %q, want %q", got, "kept and alive")
	}
}

func TestNormalClosureReadsAsEOF(t *testing.T) {
	t.Parallel()

	channel := dialFake(t, func(conn *relayConn) {
		conn.sendSID([]byte("sid"))
		conn.sendData([]byte("last words"))
		conn.close(websocket.CloseNormalClosure, "")
	})

	got, err := io.ReadAll(channel)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if string(got) != "last words" {
		t.Fatalf("ReadAll = %q, want %q", got, "last words")
	}
}

func TestPortRefusalIsExplained(t *testing.T) {
	t.Parallel()

	channel := dialFake(t, func(conn *relayConn) {
		conn.sendSID([]byte("sid"))
		conn.close(4003, "failed to connect to backend")
	})

	_, err := channel.Read(make([]byte, 1))
	if err == nil || !strings.Contains(err.Error(), "35.235.240.0/20") {
		t.Fatalf("Read = %v, want the firewall range named", err)
	}
}

func TestInstanceLookupLagIsRetryable(t *testing.T) {
	t.Parallel()

	channel := dialFake(t, func(conn *relayConn) {
		conn.sendSID([]byte("sid"))
		conn.close(4047, "Failed to lookup instance")
	})

	_, err := channel.Read(make([]byte, 1))
	if !errors.Is(err, ErrBackendNotReached) {
		t.Fatalf("Read = %v, want ErrBackendNotReached so a fresh instance's dial retries", err)
	}
}

func TestReauthenticationIsExplained(t *testing.T) {
	t.Parallel()

	channel := dialFake(t, func(conn *relayConn) {
		conn.sendSID([]byte("sid"))
		conn.close(4004, "reauthentication required")
	})

	_, err := channel.Read(make([]byte, 1))
	if err == nil || !strings.Contains(err.Error(), "reauthentication") {
		t.Fatalf("Read = %v, want reauthentication named", err)
	}
}

func TestARefusedHandshakeNamesThePermission(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "permission denied", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	_, err := Open(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), "test-token")
	if err == nil || !strings.Contains(err.Error(), "iap.tunnelInstances.accessViaIAP") {
		t.Fatalf("Open = %v, want the missing permission named", err)
	}
}

func TestOpenHonorsItsContext(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})

	connectURL := serveRelay(t, func(*relayConn) {
		// Never confirm; hold the connection until the test ends.
		<-release
	})

	// Registered after serveRelay's own cleanup, so it runs FIRST (cleanups
	// are LIFO): the server's Close waits for this handler to return.
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	_, err := Open(ctx, connectURL, "test-token")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Open = %v, want the context deadline", err)
	}
}

func TestReconnectAckIsRefused(t *testing.T) {
	t.Parallel()

	channel := dialFake(t, func(conn *relayConn) {
		conn.sendSID([]byte("sid"))

		frame := make([]byte, tagLen+8)
		binary.BigEndian.PutUint16(frame, tagReconnectSuccessAck)
		conn.sendRaw(frame)

		conn.readFrames()
	})

	_, err := channel.Read(make([]byte, 1))
	if err == nil || !strings.Contains(err.Error(), "never asked to reconnect") {
		t.Fatalf("Read = %v, want the reconnect ack refused", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	channel := dialFake(t, func(conn *relayConn) {
		conn.sendSID([]byte("sid"))
		conn.readFrames()
	})

	first := channel.Close()
	second := channel.Close()

	if first != nil || second != nil {
		t.Fatalf("Close = %v then %v, want nil twice", first, second)
	}
}

func TestWriteAfterCloseReportsClosedNotEOF(t *testing.T) {
	t.Parallel()

	channel := dialFake(t, func(conn *relayConn) {
		conn.sendSID([]byte("sid"))
		conn.readFrames()
	})

	err := channel.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err = channel.Write([]byte("late"))
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Write = %v, want net.ErrClosed", err)
	}
}

func TestConnectURLCarriesTheTarget(t *testing.T) {
	t.Parallel()

	connectURL := ConnectURL(Target{
		Project:  "my-project",
		Zone:     "us-central1-a",
		Instance: "worker-1",
		Port:     22,
	})

	parsed, err := url.Parse(connectURL)
	if err != nil {
		t.Fatalf("parsing %q: %v", connectURL, err)
	}

	if parsed.Scheme != "wss" || parsed.Host != "tunnel.cloudproxy.app" || parsed.Path != "/v4/connect" {
		t.Fatalf("URL = %q, want the relay's connect endpoint", connectURL)
	}

	query := parsed.Query()

	want := map[string]string{
		"project":      "my-project",
		"zone":         "us-central1-a",
		"instance":     "worker-1",
		"interface":    "nic0",
		"port":         "22",
		"newWebsocket": "true",
	}
	for key, value := range want {
		if got := query.Get(key); got != value {
			t.Errorf("query %s = %q, want %q", key, got, value)
		}
	}
}
