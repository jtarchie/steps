package dockerapi

// Which daemon.
//
// A docker CLI answers this from three places, and a library answers it from
// one. `client.FromEnv` reads DOCKER_HOST and stops; the CLI goes on to read
// DOCKER_CONTEXT, then the current context named in config.json, and looks
// that name up in its own on-disk store. That last rung is not exotic — it is
// what `docker context use colima` writes, and on a machine whose daemon runs
// in a VM the socket it names is under the user's home directory rather than
// in /var/run. Resolving only the environment there does not degrade, it
// fails completely: every containerized step goes looking for a socket that
// was never there and reports a daemon that is running fine.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultHost is the daemon a machine that says nothing else is assumed to
// have.
const DefaultHost = "unix:///var/run/docker.sock"

// defaultContextName is docker's name for "no context": it is written into
// config.json by `docker context use default` like any other name, but has no
// entry in the store, because what it selects IS the environment-and-platform
// answer the store exists to override.
const defaultContextName = "default"

// errNoDockerEndpoint is a context that exists but describes something that is
// not a docker daemon — a kubernetes-only context is a real thing to find in
// a store.
var errNoDockerEndpoint = errors.New("names no docker endpoint")

// ResolveHost answers which daemon this process should talk to, in the order
// a docker CLI would: DOCKER_HOST, then DOCKER_CONTEXT, then the current
// context in the configuration directory, then the platform default.
func ResolveHost() (string, error) {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return host, nil
	}

	name := selectedContext()
	if name == "" || name == defaultContextName {
		return DefaultHost, nil
	}

	return contextEndpoint(name)
}

// selectedContext returns the context name in force, or "" for none.
//
// A config.json that cannot be read or parsed is treated as absent rather than
// fatal: the file holds credentials and display preferences besides this, and
// refusing to run because one of those is malformed would be a worse answer
// than falling back to the environment the way a machine with no config at all
// does.
func selectedContext() string {
	if name := os.Getenv("DOCKER_CONTEXT"); name != "" {
		return name
	}

	raw, err := os.ReadFile(filepath.Join(configDir(), "config.json"))
	if err != nil {
		return ""
	}

	var config struct {
		CurrentContext string `json:"currentContext"`
	}

	err = json.Unmarshal(raw, &config)
	if err != nil {
		return ""
	}

	return config.CurrentContext
}

// contextEndpoint reads a context's docker endpoint out of the store.
//
// A named context that is not there is an ERROR, not a fall back to the
// default socket. The fallback is the dangerous answer: a store that was moved
// or half-copied would report "cannot connect to the docker daemon" and send
// the operator to look at a daemon that is running perfectly well.
func contextEndpoint(name string) (string, error) {
	// The hex sha256 of the name is docker's own layout for the store, not a
	// choice this package gets to make.
	digest := sha256.Sum256([]byte(name))
	path := filepath.Join(configDir(), "contexts", "meta", hex.EncodeToString(digest[:]), "meta.json")

	raw, err := os.ReadFile(path) //nolint:gosec // path is docker's own layout: a digest of the context name under the config directory
	if err != nil {
		return "", fmt.Errorf("docker context %q: %w", name, err)
	}

	var meta struct {
		Endpoints map[string]struct {
			Host string `json:"Host"`
		} `json:"Endpoints"`
	}

	err = json.Unmarshal(raw, &meta)
	if err != nil {
		return "", fmt.Errorf("docker context %q: reading %s: %w", name, path, err)
	}

	endpoint, ok := meta.Endpoints["docker"]
	if !ok || endpoint.Host == "" {
		return "", fmt.Errorf("docker context %q %w", name, errNoDockerEndpoint)
	}

	return endpoint.Host, nil
}

// configDir is where docker keeps config.json and the context store.
func configDir() string {
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return dir
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ".docker"
	}

	return filepath.Join(home, ".docker")
}
