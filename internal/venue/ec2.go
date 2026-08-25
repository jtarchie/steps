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
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Rung is how much has to happen before a worker can be dialed.
type Rung string

const (
	// RungStatic is a machine that is already running.
	RungStatic Rung = ""
	// RungStopped is a parked machine: started for the job, stopped after.
	RungStopped Rung = "stopped"
	// RungLaunch is a machine born for the job and terminated with it.
	RungLaunch Rung = "launch"
)

// Capacity says which EC2 capacity to ask for on the launch rung.
type Capacity string

const (
	// CapacitySpot asks for spot only, and fails when there is none.
	CapacitySpot Capacity = "spot"
	// CapacitySpotThenOD asks for spot and falls back to on-demand in the
	// same call, so a job runs rather than failing on a busy pool.
	CapacitySpotThenOD Capacity = "spot-then-od"
	// CapacityOnDemand asks for on-demand only.
	CapacityOnDemand Capacity = "od"
)

// defaultIdle is how long a parked instance is left running after the job
// that started it, so a pipeline whose jobs run back to back does not pay the
// start-up cost every time.
//
// Zero would mean stopping the instance the moment a job ends, which for a
// pipeline of several jobs against one tag is the worst of both worlds: the
// full boot cost, every job, with the EBS bill anyway. A knob rather than a
// hardcode because the right answer depends on how often a fleet runs, and
// ?idle=0 is the honest way to say "stop it immediately".
const defaultIdle = 5 * time.Minute

// acquireTimeout bounds waiting for a machine to reach a usable state. Cloud
// acquisition is 20-90 seconds; a Windows instance without fast launch is
// minutes, which is one more reason Windows is refused earlier.
const acquireTimeout = 10 * time.Minute

// acquirePoll is how often a starting instance is asked whether it is ready.
const acquirePoll = 5 * time.Second

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

	return ec2.NewFromConfig(cfg), nil
}

// needsAcquisition reports whether a worker names a machine that has to be
// brought into existence before it can be dialed.
func (w Worker) needsAcquisition() bool {
	return w.Rung == RungStopped || w.Rung == RungLaunch
}

// errNoCapacity is a launch that produced no instance.
var errNoCapacity = errors.New("no capacity for the requested worker")

// acquire brings a worker's machine into existence and returns the static
// worker that names it, plus how to give it back.
func acquire(ctx context.Context, worker Worker) (Worker, func(context.Context) error, error) {
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
	_, err := api.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{worker.Instance}})
	if err != nil {
		return Worker{}, nil, fmt.Errorf("starting %s for %q: %w", worker.Instance, worker.URL, err)
	}

	err = waitForRunning(ctx, api, worker, worker.Instance)
	if err != nil {
		return Worker{}, nil, err
	}

	release := func(ctx context.Context) error {
		// After the idle window, not immediately: a pipeline whose jobs run
		// back to back would otherwise pay a cold start for every one of
		// them. ?idle=0 stops it at once.
		if worker.Idle > 0 {
			select {
			case <-time.After(worker.Idle):
			case <-ctx.Done():
			}
		}

		_, stopErr := api.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{worker.Instance}})
		if stopErr != nil {
			return fmt.Errorf("stopping %s for %q: %w", worker.Instance, worker.URL, stopErr)
		}

		return nil
	}

	return worker.asStatic(worker.Instance), release, nil
}

// launchInstance creates one instance from a launch template and terminates
// it when the job ends.
func launchInstance(ctx context.Context, api ec2API, worker Worker) (Worker, func(context.Context) error, error) {
	out, err := api.CreateFleet(ctx, fleetRequest(worker))
	if err != nil {
		return Worker{}, nil, fmt.Errorf("launching a worker for %q: %w", worker.URL, err)
	}

	instance := firstFleetInstance(out)
	if instance == "" {
		return Worker{}, nil, fmt.Errorf("%w %q: %s", errNoCapacity, worker.URL, fleetErrors(out))
	}

	err = waitForRunning(ctx, api, worker, instance)
	if err != nil {
		// Terminated rather than left behind: a machine this end cannot use
		// is still a machine somebody is paying for.
		_, _ = api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{instance}})

		return Worker{}, nil, err
	}

	release := func(ctx context.Context) error {
		_, stopErr := api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{instance}})
		if stopErr != nil {
			return fmt.Errorf("terminating %s for %q: %w", instance, worker.URL, stopErr)
		}

		return nil
	}

	return worker.asStatic(instance), release, nil
}

// fleetRequest asks for exactly one instance from the worker's template.
//
// CreateFleet in `instant` mode rather than RunInstances, because it is the
// only call that can ask for spot with an on-demand fallback and instance
// type diversification in ONE request — which is what makes a spot worker
// something a job can rely on rather than a gamble.
func fleetRequest(worker Worker) *ec2.CreateFleetInput {
	request := &ec2.CreateFleetInput{
		Type: ec2types.FleetTypeInstant,
		LaunchTemplateConfigs: []ec2types.FleetLaunchTemplateConfigRequest{{
			LaunchTemplateSpecification: &ec2types.FleetLaunchTemplateSpecificationRequest{
				LaunchTemplateId: aws.String(worker.Template),
				Version:          aws.String("$Default"),
			},
		}},
		TargetCapacitySpecification: &ec2types.TargetCapacitySpecificationRequest{
			TotalTargetCapacity: aws.Int32(1),
		},
	}

	spot := &ec2types.SpotOptionsRequest{AllocationStrategy: ec2types.SpotAllocationStrategyPriceCapacityOptimized}

	switch worker.Capacity {
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
// minutes to every acquisition. The dial that follows waits for the agent on
// its own terms and reports that failure in its own words.
func waitForRunning(ctx context.Context, api ec2API, worker Worker, instance string) error {
	deadline, cancel := context.WithTimeout(ctx, acquireTimeout)
	defer cancel()

	ticker := time.NewTicker(acquirePoll)
	defer ticker.Stop()

	for {
		out, err := api.DescribeInstances(deadline, &ec2.DescribeInstancesInput{InstanceIds: []string{instance}})
		if err != nil {
			return fmt.Errorf("waiting for %s for %q: %w", instance, worker.URL, err)
		}

		state := instanceState(out)
		switch state {
		case ec2types.InstanceStateNameRunning:
			return nil
		case ec2types.InstanceStateNameTerminated, ec2types.InstanceStateNameShuttingDown,
			ec2types.InstanceStateNameStopping, ec2types.InstanceStateNameStopped:
			// A machine going the other way is never going to arrive: an
			// eviction, or a hard-TTL shutdown that fired mid-acquisition.
			return fmt.Errorf("%w %q: %s went to %s while being acquired", ErrWorker, worker.URL, instance, state)
		case ec2types.InstanceStateNamePending:
		}

		select {
		case <-ticker.C:
		case <-deadline.Done():
			return fmt.Errorf("waiting for %s for %q: %w", instance, worker.URL, deadline.Err())
		}
	}
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

// asStatic is this worker as the already-running machine it just became, so
// everything downstream dials it without knowing how it got there.
func (w Worker) asStatic(instance string) Worker {
	w.Rung = RungStatic
	w.Instance = instance

	return w
}
