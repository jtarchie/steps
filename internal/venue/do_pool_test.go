package venue

import (
	"context"
	"slices"
	"testing"
	"time"
)

// ?exclusive works on do:// as on every launch rung: two concurrent jobs, two droplets, both deleted.
func TestDOExclusiveGivesTwoConcurrentJobsTwoDroplets(t *testing.T) {
	fake := newFakeDO(t)
	fake.noBoot = true

	workers := boxWorker(t, "do://launch/?exclusive&max=2")
	registry := NewRegistry("")
	first, second := registry.Leases(workers), registry.Leases(workers)

	one, other := mustResolve(t, first), mustResolve(t, second)
	if one.Instance == other.Instance {
		t.Fatalf("two concurrent jobs share droplet %s", one.Instance)
	}

	mustRelease(t, first)
	mustRelease(t, second)

	if creates, deletes, live := fake.snapshot(); len(creates) != 2 || len(deletes) != 2 || live != 0 {
		t.Errorf("%d creates, deletes %v, %d left — want two droplets, both deleted", len(creates), deletes, live)
	}
}

func TestDOSweepDeletesEveryDropletOfAPool(t *testing.T) {
	fake := newFakeDO(t)
	now := time.Now()
	ours := labelOwner + ":" + stamp("me", RungLaunch, now)

	fake.plant(1, "nyc3", "docker-20-04", "s-1vcpu-1gb", "active", now, ours)
	fake.plant(2, "nyc3", "docker-20-04", "s-1vcpu-1gb", "active", now, ours)

	err := NewRegistry("me").Sweep(context.Background(), map[string]Worker{"do": mustParse(t, "do://launch/?exclusive&max=2")})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	_, deletes, _ := fake.snapshot()
	slices.Sort(deletes)

	if !slices.Equal(deletes, []int{1, 2}) {
		t.Errorf("swept %v, want both droplets of the pool", deletes)
	}
}
