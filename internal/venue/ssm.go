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
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/jtarchie/steps/internal/blobstore"
	"github.com/jtarchie/steps/internal/shim"
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

	platform, err := ssmdial.PlatformOf(ctx, api, worker.Instance)
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
		close: func(context.Context) error {
			// Closing the channel is the goodbye. The remote shim serves one
			// connection and exits (see the bootstrap), so nothing is left
			// running on the instance and there is nothing to reap.
			return channel.Close()
		},
	}, nil
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

	build, err := shim.BuildOf(worker.Binary)
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
	opts, err := blobstore.Parse(worker.ArtifactStore)
	if err != nil {
		return "", err //nolint:wrapcheck // blobstore names the URL and the rule it broke
	}

	store, err := blobstore.New(ctx, opts)
	if err != nil {
		return "", err //nolint:wrapcheck // as above
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
		return shimStartScript(worker, binary.path)
	}

	root := worker.Root
	if root == "" {
		root = "/tmp"
	}

	target := fmt.Sprintf("%s/steps-shim/%s/steps", root, binary.build)

	return fmt.Sprintf(`set -eu
BIN=%q
mkdir -p "$(dirname "$BIN")"
if [ ! -x "$BIN" ]; then
  curl -fsSL -o "$BIN.$$.part" %q
  chmod 0700 "$BIN.$$.part"
  mv -f "$BIN.$$.part" "$BIN"
fi
%s`, target, binary.url, shimStartScript(worker, "$BIN"))
}

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
		root = " --root " + worker.Root
	}

	return fmt.Sprintf(`LOG=$(mktemp)
nohup %s _shim --listen 127.0.0.1:0 --once%s >"$LOG" 2>&1 &
for _ in $(seq 1 100); do
  if grep -q 'listening on' "$LOG"; then break; fi
  sleep 0.1
done
cat "$LOG"`, binary, root)
}

// awsInstance matches an EC2 instance id, so a mapping that can never name a
// machine is refused when it is read rather than at dial time.
var awsInstance = regexp.MustCompile(`^i-[0-9a-f]{8,}$`)

// checkAWS refuses an aws:// mapping this venue cannot act on.
func checkAWS(worker Worker) error {
	if !awsInstance.MatchString(worker.Instance) {
		return fmt.Errorf("%w %q: aws needs an instance id, as in aws://i-0abc123def456789", ErrWorker, worker.URL)
	}

	if worker.Shim != "" && worker.Binary != "" {
		return fmt.Errorf("%w %q: ?binary= and ?shim= are two answers to the same question — push a local binary, or name one already on the instance",
			ErrWorker, worker.URL)
	}

	return nil
}
