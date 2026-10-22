package venue

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"testing"

	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/testdocker"
)

// The rungs that are refused or have nothing must say so before any cloud call: neither can be reached without credentials, and neither needs any.
func TestClearTaskCachesAnswersTheRungsWithoutDialling(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want error
	}{
		{"aws://launch/lt-0123456789abcdef0?region=us-east-2", ErrKeepsNoCache},
		{"gcp://launch/template-1?project=p&zone=us-central1-a", ErrKeepsNoCache},
		{"do://launch?region=nyc3&size=s-1vcpu-1gb", ErrKeepsNoCache},
		{"aws://stopped/i-0123456789abcdef0?region=us-east-2", ErrParked},
		{"gcp://stopped/worker-1?project=p&zone=us-central1-a", ErrParked},
	} {
		t.Run(tc.url, func(t *testing.T) {
			_, err := ClearTaskCaches(t.Context(), tc.url, []string{"k"}, false)
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// A key the script never answered for was not reached, which must not read as an absent cache.
func TestReadClearedCountsASilentKeyAsAFailure(t *testing.T) {
	cleared := readCleared("a absent\nb removed\n", "rm: denied", []string{"a", "b", "c"})

	if cleared[0].Found || cleared[0].Err != nil {
		t.Errorf("a: %+v, want absent", cleared[0])
	}

	if !cleared[1].Found || cleared[1].Err != nil {
		t.Errorf("b: %+v, want removed", cleared[1])
	}

	if cleared[2].Err == nil {
		t.Errorf("c: %+v, want a failure", cleared[2])
	}
}

// One volume a container mounts is the daemon's refusal for that cache alone; the next is still removed.
func TestClearTaskCachesKeepsGoingPastAVolumeInUse(t *testing.T) {
	testdocker.Require(t)

	docker, err := dockerapi.New("")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = docker.Close() })

	suffix := strconv.Itoa(os.Getpid()) + "-" + t.Name()
	busy, free, never := "busy-"+suffix, "free-"+suffix, "never-"+suffix

	makeCacheVolumes(t, docker, busy, free)
	holdVolume(t, docker, taskCacheVolume("", busy))

	cleared, err := ClearTaskCaches(t.Context(), "local:", []string{busy, free, never}, false)
	if err != nil {
		t.Fatal(err)
	}

	if len(cleared) != 3 {
		t.Fatalf("got %d results, want one per key: %+v", len(cleared), cleared)
	}

	for i, want := range []struct{ found, failed bool }{{true, true}, {true, false}, {false, false}} {
		if cleared[i].Found != want.found || (cleared[i].Err != nil) != want.failed {
			t.Errorf("%s: %+v, want found=%v failed=%v", cleared[i].Key, cleared[i], want.found, want.failed)
		}
	}

	_, err = docker.InspectVolume(t.Context(), taskCacheVolume("", free))
	if !dockerapi.IsNotFound(err) {
		t.Errorf("the free cache's volume is still there: %v", err)
	}
}

func makeCacheVolumes(t *testing.T, docker *dockerapi.Client, keys ...string) {
	t.Helper()

	for _, key := range keys {
		_, err := docker.CreateVolume(t.Context(), taskCacheVolume("", key), shell.OwnershipLabels(), nil)
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = docker.RemoveVolume(context.WithoutCancel(t.Context()), taskCacheVolume("", key)) })
	}
}

// holdVolume mounts volume into a never-started container, which is enough for the daemon to refuse removing it.
func holdVolume(t *testing.T, docker *dockerapi.Client, volume string) {
	t.Helper()

	if !docker.ImagePresent(t.Context(), "alpine:3") {
		err := docker.Pull(t.Context(), "alpine:3", io.Discard)
		if err != nil {
			t.Fatal(err)
		}
	}

	holder, err := docker.CreateHolder(t.Context(), "steps-test-"+randomSuffix(), "alpine:3", shell.OwnershipLabels(), []dockerapi.Bind{{Source: volume, Target: "/c"}})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = docker.RemoveContainer(context.WithoutCancel(t.Context()), holder) })
}
