package venue

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/smithy-go"
)

// stopCtxEC2 records the context error each StopInstances arrived under.
type stopCtxEC2 struct {
	*fakeEC2

	cmu      sync.Mutex
	stopErrs []error
}

func (s *stopCtxEC2) StopInstances(ctx context.Context, in *ec2.StopInstancesInput, opts ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
	s.cmu.Lock()
	s.stopErrs = append(s.stopErrs, ctx.Err())
	s.cmu.Unlock()

	return s.fakeEC2.StopInstances(ctx, in, opts...)
}

// A start AWS may have accepted (cancelled, timed out, answer lost) is stopped again under its own context; only a client-fault refusal is left alone.
func TestAnUncertainStartIsStoppedAndARefusedOneIsNot(t *testing.T) {
	const instance = "i-0abc123def456789"

	for name, tc := range map[string]struct {
		err      error
		cancel   bool
		wantStop bool
	}{
		"cancelled":     {err: context.Canceled, cancel: true, wantStop: true},
		"lost response": {err: errors.New("operation error EC2: StartInstances, https response error: read: connection reset by peer"), wantStop: true},
		"server fault":  {err: &smithy.GenericAPIError{Code: "InternalError", Message: "boom", Fault: smithy.FaultServer}, wantStop: true},
		"refused":       {err: &smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "no", Fault: smithy.FaultClient}},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &stopCtxEC2{fakeEC2: &fakeEC2{startErr: tc.err}}
			shrinkAcquireWaits(t)

			previous := ec2For
			ec2For = func(context.Context, Worker) (ec2API, error) { return fake, nil }

			t.Cleanup(func() { ec2For = previous })

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if tc.cancel {
				cancel()
			}

			_, err := NewRegistry(testOwner).Leases(boxWorker(t, "aws://stopped/"+instance)).Resolve(ctx, "box")
			if err == nil {
				t.Fatal("a failed start was acquired")
			}

			stopped := slices.Contains(fake.callLog(), "stop "+instance)
			if stopped != tc.wantStop {
				t.Fatalf("calls = %v, stop issued = %v, want %v", fake.callLog(), stopped, tc.wantStop)
			}

			for _, e := range fake.stopErrs {
				if e != nil {
					t.Errorf("stop ran under a spent context: %v", e)
				}
			}
		})
	}
}
