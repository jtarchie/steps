package venue

// The aws:// venue: a worker reached through SSM, with no inbound port, no
// sshd, and no host key.
//
// SSM gives a port forward and a command channel. The command installs this
// process's ssh key (awsssh.go); the forward reaches the instance's sshd, and
// the docker daemon behind it is driven exactly as docker+ssh:// drives one.

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

// ssmAPIFor builds the SSM client for a worker.
//
// A package variable so a test can stand in for the whole control plane
// without a network or credentials — the only way to exercise this path at
// all without an AWS account.
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
	return applyRungs(worker, parsed, "aws://stopped/i-0abc123 or aws://launch/lt-0def456")
}

// applyRungs reads the acquisition rung out of a scheme's URL. Shared,
// because the grammar is: gcp://stopped/worker-1 and aws://stopped/i-0abc123
// differ only in the example a refusal quotes, and a fourth rung — or a
// change to how the root is split off — has one place to be made rather than
// one per cloud.
func applyRungs(worker Worker, parsed *url.URL, examples string) (Worker, error) {
	target, root := parsed.Host, parsed.Path

	switch Rung(parsed.Host) {
	case RungStopped, RungLaunch:
		worker.Rung = Rung(parsed.Host)

		target, root = splitFirstSegment(parsed.Path)
		if target == "" {
			return Worker{}, fmt.Errorf("%w %q: %s needs something to acquire, as in %s",
				ErrWorker, worker.URL, parsed.Host, examples)
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

// ImageCheck refuses a step whose need for a container this worker cannot meet, while the run can still refuse before any step: ssh:// runs bare and docker+ runs nothing else.
func (w Worker) ImageCheck(hasImage bool) error {
	switch {
	case w.Scheme == SchemeSSH && hasImage:
		return fmt.Errorf("%w %q: %w", ErrWorker, w.URL, errImageOnSSH)
	case w.dockerPlus() && !hasImage:
		return fmt.Errorf("%w %q: %w", ErrWorker, w.URL, errNoImageOnDocker)
	default:
		return nil
	}
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
