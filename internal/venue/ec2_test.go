package venue

// The acquisition ladder, against a fake EC2.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// fakeEC2 records what was asked of it and answers with the states a test set
// up. Deliberately not a simulator: what these tests pin is which calls the
// ladder makes, in what order, and what it does with the answers.
type fakeEC2 struct {
	mu sync.Mutex

	// notFoundBefore is how many describes answer InvalidInstanceID.NotFound
	// before the instance becomes visible — a fleet's instance is not in
	// DescribeInstances the moment CreateFleet returns its id.
	notFoundBefore int
	// stoppingBefore is how many describes report "stopping" before anything
	// else, standing in for a previous job's park still settling: the release
	// returns once StopInstances is ACCEPTED.
	stoppingBefore int
	// stoppedBefore is how many describes still report "stopped" after a
	// start, standing in for EC2's read-after-write lag: DescribeInstances
	// answers from a replica that has not seen the StartInstances yet.
	stoppedBefore int
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
	// describes counts from the acquisition action, so a post-action script
	// says what the START looks like; probes counts every describe ever,
	// which is what an unsettled stop is scripted against.
	describes int
	probes    int
	fleets    []*ec2.CreateFleetInput
}

func (f *fakeEC2) StartInstances(_ context.Context, in *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.started = append(f.started, in.InstanceIds...)
	f.describes = 0

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

	f.probes++
	f.describes++

	// A machine still on its way down answers "stopping" however it is asked,
	// and a start does not change that — which is the whole reason the parked
	// rung has to wait the stop out before starting. Counted from the very
	// first describe, so skipping the wait is VISIBLE rather than merely
	// unscripted.
	if f.probes <= f.stoppingBefore {
		return describeOne(ec2types.InstanceStateNameStopping)
	}

	// Before any acquisition action, this is a machine at rest with its park
	// settled. Not counted against the scripts below — those say what happens
	// after the start, and a probe that precedes it must not consume a step.
	if len(f.started) == 0 && len(f.fleets) == 0 {
		return describeOne(ec2types.InstanceStateNameStopped)
	}

	if f.describes <= f.notFoundBefore {
		return nil, &smithy.GenericAPIError{
			Code:    "InvalidInstanceID.NotFound",
			Message: "The instance ID 'i-0faced0123456789' does not exist",
		}
	}

	state := ec2types.InstanceStateNameRunning

	// The scripted prefix runs before endState, so a test can say "pending,
	// then it went away" — the shape endState alone cannot express.
	switch {
	case f.describes <= f.stoppedBefore:
		state = ec2types.InstanceStateNameStopped
	case f.describes <= f.stoppedBefore+f.pendingBefore:
		state = ec2types.InstanceStateNamePending
	case f.endState != "":
		state = f.endState
	}

	return describeOne(state)
}

// describeOne is a describe response naming one instance in one state.
func describeOne(state ec2types.InstanceStateName) (*ec2.DescribeInstancesOutput, error) {
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
		Instances: []ec2types.CreateFleetInstance{{InstanceIds: []string{"i-0faced0123456789"}}},
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

	if resolved.Instance != "i-0faced0123456789" {
		t.Errorf("resolved instance = %q, want the launched one", resolved.Instance)
	}

	err = leases.ReleaseAll(context.Background())
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.terminated) != 1 || fake.terminated[0] != "i-0faced0123456789" {
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
		"aws://launch/lt-0def4567890abcde?version=lastest", // the typo the whole check exists for
		"aws://launch/lt-0def4567890abcde?version=0",       // EC2 numbers versions from 1
		"aws://launch/lt-0def4567890abcde?version=-1",
		"aws://launch/lt-0def4567890abcde?version=2.1",
		"aws://launch/lt-0def4567890abcde?version=",
		"aws://i-0abc123def456789?version=2",         // no template to have versions
		"aws://stopped/i-0abc123def456789?version=2", // nor here
		"ssh://jt@gpu-box?version=2",                 // nor off aws:// entirely
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

// TestLeaseResolvedWorkerDialsTheAcquiredInstance pins the rebuild asStatic
// does, against the break every reviewer missed: the pipeline hands a runner
// the worker's URL as a STRING and the venue parses it back — so a launch
// worker whose URL still said aws://launch/lt-… re-parsed into a template
// with no instance, and the session dialed nothing at all, on a machine the
// job had just paid to start.
func TestLeaseResolvedWorkerDialsTheAcquiredInstance(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	// version= is here because it is the option that broke this seam: it was
	// added to the grammar as launch-rung-only without being added to what
	// the rebuild strips, so the URL a paid-for instance was named by no
	// longer parsed. Every acquisition-only option belongs in this mapping.
	worker, err := ParseWorker("aws://launch/lt-0def4567890abcde/mnt/fast?capacity=spot&version=7&region=us-west-2&shim=/usr/local/bin/steps")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"burst": worker})

	resolved, err := leases.Resolve(context.Background(), "burst")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// The URL a runner will re-parse has to name the MACHINE, keep the
	// connection options, and drop the acquisition ones a static parse
	// refuses.
	reparsed, err := ParseWorker(resolved.URL)
	if err != nil {
		t.Fatalf("the resolved URL %q does not parse: %v", resolved.URL, err)
	}

	if reparsed.Instance != "i-0faced0123456789" || reparsed.Rung != RungStatic {
		t.Errorf("re-parsed = %+v, want the launched instance as a static worker", reparsed)
	}

	if reparsed.Root != "/mnt/fast" || reparsed.Region != "us-west-2" || reparsed.Shim != "/usr/local/bin/steps" {
		t.Errorf("re-parsed = %+v, want the root and connection options carried over", reparsed)
	}
}

// TestResolvedWorkerDropsEveryAcquisitionOption is the same seam by the SET
// rather than by name, so an option added to the grammar and forgotten in the
// rebuild fails here rather than on a machine the job has already paid for.
// version= was exactly that: launch-rung-only, refused on a static parse, and
// left in the URL every step re-parses.
func TestResolvedWorkerDropsEveryAcquisitionOption(t *testing.T) {
	seamEC2(t, &fakeEC2{})

	worker, err := ParseWorker("aws://launch/lt-0def4567890abcde?capacity=spot&version=7&shim=/usr/local/bin/steps")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	resolved := worker.asStatic("i-0faced0123456789")

	for _, key := range acquisitionKeys {
		if strings.Contains(resolved.URL, key+"=") {
			t.Errorf("resolved URL %q still carries the acquisition-only option %s=", resolved.URL, key)
		}
	}

	_, err = ParseWorker(resolved.URL)
	if err != nil {
		t.Fatalf("the resolved URL %q does not parse: %v", resolved.URL, err)
	}
}

// TestLeaseAbandonForgetsWithoutDestroying is the reclamation contract: AWS
// owns a reclaimed machine's end, and a sibling step may still be spending
// the two-minute grace on it — so an abandon must not Stop or Terminate
// anything, only make the next Resolve acquire fresh.
func TestLeaseAbandonForgetsWithoutDestroying(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	worker, err := ParseWorker("aws://launch/lt-0def4567890abcde")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"burst": worker})

	first, err := leases.Resolve(context.Background(), "burst")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	leases.Abandon("burst", first.URL)

	fake.mu.Lock()
	terminated := len(fake.terminated)
	fake.mu.Unlock()

	if terminated != 0 {
		t.Fatalf("Abandon destroyed the machine (%d terminates) — a sibling's grace was cut to zero", terminated)
	}

	// The next resolve acquires a fresh machine rather than the corpse.
	_, err = leases.Resolve(context.Background(), "burst")
	if err != nil {
		t.Fatalf("Resolve after abandon: %v", err)
	}

	fake.mu.Lock()
	fleets := len(fake.fleets)
	fake.mu.Unlock()

	if fleets != 2 {
		t.Errorf("fleets = %d, want a second acquisition after the abandon", fleets)
	}
}

// TestLeaseAbandonIsIdentityChecked pins the fix for the tag-keyed footgun: a
// stale abandon naming the machine that died must not touch the FRESH lease a
// parallel sibling has since acquired under the same tag.
func TestLeaseAbandonIsIdentityChecked(t *testing.T) {
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

	// A stale notice about some other machine: identity mismatch, no-op.
	leases.Abandon("gpu", "aws://i-0someoneelse1234")

	again, err := leases.Resolve(context.Background(), "gpu")
	if err != nil {
		t.Fatalf("Resolve after a mismatched abandon: %v", err)
	}

	if again.URL != resolved.URL {
		t.Errorf("a mismatched abandon evicted the lease: %q became %q", resolved.URL, again.URL)
	}

	fake.mu.Lock()
	started := len(fake.started)
	fake.mu.Unlock()

	if started != 1 {
		t.Errorf("the machine was acquired %d times, want once — the mismatched abandon must not have touched it", started)
	}

	// After the abandon that DOES match, the machine is no longer bound to
	// the tag — but the job's end still gives it back. This assertion used to
	// read the other way, on the theory that a reclaimed machine's fate
	// belongs to AWS; that is true only of a spot TERMINATE, and a stop or a
	// hibernate leaves a live instance nothing would ever have released.
	leases.Abandon("gpu", again.URL)

	err = leases.ReleaseAll(context.Background())
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.stopped) != 1 {
		t.Errorf("stopped = %v, want the abandoned machine still given back at the job's end", fake.stopped)
	}
}

// TestLeaseReleaseAllRunsConcurrently pins that one slow release cannot spend
// the whole budget the others needed: three parked workers with holds release
// in one window, not three.
func TestLeaseReleaseAllRunsConcurrently(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	workers := map[string]Worker{}

	for _, tag := range []string{"a", "b", "c"} {
		worker, err := ParseWorker("aws://stopped/i-0abc123def45678" + tag + "?idle=300ms")
		if err != nil {
			t.Fatalf("ParseWorker: %v", err)
		}

		workers[tag] = worker
	}

	leases := NewLeases(workers)

	for tag := range workers {
		_, err := leases.Resolve(context.Background(), tag)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", tag, err)
		}
	}

	started := time.Now()

	err := leases.ReleaseAll(context.Background())
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}

	if elapsed := time.Since(started); elapsed > 900*time.Millisecond {
		t.Errorf("ReleaseAll took %s for three 300ms holds — releases are serialized", elapsed)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.stopped) != 3 {
		t.Errorf("stopped = %v, want all three parked", fake.stopped)
	}
}

// TestLeaseWaitsOutTheStoppedStateAfterStarting is the parked rung against
// EC2's own consistency model, which the fixture caught and no fake had:
// DescribeInstances keeps answering "stopped" for a second or two after
// StartInstances succeeded. Read as terminal, that fails the acquisition on
// its first poll AND runs the failed-acquire cleanup, stopping the machine it
// just started — so the assertion below is as much about what was NOT called.
func TestLeaseWaitsOutTheStoppedStateAfterStarting(t *testing.T) {
	fake := &fakeEC2{stoppedBefore: 1}
	seamEC2(t, fake)

	worker, err := ParseWorker("aws://stopped/i-0abc123def456789?idle=0")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"parked": worker})

	resolved, err := leases.Resolve(context.Background(), "parked")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if resolved.Instance != "i-0abc123def456789" {
		t.Errorf("resolved instance = %q, want the parked one", resolved.Instance)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.stopped) != 0 {
		t.Errorf("stopped = %v, want the machine it just started left running", fake.stopped)
	}
}

// TestParkedRungWaitsOutAnUnfinishedStop is the window BEFORE the start.
// The release returns once StopInstances is accepted, so back-to-back serial
// jobs on one parked worker find the instance still "stopping" — a state EC2
// refuses to start from, and one arrivedOrDead reads as terminal, so the
// acquisition would fail and then stop the machine the next job wanted.
func TestParkedRungWaitsOutAnUnfinishedStop(t *testing.T) {
	fake := &fakeEC2{stoppingBefore: 1}
	seamEC2(t, fake)

	worker, err := ParseWorker("aws://stopped/i-0abc123def456789?idle=0")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	resolved, release, err := acquire(context.Background(), worker)
	if err != nil {
		t.Fatalf("acquire through an unfinished stop: %v", err)
	}

	_ = release(context.Background(), true)

	if resolved.Instance != "i-0abc123def456789" {
		t.Errorf("resolved instance = %q, want the parked one", resolved.Instance)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.started) != 1 {
		t.Errorf("started = %v, want exactly one, after the stop settled", fake.started)
	}
}

// TestLeaseFailsAMachineThatGoesBackToStopped is the other half: tolerating
// "stopped" is about the state the instance is LEAVING, so once it has been
// seen to leave, going back is terminal exactly as it was before.
func TestLeaseFailsAMachineThatGoesBackToStopped(t *testing.T) {
	fake := &fakeEC2{pendingBefore: 1, endState: ec2types.InstanceStateNameStopped}
	seamEC2(t, fake)

	worker, err := ParseWorker("aws://stopped/i-0abc123def456789?idle=0")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"parked": worker})

	_, err = leases.Resolve(context.Background(), "parked")
	if err == nil {
		t.Fatal("an instance that went back to stopped was accepted")
	}

	if !strings.Contains(err.Error(), "went to stopped") {
		t.Errorf("error = %v, want it to name the state the machine went to", err)
	}
}

// TestParseWorkerReadsLaunchTemplateVersion pins the spellings EC2 accepts,
// plus the two bare ones this repo adds.
//
// $Default and $Latest are EC2's own, and a shell eats them: unquoted,
// ?version=$Latest arrives as ?version= and would otherwise mean "default"
// silently. Accepting the bare words gives an operator a spelling that
// survives the command line, normalized here so everything downstream sees
// only what EC2 understands.
func TestParseWorkerReadsLaunchTemplateVersion(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]string{
		"":         "$Default", // unstated: the version the template itself blesses
		"$Default": "$Default",
		"$Latest":  "$Latest",
		"default":  "$Default",
		"latest":   "$Latest",
		"1":        "1",
		"27":       "27",
	} {
		url := "aws://launch/lt-0def4567890abcde"
		if raw != "" {
			url += "?version=" + raw
		}

		worker, err := ParseWorker(url)
		if err != nil {
			t.Fatalf("ParseWorker(%q): %v", url, err)
		}

		if worker.Version != want {
			t.Errorf("ParseWorker(%q).Version = %q, want %q", url, worker.Version, want)
		}
	}
}

// TestLaunchAsksForTheRequestedTemplateVersion is the seam: a version parsed
// off the URL and never handed to CreateFleet is a knob that reads as
// configured and launches the default machine anyway — the exact failure the
// capacity= default was written to avoid.
func TestLaunchAsksForTheRequestedTemplateVersion(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	worker, err := ParseWorker("aws://launch/lt-0def4567890abcde?version=7")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"burst": worker})

	_, err = leases.Resolve(context.Background(), "burst")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.fleets) != 1 {
		t.Fatalf("fleets = %d, want one", len(fake.fleets))
	}

	spec := fake.fleets[0].LaunchTemplateConfigs[0].LaunchTemplateSpecification
	if aws.ToString(spec.Version) != "7" {
		t.Errorf("fleet version = %q, want the version the mapping named", aws.ToString(spec.Version))
	}
}

// TestLaunchDefaultsToTheBlessedTemplateVersion pins the unstated case:
// $Default, never $Latest, because $Latest means someone editing a template
// changes what every job runs on without touching a pipeline.
func TestLaunchDefaultsToTheBlessedTemplateVersion(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	worker, err := ParseWorker("aws://launch/lt-0def4567890abcde")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"burst": worker})

	_, err = leases.Resolve(context.Background(), "burst")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	spec := fake.fleets[0].LaunchTemplateConfigs[0].LaunchTemplateSpecification
	if aws.ToString(spec.Version) != "$Default" {
		t.Errorf("fleet version = %q, want $Default", aws.ToString(spec.Version))
	}
}

// TestLaunchWaitsForAnInstanceEC2CannotSeeYet is the launch rung against EC2's
// consistency model, which a real fleet caught and no fake had.
//
// CreateFleet returns an instance id, and DescribeInstances then answers
// InvalidInstanceID.NotFound for a few seconds — the id is real, the replica
// has not caught up. Read as a hard error, the launch rung fails every time
// before it ever dials, which is exactly what it did: the rung had never once
// worked against real AWS.
//
// Sibling of the parked rung's stopped-state lag; same cause, different API.
func TestLaunchWaitsForAnInstanceEC2CannotSeeYet(t *testing.T) {
	fake := &fakeEC2{notFoundBefore: 1}
	seamEC2(t, fake)

	worker, err := ParseWorker("aws://launch/lt-0def4567890abcde")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"burst": worker})

	resolved, err := leases.Resolve(context.Background(), "burst")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if resolved.Instance != "i-0faced0123456789" {
		t.Errorf("resolved instance = %q, want the launched one", resolved.Instance)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if fake.describes < 2 {
		t.Errorf("describes = %d, want the not-found answer to have been retried", fake.describes)
	}

	// It must NOT have thrown the machine away: terminating an instance EC2
	// merely had not indexed yet is how a launch rung leaks money.
	if len(fake.terminated) != 0 {
		t.Errorf("terminated = %v, want the instance kept", fake.terminated)
	}
}

// TestAbandonedMachineIsStillReleasedAtJobEnd is the spot stop/hibernate
// leak.
//
// A spot instance-action is one of terminate, stop or hibernate, and steps
// treats all three as terminal — correctly, since all three mean stop using
// this machine. But only terminate destroys it. Under
// InstanceInterruptionBehavior=stop the instance is STOPPED and stays alive,
// so dropping the release closure meant nothing ever called
// TerminateInstances and the instance plus its EBS root volume billed
// indefinitely, with nothing left that would look for them.
//
// The release stays, but stays DEFERRED to the job's end — which is the other
// half of why it was dropped: a sibling step may still be finishing inside
// the two-minute grace, and releasing on the spot would cut that to zero.
// Job end is after the sibling, by construction.
func TestAbandonedMachineIsStillReleasedAtJobEnd(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	worker, err := ParseWorker("aws://launch/lt-0def4567890abcde")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"burst": worker})

	resolved, err := leases.Resolve(context.Background(), "burst")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// AWS said it is taking the machine; the step is re-placed elsewhere.
	leases.Abandon("burst", resolved.URL)

	// The next step on the tag must get a FRESH machine, not the doomed one.
	again, err := leases.Resolve(context.Background(), "burst")
	if err != nil {
		t.Fatalf("Resolve after abandon: %v", err)
	}

	fake.mu.Lock()
	fleets := len(fake.fleets)
	fake.mu.Unlock()

	if fleets != 2 {
		t.Errorf("fleets = %d, want a fresh machine acquired after the abandon", fleets)
	}

	err = leases.ReleaseAll(context.Background())
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	// Both: the abandoned one AND the replacement. Terminating an instance
	// AWS already terminated is a no-op, so the redundant call on a genuine
	// terminate costs nothing — while omitting it on a stop bills forever.
	if len(fake.terminated) != 2 {
		t.Errorf("terminated = %v, want both the abandoned machine and its replacement", fake.terminated)
	}

	_ = again
}

// TestRetiredMachineIsReleasedWithoutWaitingOutItsIdleWindow pins which
// release a reclaimed machine gets.
//
// A lease is retired only because AWS said it is taking the machine, so
// holding it out for its ?idle= window means blocking the job's teardown for
// that long to keep a machine warm that is being destroyed. That is exactly
// what the immediate flag exists to skip, and nothing had ever passed it.
func TestRetiredMachineIsReleasedWithoutWaitingOutItsIdleWindow(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	// Long enough that waiting it out is unmistakable, short enough that a
	// regression costs the suite seconds rather than minutes.
	worker, err := ParseWorker("aws://stopped/i-0abc123def456789?idle=5s")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"box": worker})

	resolved, err := leases.Resolve(context.Background(), "box")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// AWS said it is taking the machine.
	leases.Abandon("box", resolved.URL)

	started := time.Now()

	err = leases.ReleaseAll(context.Background())
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}

	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("ReleaseAll took %s — it waited out ?idle= on a machine being reclaimed", elapsed)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.stopped) != 1 {
		t.Errorf("stopped = %v, want the retired machine given back", fake.stopped)
	}
}
