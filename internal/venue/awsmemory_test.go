package venue

import (
	"context"
	"sync"
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
	awsInstalling.Store(instance, &sync.Mutex{})

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
