package venue

// The acquisition ladder, against a fake EC2.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// fakeEC2 records what was asked of it and answers with the states a test set
// up. Deliberately not a simulator: what these tests pin is which calls the
// ladder makes, in what order, and what it does with the answers.
type fakeEC2 struct {
	mu sync.Mutex

	// pendingBefore is how many describes report "pending" before the
	// instance is reported running, standing in for a boot.
	pendingBefore int
	// endState, when set, is what the instance goes to instead of running —
	// an eviction or a hard-TTL shutdown mid-acquisition.
	endState ec2types.InstanceStateName
	// fleetEmpty makes CreateFleet return no instance, the shape of an
	// exhausted spot pool.
	fleetEmpty bool

	started    []string
	stopped    []string
	terminated []string
	describes  int
	fleets     []*ec2.CreateFleetInput
}

func (f *fakeEC2) StartInstances(_ context.Context, in *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.started = append(f.started, in.InstanceIds...)

	return &ec2.StartInstancesOutput{}, nil
}

func (f *fakeEC2) StopInstances(_ context.Context, in *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.stopped = append(f.stopped, in.InstanceIds...)

	return &ec2.StopInstancesOutput{}, nil
}

func (f *fakeEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.terminated = append(f.terminated, in.InstanceIds...)

	return &ec2.TerminateInstancesOutput{}, nil
}

func (f *fakeEC2) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.describes++

	state := ec2types.InstanceStateNameRunning

	switch {
	case f.endState != "":
		state = f.endState
	case f.describes <= f.pendingBefore:
		state = ec2types.InstanceStateNamePending
	}

	return &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{
			Instances: []ec2types.Instance{{State: &ec2types.InstanceState{Name: state}}},
		}},
	}, nil
}

func (f *fakeEC2) CreateFleet(_ context.Context, in *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.fleets = append(f.fleets, in)

	if f.fleetEmpty {
		return &ec2.CreateFleetOutput{
			Errors: []ec2types.CreateFleetError{{ErrorMessage: aws.String("InsufficientInstanceCapacity")}},
		}, nil
	}

	return &ec2.CreateFleetOutput{
		Instances: []ec2types.CreateFleetInstance{{InstanceIds: []string{"i-0launched1234567"}}},
	}, nil
}

// seamEC2 points acquisition at the fake.
func seamEC2(t *testing.T, fake *fakeEC2) {
	t.Helper()

	previous := ec2For
	ec2For = func(context.Context, Worker) (ec2API, error) { return fake, nil }

	t.Cleanup(func() { ec2For = previous })
}

// TestLeaseStartsAndParksAWorker is the stopped rung: the first step starts
// the machine, and the job's end parks it again.
func TestLeaseStartsAndParksAWorker(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	worker, err := ParseWorker("aws://stopped/i-0abc123def456789?idle=0")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"gpu": worker})

	resolved, err := leases.Resolve(context.Background(), "gpu")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// What comes back is a plain running instance: everything downstream
	// dials it without knowing how it got there.
	if resolved.Rung != RungStatic || resolved.Instance != "i-0abc123def456789" {
		t.Errorf("resolved = %+v, want the static instance it became", resolved)
	}

	err = leases.ReleaseAll(context.Background())
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.started) != 1 || fake.started[0] != "i-0abc123def456789" {
		t.Errorf("started = %v, want the parked instance", fake.started)
	}

	if len(fake.stopped) != 1 {
		t.Errorf("stopped = %v, want the instance parked again", fake.stopped)
	}

	if len(fake.terminated) != 0 {
		t.Errorf("terminated = %v, want a parked instance never destroyed", fake.terminated)
	}
}

// TestLeaseAcquiresOncePerJob is the reason the lease exists: acquisition
// costs 20-90 seconds and real money, so every step of a job that shares a
// tag shares one machine — including steps running in parallel.
func TestLeaseAcquiresOncePerJob(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	worker, err := ParseWorker("aws://stopped/i-0abc123def456789?idle=0")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"gpu": worker})

	var wg sync.WaitGroup

	for range 8 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_, resolveErr := leases.Resolve(context.Background(), "gpu")
			if resolveErr != nil {
				t.Errorf("Resolve: %v", resolveErr)
			}
		}()
	}

	wg.Wait()

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.started) != 1 {
		t.Errorf("the instance was started %d times, want once for the whole job", len(fake.started))
	}
}

// TestLeaseLaunchesAndTerminates is the launch rung: a machine born for the
// job and destroyed with it.
func TestLeaseLaunchesAndTerminates(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	worker, err := ParseWorker("aws://launch/lt-0def4567890abcde?capacity=spot-then-od")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"burst": worker})

	resolved, err := leases.Resolve(context.Background(), "burst")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if resolved.Instance != "i-0launched1234567" {
		t.Errorf("resolved instance = %q, want the launched one", resolved.Instance)
	}

	err = leases.ReleaseAll(context.Background())
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.terminated) != 1 || fake.terminated[0] != "i-0launched1234567" {
		t.Errorf("terminated = %v, want the launched instance destroyed", fake.terminated)
	}

	if len(fake.stopped) != 0 {
		t.Errorf("stopped = %v, want a launched instance terminated rather than parked", fake.stopped)
	}

	assertSpotWithFallback(t, fake.fleets[0])
}

// assertSpotWithFallback pins the request shape that makes a spot worker
// something a job can rely on: spot preferred, on-demand rather than nothing,
// both asked for in one call.
func assertSpotWithFallback(t *testing.T, request *ec2.CreateFleetInput) {
	t.Helper()

	if request.TargetCapacitySpecification.DefaultTargetCapacityType != ec2types.DefaultTargetCapacityTypeSpot {
		t.Errorf("capacity type = %v, want spot", request.TargetCapacitySpecification.DefaultTargetCapacityType)
	}

	if request.OnDemandOptions == nil {
		t.Error("spot-then-od asked for no on-demand fallback")
	}

	if request.SpotOptions.AllocationStrategy != ec2types.SpotAllocationStrategyPriceCapacityOptimized {
		t.Errorf("allocation strategy = %v, want price-capacity-optimized", request.SpotOptions.AllocationStrategy)
	}
}

// TestLeaseReportsAnExhaustedPool pins the error a spot worker actually hits:
// EC2's own account of why there was no capacity.
func TestLeaseReportsAnExhaustedPool(t *testing.T) {
	fake := &fakeEC2{fleetEmpty: true}
	seamEC2(t, fake)

	worker, err := ParseWorker("aws://launch/lt-0def4567890abcde?capacity=spot")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"burst": worker})

	_, err = leases.Resolve(context.Background(), "burst")
	if !errors.Is(err, errNoCapacity) {
		t.Fatalf("Resolve = %v, want errNoCapacity", err)
	}

	if !strings.Contains(err.Error(), "InsufficientInstanceCapacity") {
		t.Errorf("error = %v, want EC2's own account of the refusal", err)
	}
}

// TestLeaseTerminatesAMachineItCannotUse pins that a launched instance which
// never reaches running is destroyed rather than left billing.
func TestLeaseTerminatesAMachineItCannotUse(t *testing.T) {
	fake := &fakeEC2{endState: ec2types.InstanceStateNameTerminated}
	seamEC2(t, fake)

	worker, err := ParseWorker("aws://launch/lt-0def4567890abcde")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"burst": worker})

	_, err = leases.Resolve(context.Background(), "burst")
	if err == nil {
		t.Fatal("an instance that went to terminated was accepted")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.terminated) != 1 {
		t.Errorf("terminated = %v, want the unusable instance destroyed", fake.terminated)
	}
}

// TestLeaseIsInertForAMachineThatExists pins that the ladder costs nothing
// for the schemes that name a running machine — which is every other worker
// this repo supports.
func TestLeaseIsInertForAMachineThatExists(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	static, err := ParseWorker("aws://i-0abc123def456789")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"gpu": static, "here": {URL: "local:", Scheme: SchemeLocal}})

	for _, tag := range []string{"gpu", "here"} {
		resolved, resolveErr := leases.Resolve(context.Background(), tag)
		if resolveErr != nil {
			t.Fatalf("Resolve(%q): %v", tag, resolveErr)
		}

		if resolved.URL == "" {
			t.Errorf("Resolve(%q) returned nothing", tag)
		}
	}

	err = leases.ReleaseAll(context.Background())
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.started) != 0 || len(fake.terminated) != 0 || fake.describes != 0 {
		t.Errorf("a worker that already exists cost %d starts, %d terminates and %d describes",
			len(fake.started), len(fake.terminated), fake.describes)
	}
}

// TestParseAcquisitionRungs pins the three URL forms and what each names.
func TestParseAcquisitionRungs(t *testing.T) {
	t.Parallel()

	assertRungForms(t)

	for _, raw := range []string{
		"aws://launch/i-0abc123def456789",   // an instance where a template belongs
		"aws://stopped/lt-0def4567890abcde", // a template where an instance belongs
		"aws://stopped/",                    // nothing to acquire
		"aws://launch/lt-0def4567890abcde?capacity=cheap",
		"aws://i-0abc123def456789?capacity=spot", // capacity describes a launch
		"aws://stopped/i-0abc123def456789?idle=soon",
	} {
		_, err := ParseWorker(raw)
		if !errors.Is(err, ErrWorker) {
			t.Errorf("ParseWorker(%q) = %v, want ErrWorker", raw, err)
		}
	}
}

// assertRungForms pins what each acquisition URL names.
func assertRungForms(t *testing.T) {
	t.Helper()

	parked, err := ParseWorker("aws://stopped/i-0abc123def456789/mnt/fast?idle=90s")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	if parked.Rung != RungStopped || parked.Instance != "i-0abc123def456789" ||
		parked.Root != "/mnt/fast" || parked.Idle.String() != "1m30s" {
		t.Errorf("parsed = %+v, want the parked instance, its root and its idle window", parked)
	}

	if got := parked.Address(); got != "aws://stopped/i-0abc123def456789/mnt/fast" {
		t.Errorf("Address() = %q, want the rung to be part of where the step ran", got)
	}

	assertLaunchForm(t)
}

// assertLaunchForm pins what the launch rung names.
func assertLaunchForm(t *testing.T) {
	t.Helper()

	launched, err := ParseWorker("aws://launch/lt-0def4567890abcde?capacity=od")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	if launched.Rung != RungLaunch || launched.Template != "lt-0def4567890abcde" || launched.Capacity != CapacityOnDemand {
		t.Errorf("parsed = %+v, want the launch template and its capacity", launched)
	}
}
