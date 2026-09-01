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
// expiring-key form, so the guest agent stops honoring it later), and reads the
// instance's own SSH host keys back out of guest attributes — the one channel
// that can attest a machine created moments ago. ?hostkey= remains for an
// image whose template cannot set enable-guest-attributes=TRUE.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/oauth2"
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

	return applyRungs(worker, parsed, "gcp://stopped/worker-1 or gcp://launch/template-1")
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

	if credentials.ProjectID != "" {
		return credentials.ProjectID, nil
	}

	// The authorized_user file `gcloud auth application-default login`
	// writes carries no project_id — only a quota_project_id, stamped from
	// the active project at login. Reading it is what makes the fallback
	// real for the one credential type every error message here prescribes;
	// without it the refusal's advice was to re-run the login the user had
	// already run.
	var quota struct {
		QuotaProjectID string `json:"quota_project_id"`
	}

	if len(credentials.JSON) > 0 && json.Unmarshal(credentials.JSON, &quota) == nil {
		return quota.QuotaProjectID, nil
	}

	return "", nil
}

// gcpRelaySource is the process's credential stack for the relay, resolved
// once and cached only on success: DefaultTokenSource wraps the ADC in a
// ReuseTokenSource, so Token answers from cache until near expiry and then
// refreshes itself — one credential resolution per process instead of one
// per dial, and a token that cannot go stale across a long boot wait.
//
//nolint:gochecknoglobals // process-lifetime credentials, resolved once
var (
	gcpRelayMu     sync.Mutex
	gcpRelaySource oauth2.TokenSource
)

// gcpToken mints the OAuth access token the relay's websocket handshake
// carries.
//
//nolint:gochecknoglobals // a test seam for ambient credentials
var gcpToken = func(ctx context.Context) (string, error) {
	gcpRelayMu.Lock()

	source := gcpRelaySource
	if source == nil {
		var err error

		// Deliberately not the caller's context: the source outlives every
		// dial, and a later refresh must not fail because the FIRST caller's
		// step happened to be cancelled. It outlives the caller's DEADLINE
		// too, though, and oauth2 with no client of its own falls back to
		// http.DefaultClient — whose zero Timeout would let a refresh against
		// an accepted-but-silent token endpoint hang with nothing left to
		// interrupt it, so the source carries its own bound instead.
		base := context.WithValue(context.WithoutCancel(ctx), oauth2.HTTPClient,
			&http.Client{Timeout: dialTimeout})

		source, err = google.DefaultTokenSource(base, gcpScope)
		if err != nil {
			gcpRelayMu.Unlock()

			return "", fmt.Errorf("no GCP credentials for the IAP relay (log in with `gcloud auth application-default login`): %w", err)
		}

		gcpRelaySource = source
	}

	gcpRelayMu.Unlock()

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

	hostKeys, algorithms, err := gcpHostKeys(ctx, api, worker, project, zone)
	if err != nil {
		return nil, err
	}

	config := &ssh.ClientConfig{
		User:              gcpUser,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback:   hostKeys,
		HostKeyAlgorithms: algorithms,
		Timeout:           dialTimeout,
	}

	client, err := gcpConnect(ctx, api, worker, project, zone, config)
	if err != nil {
		return nil, err
	}

	remote, build, err := pushShim(client, worker)
	if err != nil {
		_ = client.Close()

		return nil, err
	}

	return startShim(client, remote, build)
}

// gcpConnect opens the tunnel and completes the SSH handshake, retrying an
// authentication refusal inside the ready window: AddSSHKey returns once the
// API holds the key, but the guest agent ON the instance applies it to the
// account asynchronously — typically within seconds — and gcloud's own
// client waits out exactly this gap. Every other handshake answer (a host
// key mismatch above all) is final.
func gcpConnect(ctx context.Context, api gceAPI, worker Worker, project, zone string, config *ssh.ClientConfig) (*ssh.Client, error) {
	deadline, cancel := context.WithTimeout(ctx, gcpReadyTimeout)
	defer cancel()

	ticker := time.NewTicker(gcpReadyPoll)
	defer ticker.Stop()

	address := worker.Instance + ":22"

	// A dial that gives up after an authentication refusal stops trusting
	// the install cache: the likeliest reason a key never becomes usable is
	// an instance recreated under the same name since the cached install,
	// and the next dial must write the key again rather than skip it for
	// hours. Deleted on every failing exit past the first refusal, so a
	// deadline that fires mid-redial cannot leave the stale entry behind.
	authRefused := false
	forgetInstall := func() {
		if authRefused {
			gcpInstalled.Delete(project + "/" + zone + "/" + worker.Instance)
		}
	}

	for {
		conn, err := gcpDialRelay(deadline, api, worker, project, zone)
		if err != nil {
			forgetInstall()

			return nil, err
		}

		client, err := sshHandshake(conn, address, config)
		if err == nil {
			return client, nil
		}

		if !strings.Contains(err.Error(), "ssh: unable to authenticate") {
			forgetInstall()

			return nil, fmt.Errorf("connecting to %s for %q: %w", worker.Instance, worker.URL, err)
		}

		authRefused = true

		select {
		case <-ticker.C:
		case <-deadline.Done():
			forgetInstall()

			return nil, fmt.Errorf("connecting to %s for %q: %w", worker.Instance, worker.URL, err)
		}
	}
}

// sshHandshake runs the SSH handshake with a watchdog. The tunnel conn has
// no deadlines (its SetDeadline is a no-op) and NewClientConn ignores
// config.Timeout — only ssh.Dial's TCP dial honors it — so a peer that
// stalls mid-handshake would otherwise hold the dial forever, and closing
// the conn is the one interruption that reaches a blocked handshake.
func sshHandshake(conn net.Conn, address string, config *ssh.ClientConfig) (*ssh.Client, error) {
	watchdog := time.AfterFunc(config.Timeout, func() { _ = conn.Close() })
	defer watchdog.Stop()

	sshConn, channels, requests, err := ssh.NewClientConn(conn, address, config)
	if err != nil {
		_ = conn.Close()

		return nil, err //nolint:wrapcheck // the caller names the worker and instance
	}

	// Stop answers false once the watchdog has already fired, which means the
	// conn this client would ride on is being closed right now: report the
	// timeout the watchdog exists to report, rather than hand back a live
	// client over a dead transport whose first use fails as something else.
	if !watchdog.Stop() {
		_ = sshConn.Close()

		return nil, fmt.Errorf("the SSH handshake did not finish within %s", config.Timeout)
	}

	return ssh.NewClient(sshConn, channels, requests), nil
}

// gcpDialRelay opens the tunnel, waiting out the window where the instance
// is up but sshd is not yet answering — the relay reports that as "could not
// reach the backend", which for a machine acquired seconds ago means "yet".
// The control plane referees, because the relay cannot tell "not yet" from
// "never": a parked or vanished machine will refuse the backend exactly the
// same way forever, and waiting four minutes to then blame the firewall
// sends an operator auditing rules that were never wrong.
func gcpDialRelay(ctx context.Context, api gceAPI, worker Worker, project, zone string) (net.Conn, error) {
	target := iapdial.Target{
		Project:  project,
		Zone:     zone,
		Instance: worker.Instance,
		Port:     22,
	}

	// The caller owns the ready budget — gcpConnect already bounded ctx with
	// gcpReadyTimeout — so a second bound of the same size can never fire on
	// its own, and reporting it as the wait would name four minutes on a
	// redial that waited seconds.
	ticker := time.NewTicker(gcpReadyPoll)
	defer ticker.Stop()

	started := time.Now()

	for {
		token, err := gcpToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("worker %q: %w", worker.URL, err)
		}

		conn, err := iapOpen(ctx, target, token)
		if err == nil {
			return conn, nil
		}

		if !errors.Is(err, iapdial.ErrBackendNotReached) {
			return nil, fmt.Errorf("worker %q: %w", worker.URL, err)
		}

		refusal := gcpBackendRefusal(ctx, api, worker, project, zone)
		if refusal != nil {
			return nil, refusal
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting %s for sshd on %s for %q: %w",
				time.Since(started).Round(time.Second), worker.Instance, worker.URL, err)
		}
	}
}

// gcpBackendRefusal decides whether the relay failing to reach the port is
// worth waiting out. nil means keep trying: the machine is RUNNING or still
// booting, so sshd may answer the next attempt — and a control-plane blip
// answers nil too, so a 503 from the API cannot kill a dial the relay
// itself is still willing to retry.
func gcpBackendRefusal(ctx context.Context, api gceAPI, worker Worker, project, zone string) error {
	status, err := api.Status(ctx, project, zone, worker.Instance)
	if err != nil {
		if errors.Is(err, errGCENotFound) {
			return fmt.Errorf("%w %q: %s does not exist", ErrWorker, worker.URL, worker.Instance)
		}

		return nil
	}

	switch status {
	case "RUNNING", "PROVISIONING", "STAGING", "REPAIRING", "PENDING":
		return nil
	}

	return fmt.Errorf("%w %q: %s is %s, so nothing can answer on its port — start it, or name it gcp://stopped/%s to have steps start and park it around each job",
		ErrWorker, worker.URL, worker.Instance, status, worker.Instance)
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

// gcpKeyExpiryLayout renders and parses a google-ssh entry's expireOn — the
// shape gcloud writes and the guest agent checks. The zone element matters:
// a literal "+0000" would render UTC correctly too, but only for a time
// something upstream remembered to convert, and the wrong string would be
// accepted and then expire hours early with nothing pointing here.
const gcpKeyExpiryLayout = "2006-01-02T15:04:05-0700"

// gcpEnsureKey installs this process's public key on the instance, in the
// google-ssh expiring form: the guest agent stops honoring the entry after
// the TTL, and mergeSSHKey prunes what expired the next time a key is
// installed — the agent itself has no credentials to clean metadata with.
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

	expiry := time.Now().Add(gcpKeyTTL).UTC().Format(gcpKeyExpiryLayout)
	entry := fmt.Sprintf(`%s:%s google-ssh {"userName":%q,"expireOn":%q}`,
		gcpUser,
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))),
		gcpUser, expiry)

	err = api.AddSSHKey(ctx, project, zone, worker.Instance, entry)
	if err != nil {
		return nil, fmt.Errorf("installing an SSH key on %s for %q: %w", worker.Instance, worker.URL, err)
	}

	gcpInstalled.Store(cacheKey, time.Now())

	// Installs are rare, so sweep here: a launched instance never dials
	// again once its job ends, and a daemon would otherwise accrue one
	// permanent entry per machine it ever created.
	gcpInstalled.Range(func(key, at any) bool {
		if installed, ok := at.(time.Time); !ok || time.Since(installed) > gcpKeyTTL {
			gcpInstalled.Delete(key)
		}

		return true
	})

	return signer, nil
}

// errNoHostKeys is an instance that never published SSH host keys.
var errNoHostKeys = errors.New("the instance published no SSH host keys in guest attributes")

// gcpHostKeys is how the dial knows the machine on the far end of the tunnel
// is the instance it asked for. ?hostkey= pins it outright; otherwise the
// instance's guest attributes — written by its own guest agent at boot, read
// back over the authenticated API — are the attestation, polled because a
// machine acquired moments ago has not finished booting.
// It also reports which host key algorithms the attestation can verify: an
// instance publishes its keys one at a time, so a poll during boot can see
// one of them, and a client left to its own preference order then negotiates
// an algorithm nothing attested. That reads as an impostor — a final error
// that deletes a launched machine — when the truth is "not yet".
func gcpHostKeys(ctx context.Context, api gceAPI, worker Worker, project, zone string) (ssh.HostKeyCallback, []string, error) {
	if worker.HostKey != "" {
		// A fingerprint names no algorithm, so a pin cannot narrow the
		// negotiation the way an attestation can.
		return pinnedHostKey(worker.HostKey), nil, nil
	}

	deadline, cancel := context.WithTimeout(ctx, gcpReadyTimeout)
	defer cancel()

	ticker := time.NewTicker(gcpReadyPoll)
	defer ticker.Stop()

	for {
		attributes, err := api.GuestAttributes(deadline, project, zone, worker.Instance, "hostkeys/")

		switch {
		case err == nil:
			var (
				callback   ssh.HostKeyCallback
				algorithms []string
			)

			callback, algorithms, err = hostKeysFromAttributes(worker, attributes)
			if err == nil {
				return callback, algorithms, nil
			}
		case errors.Is(err, errGuestAttributesDisabled):
			return nil, nil, fmt.Errorf("%w %q: %s could not be asked for its host keys, so they cannot be attested — either guest attributes are disabled (set enable-guest-attributes=TRUE in the instance metadata, or the template) or this credential lacks compute.instances.getGuestAttributes; pin the key with ?hostkey= to skip the attestation: %w",
				ErrWorker, worker.URL, worker.Instance, err)
		default:
			// A transient control-plane error is waited out like an empty
			// answer: one 503 mid-boot must not fail a dial — and delete the
			// machine a launch rung just paid for — when the next poll would
			// have connected. The deadline reports the last error if it
			// never clears.
			err = fmt.Errorf("reading host keys for %s for %q: %w", worker.Instance, worker.URL, err)
		}

		select {
		case <-ticker.C:
		case <-deadline.Done():
			return nil, nil, fmt.Errorf("waiting %s for %s to publish host keys for %q: %w",
				gcpReadyTimeout, worker.Instance, worker.URL, err)
		}
	}
}

// hostKeysFromAttributes turns the hostkeys/ namespace into a verifier that
// accepts any key the instance itself published.
func hostKeysFromAttributes(worker Worker, attributes map[string]string) (ssh.HostKeyCallback, []string, error) {
	var (
		accepted   [][]byte
		algorithms []string
	)

	for keyType, value := range attributes {
		line := keyType + " " + strings.TrimSpace(value)

		public, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err == nil {
			accepted = append(accepted, public.Marshal())

			if public.Type() == ssh.KeyAlgoRSA {
				// One RSA host key signs under three algorithm names, and a
				// modern sshd offers the SHA-2 pair: naming only "ssh-rsa"
				// would refuse the very key that was attested.
				algorithms = append(algorithms, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512)
			}

			algorithms = append(algorithms, public.Type())
		}
	}

	if len(accepted) == 0 {
		return nil, nil, errNoHostKeys
	}

	return func(_ string, remote net.Addr, key ssh.PublicKey) error {
		offered := key.Marshal()
		for _, candidate := range accepted {
			if bytes.Equal(offered, candidate) {
				return nil
			}
		}

		return fmt.Errorf("%w: %s offered %s, and %s attested %d other key(s) in guest attributes",
			errHostKeyMismatch, remote, ssh.FingerprintSHA256(key), worker.Instance, len(accepted))
	}, algorithms, nil
}
