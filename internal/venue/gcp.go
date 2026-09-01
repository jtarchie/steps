package venue

// The gcp:// venue: a Compute Engine instance reached through IAP TCP
// forwarding to its own sshd.
//
// The shape differs from aws:// in one way that matters. GCP has no
// SSM-shaped exec channel — no way to run a bootstrap command through the
// control plane — so the SSH contract IS the transport: the relay tunnel
// terminates at port 22, and everything ssh:// does (push a binary over
// sftp, exec it, talk over its stdio) happens over that tunnel unchanged.
// Two consequences an aws:// reader would not expect: ?binary= needs no
// artifact store, because sftp carries it, and the instance needs sshd —
// which every stock image runs — plus one firewall rule admitting Google's
// relay range 35.235.240.0/20 to port 22. No public address is needed.
//
// Authentication is minted, not configured. The dial generates an ephemeral
// key, installs its public half through instance metadata (the google-ssh
// expiring-key form, so the guest agent removes it later), and reads the
// instance's own SSH host keys back out of guest attributes — the one channel
// that can attest a machine created moments ago. ?hostkey= remains for an
// image whose template cannot set enable-guest-attributes=TRUE.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/oauth2/google"

	"github.com/jtarchie/steps/internal/venue/iapdial"
)

// gcpUser is the account a dial connects as. The guest agent creates it from
// the metadata key entry; nothing about the image has to know the name.
const gcpUser = "steps"

// gcpKeyTTL is how long an installed public key stays valid. Long enough
// that every step of a long job authenticates on one install, short enough
// that a static worker does not accumulate a permanent grant from every
// machine that ever ran a job against it.
const gcpKeyTTL = 12 * time.Hour

// gcpScope is the OAuth scope both the relay and the compute control plane
// accept.
const gcpScope = "https://www.googleapis.com/auth/cloud-platform"

// gcpName matches what Compute Engine accepts as an instance or template
// name (RFC 1035 labels), so a mapping that can never name a machine is
// refused when it is read.
var gcpName = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// applyGCP reads the three gcp:// forms.
//
//	gcp://worker-1[/root]           a running instance
//	gcp://stopped/worker-1[/root]   a parked instance
//	gcp://launch/template-1[/root]  an instance template to be born from
//
// The rung is the authority for the same reason it is on aws://. One
// consequence of GCP naming machines rather than numbering them: an instance
// literally named "stopped" or "launch" cannot be dialed on the static rung —
// the rung reading wins.
func applyGCP(worker Worker, parsed *url.URL) (Worker, error) {
	if parsed.User != nil {
		return Worker{}, fmt.Errorf("%w %q: gcp:// connects as an ephemeral %q account it creates itself, so a username cannot be chosen",
			ErrWorker, worker.URL, gcpUser)
	}

	target, root := parsed.Host, parsed.Path

	switch Rung(parsed.Host) {
	case RungStopped, RungLaunch:
		worker.Rung = Rung(parsed.Host)

		target, root = splitFirstSegment(parsed.Path)
		if target == "" {
			return Worker{}, fmt.Errorf("%w %q: %s needs something to acquire, as in gcp://stopped/worker-1 or gcp://launch/template-1",
				ErrWorker, worker.URL, parsed.Host)
		}
	case RungStatic:
	}

	if worker.Rung == RungLaunch {
		worker.Template = target
	} else {
		worker.Instance = target
	}

	// Absolute, for the same reason ssh:// keeps it absolute.
	worker.Root = root

	return worker, nil
}

// checkGCP refuses a gcp:// mapping this venue cannot act on. The options
// that belong to other schemes — ?capacity=, ?version=, ?shim=, ?region= —
// are already refused by the grammar itself: an instance template is an
// immutable object with no numbered versions (name a different template),
// and it owns the capacity decision (provisioningModel lives in the
// template), so neither knob exists here.
func checkGCP(worker Worker) error {
	target, kind := worker.Instance, "an instance"
	if worker.Rung == RungLaunch {
		target, kind = worker.Template, "an instance template"
	}

	if !gcpName.MatchString(target) {
		return fmt.Errorf("%w %q: %q cannot name %s — GCP names are lowercase letters, digits and hyphens, starting with a letter",
			ErrWorker, worker.URL, target, kind)
	}

	if worker.IdleSet && worker.Rung != RungStopped {
		return fmt.Errorf("%w %q: idle= describes how long a PARKED machine stays warm, and this worker is not on the stopped rung",
			ErrWorker, worker.URL)
	}

	return nil
}

// gcpPlacementCheck refuses a mapping whose dial is certain to fail, before
// an acquisition rung bills a machine finding out. A GCE instance runs
// Linux, so an orchestrator on any other OS can never push its own binary.
func (w Worker) gcpPlacementCheck() error {
	if runtime.GOOS != "linux" && w.Binary == "" {
		return fmt.Errorf("%w %q: a gcp:// worker runs Linux and this machine's own binary is %s — build one with CGO_ENABLED=0 GOOS=linux and name it with ?binary=",
			ErrWorker, w.URL, runtime.GOOS)
	}

	return nil
}

// gcpLocation resolves which project and zone a worker lives in: the mapping
// first, then the environment gcloud itself honors, then — for the project
// only — what the application default credentials name. There is no ambient
// source for a zone, so a mapping that names none and an environment that
// says nothing is refused with the fix spelled out.
func gcpLocation(ctx context.Context, worker Worker) (string, string, error) {
	zone := worker.Zone
	if zone == "" {
		zone = os.Getenv("CLOUDSDK_COMPUTE_ZONE")
	}

	if zone == "" {
		return "", "", fmt.Errorf("%w %q: no GCP zone — name the instance's zone with ?zone=us-central1-a, or set CLOUDSDK_COMPUTE_ZONE",
			ErrWorker, worker.URL)
	}

	project := worker.Project
	if project == "" {
		project = os.Getenv("GOOGLE_CLOUD_PROJECT")
	}

	if project == "" {
		project = os.Getenv("CLOUDSDK_CORE_PROJECT")
	}

	if project == "" {
		var err error

		project, err = gcpDefaultProject(ctx)
		if err != nil || project == "" {
			return "", "", fmt.Errorf("%w %q: no GCP project — name it with ?project=, set GOOGLE_CLOUD_PROJECT, or log in with `gcloud auth application-default login`",
				ErrWorker, worker.URL)
		}
	}

	return project, zone, nil
}

// gcpDefaultProject asks the application default credentials which project
// they belong to. A file read, never a network call.
//
//nolint:gochecknoglobals // a test seam for ambient credentials
var gcpDefaultProject = func(ctx context.Context) (string, error) {
	credentials, err := google.FindDefaultCredentials(ctx, gcpScope)
	if err != nil {
		return "", fmt.Errorf("reading application default credentials: %w", err)
	}

	return credentials.ProjectID, nil
}

// gcpToken mints the OAuth access token the relay's websocket handshake
// carries.
//
//nolint:gochecknoglobals // a test seam for ambient credentials
var gcpToken = func(ctx context.Context) (string, error) {
	source, err := google.DefaultTokenSource(ctx, gcpScope)
	if err != nil {
		return "", fmt.Errorf("no GCP credentials for the IAP relay (log in with `gcloud auth application-default login`): %w", err)
	}

	token, err := source.Token()
	if err != nil {
		return "", fmt.Errorf("minting a GCP access token: %w", err)
	}

	return token.AccessToken, nil
}

// iapOpen dials the relay, seamed so a venue test can stand a local sshd in
// for the whole tunnel — which is what lets the resolve→dial spine be tested
// without a Google account.
//
//nolint:gochecknoglobals // a test seam for the relay, documented above
var iapOpen = func(ctx context.Context, target iapdial.Target, token string) (net.Conn, error) {
	channel, err := iapdial.Open(ctx, iapdial.ConnectURL(target), token)
	if err != nil {
		return nil, err //nolint:wrapcheck // iapdial explains the relay's refusals itself
	}

	return channel, nil
}

// gcpReadyTimeout bounds the two boot waits a fresh instance needs — host
// keys appearing in guest attributes, and sshd answering the forwarded port.
// gcpReadyPoll is how often each is retried.
//
// Variables rather than constants so a test can shrink them, exactly as the
// SSM registration waits are.
//
//nolint:gochecknoglobals // test seams for waits measured in minutes
var (
	gcpReadyTimeout = 4 * time.Minute
	gcpReadyPoll    = 5 * time.Second
)

// dialGCP reaches an instance through the relay and starts a shim over SSH.
func dialGCP(ctx context.Context, worker Worker) (*transport, error) {
	api, err := gceFor(ctx, worker)
	if err != nil {
		return nil, err
	}

	project, zone, err := gcpLocation(ctx, worker)
	if err != nil {
		return nil, err
	}

	signer, err := gcpEnsureKey(ctx, api, worker, project, zone)
	if err != nil {
		return nil, err
	}

	hostKeys, err := gcpHostKeys(ctx, api, worker, project, zone)
	if err != nil {
		return nil, err
	}

	conn, err := gcpDialRelay(ctx, worker, project, zone)
	if err != nil {
		return nil, err
	}

	config := &ssh.ClientConfig{
		User:            gcpUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeys,
		Timeout:         dialTimeout,
	}

	address := worker.Instance + ":22"

	sshConn, channels, requests, err := ssh.NewClientConn(conn, address, config)
	if err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("connecting to %s for %q: %w", worker.Instance, worker.URL, err)
	}

	client := ssh.NewClient(sshConn, channels, requests)

	remote, build, err := pushShim(client, worker)
	if err != nil {
		_ = client.Close()

		return nil, err
	}

	return startShim(client, remote, build)
}

// gcpDialRelay opens the tunnel, waiting out the window where the instance
// is up but sshd is not yet answering — the relay reports that as "could not
// reach the backend", which for a machine acquired seconds ago means "yet".
func gcpDialRelay(ctx context.Context, worker Worker, project, zone string) (net.Conn, error) {
	target := iapdial.Target{
		Project:  project,
		Zone:     zone,
		Instance: worker.Instance,
		Port:     22,
	}

	deadline, cancel := context.WithTimeout(ctx, gcpReadyTimeout)
	defer cancel()

	ticker := time.NewTicker(gcpReadyPoll)
	defer ticker.Stop()

	token, err := gcpToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("worker %q: %w", worker.URL, err)
	}

	for {
		conn, err := iapOpen(deadline, target, token)
		if err == nil {
			return conn, nil
		}

		if !errors.Is(err, iapdial.ErrBackendNotReached) {
			return nil, fmt.Errorf("worker %q: %w", worker.URL, err)
		}

		select {
		case <-ticker.C:
		case <-deadline.Done():
			return nil, fmt.Errorf("waiting %s for sshd on %s for %q: %w",
				gcpReadyTimeout, worker.Instance, worker.URL, err)
		}
	}
}

// The dial's ephemeral identity: one keypair per process, installed into an
// instance's metadata at most once per TTL window.
//
//nolint:gochecknoglobals // process-lifetime identity, generated once
var (
	gcpKeyOnce   sync.Once
	gcpKeySigner ssh.Signer
	gcpKeyErr    error
	// gcpInstalled remembers when this process last installed its key on an
	// instance (keyed project/zone/instance), so every later step of a job —
	// and every redial — skips the metadata write.
	gcpInstalled sync.Map
)

// gcpKey is this process's SSH identity for GCP workers.
func gcpKey() (ssh.Signer, error) {
	gcpKeyOnce.Do(func() {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			gcpKeyErr = fmt.Errorf("generating an SSH key for gcp:// workers: %w", err)

			return
		}

		signer, err := ssh.NewSignerFromKey(private)
		if err != nil {
			gcpKeyErr = fmt.Errorf("wrapping the SSH key for gcp:// workers: %w", err)

			return
		}

		gcpKeySigner = signer
	})

	if gcpKeyErr != nil {
		return nil, gcpKeyErr
	}

	if gcpKeySigner == nil {
		// Unreachable while the Once above sets one of the two — stated
		// locally so a caller's nil check never depends on inferring that.
		return nil, errors.New("no SSH key was generated for gcp:// workers")
	}

	return gcpKeySigner, nil
}

// gcpEnsureKey installs this process's public key on the instance, in the
// google-ssh expiring form so the guest agent removes it after the TTL.
func gcpEnsureKey(ctx context.Context, api gceAPI, worker Worker, project, zone string) (ssh.Signer, error) {
	signer, err := gcpKey()
	if err != nil {
		return nil, err
	}

	cacheKey := project + "/" + zone + "/" + worker.Instance
	if at, ok := gcpInstalled.Load(cacheKey); ok {
		// Reinstalled at half the TTL, so no dial ever presents a key the
		// guest agent is about to expire.
		if installed, ok := at.(time.Time); ok && time.Since(installed) < gcpKeyTTL/2 {
			return signer, nil
		}
	}

	expiry := time.Now().Add(gcpKeyTTL).UTC().Format("2006-01-02T15:04:05+0000")
	entry := fmt.Sprintf(`%s:%s google-ssh {"userName":%q,"expireOn":%q}`,
		gcpUser,
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))),
		gcpUser, expiry)

	err = api.AddSSHKey(ctx, project, zone, worker.Instance, entry)
	if err != nil {
		return nil, fmt.Errorf("installing an SSH key on %s for %q: %w", worker.Instance, worker.URL, err)
	}

	gcpInstalled.Store(cacheKey, time.Now())

	return signer, nil
}

// errNoHostKeys is an instance that never published SSH host keys.
var errNoHostKeys = errors.New("the instance published no SSH host keys in guest attributes")

// gcpHostKeys is how the dial knows the machine on the far end of the tunnel
// is the instance it asked for. ?hostkey= pins it outright; otherwise the
// instance's guest attributes — written by its own guest agent at boot, read
// back over the authenticated API — are the attestation, polled because a
// machine acquired moments ago has not finished booting.
func gcpHostKeys(ctx context.Context, api gceAPI, worker Worker, project, zone string) (ssh.HostKeyCallback, error) {
	if worker.HostKey != "" {
		return pinnedHostKey(worker.HostKey), nil
	}

	deadline, cancel := context.WithTimeout(ctx, gcpReadyTimeout)
	defer cancel()

	ticker := time.NewTicker(gcpReadyPoll)
	defer ticker.Stop()

	for {
		attributes, err := api.GuestAttributes(deadline, project, zone, worker.Instance, "hostkeys/")
		if err != nil {
			if errors.Is(err, errGuestAttributesDisabled) {
				return nil, fmt.Errorf("%w %q: guest attributes are disabled on %s, so its host keys cannot be attested — set enable-guest-attributes=TRUE in the instance metadata (or template), or pin the key with ?hostkey=",
					ErrWorker, worker.URL, worker.Instance)
			}

			return nil, fmt.Errorf("reading host keys for %s for %q: %w", worker.Instance, worker.URL, err)
		}

		callback, err := hostKeysFromAttributes(worker, attributes)
		if err == nil {
			return callback, nil
		}

		select {
		case <-ticker.C:
		case <-deadline.Done():
			return nil, fmt.Errorf("waiting %s for %s to publish host keys for %q: %w",
				gcpReadyTimeout, worker.Instance, worker.URL, err)
		}
	}
}

// hostKeysFromAttributes turns the hostkeys/ namespace into a verifier that
// accepts any key the instance itself published.
func hostKeysFromAttributes(worker Worker, attributes map[string]string) (ssh.HostKeyCallback, error) {
	var accepted []ssh.PublicKey

	for keyType, value := range attributes {
		line := keyType + " " + strings.TrimSpace(value)

		public, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err == nil {
			accepted = append(accepted, public)
		}
	}

	if len(accepted) == 0 {
		return nil, errNoHostKeys
	}

	return func(_ string, remote net.Addr, key ssh.PublicKey) error {
		for _, candidate := range accepted {
			if bytes.Equal(key.Marshal(), candidate.Marshal()) {
				return nil
			}
		}

		return fmt.Errorf("%w: %s offered %s, and %s attested %d other key(s) in guest attributes",
			errHostKeyMismatch, remote, ssh.FingerprintSHA256(key), worker.Instance, len(accepted))
	}, nil
}
