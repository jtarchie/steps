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
	"regexp"
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
	// Root overrides where the worker keeps both the pushed binary and the
	// step's scratch. Empty takes the worker's own temp directory. It is the
	// URL's path, kept ABSOLUTE: ssh://box/mnt/fast names /mnt/fast, and
	// trimming the leading slash silently moved it under the login user's
	// home instead -- a machine with a fast disk mounted at /mnt would take
	// the mapping, put nothing there, and fill the root filesystem.
	Root string
	// Binary is a locally-built shim to push instead of this process's own,
	// for a worker whose platform this machine cannot produce a binary for.
	// steps has no Go toolchain in the field, so a mismatched worker is an
	// operator supplying a binary they built rather than a cross-compile.
	Binary string
	// Identity is a private key file to authenticate with, on top of whatever
	// an SSH agent offers. An encrypted key has to go through the agent.
	Identity string
	// KnownHosts overrides ~/.ssh/known_hosts. Host keys are always checked;
	// this only says against which file.
	KnownHosts string
	// SSHConfig overrides ~/.ssh/config, and "none" says to read no config at
	// all -- the spelling OpenSSH's -F uses, and the answer to every refusal
	// the subset raises.
	SSHConfig string
	// HostKey pins the worker's host key by SHA256 fingerprint, for a machine
	// that has no known_hosts entry and never will: one acquired on demand,
	// used, and destroyed. Whatever created it attested its key out of band,
	// and this is where that attestation arrives. Host keys are still always
	// checked -- this says against WHAT rather than against which file.
	HostKey string
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

	query := parsed.Query()
	worker.Binary = query.Get("binary")
	worker.Identity = query.Get("identity")
	worker.KnownHosts = query.Get("known_hosts")
	worker.HostKey = query.Get("hostkey")
	worker.SSHConfig = query.Get("ssh_config")

	err = checkHostKey(worker)
	if err != nil {
		return Worker{}, err
	}

	return worker, nil
}

// fingerprintPattern is OpenSSH's own SHA256 spelling: the prefix, then the
// digest in unpadded base64. Exactly what ssh-keyscan, ssh-keygen -l and the
// EC2 console all print, so an operator pastes rather than converts.
var fingerprintPattern = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)

// checkHostKey refuses a pin that can never match, and refuses a mapping that
// names two different answers to the same question.
//
// At PARSE time, not at dial time. A typo in a fingerprint that only failed on
// connection looks exactly like the machine having been replaced -- which is
// the alarm this whole feature exists to raise, and a false one teaches
// operators to ignore it.
func checkHostKey(worker Worker) error {
	if worker.HostKey == "" {
		return nil
	}

	if !fingerprintPattern.MatchString(worker.HostKey) {
		return fmt.Errorf("%w %q: hostkey= must be an OpenSSH SHA256 fingerprint, as in SHA256:abc... (ssh-keyscan -t ed25519 host | ssh-keygen -lf - prints one)",
			ErrWorker, worker.URL)
	}

	if worker.KnownHosts != "" {
		return fmt.Errorf("%w %q: hostkey= and known_hosts= are two answers to the same question — pin the key, or name the file that holds it",
			ErrWorker, worker.URL)
	}

	return nil
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

		worker.Root = parsed.Path

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

// Address is the machine, without the credentials for reaching it.
//
// The query string is dropped deliberately: ?identity= and ?hostkey= say how
// to authenticate, and this is what gets written to the run record and drawn
// in a browser. What survives is what identifies the machine — the scheme, the
// user, the host, and the disk a step was told to use.
func (w Worker) Address() string {
	if w.Scheme == SchemeLocal {
		return "local:"
	}

	address := string(w.Scheme) + "://"
	if w.User != "" {
		address += w.User + "@"
	}

	return address + w.Host + w.Root
}
