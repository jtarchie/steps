package venue

// The acquisition ladder: workers that do not exist when a step asks for one.
//
// Three rungs share the aws:// scheme, in ascending order of how much steps
// has to do before there is a machine to dial:
//
//	aws://i-0abc123          a running instance — dial it
//	aws://stopped/i-0abc123  a parked instance — start it, dial it, stop it
//	aws://launch/lt-0def456  no instance — launch one, dial it, terminate it
//
// The launch template owns the ENTIRE EC2 vocabulary: AMI, instance type and
// its overrides, subnet, security groups, instance profile, spot options,
// user data. steps grows no EC2 configuration surface at all, which is why
// the only knobs here are the two that describe how steps itself behaves —
// which capacity to ask for, and how long to hold a parked machine.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/jtarchie/steps/internal/events"
)

// ec2API is the slice of EC2 this package uses, declared so a test can stand
// in for it without an account.
type ec2API interface {
	StartInstances(ctx context.Context, in *ec2.StartInstancesInput, opts ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	StopInstances(ctx context.Context, in *ec2.StopInstancesInput, opts ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, opts ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	CreateFleet(ctx context.Context, in *ec2.CreateFleetInput, opts ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error)
}

// ec2For builds the EC2 client for a worker.
//
// A package variable for the same reason ssmAPIFor is one: acquiring a
// machine is the part of this feature that cannot be exercised without an AWS
// account, so it is the part a test has to be able to stand in for.
//
//nolint:gochecknoglobals // a test seam for a control plane, documented above
var ec2For = func(ctx context.Context, worker Worker) (ec2API, error) {
	loaders := []func(*awsconfig.LoadOptions) error{}
	if worker.Region != "" {
		loaders = append(loaders, awsconfig.WithRegion(worker.Region))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, loaders...)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", ErrWorker, worker.URL, err)
	}

	return newEC2Funcs(ec2.NewFromConfig(cfg)), nil
}

// errNoCapacity is a launch that produced no instance.
var errNoCapacity = errors.New("no capacity for the requested worker")

// acquireEC2 is acquire for the aws:// rungs.
func acquireEC2(ctx context.Context, worker Worker) (Worker, func(context.Context) error, error) {
	api, err := ec2For(ctx, worker)
	if err != nil {
		return Worker{}, nil, err
	}

	switch worker.Rung {
	case RungStopped:
		return startParked(ctx, api, worker)
	case RungLaunch:
		return launchInstance(ctx, api, worker)
	case RungStatic:
		return worker, nil, nil
	default:
		return Worker{}, nil, fmt.Errorf("%w %q: unknown acquisition rung %q", ErrWorker, worker.URL, worker.Rung)
	}
}

// startParked starts a stopped instance and parks it again when the job is
// done with it.
func startParked(ctx context.Context, api ec2API, worker Worker) (Worker, func(context.Context) error, error) {
	// A previous park may still be settling — release returns once StopInstances is ACCEPTED, and EC2 refuses to start a `stopping` instance.
	state, err := awaitParkComplete(ctx, api, worker)
	if err != nil {
		return Worker{}, nil, err
	}

	if state == ec2types.InstanceStateNameRunning || state == ec2types.InstanceStateNamePending {
		return adoptRunning(ctx, api, worker)
	}

	_, err = api.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{worker.Instance}})
	if err != nil {
		return Worker{}, nil, fmt.Errorf("starting %s for %q: %w", worker.Instance, worker.URL, err)
	}

	err = waitForRunning(ctx, api, worker, worker.Instance, ec2types.InstanceStateNameStopped)
	if err != nil {
		// A machine this end started and cannot use is still a machine
		// somebody is paying for, and nothing later will stop it: the lease
		// records no release on a failed acquire.
		//nolint:contextcheck // deliberately not the caller's context: its being cancelled is the likeliest reason to be here
		stopInstance(api, worker, worker.Instance)

		return Worker{}, nil, err
	}

	release := cleanupRelease(worker, worker.Instance, "stopping", "parked", stopEC2(api, worker.Instance))

	events.Note(ctx, events.NoteInfo, fmt.Sprintf("worker %s: started %s", worker.URL, worker.Instance))

	return worker.asStatic(worker.Instance), release, nil
}

// adoptRunning gives no release, because a machine steps did not start may be anybody's work in progress — and says so aloud, because an instance nobody stops is one somebody has to find.
func adoptRunning(ctx context.Context, api ec2API, worker Worker) (Worker, func(context.Context) error, error) {
	err := waitForRunning(ctx, api, worker, worker.Instance, "")
	if err != nil {
		return Worker{}, nil, err
	}

	noteAdopted(ctx, worker)

	return worker.asStatic(worker.Instance), nil, nil
}

// stopInstance is the best-effort stop for a machine a failed acquisition
// leaves running, under its own context for the reason cleanupTimeout gives.
func stopInstance(api ec2API, worker Worker, instance string) {
	cleanupAfterFailure(worker, instance, "venue.stop_failed", stopEC2(api, instance))
}

func stopEC2(api ec2API, instance string) func(context.Context) error {
	return func(ctx context.Context) error {
		_, err := api.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{instance}})

		return err //nolint:wrapcheck // the caller names the instance and the worker
	}
}

// launchInstance creates one instance from a launch template and terminates
// it when the job ends.
func launchInstance(ctx context.Context, api ec2API, worker Worker) (Worker, func(context.Context) error, error) {
	labels := worker.launchLabels()

	// Said BEFORE the money is spent, as gceLaunch does: EC2 chooses the id, so a crash past CreateFleet leaves this label as the transcript's only way to the machine.
	events.Note(ctx, events.NoteInfo, fmt.Sprintf("worker %s: launching from template %s (%s=%s)",
		worker.URL, worker.Template, labelWorker, labels[labelWorker]))

	token := launchToken(labels)
	labels[labelLaunch] = token

	request := fleetRequest(worker, labels)
	request.ClientToken = aws.String(token)

	// Not the caller's context: cancelled once AWS has accepted, the SDK
	// returns an error and no instance id, leaving a billing machine nothing
	// can name. Bounded instead, and the caller checked once the id is known.
	fleetCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), acquireTimeout)
	defer cancel()

	out, err := api.CreateFleet(fleetCtx, request)
	if err != nil {
		if !refused(err) {
			//nolint:contextcheck // as stopInstance: the caller's context may be the problem
			terminateLostLaunch(api, worker, token)
		}

		return Worker{}, nil, fmt.Errorf("launching a worker for %q: %w", worker.URL, err)
	}

	instance := firstFleetInstance(out)
	if instance == "" {
		return Worker{}, nil, fmt.Errorf("%w %q: %s", errNoCapacity, worker.URL, fleetErrors(out))
	}

	if ctx.Err() != nil {
		slog.WarnContext(ctx, "venue.launch_abandoned", "instance", instance, "worker", worker.URL, "error", ctx.Err())
		//nolint:contextcheck // as stopInstance: the caller's context is the problem
		terminateInstance(api, worker, instance)

		return Worker{}, nil, fmt.Errorf("launching a worker for %q: %w", worker.URL, ctx.Err())
	}

	// No state to leave: a fleet's instance did not exist a moment ago.
	err = waitForRunning(ctx, api, worker, instance, "")
	if err != nil {
		// Terminated rather than left behind: a machine this end cannot use
		// is still a machine somebody is paying for. Its own context, because
		// the likeliest reason to be here is the caller's being cancelled —
		// and a terminate the SDK aborts before it reaches AWS leaves a
		// billing instance nothing will ever look for.
		//nolint:contextcheck // as stopInstance: the caller's context may be the problem
		terminateInstance(api, worker, instance)

		return Worker{}, nil, err
	}

	release := cleanupRelease(worker, instance, "terminating", "terminated", terminateEC2(api, instance))

	events.Note(ctx, events.NoteInfo, fmt.Sprintf("worker %s: launched %s", worker.URL, instance))

	return worker.asStatic(instance), release, nil
}

// launchToken is CreateFleet's idempotency key: one per launch, so the SDK's
// retry of a request whose answer was lost is the same launch rather than a
// second machine, while the next launch is never answered with this one's.
func launchToken(labels map[string]string) string {
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)

	return labels[labelWorker] + "-" + hex.EncodeToString(nonce)
}

// labelLaunch tags an instance with its launch's ClientToken, the one value
// unique to a launch: the other labels are shared by every launch of a worker
// from this process, so a search by them could terminate a sibling's machine.
const labelLaunch = "steps-launch"

// lostLaunchSearches is how many acquirePoll-spaced searches a lost launch
// gets, because a tag search reads a replica that lags a launch it has not
// yet seen.
const lostLaunchSearches = 12

// refused reports whether AWS answered CreateFleet with a client fault, which
// is a request it refused rather than one it may have acted on.
func refused(err error) bool {
	var apiErr smithy.APIError

	return errors.As(err, &apiErr) && apiErr.ErrorFault() == smithy.FaultClient
}

// terminateLostLaunch finds and terminates what a CreateFleet with no answer
// launched.
//
// Searched by tag rather than re-sent with the same ClientToken: AWS documents
// a retry of a request that COMPLETED, not what a re-send returns while the
// original is in flight or after it failed, nor how long a token is kept.
func terminateLostLaunch(api ec2API, worker Worker, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	ticker := time.NewTicker(acquirePoll)
	defer ticker.Stop()

	input := &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		{Name: aws.String("tag:" + labelLaunch), Values: []string{token}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
	}}

search:
	for range lostLaunchSearches {
		out, err := api.DescribeInstances(ctx, input)
		if err != nil {
			slog.Warn("venue.lost_launch_search_failed", labelLaunch, token, "worker", worker.URL, "error", err)
		}

		if ids := describedIDs(out); len(ids) > 0 {
			slog.Warn("venue.lost_launch_terminated", labelLaunch, token, "worker", worker.URL, "instances", ids)

			for _, id := range ids {
				terminateInstance(api, worker, id)
			}

			return
		}

		select {
		case <-ctx.Done():
			break search
		case <-ticker.C:
		}
	}

	slog.Warn("venue.lost_launch_not_found", labelLaunch, token, "worker", worker.URL,
		"advice", "if an instance carries this tag, nothing will terminate it")
}

// describedIDs lists the instances a describe response names.
func describedIDs(out *ec2.DescribeInstancesOutput) []string {
	var ids []string

	if out == nil {
		return ids
	}

	for _, reservation := range out.Reservations {
		for _, instance := range reservation.Instances {
			ids = append(ids, aws.ToString(instance.InstanceId))
		}
	}

	return ids
}

// terminateInstance is stopInstance for a machine that should not exist at
// all once this end cannot use it.
func terminateInstance(api ec2API, worker Worker, instance string) {
	cleanupAfterFailure(worker, instance, "venue.terminate_failed", terminateEC2(api, instance))
}

func terminateEC2(api ec2API, instance string) func(context.Context) error {
	return func(ctx context.Context) error {
		_, err := api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{instance}})

		return err //nolint:wrapcheck // the caller names the instance and the worker
	}
}

// fleetRequest asks for exactly one instance from the worker's template.
//
// CreateFleet in `instant` mode rather than RunInstances, because it is the
// only call that can ask for spot with an on-demand fallback and instance
// type diversification in ONE request — which is what makes a spot worker
// something a job can rely on rather than a gamble.
//
// The labels ride IN the request, never a CreateTags after it: a separate call
// leaves a window where a crash strands an untagged, billing machine — the
// case the labels exist for — while a refused tag here refuses the launch.
func fleetRequest(worker Worker, labels map[string]string) *ec2.CreateFleetInput {
	tags := make([]ec2types.Tag, 0, len(labels))

	for _, key := range slices.Sorted(maps.Keys(labels)) {
		tags = append(tags, ec2types.Tag{Key: aws.String(key), Value: aws.String(labels[key])})
	}

	request := &ec2.CreateFleetInput{
		Type: ec2types.FleetTypeInstant,
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeInstance,
			Tags:         tags,
		}},
		LaunchTemplateConfigs: []ec2types.FleetLaunchTemplateConfigRequest{{
			LaunchTemplateSpecification: &ec2types.FleetLaunchTemplateSpecificationRequest{
				LaunchTemplateId: aws.String(worker.Template),
				Version:          aws.String(worker.Version),
			},
		}},
		TargetCapacitySpecification: &ec2types.TargetCapacitySpecificationRequest{
			TotalTargetCapacity: aws.Int32(1),
		},
	}

	spot := &ec2types.SpotOptionsRequest{AllocationStrategy: ec2types.SpotAllocationStrategyPriceCapacityOptimized}

	switch worker.Capacity {
	case "":
		// Explicitly on-demand, not left to the service: AWS has no
		// "template decides" semantics for a fleet's capacity type, and an
		// omitted field silently defaulting somewhere is exactly the wrong
		// property for the knob that decides what a machine costs.
		request.TargetCapacitySpecification.DefaultTargetCapacityType = ec2types.DefaultTargetCapacityTypeOnDemand
		request.TargetCapacitySpecification.OnDemandTargetCapacity = aws.Int32(1)
	case CapacityOnDemand:
		request.TargetCapacitySpecification.DefaultTargetCapacityType = ec2types.DefaultTargetCapacityTypeOnDemand
		request.TargetCapacitySpecification.OnDemandTargetCapacity = aws.Int32(1)
	case CapacitySpot:
		request.TargetCapacitySpecification.DefaultTargetCapacityType = ec2types.DefaultTargetCapacityTypeSpot
		request.TargetCapacitySpecification.SpotTargetCapacity = aws.Int32(1)
		request.SpotOptions = spot
	case CapacitySpotThenOD:
		// Spot preferred, on-demand rather than nothing: the fallback is what
		// this rung means by asking for spot at all.
		request.TargetCapacitySpecification.DefaultTargetCapacityType = ec2types.DefaultTargetCapacityTypeSpot
		request.TargetCapacitySpecification.SpotTargetCapacity = aws.Int32(1)
		request.SpotOptions = spot
		request.OnDemandOptions = &ec2types.OnDemandOptionsRequest{
			AllocationStrategy: ec2types.FleetOnDemandAllocationStrategyLowestPrice,
		}
	}

	return request
}

// firstFleetInstance reads the one instance a fleet produced.
func firstFleetInstance(out *ec2.CreateFleetOutput) string {
	for _, instances := range out.Instances {
		for _, id := range instances.InstanceIds {
			return id
		}
	}

	return ""
}

// fleetErrors is what EC2 said about a fleet that produced nothing, which is
// the only account of an exhausted spot pool or a malformed template.
func fleetErrors(out *ec2.CreateFleetOutput) string {
	for _, failure := range out.Errors {
		if failure.ErrorMessage != nil {
			return aws.ToString(failure.ErrorMessage)
		}
	}

	return "EC2 returned no instance and no error"
}

// waitForRunning blocks until an instance is running, or says why it will not
// be.
//
// Running, not "passed its status checks": what has to be true is that the
// SSM agent can register, and asking EC2 to confirm reachability would add
// minutes to every acquisition. The dial that follows waits for the agent
// itself, in waitForManagedNode — a promise this comment made for a long time
// before anything kept it, which is why the launch rung never once worked
// against real AWS.
//
// leaving names the state the instance is departing, and is empty for a
// machine that did not exist a moment ago. It is what keeps the parked rung
// honest: DescribeInstances answers from a replica, so it reports "stopped"
// for a second or two AFTER StartInstances succeeded — read as terminal that
// fails the acquisition on its first poll and then stops the machine it just
// started. Tolerated only until the instance is seen to leave, so a start
// that silently did not take is still bounded by acquireTimeout, and a
// machine that goes back is terminal exactly as it was.
func waitForRunning(ctx context.Context, api ec2API, worker Worker, instance string, leaving ec2types.InstanceStateName) error {
	left := leaving == ""

	err := pollUntil(ctx, acquireTimeout, acquirePoll, func(ctx context.Context) (bool, error) {
		out, err := api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instance}})
		if err != nil {
			return false, waitOutInvisible(worker, instance, err)
		}

		state := instanceState(out)

		if !left {
			// Still reading the state it was asked to leave: a replica lagging
			// behind a StartInstances that already succeeded, not an answer.
			if state == leaving {
				return false, nil
			}

			left = true
		}

		return arrivedOrDead(state, worker, instance)
	})

	return waitingFor(err, worker, instance)
}

// awaitParkComplete reports the settled state because a running one is somebody else's machine, not one to start.
func awaitParkComplete(ctx context.Context, api ec2API, worker Worker) (ec2types.InstanceStateName, error) {
	var state ec2types.InstanceStateName

	err := pollUntil(ctx, acquireTimeout, acquirePoll, func(ctx context.Context) (bool, error) {
		out, err := api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{worker.Instance}})
		if err != nil {
			return false, waitOutInvisible(worker, worker.Instance, err)
		}

		state = instanceState(out)

		switch state {
		case ec2types.InstanceStateNameStopping, ec2types.InstanceStateNameShuttingDown:
		case ec2types.InstanceStateNamePending, ec2types.InstanceStateNameRunning,
			ec2types.InstanceStateNameStopped, ec2types.InstanceStateNameTerminated:
			// Settled either way: whether starting it can work is the next call's answer.
			return true, nil
		}

		return false, nil
	})
	if err != nil {
		return "", waitingFor(err, worker, worker.Instance)
	}

	return state, nil
}

// arrivedOrDead reads a polled state as one of the three answers this loop
// has: it is here, it is never coming, or keep waiting.
func arrivedOrDead(state ec2types.InstanceStateName, worker Worker, instance string) (bool, error) {
	switch state {
	case ec2types.InstanceStateNameRunning:
		return true, nil
	case ec2types.InstanceStateNameTerminated, ec2types.InstanceStateNameShuttingDown,
		ec2types.InstanceStateNameStopping, ec2types.InstanceStateNameStopped:
		// A machine going the other way is never going to arrive: an
		// eviction, or a hard-TTL shutdown that fired mid-acquisition.
		return false, fmt.Errorf("%w %q: %s went to %s while being acquired", ErrWorker, worker.URL, instance, state)
	case ec2types.InstanceStateNamePending:
	}

	return false, nil
}

// waitOutInvisible reads EC2's replica lag as "keep waiting", and gives back
// any error that was not lag.
//
// The same lag as the departing state waitForRunning tolerates, one step
// earlier: EC2 hands back an instance id from CreateFleet and then denies the
// instance exists for a few seconds. Tolerated for the whole acquireTimeout
// rather than only until the instance is first seen — every id that reaches
// here came from EC2 itself, CreateFleet's answer or one StartInstances
// already accepted, so lag is the only thing this can be and a guard against
// a bad id would be one no test could reach.
func waitOutInvisible(worker Worker, instance string, cause error) error {
	if !notYetVisible(cause) {
		return fmt.Errorf("waiting for %s for %q: %w", instance, worker.URL, cause)
	}

	return nil
}

// notYetVisible reports whether EC2 answered that an instance does not exist.
//
// By error CODE, never by message text: the message names the id and is
// phrased for a human, while the code is the contract. An instance that truly
// does not exist reports the same thing, which is why the caller only
// tolerates this before it has ever seen the instance.
func notYetVisible(err error) bool {
	var apiErr smithy.APIError

	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidInstanceID.NotFound"
}

// instanceState reads the one instance's state out of a describe response.
func instanceState(out *ec2.DescribeInstancesOutput) ec2types.InstanceStateName {
	for _, reservation := range out.Reservations {
		for _, instance := range reservation.Instances {
			if instance.State != nil {
				return instance.State.Name
			}
		}
	}

	return ec2types.InstanceStateNamePending
}
