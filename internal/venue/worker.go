// Package venue runs a step's commands somewhere other than this machine.
//
// It sits one tier above internal/shell and answers a different question.
// shell decides how a command runs — on the host or in a container. venue
// decides WHERE, and when the answer is "here" it hands the decision straight
// back to shell unchanged. That split is why an SSH client lives in this
// package and not in the leaf every execution path imports.
//
// A venue is an execution venue and nothing more. The orchestrator's
// filesystem stays the single artifact store; a worker gets a tree, runs a
// command against it, and gives back what was asked for. There is no worker
// registration, no daemon, no worker-to-worker transfer, and no state on the
// far end that outlives a step.
package venue

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Scheme names how to reach a worker.
type Scheme string

const (
	// SchemeLocal runs the shim as a child process on this machine, talking
	// over its pipes.
	//
	// It is not a test seam. It is how a tagged pipeline runs on a laptop with
	// no worker to reach, and how the protocol gets exercised end to end
	// without a network — which also happens to make it the thing the docs
	// corpus can execute.
	SchemeLocal Scheme = "local"
	// SchemeSSH reaches a worker over SSH, pushing this binary to it first.
	SchemeSSH Scheme = "ssh"
)

// Worker is a parsed worker URL: where a tagged step goes.
type Worker struct {
	// URL is the mapping as written, kept for error messages. A red build has
	// to say which machine, spelled the way the operator spelled it.
	URL    string
	Scheme Scheme
	User   string
	Host   string
	// Root overrides where the shim makes its scratch on the worker. Empty
	// takes the worker's own temp directory.
	Root string
	// Binary is a locally-built shim to push instead of this process's own,
	// for a worker whose platform this machine cannot produce a binary for.
	Binary string
}

// ErrWorker is a worker mapping that cannot be reached as written.
var ErrWorker = errors.New("invalid worker")

// ParseWorker reads one --worker mapping.
//
// The grammar is deliberately small: a scheme, a host, and a couple of
// options. Anything that describes the MACHINE rather than the connection —
// which image, how much memory, which region — belongs to whatever provisioned
// it, not to a pipeline runner dialing in.
func ParseWorker(raw string) (Worker, error) {
	if raw == "" {
		return Worker{}, fmt.Errorf("%w: empty", ErrWorker)
	}

	// local: has no authority, so url.Parse's opaque form is the honest read
	// of it rather than a host that happens to be empty.
	if raw == "local:" || raw == "local://" {
		return Worker{URL: raw, Scheme: SchemeLocal}, nil
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return Worker{}, fmt.Errorf("%w %q: %w", ErrWorker, raw, err)
	}

	worker, err := applyScheme(Worker{URL: raw, Scheme: Scheme(parsed.Scheme)}, parsed)
	if err != nil {
		return Worker{}, err
	}

	worker.Binary = parsed.Query().Get("binary")

	return worker, nil
}

// applyScheme fills in whatever a scheme needs from the parsed URL.
func applyScheme(worker Worker, parsed *url.URL) (Worker, error) {
	switch worker.Scheme {
	case SchemeSSH:
		if parsed.Host == "" {
			return Worker{}, fmt.Errorf("%w %q: ssh needs a host, as in ssh://user@box", ErrWorker, worker.URL)
		}

		worker.Host = parsed.Host
		if parsed.User != nil {
			worker.User = parsed.User.Username()
		}

		worker.Root = strings.TrimPrefix(parsed.Path, "/")

		return worker, nil
	case SchemeLocal:
		// local://something is a mapping that looks like it names a machine
		// and does not. Refusing beats running it here and letting the author
		// believe otherwise.
		if parsed.Host != "" || parsed.Path != "" {
			return Worker{}, fmt.Errorf("%w %q: local: takes no host — it means this machine", ErrWorker, worker.URL)
		}

		return worker, nil
	default:
		return Worker{}, fmt.Errorf("%w %q: unknown scheme %q, want local: or ssh://", ErrWorker, worker.URL, parsed.Scheme)
	}
}

// String is the mapping as the operator wrote it.
func (w Worker) String() string { return w.URL }
