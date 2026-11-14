// Package dockerapi talks to a docker engine, and is the only package in this
// repo that holds an engine client.
//
// It exists because container execution used to spawn the `docker` binary, and
// a binary answers questions a library does not. Which daemon (the context
// store, not just DOCKER_HOST — see host.go) and which credentials (the
// config.json auths and the credential helpers — see auth.go) both arrived
// free with the CLI and stop being free the moment the daemon is reached any
// other way. Those answers live here rather than at the call sites, so there
// is one place that knows what "the docker CLI would have done" means.
//
// It is deliberately a separate package from internal/shell rather than a few
// more imports inside it. shell is reached by nearly everything and had been
// stdlib-only; an engine client in that leaf makes "stdlib only" false for
// every package downstream of it, which is the same reasoning that put an ssh
// client in internal/venue instead.
package dockerapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/moby/moby/client"
)

// Client is a connection to one docker daemon.
type Client struct {
	api  *client.Client
	host string
}

// errSSHHost is a daemon reached over ssh.
var errSSHHost = errors.New("an ssh:// docker host is not supported")

// New connects to a daemon. An empty host resolves the same way a docker CLI
// would; a non-empty one is used verbatim, which is how a venue aims a step at
// the socket it forwards to a worker.
//
// The client negotiates its API version with whatever daemon it finds, which
// this version does by default. That matters and is worth naming: the
// compiled-in version is newer than plenty of running daemons, and a pinned
// client would fail every call against an older one on the version rather
// than on anything about the request.
func New(host string) (*Client, error) {
	if host == "" {
		resolved, err := ResolveHost()
		if err != nil {
			return nil, err
		}

		host = resolved
	}

	// Refused rather than dialled: the engine client has no ssh transport, and a docker+ssh:// worker already is one — it forwards the daemon's socket over its own ssh connection, so the mapping is the one place an ssh daemon is spelled.
	if strings.HasPrefix(host, "ssh://") {
		return nil, fmt.Errorf("%q: %w; map a tag to docker+ssh://%s with --worker, which reaches that daemon by forwarding its socket, or point DOCKER_HOST at a local socket",
			host, errSSHHost, strings.TrimPrefix(host, "ssh://"))
	}

	api, err := client.New(client.WithHost(host))
	if err != nil {
		return nil, fmt.Errorf("connecting to the docker daemon at %s: %w", host, err)
	}

	return &Client{api: api, host: host}, nil
}

// NewDialer reaches a daemon through dial, for a socket with no address of its own here (one forwarded over ssh); name is what errors call it.
func NewDialer(name string, dial func(ctx context.Context) (net.Conn, error)) (*Client, error) {
	api, err := client.New(
		client.WithHost(DefaultHost),
		client.WithDialContext(func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) }),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to the docker daemon at %s: %w", name, err)
	}

	return &Client{api: api, host: name}, nil
}

// Host is the address this client talks to, for a message that has to say
// which daemon it means.
func (c *Client) Host() string { return c.host }

// Close releases the client's connections.
func (c *Client) Close() error {
	err := c.api.Close()
	if err != nil {
		return fmt.Errorf("closing the docker client: %w", err)
	}

	return nil
}

// Ping reports whether the daemon is actually reachable.
//
// Constructing a client does not connect — it only parses the address — so
// this is what a preflight check has to call. It is the `docker info` of the
// old shape, minus the payload nothing read.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.api.Ping(ctx, client.PingOptions{})
	if err != nil {
		return fmt.Errorf("docker daemon unreachable at %s: %w", c.host, err)
	}

	return nil
}

// Platform is the daemon's own OS and architecture, in Go's spelling (linux, arm64), for a record of where a step ran.
func (c *Client) Platform(ctx context.Context) (string, string, error) {
	// /version, not /info: its Arch is already GOARCH, where /info's is uname -m and would need a table that is never complete.
	version, err := c.api.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return "", "", fmt.Errorf("asking %s what it runs on: %w", c.host, err)
	}

	return version.Os, version.Arch, nil
}
