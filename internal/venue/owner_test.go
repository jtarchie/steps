package venue

// The steps-owner stamp (#120), against the fake clouds.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"google.golang.org/api/googleapi"

	"github.com/jtarchie/steps/internal/events"
)

const testOwner = "feedfacecafebeef"

// captureLogs swaps the default logger for the test's, since the denied-permission line is the operator's only sign.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var logged bytes.Buffer

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))

	t.Cleanup(func() { slog.SetDefault(previous) })

	return &logged
}

// forgetDenied lets a test see the once-per-process warning again.
func forgetDenied(t *testing.T) {
	t.Helper()

	ownerTagDenied.Clear()
	t.Cleanup(ownerTagDenied.Clear)
}

func (f *fakeEC2) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.calls)
}

func (f *fakeGCE) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.calls)
}

// A crash can land between any two calls, so the stamp goes on before the start and comes off before the stop: either gap leaves a machine reading "not ours".
func TestParkedRungTagsBeforeStartingAndUntagsBeforeStopping(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	leases := NewRegistry(testOwner).Leases(boxWorker(t, "aws://stopped/i-0abc123def456789"))

	mustResolve(t, leases)

	fake.mu.Lock()
	stamped := fake.tags[labelOwner]
	fake.mu.Unlock()

	if _, ours := ownedBy(testOwner, stamped, time.Now()); !ours || !strings.Contains(stamped, "-stopped-") {
		t.Errorf("stamp = %q, want this daemon's stamp for a parked start", stamped)
	}

	mustRelease(t, leases)

	want := []string{"tag i-0abc123def456789", "start i-0abc123def456789", "untag i-0abc123def456789", "stop i-0abc123def456789"}
	if got := fake.callLog(); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestGCPParkedRungLabelsBeforeStartingAndUnlabelsBeforeStopping(t *testing.T) {
	fake := &fakeGCE{statuses: []string{"TERMINATED", "RUNNING"}}
	seamGCP(t, fake, nil)

	leases := NewRegistry(testOwner).Leases(boxWorker(t, "gcp://stopped/worker-1?project=test-project&zone=us-central1-a"))

	mustResolve(t, leases)
	mustRelease(t, leases)

	want := []string{"label worker-1", "start worker-1", "unlabel worker-1", "stop worker-1"}
	if got := fake.callLog(); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// A one-shot command has no identity, and stamps nothing.
func TestParkedRungWithoutAnOwnerTagsNothing(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	leases := NewLeases(boxWorker(t, "aws://stopped/i-0abc123def456789"))

	mustResolve(t, leases)
	mustRelease(t, leases)

	want := []string{"start i-0abc123def456789", "stop i-0abc123def456789"}
	if got := fake.callLog(); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestGCPParkedRungWithoutAnOwnerLabelsNothing(t *testing.T) {
	fake := &fakeGCE{statuses: []string{"TERMINATED", "RUNNING"}}
	seamGCP(t, fake, nil)

	leases := NewLeases(boxWorker(t, "gcp://stopped/worker-1?project=test-project&zone=us-central1-a"))

	mustResolve(t, leases)
	mustRelease(t, leases)

	want := []string{"start worker-1", "stop worker-1"}
	if got := fake.callLog(); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// The restart case:a machine found running with this daemon's fresh stamp is its own, given back on last release; with no stamp, or a stale one, or another daemon's, it is somebody else's.
func TestParkedRungFoundRunningIsGivenBackOnlyWhenStampedByThisDaemon(t *testing.T) {
	now := time.Now()

	for _, tc := range []struct {
		name   string
		tags   map[string]string
		parked bool
	}{
		{"this daemon's", map[string]string{labelOwner: stamp(testOwner, RungStopped, now)}, true},
		{"untagged", nil, false},
		{"another daemon's", map[string]string{labelOwner: stamp("0123456789abcdef", RungStopped, now)}, false},
		{"stamped long before its last start", map[string]string{labelOwner: stamp(testOwner, RungStopped, now.Add(-time.Hour))}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeEC2{alreadyRunning: true, tags: tc.tags, launchTime: now}
			seamEC2(t, fake)

			leases := NewRegistry(testOwner).Leases(boxWorker(t, "aws://stopped/i-0abc123def456789"))

			var notes bytes.Buffer

			mustResolveTag(events.WithOutput(context.Background(), events.Output{Stdout: &notes}), t, leases, "box")
			mustRelease(t, leases)

			want := []string{}
			if tc.parked {
				want = []string{"untag i-0abc123def456789", "stop i-0abc123def456789"}
			} else {
				assertAdoptedWarning(t, notes.String(), "i-0abc123def456789")
			}

			if got := fake.callLog(); !slices.Equal(got, want) {
				t.Errorf("calls = %v, want %v", got, want)
			}
		})
	}
}

func TestGCPParkedRungFoundRunningIsGivenBackOnlyWhenStampedByThisDaemon(t *testing.T) {
	now := time.Now()

	for _, tc := range []struct {
		name   string
		label  string
		parked bool
	}{
		{"this daemon's", stamp(testOwner, RungStopped, now), true},
		{"unlabelled", "", false},
		{"stamped long after its last start", stamp(testOwner, RungStopped, now.Add(time.Hour)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeGCE{statuses: []string{"RUNNING"}, ownerLabel: tc.label, lastStart: now}
			seamGCP(t, fake, nil)

			leases := NewRegistry(testOwner).Leases(boxWorker(t, "gcp://stopped/worker-1?project=test-project&zone=us-central1-a"))

			mustResolve(t, leases)
			mustRelease(t, leases)

			want := []string{}
			if tc.parked {
				want = []string{"unlabel worker-1", "stop worker-1"}
			}

			if got := fake.callLog(); !slices.Equal(got, want) {
				t.Errorf("calls = %v, want %v", got, want)
			}
		})
	}
}

// A launched machine takes the stamp in the create request, so there is no moment it runs unstamped.
func TestLaunchStampsTheInstanceInTheFleetRequest(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	leases := NewRegistry(testOwner).Leases(boxWorker(t, "aws://launch/lt-0def4567890abcde"))

	mustResolve(t, leases)
	mustRelease(t, leases)

	fake.mu.Lock()
	defer fake.mu.Unlock()

	value := ""

	for _, tag := range fake.fleets[0].TagSpecifications[0].Tags {
		if aws.ToString(tag.Key) == labelOwner {
			value = aws.ToString(tag.Value)
		}
	}

	if rung, ours := ownedBy(testOwner, value, time.Now()); !ours || rung != RungLaunch {
		t.Errorf("steps-owner = %q, want this daemon's launch stamp", value)
	}

	if slices.ContainsFunc(fake.calls, func(call string) bool { return strings.HasPrefix(call, "tag ") || strings.HasPrefix(call, "untag ") }) {
		t.Errorf("calls = %v, want no tag calls around a machine stamped at birth and terminated", fake.calls)
	}
}

func TestGCPLaunchStampsTheInstanceInTheInsert(t *testing.T) {
	fake := &fakeGCE{}
	seamGCP(t, fake, nil)

	leases := NewRegistry(testOwner).Leases(boxWorker(t, "gcp://launch/template-1?project=test-project&zone=us-central1-a"))

	mustResolve(t, leases)
	mustRelease(t, leases)

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if rung, ours := ownedBy(testOwner, fake.labels[0][labelOwner], time.Now()); !ours || rung != RungLaunch {
		t.Errorf("steps-owner = %q, want this daemon's launch stamp", fake.labels[0][labelOwner])
	}
}

// A missing permission falls back to an unstamped machine, said loudly once: the job still runs and the machine is still parked.
func TestATagRefusedForPermissionIsSaidOnceAndNeverFailsTheJob(t *testing.T) {
	forgetDenied(t)

	logged := captureLogs(t)
	fake := &fakeEC2{tagErr: &smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "not authorized to perform ec2:CreateTags"}}
	seamEC2(t, fake)

	registry := NewRegistry(testOwner)

	for _, instance := range []string{"i-0abc123def456789", "i-0def456789abcdef"} {
		leases := registry.Leases(boxWorker(t, "aws://stopped/"+instance))
		mustResolve(t, leases)
		mustRelease(t, leases)
	}

	if count := strings.Count(logged.String(), "worker.owner_tag_denied"); count != 1 {
		t.Errorf("%d denied lines, want exactly one: %s", count, logged)
	}

	if !strings.Contains(logged.String(), "ec2:CreateTags") {
		t.Errorf("log = %q, want the permission to grant named", logged)
	}

	want := []string{"tag i-0abc123def456789", "start i-0abc123def456789", "stop i-0abc123def456789"}
	if got := fake.callLog(); !slices.Equal(got[:3], want) {
		t.Errorf("calls = %v, want %v first: started and parked, with no untag of a tag that never landed", got, want)
	}
}

func TestGCPALabelRefusedForPermissionIsSaidOnceAndNeverFailsTheJob(t *testing.T) {
	forgetDenied(t)

	logged := captureLogs(t)
	fake := &fakeGCE{statuses: []string{"TERMINATED", "RUNNING"}, setOwnerErr: fmt.Errorf("writing labels: %w", &googleapi.Error{Code: 403})}
	seamGCP(t, fake, nil)

	leases := NewRegistry(testOwner).Leases(boxWorker(t, "gcp://stopped/worker-1?project=test-project&zone=us-central1-a"))
	mustResolve(t, leases)
	mustRelease(t, leases)

	if !strings.Contains(logged.String(), "worker.owner_tag_denied") || !strings.Contains(logged.String(), "compute.instances.setLabels") {
		t.Errorf("log = %q, want the denied line naming the permission", logged)
	}

	want := []string{"label worker-1", "start worker-1", "stop worker-1"}
	if got := fake.callLog(); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// Any other tagging failure is a warning per machine, not the once-only denial: it says nothing about the next call.
func TestATagThatFailsForAnotherReasonIsNotTheDenial(t *testing.T) {
	forgetDenied(t)

	logged := captureLogs(t)
	fake := &fakeEC2{tagErr: errors.New("connection reset")}
	seamEC2(t, fake)

	leases := NewRegistry(testOwner).Leases(boxWorker(t, "aws://stopped/i-0abc123def456789"))
	mustResolve(t, leases)
	mustRelease(t, leases)

	if strings.Contains(logged.String(), "worker.owner_tag_denied") || !strings.Contains(logged.String(), "worker.owner_tag_failed") {
		t.Errorf("log = %q, want a plain failure, not the denial", logged)
	}
}

// sweepingEC2 answers the sweep's filtered describe with a scripted set of running instances, one per page, so a sweep that reads only the first page misses the rest.
type sweepingEC2 struct {
	*fakeEC2

	running []ownedInstance
	t       *testing.T
}

func (s sweepingEC2) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if len(in.Filters) == 0 {
		return s.fakeEC2.DescribeInstances(ctx, in, opts...)
	}

	// A machine still pending when a quickly restarted daemon looks is one no later sweep would find.
	for _, filter := range in.Filters {
		if aws.ToString(filter.Name) == "instance-state-name" && !slices.Contains(filter.Values, "pending") {
			s.t.Errorf("the sweep lists states %v, want pending among them", filter.Values)
		}
	}

	page, _ := strconv.Atoi(aws.ToString(in.NextToken))
	if page >= len(s.running) {
		return &ec2.DescribeInstancesOutput{}, nil
	}

	one := s.running[page]
	out := &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{
		InstanceId: aws.String(one.name),
		LaunchTime: aws.Time(one.started),
		Tags: []ec2types.Tag{
			{Key: aws.String(labelOwner), Value: aws.String(one.stamp)},
			{Key: aws.String("aws:ec2launchtemplate:id"), Value: aws.String(one.template)},
		},
	}}}}}

	if page+1 < len(s.running) {
		out.NextToken = aws.String(strconv.Itoa(page + 1))
	}

	return out, nil
}

// The leak fix: on boot, a daemon parks the parked machines and terminates the launched ones it left running, and touches nothing it cannot prove it started.
func TestSweepGivesBackOnlyThisDaemonsRunningMachines(t *testing.T) {
	now := time.Now()
	fake := &fakeEC2{}
	seamEC2(t, fake)

	api := sweepingEC2{fakeEC2: fake, t: t, running: []ownedInstance{
		{name: "i-0abc123def456789", stamp: stamp(testOwner, RungStopped, now), started: now},
		{name: "i-0launched", stamp: stamp(testOwner, RungLaunch, now), started: now, template: "lt-0def4567890abcde"},
		{name: "i-0theirs", stamp: stamp("0123456789abcdef", RungLaunch, now), started: now, template: "lt-0def4567890abcde"},
		{name: "i-0restarted", stamp: stamp(testOwner, RungStopped, now.Add(-time.Hour)), started: now},
		{name: "i-0garbled", stamp: testOwner + "-launch", started: now, template: "lt-0def4567890abcde"},
		// A stamp is public, so one forged onto a machine no mapping names proves nothing.
		{name: "i-0forgedparked", stamp: stamp(testOwner, RungStopped, now), started: now},
		{name: "i-0forgedlaunch", stamp: stamp(testOwner, RungLaunch, now), started: now, template: "lt-0production"},
	}}
	ec2For = func(context.Context, Worker) (ec2API, error) { return api, nil }

	workers := map[string]Worker{
		"a": mustParse(t, "aws://stopped/i-0abc123def456789"),
		"b": mustParse(t, "aws://launch/lt-0def4567890abcde"),
		"c": mustParse(t, "aws://i-0123456789abcdef0?region=eu-west-1"),
	}

	err := NewRegistry(testOwner).Sweep(context.Background(), workers)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	want := []string{"untag i-0abc123def456789", "stop i-0abc123def456789", "terminate i-0launched"}
	if got := fake.callLog(); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v, once for the region both acquirable workers share", got, want)
	}
}

func TestGCPSweepGivesBackOnlyThisDaemonsRunningMachines(t *testing.T) {
	now := time.Now()
	fake := &fakeGCE{running: []ownedInstance{
		{name: "worker-1", stamp: stamp(testOwner, RungStopped, now), started: now},
		{name: "steps-0a1b2c", stamp: stamp(testOwner, RungLaunch, now), started: now},
		{name: "someone-else", stamp: stamp("0123456789abcdef", RungStopped, now), started: now},
		{name: "prod-db", stamp: stamp(testOwner, RungLaunch, now), started: now},
		{name: "worker-2", stamp: stamp(testOwner, RungStopped, now), started: now},
	}}
	seamGCP(t, fake, nil)

	err := NewRegistry(testOwner).Sweep(context.Background(), map[string]Worker{
		"a": mustParse(t, "gcp://stopped/worker-1?project=test-project&zone=us-central1-a"),
		"b": mustParse(t, "gcp://launch/template-1?project=test-project&zone=us-central1-a"),
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	want := []string{"unlabel worker-1", "stop worker-1", "delete steps-0a1b2c"}
	if got := fake.callLog(); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// With no launch mapping at the location, nothing there is this daemon's to delete, whatever its name and stamp say.
func TestGCPSweepDeletesNothingWhereNoMappingLaunches(t *testing.T) {
	now := time.Now()
	fake := &fakeGCE{running: []ownedInstance{{name: "steps-0a1b2c", stamp: stamp(testOwner, RungLaunch, now), started: now}}}
	seamGCP(t, fake, nil)

	err := NewRegistry(testOwner).Sweep(context.Background(), map[string]Worker{
		"a": mustParse(t, "gcp://stopped/worker-1?project=test-project&zone=us-central1-a"),
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if got := fake.callLog(); len(got) != 0 {
		t.Errorf("calls = %v, want none", got)
	}
}

// A one-shot command's registry has no identity, so it has nothing to sweep and must not ask a cloud.
func TestSweepWithoutAnOwnerAsksNothing(t *testing.T) {
	seamEC2(t, &fakeEC2{})

	ec2For = func(context.Context, Worker) (ec2API, error) {
		t.Error("a registry with no owner asked EC2 for its machines")

		return nil, errors.New("unreachable")
	}

	err := NewRegistry("").Sweep(context.Background(), map[string]Worker{"a": mustParse(t, "aws://stopped/i-0abc123def456789")})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
}

func TestOwnedByReadsOnlyAWellFormedFreshStamp(t *testing.T) {
	t.Parallel()

	at := time.Unix(1_760_000_000, 0)

	for _, tc := range []struct {
		value   string
		started time.Time
		want    bool
	}{
		{stamp(testOwner, RungStopped, at), at, true},
		{stamp(testOwner, RungLaunch, at), at.Add(stampWindow), true},
		{stamp(testOwner, RungLaunch, at), at.Add(-stampWindow), true},
		{stamp(testOwner, RungLaunch, at), at.Add(stampWindow + time.Second), false},
		{stamp(testOwner, RungLaunch, at), at.Add(-stampWindow - time.Second), false},
		{stamp(testOwner, RungStopped, at), time.Time{}, false},
		{stamp(testOwner, RungStatic, at), at, false},
		{testOwner + "-stopped-soon", at, false},
		{testOwner + "-stopped-1-2", at, false},
		{"", at, false},
	} {
		if _, got := ownedBy(testOwner, tc.value, tc.started); got != tc.want {
			t.Errorf("ownedBy(%q, %s) = %v, want %v", tc.value, tc.started, got, tc.want)
		}
	}

	if _, got := ownedBy("", stamp("", RungStopped, at), at); got {
		t.Error("an empty owner owns an empty owner's stamp")
	}
}

// A start that never arrives is parked by the failed acquisition itself, and untagged first like any other park.
func TestAFailedStartIsUntaggedBeforeItIsStopped(t *testing.T) {
	fake := &fakeEC2{pendingBefore: 1, endState: "stopped"}
	seamEC2(t, fake)

	_, err := NewRegistry(testOwner).Leases(boxWorker(t, "aws://stopped/i-0abc123def456789")).Resolve(context.Background(), "box")
	if err == nil {
		t.Fatal("an instance that went back to stopped was acquired")
	}

	want := []string{"tag i-0abc123def456789", "start i-0abc123def456789", "untag i-0abc123def456789", "stop i-0abc123def456789"}
	if got := fake.callLog(); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestGCPAFailedStartIsUnlabelledBeforeItIsStopped(t *testing.T) {
	fake := &fakeGCE{statuses: []string{"SUSPENDED"}}
	seamGCP(t, fake, nil)

	_, err := NewRegistry(testOwner).Leases(boxWorker(t, "gcp://stopped/worker-1?project=test-project&zone=us-central1-a")).Resolve(context.Background(), "box")
	if err == nil {
		t.Fatal("an instance that went to SUSPENDED was acquired")
	}

	want := []string{"label worker-1", "start worker-1", "unlabel worker-1", "stop worker-1"}
	if got := fake.callLog(); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// A start EC2 refuses leaves a stopped machine; its stamp comes off, or a hand start inside the stamp window reads as this daemon's and the next restart's sweep stops it.
func TestARefusedStartIsUntagged(t *testing.T) {
	fake := &fakeEC2{startErr: &smithy.GenericAPIError{Code: "InsufficientInstanceCapacity", Message: "no capacity", Fault: smithy.FaultClient}}
	seamEC2(t, fake)

	_, err := NewRegistry(testOwner).Leases(boxWorker(t, "aws://stopped/i-0abc123def456789")).Resolve(context.Background(), "box")
	if err == nil {
		t.Fatal("a refused start was acquired")
	}

	want := []string{"tag i-0abc123def456789", "start i-0abc123def456789", "untag i-0abc123def456789"}
	if got := fake.callLog(); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v: untagged, and not stopped, since a refused start may be somebody else's machine booting", got, want)
	}
}

// Re-acquired after an eviction in the same process, the stamp is this daemon's but nothing restarted: the note must not claim a restart.
func TestAReacquiredStampedMachineIsNotCalledARestart(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	leases := NewRegistry(testOwner).Leases(boxWorker(t, "aws://stopped/i-0abc123def456789"))
	dying := mustResolve(t, leases)

	var notes bytes.Buffer

	leases.Abandon("box", dying.URL)
	mustResolveTag(events.WithOutput(context.Background(), events.Output{Stdout: &notes}), t, leases, "box")
	mustRelease(t, leases)

	if strings.Contains(notes.String(), "restarted") || !strings.Contains(notes.String(), "steps' own earlier start") {
		t.Errorf("notes = %q, want the re-acquisition noted as this process's own start", notes.String())
	}
}
