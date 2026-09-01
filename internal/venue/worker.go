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
	"slices"
	"time"
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
	// SchemeAWS reaches an EC2 instance through SSM: no inbound port, no
	// sshd, no host key. The instance dials the control plane outward, which
	// is what makes a NAT-hidden worker reachable at all.
	SchemeAWS Scheme = "aws"
	// SchemeGCP reaches a Compute Engine instance through IAP TCP forwarding
	// to its sshd: no public address, and the only ingress is Google's own
	// relay range. GCP has no SSM-shaped exec channel, so the SSH contract is
	// the transport — the tunnel is just how the connection gets there.
	SchemeGCP Scheme = "gcp"
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
	// Instance is the EC2 instance an aws:// worker names. On the launch
	// rung it is empty until one has been acquired.
	Instance string
	// Rung is how much has to happen before this worker can be dialed: it is
	// already running, it is parked, or it does not exist yet.
	Rung Rung
	// Template is the launch template a launch-rung worker is born from. It
	// owns the entire EC2 vocabulary; steps adds none of its own.
	Template string
	// Capacity is which EC2 capacity a launch-rung worker asks for.
	Capacity Capacity
	// Version is which launch-template version the launch rung asks for,
	// always in EC2's own spelling: $Default, $Latest, or a number. A launch
	// template is a container of numbered immutable versions, so this is the
	// whole of steps' machine-shape surface — disk, instance type, AMI and
	// user data all live in a version, which is why there is no ?disk=.
	Version string
	// Idle is how long a parked worker stays running after the job that
	// started it, for an operator who wants back-to-back jobs to skip the
	// cold start and accepts that the releasing job waits out the window.
	Idle time.Duration
	// IdleSet distinguishes ?idle= being given from the default, so a rung
	// the option cannot describe can refuse it.
	IdleSet bool
	// Region overrides the ambient AWS region for an aws:// worker.
	Region string
	// Project is the GCP project a gcp:// worker lives in. Empty falls back
	// to the ambient project — the CLOUDSDK/GOOGLE_CLOUD env vars, then what
	// the application default credentials name.
	Project string
	// Zone is the GCP zone a gcp:// worker lives in. An instance lives in
	// exactly one, and unlike the project there is no credentials file to
	// fall back to — only ?zone= or CLOUDSDK_COMPUTE_ZONE can answer.
	Zone string
	// Shim is an absolute path to a steps binary ALREADY on the instance —
	// one baked into an AMI — so nothing is transferred to start a session.
	Shim string
	// ArtifactStore is the --artifact-store URL, which an aws:// worker
	// reaches its binary through. Filled in from the spec rather than the
	// URL: it describes the fleet, not this machine.
	ArtifactStore string
	// Query is the mapping's raw query string, kept so a worker that is
	// RESOLVED to another machine (an acquisition rung becoming the instance
	// it started) can carry its connection options into the URL it is dialed
	// by. See asStatic.
	Query string
	// HostKey pins the worker's host key by SHA256 fingerprint, for a machine
	// that has no known_hosts entry and never will: one acquired on demand,
	// used, and destroyed. Whatever created it attested its key out of band,
	// and this is where that attestation arrives. Host keys are still always
	// checked -- this says against WHAT rather than against which file.
	HostKey string
}

// Acquirable reports whether this worker names a machine that steps brings
// into existence — and can therefore bring into existence AGAIN, on another
// instance, after the first one is taken away. A worker that already exists
// has nowhere else to go.
func (w Worker) Acquirable() bool { return w.needsAcquisition() }

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

	worker, err = applyQuery(worker, parsed)
	if err != nil {
		return Worker{}, err
	}

	err = checkHostKey(worker)
	if err != nil {
		return Worker{}, err
	}

	err = checkScheme(worker)
	if err != nil {
		return Worker{}, err
	}

	return worker, nil
}

// checkScheme runs whichever scheme-specific refusals the mapping's scheme
// carries.
func checkScheme(worker Worker) error {
	switch worker.Scheme {
	case SchemeAWS:
		return checkAWS(worker)
	case SchemeGCP:
		return checkGCP(worker)
	case SchemeLocal, SchemeSSH:
		return nil
	default:
		return nil
	}
}

// applyQuery reads a mapping's options, refusing what the grammar does not
// know.
func applyQuery(worker Worker, parsed *url.URL) (Worker, error) {
	query := parsed.Query()

	err := checkQueryKeys(worker, query)
	if err != nil {
		return Worker{}, err
	}

	worker.Query = parsed.RawQuery
	worker.Binary = query.Get("binary")
	worker.Identity = query.Get("identity")
	worker.KnownHosts = query.Get("known_hosts")
	worker.HostKey = query.Get("hostkey")
	worker.SSHConfig = query.Get("ssh_config")
	worker.Region = query.Get("region")
	worker.Shim = query.Get("shim")
	worker.Capacity = Capacity(query.Get("capacity"))
	worker.IdleSet = query.Has("idle")
	worker.Project = query.Get("project")
	worker.Zone = query.Get("zone")

	worker.Version, err = parseTemplateVersion(worker, query)
	if err != nil {
		return Worker{}, err
	}

	worker.Idle, err = parseIdle(worker, query.Get("idle"))
	if err != nil {
		return Worker{}, err
	}

	return worker, nil
}

// queryKeys is every option a worker URL can carry, and which schemes each
// describes. Refusal on anything else is typo protection with money attached:
// ?capactiy=od silently launching spot, or ?identity= silently ignored on a
// scheme that cannot use it, is a mapping that LOOKS configured and is not.
//
//nolint:gochecknoglobals // a fact about the grammar, not state
var queryKeys = map[string][]Scheme{
	"binary":      {SchemeLocal, SchemeSSH, SchemeAWS, SchemeGCP},
	"identity":    {SchemeSSH},
	"known_hosts": {SchemeSSH},
	"hostkey":     {SchemeSSH, SchemeGCP},
	"ssh_config":  {SchemeSSH},
	"region":      {SchemeAWS},
	"shim":        {SchemeAWS},
	"capacity":    {SchemeAWS},
	"idle":        {SchemeAWS, SchemeGCP},
	"version":     {SchemeAWS},
	"project":     {SchemeGCP},
	"zone":        {SchemeGCP},
}

// acquisitionKeys are the options that describe how a machine is BROUGHT INTO
// EXISTENCE rather than how it is reached, which is why a static parse refuses
// every one of them (see checkAWS and parseTemplateVersion).
//
// staticURL strips exactly this set when it rebuilds an acquired worker's URL,
// and it lives here rather than there so the two facts cannot drift: version=
// was added to queryKeys without being added to the strip list, and the
// resulting URL launched a billed instance and then failed to re-parse.
//
//nolint:gochecknoglobals // as queryKeys: a fact about the grammar, not state
var acquisitionKeys = []string{"capacity", "idle", "version"}

// checkQueryKeys refuses an option the grammar does not know, or one that
// describes a different scheme than the mapping uses.
func checkQueryKeys(worker Worker, query url.Values) error {
	for key := range query {
		schemes, known := queryKeys[key]
		if !known {
			return fmt.Errorf("%w %q: unknown option %q", ErrWorker, worker.URL, key)
		}

		if !slices.Contains(schemes, worker.Scheme) {
			return fmt.Errorf("%w %q: %s= does not describe a %s worker", ErrWorker, worker.URL, key, worker.Scheme)
		}
	}

	return nil
}

// versionNumber is a launch-template version as EC2 numbers them: from 1, so
// 0 and negatives can only be a mistake.
var versionNumber = regexp.MustCompile(`^[1-9][0-9]*$`)

// parseTemplateVersion reads which launch-template version to launch from,
// and normalizes it to the spelling EC2 understands.
//
// The bare words are this repo's addition, because a shell eats the real
// ones: unquoted, ?version=$Latest arrives as ?version= — so the empty value
// is refused rather than read as "default", which is exactly the silent
// wrong-machine the bare spellings exist to prevent.
func parseTemplateVersion(worker Worker, query url.Values) (string, error) {
	if !query.Has("version") {
		// Not $Latest: a template someone else edits must not change what a
		// job that named no version runs on.
		return "$Default", nil
	}

	if worker.Rung != RungLaunch {
		return "", fmt.Errorf("%w %q: version= names a launch-template version, and this worker names a machine that already exists",
			ErrWorker, worker.URL)
	}

	switch raw := query.Get("version"); raw {
	case "$Default", "default":
		return "$Default", nil
	case "$Latest", "latest":
		return "$Latest", nil
	default:
		if versionNumber.MatchString(raw) {
			return raw, nil
		}

		return "", fmt.Errorf("%w %q: version= must be $Default, $Latest or a version number from 1 — default and latest spell the first two without the $ a shell eats: %q",
			ErrWorker, worker.URL, raw)
	}
}

// parseIdle reads how long a parked worker stays up after its job.
func parseIdle(worker Worker, raw string) (time.Duration, error) {
	if raw == "" {
		return defaultIdle, nil
	}

	idle, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%w %q: idle= must be a duration, as in 5m: %w", ErrWorker, worker.URL, err)
	}

	return idle, nil
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
	case SchemeAWS:
		return applyAWS(worker, parsed)
	case SchemeGCP:
		return applyGCP(worker, parsed)
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
		return Worker{}, fmt.Errorf("%w %q: unknown scheme %q, want local:, ssh://, aws:// or gcp://", ErrWorker, worker.URL, parsed.Scheme)
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

	if w.Scheme == SchemeAWS || w.Scheme == SchemeGCP {
		// The rung is part of the address: a machine that was launched for
		// this job is a different fact about where a step ran than one that
		// was already up, and the run record has to be able to say which.
		prefix := string(w.Scheme) + "://"

		switch w.Rung {
		case RungLaunch:
			return prefix + "launch/" + w.Template + w.Root
		case RungStopped:
			return prefix + "stopped/" + w.Instance + w.Root
		case RungStatic:
			return prefix + w.Instance + w.Root
		}
	}

	address := string(w.Scheme) + "://"
	if w.User != "" {
		address += w.User + "@"
	}

	return address + w.Host + w.Root
}
