package venue

import (
	"context"
	"errors"
	"testing"
)

// A terminated instance id never comes back, so the process-wide install memory for it is dead weight; a failed terminate leaves the instance alive and its memory in use.
func TestTerminateForgetsTheInstancesInstallMemory(t *testing.T) {
	const instance = "i-0gone"

	holds := func() (bool, bool) {
		_, installed := awsInstalled.Load(instance)
		_, installing := awsInstalling.Load(instance)

		return installed, installing
	}

	awsInstalled.Store(instance, true)
	awsInstalling.Store(instance, make(chan struct{}, 1))

	t.Cleanup(func() {
		awsInstalled.Delete(instance)
		awsInstalling.Delete(instance)
	})

	err := terminateEC2(refusingEC2{&fakeEC2{}}, instance)(context.Background())
	if err == nil {
		t.Fatal("terminate through a refusing API succeeded")
	}

	installed, installing := holds()
	if !installed || !installing {
		t.Errorf("a failed terminate dropped memory of a live instance: installed=%v installing=%v", installed, installing)
	}

	err = terminateEC2(&fakeEC2{}, instance)(context.Background())
	if err != nil {
		t.Fatalf("terminate: %v", err)
	}

	installed, installing = holds()
	if installed || installing {
		t.Errorf("memory outlived the instance: installed=%v installing=%v", installed, installing)
	}
}

// An aborted step must not sit out a sibling's install, which waits minutes for docker on a fresh machine.
func TestAWSInstallWaitEndsWithItsContext(t *testing.T) {
	t.Parallel()

	const instance = "i-0installing"

	unlock, err := lockInstall(context.Background(), instance)
	if err != nil {
		t.Fatalf("lockInstall: %v", err)
	}

	t.Cleanup(func() {
		unlock()
		awsInstalling.Delete(instance)
	})

	ctx, abort := context.WithCancel(context.Background())
	got := make(chan error, 1)

	go func() {
		_, err := awsHostKey(ctx, nil, Worker{URL: "aws://" + instance, Instance: instance}, "", nil)
		got <- err
	}()

	abort()

	err = within(t, "the aborted wait returned", got)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("awsHostKey = %v, want the abort", err)
	}
}
