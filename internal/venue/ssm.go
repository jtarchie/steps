package venue

// The aws:// venue: a worker reached through SSM, with no inbound port, no
// sshd, and no host key.
//
// The shape differs from ssh:// in one way that matters. SSH gives an exec
// channel, so the orchestrator pushes a binary and runs it in one motion.
// SSM gives a port forward, so getting a shim RUNNING is a separate errand —
// a command sent through the control plane, which fetches the binary from the
// artifact store and starts it listening on a loopback port. The forwarded
// session then terminates at that port, and everything above it is the same
// protocol on the same terms.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/jtarchie/steps/internal/venue/ssmdial"
)

// bootstrapTTL bounds the presigned URL the bootstrap fetches the binary
// with. Long enough for a cold instance on a slow link, short enough that a
// URL recovered from a command history is useless.
const bootstrapTTL = 20 * time.Minute

// ssmAPIFor builds the SSM client for a worker.
//
// A package variable so a test can stand in for the whole control plane
// without a network or credentials — the same seam shape the shim variants
// use, and the only way to exercise this path at all without an AWS account.
//
//nolint:gochecknoglobals // a test seam for a control plane, documented above
var ssmAPIFor = func(ctx context.Context, worker Worker) (ssmdial.API, error) {
	loaders := []func(*awsconfig.LoadOptions) error{}
	if worker.Region != "" {
		loaders = append(loaders, awsconfig.WithRegion(worker.Region))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, loaders...)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", ErrWorker, worker.URL, err)
	}

	// Said here rather than left to the SDK, which answers a missing region
	// with "endpoint rule error, Invalid Configuration: Missing Region" —
	// true, and no help at all to someone who has just written a worker
	// mapping. An instance lives in exactly one region, and the mapping is
	// where a caller says which.
	if cfg.Region == "" {
		return nil, fmt.Errorf("%w %q: no AWS region — name the instance's region with ?region=, or set one in the environment or your AWS profile",
			ErrWorker, worker.URL)
	}

	return ssmdial.NewAPI(cfg), nil
}

// ssmForward opens the forwarded session, seamed for the same reason
// ssmAPIFor is — and drawn HERE rather than deeper so a venue test exercises
// the bootstrap and the wiring without re-testing the data-channel protocol,
// which has its own tests one package over.
//
//nolint:gochecknoglobals // a test seam, documented above
var ssmForward = func(ctx context.Context, api ssmdial.API, instance string, port int) (io.ReadWriteCloser, error) {
	channel, err := ssmdial.Forward(ctx, api, instance, port)
	if err != nil {
		return nil, err //nolint:wrapcheck // ssmdial names the instance and the failure
	}

	return channel, nil
}

// dialSSM starts a shim on the instance and opens a session to it.
func dialSSM(ctx context.Context, worker Worker) (*transport, error) {
	api, err := ssmAPIFor(ctx, worker)
	if err != nil {
		return nil, err
	}

	platform, err := waitForManagedNode(ctx, api, worker)
	if err != nil {
		return nil, fmt.Errorf("worker %q: %w", worker.URL, err)
	}

	if platform == ssmdial.PlatformWindows {
		// Refused here rather than at the handshake, where lossyGOOS would
		// catch it anyway: the handshake refusal comes after a binary has
		// been fetched and a process started on someone's machine, and there
		// is no reason to spend that to reach the same answer.
		return nil, fmt.Errorf("%w %q: the instance runs Windows, whose filesystem cannot store an executable bit — see the worker notes in docs/infra.md",
			ErrWorker, worker.URL)
	}

	binary, build, err := ssmBinary(ctx, worker)
	if err != nil {
		return nil, err
	}

	port, err := startRemoteShim(ctx, api, worker, platform, binary)
	if err != nil {
		return nil, err
	}

	channel, err := ssmForward(ctx, api, worker.Instance, port)
	if err != nil {
		return nil, fmt.Errorf("worker %q: %w", worker.URL, err)
	}

	return &transport{
		in:    channel,
		out:   channel,
		build: build,
		// Closing the channel unblocks its reads via the stop signal and
		// errors its writes on the dead websocket.
		interrupt: func() { _ = channel.Close() },
		close: func(context.Context) error {
			// Closing the channel is the goodbye. The remote shim serves one
			// connection and exits (see the bootstrap), so nothing is left
			// running on the instance and there is nothing to reap.
			return channel.Close()
		},
	}, nil
}

// registerTimeout bounds waiting for amazon-ssm-agent to register, and
// registerPoll is how often it is asked.
//
// Variables rather than constants so a test can shrink them: the branch worth
// proving is that the dial retries at all, and a wait measured in minutes is
// not one a test suite can sit through.
//
//nolint:gochecknoglobals // test seams for a wait measured in minutes
var (
	registerTimeout = 4 * time.Minute
	registerPoll    = 5 * time.Second
)

// waitForManagedNode waits for SSM to admit it can reach the instance.
//
// An instance EC2 already calls "running" has no registered agent for another
// one to three minutes — hack/aws-fixture.sh waits 180s for exactly this when
// it provisions one — so asking a single time fails every acquisition. This
// is the wait waitForRunning's comment has always promised and nothing
// implemented, which is why the launch rung never worked against real AWS.
//
// Unconditional rather than only for acquired workers: by the time anything
// dials, an acquired machine has been rebuilt as a static one, so the rung is
// no longer knowable here. The cost is that a genuinely misconfigured
// instance takes the full bound to say so, in the port's own words.
func waitForManagedNode(ctx context.Context, api ssmdial.API, worker Worker) (ssmdial.Platform, error) {
	deadline, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()

	ticker := time.NewTicker(registerPoll)
	defer ticker.Stop()

	for {
		platform, err := ssmdial.PlatformOf(deadline, api, worker.Instance)
		if err == nil {
			return platform, nil
		}

		// A throttle or a transient service error is polled through, not
		// failed on: this loop asks every five seconds, per worker, and
		// giving up on the first one throws away a machine that is already
		// launched and billing.
		if !errors.Is(err, ssmdial.ErrNotManaged) && !ssmdial.Retryable(err) {
			return "", fmt.Errorf("asking SSM about %s for %q: %w", worker.Instance, worker.URL, err)
		}

		select {
		case <-ticker.C:
		case <-deadline.Done():
			// The port's own words about what to check, with how long we were
			// willing to wait for it.
			return "", fmt.Errorf("waiting %s for the SSM agent on %s for %q: %w",
				registerTimeout, worker.Instance, worker.URL, err)
		}
	}
}

// remoteBinary is where the bootstrap fetches the shim from, and under what
// identity.
type remoteBinary struct {
	// path is an absolute path on the INSTANCE, when the operator says the
	// binary is already there.
	path string
	// url is a presigned GET for a binary uploaded to the artifact store.
	url string
	// build keys where the fetched binary is cached on the instance, and is
	// what the handshake compares against.
	build string
}

// ssmBinary decides how the instance gets a shim.
//
// Two answers, because two situations are real. An operator who bakes steps
// into an AMI names the path with ?shim= and nothing is transferred. Everyone
// else supplies a binary built for the instance (steps has no Go toolchain in
// the field, so this is the same ?binary= answer ssh:// gives) which is
// uploaded to the artifact store once, keyed by its own content hash, and
// fetched from a presigned URL the instance needs no AWS identity to use.
func ssmBinary(ctx context.Context, worker Worker) (remoteBinary, string, error) {
	if worker.Shim != "" {
		// Nothing to compare: the operator asserted what is there, and a
		// shim reporting a build this end never pushed is accepted for the
		// same reason an empty one is.
		return remoteBinary{path: worker.Shim}, "", nil
	}

	if worker.Binary == "" {
		return remoteBinary{}, "", fmt.Errorf("%w %q: an aws:// worker needs a shim binary built for it — name a local one with ?binary=/path/to/steps-linux-amd64, or one already on the instance with ?shim=/usr/local/bin/steps",
			ErrWorker, worker.URL)
	}

	if worker.ArtifactStore == "" {
		return remoteBinary{}, "", fmt.Errorf("%w %q: ?binary= reaches an aws:// worker through the artifact store, so --artifact-store must be set — or name a binary already on the instance with ?shim=",
			ErrWorker, worker.URL)
	}

	build, err := buildOf(worker)
	if err != nil {
		return remoteBinary{}, "", fmt.Errorf("worker %q: %w", worker.URL, err)
	}

	url, err := publishShim(ctx, worker, build)
	if err != nil {
		return remoteBinary{}, "", err
	}

	return remoteBinary{url: url, build: build}, build, nil
}

// publishShim puts the operator's binary in the artifact store, unless it is
// already there, and mints the URL the instance fetches it with. Keyed by the
// binary's own content hash, so a fleet uploads each build once.
func publishShim(ctx context.Context, worker Worker, build string) (string, error) {
	//nolint:contextcheck // the constructor reads only local configuration; see artifactStoreFor
	store, err := artifactStoreFor(worker.ArtifactStore)
	if err != nil {
		return "", err
	}

	key := "bin/" + build

	has, err := store.Has(ctx, key)
	if err != nil {
		return "", fmt.Errorf("worker %q: %w", worker.URL, err)
	}

	if !has {
		err = store.PutFile(ctx, key, worker.Binary)
		if err != nil {
			return "", fmt.Errorf("worker %q: %w", worker.URL, err)
		}
	}

	url, err := store.PresignGet(ctx, key, bootstrapTTL)
	if err != nil {
		return "", fmt.Errorf("worker %q: %w", worker.URL, err)
	}

	return url, nil
}

// listeningPattern reads the port out of what a listening shim printed.
var listeningPattern = regexp.MustCompile(`listening on .*:(\d+)`)

// startRemoteShim runs the bootstrap and returns the port it came up on.
//
// The port is CHOSEN BY THE INSTANCE, not by this end: the shim binds :0 and
// prints what it got. Picking a port here would mean guessing about a machine
// this process cannot see, and two concurrent steps on one instance would
// eventually guess the same one.
func startRemoteShim(ctx context.Context, api ssmdial.API, worker Worker, platform ssmdial.Platform, binary remoteBinary) (int, error) {
	output, err := ssmdial.Run(ctx, api, worker.Instance, platform, bootstrapScript(worker, binary))
	if err != nil {
		return 0, fmt.Errorf("worker %q: %w", worker.URL, err)
	}

	match := listeningPattern.FindStringSubmatch(output)
	if match == nil {
		return 0, fmt.Errorf("%w %q: the shim did not report a port it was listening on: %s",
			ErrWorker, worker.URL, strings.TrimSpace(output))
	}

	port := 0

	_, err = fmt.Sscanf(match[1], "%d", &port)
	if err != nil {
		return 0, fmt.Errorf("%w %q: unreadable port %q", ErrWorker, worker.URL, match[1])
	}

	return port, nil
}

// bootstrapScript is what SSM runs on the instance: put a shim in place if it
// is not already there, start it listening on a loopback port, and report the
// port.
//
// Loopback deliberately: the session reaches it through the SSM agent, which
// is on the instance, so nothing about this listener is reachable from the
// network — the "no inbound ports" property is the whole reason for aws://.
//
// The binary is cached under its own content hash, so a second step on the
// same instance skips the download; the write is to a .part file renamed into
// place, so two steps racing cannot serve each other half a binary.
func bootstrapScript(worker Worker, binary remoteBinary) string {
	if binary.path != "" {
		return "set -eu\n" + shimStartScript(worker, shellQuote(binary.path))
	}

	root := worker.Root
	if root == "" {
		root = "/tmp"
	}

	target := root + "/steps-shim/" + binary.build + "/steps"

	// shellQuote, not %q: this script is handed to a POSIX shell, and Go
	// quoting leaves $ and backticks live inside its double quotes — the
	// exact hole ssh.go's own quoting comment documents. curl with a wget
	// fallback, because minimal images carry one or the other.
	return fmt.Sprintf(`set -eu
BIN=%s
URL=%s
mkdir -p "$(dirname "$BIN")"
if [ ! -x "$BIN" ]; then
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL -o "$BIN.$$.part" "$URL"
  else
    wget -qO "$BIN.$$.part" "$URL"
  fi
  chmod 0700 "$BIN.$$.part"
  mv -f "$BIN.$$.part" "$BIN"
fi
%s`, shellQuote(target), shellQuote(binary.url), shimStartScript(worker, `"$BIN"`))
}

// bootstrapLinger is how long a bootstrapped shim waits to be dialled before
// reaping itself.
//
// The dial follows the bootstrap within milliseconds when it works at all, so
// this bounds only the failure: SSM throttling the StartSession, a websocket
// that will not open, an orchestrator killed between the two calls. Generous
// rather than tight, because the cost of being wrong in one direction is a
// step that fails for no reason and in the other is one stranded process.
const bootstrapLinger = 5 * time.Minute

// shimStartScript starts a shim in the background and waits for it to say
// which port it took.
//
// Detached, because SendCommand waits for the script to finish and the shim
// has to outlive it — it is the thing the session about to be opened will
// talk to. It serves one connection and exits, so nothing lingers on the
// instance once the step is done.
func shimStartScript(worker Worker, binary string) string {
	root := ""
	if worker.Root != "" {
		// Quoted for the same reason the fetch above is: a root with a space
		// in it should be that root, not two arguments.
		root = " --root " + shellQuote(worker.Root)
	}

	// A counter rather than $(seq 1 100): seq is not in every minimal image,
	// and a command substitution that fails in a for-list does NOT trip set
	// -e — the list is simply empty, the loop runs zero times, and the script
	// cats a log the shim has not written to yet. The dial then fails saying
	// the shim reported no port while the shim is in fact coming up fine.
	return fmt.Sprintf(`LOG=$(mktemp)
nohup %s _shim --listen 127.0.0.1:0 --once --linger %s%s >"$LOG" 2>&1 &
i=0
while [ "$i" -lt 100 ]; do
  if grep -q 'listening on' "$LOG"; then break; fi
  sleep 0.1
  i=$((i+1))
done
cat "$LOG"`, binary, bootstrapLinger, root)
}

// awsInstance matches an EC2 instance id, and awsTemplate a launch template
// id, so a mapping that can never name a machine is refused when it is read
// rather than at dial time.
var (
	awsInstance = regexp.MustCompile(`^i-[0-9a-f]{8,}$`)
	awsTemplate = regexp.MustCompile(`^lt-[0-9a-f]{8,}$`)
)

// applyAWS reads the three aws:// forms.
//
//	aws://i-0abc123[/root]          a running instance
//	aws://stopped/i-0abc123[/root]  a parked instance
//	aws://launch/lt-0def456[/root]  a launch template to be born from
//
// The rung is the authority rather than a query parameter, because it changes
// what the URL NAMES — an instance in two cases, a template in the third —
// and a mapping should read as the thing it points at.
func applyAWS(worker Worker, parsed *url.URL) (Worker, error) {
	target, root := parsed.Host, parsed.Path

	switch Rung(parsed.Host) {
	case RungStopped, RungLaunch:
		worker.Rung = Rung(parsed.Host)

		target, root = splitFirstSegment(parsed.Path)
		if target == "" {
			return Worker{}, fmt.Errorf("%w %q: %s needs something to acquire, as in aws://stopped/i-0abc123 or aws://launch/lt-0def456",
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

// splitFirstSegment takes the first path segment off, returning it and
// whatever absolute path remains.
func splitFirstSegment(path string) (string, string) {
	trimmed := strings.TrimPrefix(path, "/")

	slash := strings.Index(trimmed, "/")
	if slash < 0 {
		return trimmed, ""
	}

	return trimmed[:slash], trimmed[slash:]
}

// checkAWS refuses an aws:// mapping this venue cannot act on.
func checkAWS(worker Worker) error {
	err := checkAWSTarget(worker)
	if err != nil {
		return err
	}

	if worker.Shim != "" && worker.Binary != "" {
		return fmt.Errorf("%w %q: ?binary= and ?shim= are two answers to the same question — push a local binary, or name one already on the instance",
			ErrWorker, worker.URL)
	}

	switch worker.Capacity {
	case "", CapacitySpot, CapacitySpotThenOD, CapacityOnDemand:
	default:
		return fmt.Errorf("%w %q: capacity= must be spot, spot-then-od or od", ErrWorker, worker.URL)
	}

	if worker.Capacity != "" && worker.Rung != RungLaunch {
		return fmt.Errorf("%w %q: capacity= describes a machine being launched, and this worker names one that already exists",
			ErrWorker, worker.URL)
	}

	return nil
}

// PlacementCheck refuses a mapping whose dial is certain to fail, with what
// the invocation knows before any step runs. It belongs to run-start
// validation rather than to ParseWorker, because whether an artifact store is
// configured is a fact about the INVOCATION, not the URL.
//
// The alternative was the shape money dislikes: an acquisition-rung worker
// launches a real, billed instance before the dial discovers a condition that
// was decidable while kong was still parsing.
func (w Worker) PlacementCheck(hasArtifactStore bool) error {
	if w.Scheme == SchemeGCP {
		return w.gcpPlacementCheck()
	}

	if w.Scheme != SchemeAWS {
		return nil
	}

	if w.Shim == "" && w.Binary == "" {
		return fmt.Errorf("%w %q: an aws:// worker needs a shim binary built for it — name a local one with ?binary=/path/to/steps-linux-amd64, or one already on the instance with ?shim=/usr/local/bin/steps",
			ErrWorker, w.URL)
	}

	if w.Binary != "" && !hasArtifactStore {
		return fmt.Errorf("%w %q: ?binary= reaches an aws:// worker through the artifact store, so --artifact-store must be set — or name a binary already on the instance with ?shim=",
			ErrWorker, w.URL)
	}

	return nil
}

// checkAWSTarget refuses a rung whose target is not the kind of id it needs.
func checkAWSTarget(worker Worker) error {
	if worker.Rung == RungLaunch {
		if !awsTemplate.MatchString(worker.Template) {
			return fmt.Errorf("%w %q: the launch rung needs a launch template id, as in aws://launch/lt-0def4567", ErrWorker, worker.URL)
		}

		return nil
	}

	if !awsInstance.MatchString(worker.Instance) {
		return fmt.Errorf("%w %q: aws needs an instance id, as in aws://i-0abc123def456789", ErrWorker, worker.URL)
	}

	return nil
}
