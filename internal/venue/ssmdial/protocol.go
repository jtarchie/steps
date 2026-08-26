// Package ssmdial speaks the AWS Systems Manager session data-channel
// protocol: a websocket to the SSM messaging service carrying framed agent
// messages, which for a port-forwarding session is a byte pipe to a port on
// the managed node.
//
// It is a PORT, not a dependency. The protocol has no published
// specification; it is defined by the amazon-ssm-agent source and by the
// official session-manager-plugin, and the three community Go clients each
// implement a different subset. Nothing is mature or library-shaped enough to
// import, so the parts this repo needs are written here, from
// github.com/alexbacchin/ssm-session-client (MIT, © 2020 Mike Morris, © 2026
// Alex Bacchin) with the wire format cross-checked against
// github.com/aws/session-manager-plugin (Apache-2.0, © Amazon.com).
//
// Deliberately narrow, and the narrowness is a design decision rather than an
// unfinished edge:
//
//   - BASIC port forwarding only. A client advertising version >= 1.1.70 asks
//     an agent >= 3.0.196.0 to multiplex several TCP streams over one channel
//     with smux; steps never wants that. A venue session is one byte pipe to
//     one shim, so the channel IS the pipe — no smux, no local listener, no
//     stream bookkeeping. Advertising 1.1.0 is what keeps the agent on the
//     simple path.
//   - No shell, SSH or RDP session types. Those are what the plugin is for.
//   - No KMS session encryption: the handshake reports that action
//     unsupported rather than pretending, so an account that REQUIRES
//     encrypted sessions fails at the handshake with a message saying so
//     instead of silently sending plaintext.
package ssmdial

import (
	"encoding/json"
	"time"
)

// messageType labels an agent message.
//
// https://github.com/aws/amazon-ssm-agent — agent/session/contracts/model.go
type messageType string

const (
	msgAcknowledge      messageType = "acknowledge"
	msgChannelClosed    messageType = "channel_closed"
	msgOutputStreamData messageType = "output_stream_data"
	msgInputStreamData  messageType = "input_stream_data"
	msgPausePublication messageType = "pause_publication"
	msgStartPublication messageType = "start_publication"
)

// messageFlag says where in the stream a message belongs. The first message
// of a channel carries flagSyn; acknowledgements carry flagAck.
type messageFlag uint64

const (
	flagData messageFlag = 0
	flagSyn  messageFlag = 1
	flagAck  messageFlag = 3
)

// payloadType is the format of an agent message's payload.
type payloadType uint32

const (
	payloadUndefined           payloadType = 0
	payloadOutput              payloadType = 1
	payloadHandshakeRequest    payloadType = 5
	payloadHandshakeResponse   payloadType = 6
	payloadHandshakeComplete   payloadType = 7
	payloadEncChallengeRequest payloadType = 8
	payloadFlag                payloadType = 10
)

// flagTerminateSession, sent as a Flag payload, tells the agent the session
// is over. Without it the session lingers server-side — visible in
// describe-sessions and counted against the instance's concurrent-session
// limit — until the service times it out, with the agent blocked waiting for
// a reconnect that is never coming.
const flagTerminateSession uint32 = 2

// actionType is a client action the agent asks for during the handshake.
type actionType string

const (
	actionKMSEncryption actionType = "KMSEncryption"
	actionSessionType   actionType = "SessionType"
)

// actionStatus answers one requested action.
type actionStatus int

const (
	actionSuccess     actionStatus = 1
	actionUnsupported actionStatus = 3
)

// clientVersion is what this client claims in its handshake response.
//
// Deliberately below 1.1.70: at or above it, an agent >= 3.0.196.0 switches
// the session to smux-multiplexed port forwarding. See the package doc — one
// venue session is one pipe, and the multiplexed path would be machinery
// serving a case steps does not have.
const clientVersion = "1.1.0"

// handshakeRequest is the agent's opening ask.
type handshakeRequest struct {
	AgentVersion           string                  `json:"AgentVersion"`
	RequestedClientActions []requestedClientAction `json:"RequestedClientActions"`
}

type requestedClientAction struct {
	ActionType       actionType `json:"ActionType"`
	ActionParameters any        `json:"ActionParameters"`
}

// handshakeResponse answers every requested action. The agent treats any
// non-success as a failed handshake and closes the session, which is exactly
// the behavior wanted for an action this client cannot honestly perform.
type handshakeResponse struct {
	ClientVersion          string                  `json:"ClientVersion"`
	ProcessedClientActions []processedClientAction `json:"ProcessedClientActions"`
	Errors                 []string                `json:"Errors"`
}

type processedClientAction struct {
	ActionType   actionType      `json:"ActionType"`
	ActionStatus actionStatus    `json:"ActionStatus"`
	ActionResult json.RawMessage `json:"ActionResult"`
	Error        string          `json:"Error"`
}

// acknowledgeContent names the message being acknowledged. The agent
// retransmits anything it does not see acknowledged, so every inbound data
// message gets one of these — duplicates included, or the agent never
// advances its window.
type acknowledgeContent struct {
	MessageType         messageType `json:"AcknowledgedMessageType"`
	MessageID           string      `json:"AcknowledgedMessageId"`
	SequenceNumber      int64       `json:"AcknowledgedMessageSequenceNumber"`
	IsSequentialMessage bool        `json:"IsSequentialMessage"`
}

// channelClosedPayload is the agent's goodbye, and Output is the only account
// of WHY a session ended — a port nothing listens on, a policy refusal.
type channelClosedPayload struct {
	MessageType   string `json:"MessageType"`
	MessageID     string `json:"MessageId"`
	DestinationID string `json:"DestinationId"`
	SessionID     string `json:"SessionId"`
	SchemaVersion int    `json:"SchemaVersion"`
	CreatedDate   string `json:"CreatedDate"`
	Output        string `json:"Output"`
}

// openChannelRequest is the first thing sent on a fresh websocket: the token
// StartSession minted, which is what authorizes this connection. ClientId
// and ClientVersion are marked required by the service's own schema — today
// it tolerates their absence, but "works because unenforced" is not a
// contract worth holding.
type openChannelRequest struct {
	MessageSchemaVersion string `json:"MessageSchemaVersion"`
	RequestID            string `json:"RequestId"`
	TokenValue           string `json:"TokenValue"`
	ClientID             string `json:"ClientId"`
	ClientVersion        string `json:"ClientVersion"`
}

// resendInterval is both how often the resend pass runs and how old an
// unacknowledged message must be before it is re-sent. Age-gated because the
// outbound window can hold hundreds of messages when a big write was chunked,
// and re-sending ALL of them every pass would flood the channel with copies
// of messages whose acks are simply still in flight.
const resendInterval = 200 * time.Millisecond

// maxOutboundPayload chunks what Write sends. Every known-working client —
// the official plugin and the agent itself pin StreamDataPayloadSize at 1024
// — stays at or under a kilobyte, and the messaging service's own frame
// limit is undocumented; a payload it drops would kill sessions only in
// production, precisely the class of failure the fake-agent tests cannot
// see. Matching the fleet is the only defensible number.
const maxOutboundPayload = 1024

// pingInterval keeps NAT and load-balancer idle timers open. Server-side
// keepalives are one-directional and do not hold a client-side timer open,
// which is why this end pings rather than relying on the service.
const pingInterval = 30 * time.Second
