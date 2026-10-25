package venue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/jtarchie/steps/internal/shell"
)

func shrinkGoneWaits(t *testing.T) {
	t.Helper()

	previousWindow, previousEvery := goneWindow, goneEvery
	goneWindow, goneEvery = 200*time.Millisecond, 10*time.Millisecond

	t.Cleanup(func() { goneWindow, goneEvery = previousWindow, previousEvery })
}

func goneGCPWorker(t *testing.T, statuses ...string) Worker {
	t.Helper()

	return goneGCPWorkerWith(t, &fakeGCE{statuses: statuses})
}

func goneGCPWorkerWith(t *testing.T, fake *fakeGCE) Worker {
	t.Helper()

	previous := gceFor
	gceFor = func(context.Context, Worker) (gceAPI, error) { return fake, nil }

	t.Cleanup(func() { gceFor = previous })

	worker, err := ParseWorker("gcp://worker-1?project=p&zone=us-central1-a")
	if err != nil {
		t.Fatal(err)
	}

	return worker
}

// describedEC2 answers DescribeInstances with one scripted state, or NotFound for "".
type describedEC2 struct {
	ec2API

	state ec2types.InstanceStateName
}

func (d describedEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if d.state == "" {
		return nil, &smithy.GenericAPIError{Code: "InvalidInstanceID.NotFound", Message: "gone"}
	}

	return describeOne(d.state), nil
}

func goneAWSWorker(t *testing.T, state ec2types.InstanceStateName) Worker {
	t.Helper()

	previous := ec2For
	ec2For = func(context.Context, Worker) (ec2API, error) { return describedEC2{state: state}, nil }

	t.Cleanup(func() { ec2For = previous })

	worker, err := ParseWorker("aws://i-0123456789abcdef0?region=us-east-2")
	if err != nil {
		t.Fatal(err)
	}

	return worker
}

// The relay can drop before GCE's metadata says anything (measured on the #225 bench), so the control plane is asked; a status that lags the drop is waited out within the window.
func TestGoneAwayReadsTheCloudsAnswer(t *testing.T) {
	shrinkGoneWaits(t)

	for name, c := range map[string]struct {
		worker func(*testing.T) Worker
		gone   bool
	}{
		"gce stopping after a lag": {func(t *testing.T) Worker { return goneGCPWorker(t, "RUNNING", "RUNNING", "STOPPING") }, true},
		"gce deleted":              {func(t *testing.T) Worker { return goneGCPWorker(t, "notfound") }, true},
		"gce terminated":           {func(t *testing.T) Worker { return goneGCPWorker(t, "TERMINATED") }, true},
		"gce healthy":              {func(t *testing.T) Worker { return goneGCPWorker(t, "RUNNING") }, false},
		// Measured: after simulateMaintenanceEvent the instance read RUNNING for 2.5 minutes, its guest unreachable after 30 seconds; only the operations log said so.
		"gce running but logged a reclaim": {func(t *testing.T) Worker {
			return goneGCPWorkerWith(t, &fakeGCE{reclaimOp: "simulateMaintenanceEvent"})
		}, true},
		"ec2 shutting down":       {func(t *testing.T) Worker { return goneAWSWorker(t, ec2types.InstanceStateNameShuttingDown) }, true},
		"ec2 terminated and gone": {func(t *testing.T) Worker { return goneAWSWorker(t, "") }, true},
		"ec2 healthy":             {func(t *testing.T) Worker { return goneAWSWorker(t, ec2types.InstanceStateNameRunning) }, false},
	} {
		t.Run(name, func(t *testing.T) {
			reason, gone := goneAway(context.Background(), c.worker(t))
			if gone != c.gone {
				t.Errorf("goneAway = %q, %v; want gone %v", reason, gone, c.gone)
			}

			if gone && reason == "" {
				t.Error("a machine going away needs a reason the step's error can carry")
			}
		})
	}
}

// A dropped command on a machine its cloud is taking away is an eviction, and is remembered as the session's reclaim so the lease lets the machine go; the command's own nonzero exit, or a healthy machine, stays what it was.
func TestADroppedCommandOnAMachineGoingAwayIsAnEviction(t *testing.T) {
	shrinkGoneWaits(t)

	dropped := errors.New("reading the command's status: the IAP relay closed the session")

	s := &plusSession{worker: goneGCPWorker(t, "STOPPING")}
	err := s.evictionOf(context.Background(), dropped, true)
	if !errors.Is(err, ErrEvicted) {
		t.Errorf("err = %v, want ErrEvicted for a drop on a stopping machine", err)
	}

	if s.drain.Load() == nil {
		t.Error("the reclaim was not remembered, so the lease would keep the dying machine")
	}

	answered := &shell.ExitError{Command: "false", Code: 1}
	err = (&plusSession{worker: goneGCPWorker(t, "STOPPING")}).evictionOf(context.Background(), answered, true)
	if errors.Is(err, ErrEvicted) {
		t.Error("a command that answered for itself was billed to the cloud")
	}

	err = (&plusSession{worker: goneGCPWorker(t, "RUNNING")}).evictionOf(context.Background(), dropped, true)
	if errors.Is(err, ErrEvicted) {
		t.Error("a drop on a healthy machine was read as a reclaim")
	}

	err = (&plusSession{worker: goneGCPWorker(t, "STOPPING")}).evictionOf(context.Background(), dropped, false)
	if errors.Is(err, ErrEvicted) {
		t.Error("a first connection that failed was billed to the cloud; it is the setup's answer")
	}
}
