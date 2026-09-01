package iapdial

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/gorilla/websocket"
)

// Target names the port the relay should forward to.
type Target struct {
	Project  string
	Zone     string
	Instance string
	Port     int
}

// ConnectURL is the relay URL for a target, in the exact shape the canonical
// client builds. Always the first NIC and the public relay: nothing in the
// worker grammar can name another, and the tests that stand a relay in seam
// at Open with a literal URL rather than through here.
func ConnectURL(target Target) string {
	query := url.Values{}
	query.Set("project", target.Project)
	query.Set("port", strconv.Itoa(target.Port))
	query.Set("newWebsocket", "true")
	query.Set("zone", target.Zone)
	query.Set("instance", target.Instance)
	query.Set("interface", defaultInterface)

	return defaultEndpoint + "/connect?" + query.Encode()
}

// Open dials the relay and waits for it to confirm the backend connection,
// returning a channel ready to carry bytes. The token is an OAuth2 access
// token for a principal holding iap.tunnelInstances.accessViaIAP.
func Open(ctx context.Context, connectURL, token string) (*Channel, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: handshakeTimeout,
		Subprotocols:     []string{subprotocol},
	}

	header := http.Header{}
	header.Set("Origin", relayOrigin)
	header.Set("User-Agent", "steps")
	header.Set("Authorization", "Bearer "+token)

	conn, response, err := dialer.DialContext(ctx, connectURL, header)

	// Deferred rather than closed on each path: only the status is ever read,
	// and a third early return between the paths would otherwise leak.
	defer func() {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
	}()

	if err != nil {
		// The relay answers a principal it will not carry with a plain HTTP
		// status before any websocket exists, and the status alone reads as a
		// transport hiccup rather than the IAM answer it is.
		if response != nil && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
			return nil, fmt.Errorf("the IAP relay refused the connection (HTTP %d): the caller needs iap.tunnelInstances.accessViaIAP on the instance (roles/iap.tunnelResourceAccessor): %w", response.StatusCode, err)
		}

		return nil, fmt.Errorf("dialling the IAP relay: %w", err)
	}

	// Bounded before the first read, not after: a frame over the limit fails
	// once ReadMessage has already allocated it, so the cap has to live here
	// to mean anything.
	conn.SetReadLimit(readLimit)

	channel := &Channel{
		ws:        conn,
		delivered: make(chan []byte, 64),
		connected: make(chan struct{}),
		stop:      make(chan struct{}),
	}

	channel.loops.Add(2)

	go channel.readLoop()
	go channel.ping()

	err = channel.awaitConnected(ctx)
	if err != nil {
		_ = channel.Close()

		return nil, err
	}

	return channel, nil
}

// errConnect is a relay session that could not be established.
var errConnect = errors.New("the IAP relay did not confirm the connection")

// awaitConnected blocks until the relay confirms the backend connection, the
// session ends, or the caller gives up.
func (c *Channel) awaitConnected(ctx context.Context) error {
	select {
	case <-c.connected:
		return nil
	case <-c.stop:
		return fmt.Errorf("%w: %w", errConnect, c.err())
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", errConnect, ctx.Err())
	}
}
