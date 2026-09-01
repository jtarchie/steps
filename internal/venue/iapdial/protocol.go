// Package iapdial speaks the Cloud IAP TCP-forwarding relay protocol: a
// websocket to Google's tunnel relay carrying framed bytes, which is a byte
// pipe to a TCP port on a Compute Engine instance's VPC interface.
//
// It is a PORT, not a dependency. The protocol has no published
// specification; it is defined by gcloud's own client and is a lightly
// modified descendant of the Chromium Secure Shell relay ("SSH Relay v4",
// documented in the libapps tree). The parts this repo needs are written
// here from the gcloud Python implementation
// (googlecloudsdk/api_lib/compute/iap_tunnel_websocket*.py, Apache-2.0,
// © Google LLC) cross-checked against github.com/davidspek/go-iap-tunnel
// (Apache-2.0). The canonical client is the arbiter where they disagree —
// and they do: the community Go clients read CONNECT_SUCCESS_SID as a bare
// uint64, while gcloud reads a uint32-length-prefixed byte string, which is
// what the relay actually sends.
//
// Deliberately narrow, and the narrowness is a design decision rather than
// an unfinished edge:
//
//   - CONNECT only, never RECONNECT. gcloud resumes a dropped websocket by
//     session id, re-sending what the relay had not acknowledged. A venue
//     session that loses its transport is redialed a layer up with a fresh
//     shim and a re-sent tree, exactly as an aws:// session is — so resume
//     machinery here would be a second, worse copy of that. The cost is that
//     a mid-step websocket drop fails the running command; the venue's
//     redial boundary already owns that outcome.
//   - Instance targets only. The relay also fronts on-prem hosts through
//     BeyondCorp destination groups and Cloud Run workloads; steps dials
//     Compute Engine instances.
//
// Frames are big-endian, one per websocket binary message: a uint16 tag,
// then a tag-specific body. DATA and CONNECT_SUCCESS_SID carry a
// uint32-length-prefixed byte string; ACK and RECONNECT_SUCCESS_ACK carry a
// uint64 cumulative byte count. Bytes trailing a parsed frame are discarded,
// as the canonical client discards them.
package iapdial

import "time"

// Frame tags, from the canonical client.
const (
	tagConnectSuccessSID   uint16 = 0x0001
	tagReconnectSuccessAck uint16 = 0x0002
	tagData                uint16 = 0x0004
	tagAck                 uint16 = 0x0007
)

// tagLen and headerLen are the frame preamble sizes: every frame starts with
// a uint16 tag, and a DATA frame follows it with a uint32 payload length.
const (
	tagLen    = 2
	headerLen = tagLen + 4
)

// maxDataFrame is the largest DATA payload either end sends. The relay's
// window arithmetic is stated in this unit, so it is also the ack threshold's
// base.
const maxDataFrame = 16384

// ackWindow is how far the received-byte count may run ahead of the last ack
// before this end sends another. The canonical client waits at least two
// window sizes between acks to keep both ends off the CPU; matching it keeps
// this client inside behavior the relay is known to tolerate.
const ackWindow = 2 * maxDataFrame

// subprotocol is the websocket subprotocol the relay requires.
const subprotocol = "relay.tunnel.cloudproxy.app"

// relayOrigin is the Origin header the relay expects. A browser could never
// send it, which is the point: it marks the client as a tunneler rather than
// a page.
const relayOrigin = "bot:iap-tunneler"

// defaultEndpoint is the relay's public front door.
const defaultEndpoint = "wss://tunnel.cloudproxy.app/v4"

// defaultInterface is the instance NIC the relay connects through when the
// worker URL does not say otherwise.
const defaultInterface = "nic0"

// writeTimeout bounds one websocket write, for the same reason ssmdial's
// does: writeMu is what Close() has to take to interrupt a session, and an
// unbounded write parked inside gorilla would hold it forever — queueing the
// cancellation primitive behind the thing it exists to cancel.
const writeTimeout = 30 * time.Second

// pingInterval keeps NAT and load-balancer idle timers open from this side.
// The relay documents dropping sessions idle for an hour; a placed step's
// quiet compile phase must not read as idleness to anything in between.
const pingInterval = 30 * time.Second

// handshakeTimeout bounds the websocket upgrade itself.
const handshakeTimeout = 30 * time.Second

// readLimit bounds one inbound websocket message before it is allocated. A
// DATA frame is at most headerLen+maxDataFrame; the SID frame is small but
// unspecified, so the cap leaves it an order of magnitude of headroom rather
// than a tight fit.
const readLimit = 4 * maxDataFrame
