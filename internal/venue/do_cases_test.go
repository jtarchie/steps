package venue

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/godo"
	"golang.org/x/crypto/ssh"
)

func TestParseDOLaunchDefaultsAndOptions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ raw, image, region, size string }{
		{"do://launch/", "docker-20-04", "nyc3", "s-1vcpu-1gb"},
		{"do://launch", "docker-20-04", "nyc3", "s-1vcpu-1gb"},
		{"do://launch/ubuntu-24-04-x64?region=sfo3&size=s-2vcpu-4gb&idle=5m", "ubuntu-24-04-x64", "sfo3", "s-2vcpu-4gb"},
		{"do://launch/12345", "12345", "nyc3", "s-1vcpu-1gb"},
	} {
		worker, err := ParseWorker(tc.raw)
		if err != nil {
			t.Fatalf("ParseWorker(%q): %v", tc.raw, err)
		}

		if worker.Rung != RungLaunch || worker.Template != tc.image || worker.Region != tc.region || worker.Size != tc.size {
			t.Errorf("ParseWorker(%q) = rung %q image %q region %q size %q, want launch %s/%s/%s", tc.raw, worker.Rung, worker.Template, worker.Region, worker.Size, tc.image, tc.region, tc.size)
		}

		if !worker.dockerPlus() || !worker.Acquirable() {
			t.Errorf("ParseWorker(%q) is not an acquirable docker+ worker", tc.raw)
		}
	}
}

func TestDOImageIDOrSlug(t *testing.T) {
	t.Parallel()

	if got := doImage("12345"); got.ID != 12345 || got.Slug != "" {
		t.Errorf("doImage(12345) = %+v, want an id", got)
	}

	if got := doImage("docker-20-04"); got.ID != 0 || got.Slug != "docker-20-04" {
		t.Errorf("doImage(docker-20-04) = %+v, want a slug", got)
	}
}

func TestParseDORefusals(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]string{
		"do://root@launch/":                       "username",
		"do://stopped/123":                        "only launches",
		"do://123":                                "only launches",
		"do://launch/docker-20-04/var/tmp":        "no path past the image",
		"do://launch/Docker_Image":                "cannot name a DigitalOcean image",
		"do://launch/?region=NYC3":                "cannot name a DigitalOcean region",
		"do://launch/?size=big%20one":             "cannot name a DigitalOcean size",
		"do://launch/?region=":                    "cannot name a DigitalOcean region",
		"do://launch/?size=":                      "cannot name a DigitalOcean size",
		"do://launch/?capacity=spot":              "does not describe a do worker",
		"do://launch/?hostkey=SHA256:abc":         "does not describe a do worker",
		"do://launch/?version=2":                  "does not describe a do worker",
		"do://launch/?binary=./steps":             "binary= is gone",
		"aws://launch/lt-0def4567890abcde?size=x": "does not describe a aws worker",
	} {
		_, err := ParseWorker(raw)
		if err == nil || !errors.Is(err, ErrWorker) || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseWorker(%q) = %v, want an ErrWorker saying %q", raw, err, want)
		}
	}
}

// The resolved URL is what crosses into the runner as a string, so it must re-parse into the same droplet — and only because this process launched it.
func TestDOResolvedURLReparsesOnlyWhileLaunched(t *testing.T) {
	fake := newFakeDO(t)
	fake.noBoot = true

	worker, err := ParseWorker("do://launch/?region=sfo3&size=s-2vcpu-4gb&idle=5m")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"do": worker})

	resolved, err := leases.Resolve(context.Background(), "do")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	reparsed, err := ParseWorker(resolved.URL)
	if err != nil {
		t.Fatalf("re-parsing %q: %v", resolved.URL, err)
	}

	if reparsed.Instance != resolved.Instance || reparsed.Rung != RungStatic || resolved.Address() != "do://"+resolved.Instance || worker.Address() != "do://launch/docker-20-04" {
		t.Errorf("re-parsed %+v from %q; addresses %q and %q", reparsed, resolved.URL, resolved.Address(), worker.Address())
	}

	mustReleaseWith(context.Background(), t, leases)

	_, err = ParseWorker(resolved.URL)
	if err == nil || !strings.Contains(err.Error(), "only launches") {
		t.Errorf("after delete, ParseWorker(%q) = %v, want the launch-only refusal", resolved.URL, err)
	}
}

// size= describes a launch; on a droplet that exists it can only be a mapping that looks configured and is not.
func TestDOLaunchedDropletRefusesSize(t *testing.T) {
	newFakeDO(t)
	resolved, _ := launchedMachine(t)

	_, err := ParseWorker("do://" + resolved.Instance + "?size=s-1vcpu-1gb")
	if err == nil || !strings.Contains(err.Error(), "size= is the shape") {
		t.Errorf("a launched droplet with ?size= parsed: %v, want size= refused on a droplet that exists", err)
	}
}

func TestDOWithoutATokenIsRefusedBeforeAnythingIsCreated(t *testing.T) {
	fake := newFakeDO(t)
	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", "")

	worker, _ := ParseWorker("do://launch/")

	_, _, err := acquire(context.Background(), worker)
	if err == nil || !errors.Is(err, ErrWorker) || !strings.Contains(err.Error(), "DIGITALOCEAN_ACCESS_TOKEN") {
		t.Fatalf("acquire with no token = %v, want a refusal naming the variable", err)
	}

	if creates, _, _ := fake.snapshot(); len(creates) != 0 {
		t.Errorf("creates = %d, want none", len(creates))
	}
}

func TestDOFallsBackToDigitaloceanToken(t *testing.T) {
	fake := newFakeDO(t)
	fake.noBoot = true

	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", "")
	t.Setenv("DIGITALOCEAN_TOKEN", "test-token")

	worker, _ := ParseWorker("do://launch/")

	_, release, err := acquire(context.Background(), worker)
	if err != nil {
		t.Fatalf("acquire with DIGITALOCEAN_TOKEN: %v", err)
	}

	_ = release(context.Background())
}

func TestDORefusedCreateLeavesNothing(t *testing.T) {
	fake := newFakeDO(t)
	fake.createStatus = http.StatusUnprocessableEntity

	worker, _ := ParseWorker("do://launch/")

	_, _, err := acquire(context.Background(), worker)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("acquire = %v, want the API's refusal", err)
	}

	if _, _, live := fake.snapshot(); live != 0 {
		t.Errorf("%d droplets left, want none", live)
	}
}

// A create the API carried out but whose answer was lost: the droplet exists, and only its name — chosen by steps — finds it.
func TestDOLostCreateIsFoundByNameAndDeleted(t *testing.T) {
	fake := newFakeDO(t)
	fake.noBoot = true
	fake.createStatus, fake.createThenFail = http.StatusInternalServerError, true

	worker, _ := ParseWorker("do://launch/")

	_, _, err := acquire(context.Background(), worker)
	if err == nil {
		t.Fatal("acquire succeeded on a lost create")
	}

	if _, deletes, live := fake.snapshot(); live != 0 || len(deletes) != 1 {
		t.Errorf("after a lost create: %d left, deletes %v — want the droplet found by name and deleted", live, deletes)
	}
}

func TestDODropletThatNeverArrivesIsDeleted(t *testing.T) {
	fake := newFakeDO(t)
	fake.noBoot = true
	fake.status = "off"

	worker, _ := ParseWorker("do://launch/")

	_, _, err := acquire(context.Background(), worker)
	if err == nil || !strings.Contains(err.Error(), "went to off") {
		t.Fatalf("acquire = %v, want the droplet named as going to off", err)
	}

	if _, deletes, live := fake.snapshot(); live != 0 || len(deletes) != 1 {
		t.Errorf("%d left, deletes %v — want it deleted", live, deletes)
	}
}

// Cancelled mid-wait is the likeliest failure, and the delete must still reach the API.
func TestDOCancelledAcquisitionStillDeletes(t *testing.T) {
	fake := newFakeDO(t)
	fake.noBoot = true
	fake.status = "new"

	worker, _ := ParseWorker("do://launch/")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, _, err := acquire(ctx, worker)
	if err == nil {
		t.Fatal("acquire succeeded on a droplet that never became active")
	}

	if _, deletes, live := fake.snapshot(); live != 0 || len(deletes) != 1 {
		t.Errorf("%d left, deletes %v — want it deleted despite the cancelled context", live, deletes)
	}
}

func launchedMachine(t *testing.T) (Worker, *doMachine) {
	t.Helper()

	worker, _ := ParseWorker("do://launch/")

	resolved, release, err := acquire(context.Background(), worker)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	t.Cleanup(func() { _ = release(context.Background()) })

	value, ok := doMachines.Load(resolved.Instance)
	if !ok {
		t.Fatalf("droplet %s is not remembered", resolved.Instance)
	}

	return resolved, value.(*doMachine) //nolint:forcetypeassert // the map holds one type
}

func doClientConfig(t *testing.T, machine *doMachine) *ssh.ClientConfig {
	t.Helper()

	signer, err := sshIdentity()
	if err != nil {
		t.Fatal(err)
	}

	return &ssh.ClientConfig{
		User: doUser, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: fixedHostKey(machine.hostKey), HostKeyAlgorithms: hostKeyAlgorithms(machine.hostKey), Timeout: dialTimeout,
	}
}

func (f *fakeDO) dials() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.dialed)
}

// Until cloud-init swaps it, a fresh droplet offers its image's own host key: refused, retried, and the pinned one accepted once it appears.
func TestDODialWaitsOutTheImageHostKey(t *testing.T) {
	fake := newFakeDO(t)
	fake.imageKeyDials = 2

	resolved, machine := launchedMachine(t)

	client, err := doAwaitBoot(context.Background(), resolved, machine, doClientConfig(t, machine))
	if err != nil {
		t.Fatalf("doAwaitBoot: %v", err)
	}

	_ = client.Close()

	if got := fake.dials(); got != 3 {
		t.Errorf("dialed %d times, want two refusals of the image key then the pinned one", got)
	}
}

func TestDOWrongHostKeyIsNeverAccepted(t *testing.T) {
	fake := newFakeDO(t)
	fake.wrongKeyForever = true

	resolved, machine := launchedMachine(t)
	doReadyTimeout = 300 * time.Millisecond

	client, err := doAwaitBoot(context.Background(), resolved, machine, doClientConfig(t, machine))
	if err == nil {
		_ = client.Close()

		t.Fatal("a droplet offering a host key steps never minted was accepted")
	}

	if !errors.Is(err, errHostKeyMismatch) || fake.dials() < 2 {
		t.Errorf("err = %v after %d dials, want a host key mismatch, retried", err, fake.dials())
	}
}

// A job ending mid-boot is reported as the job ending, never as the image key the wait saw on the way.
func TestDOCancelledBootWaitIsNotAMismatch(t *testing.T) {
	fake := newFakeDO(t)
	fake.wrongKeyForever = true

	resolved, machine := launchedMachine(t)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	client, err := doAwaitBoot(ctx, resolved, machine, doClientConfig(t, machine))
	if err == nil {
		_ = client.Close()

		t.Fatal("a droplet offering a host key steps never minted was accepted")
	}

	if errors.Is(err, errHostKeyMismatch) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the job's own deadline, not a mismatch", err)
	}
}

// A droplet that has answered once is not waited on again: a warm droplet that died must fail its first dial, so the lease re-places the step.
func TestDOReadyDropletFailsItsFirstDial(t *testing.T) {
	fake := newFakeDO(t)

	resolved, machine := launchedMachine(t)
	machine.ready.Store(true)

	fake.mu.Lock()
	fake.wrongKeyForever = true
	fake.mu.Unlock()

	started := time.Now()

	_, err := doSSHClient(context.Background(), resolved)
	if err == nil || !errors.Is(err, errHostKeyMismatch) {
		t.Fatalf("doSSHClient = %v, want the mismatch at once", err)
	}

	if got := fake.dials(); got != 1 || time.Since(started) > time.Second {
		t.Errorf("dialed %d times in %s, want one dial and no wait", got, time.Since(started))
	}
}

func TestDOIdleKeepsTheDropletForTheNextJob(t *testing.T) {
	fake := newFakeDO(t)
	fake.noBoot = true

	workers := boxWorker(t, "do://launch/?idle=1m")
	registry := NewRegistry("")

	for range 2 {
		leases := registry.Leases(workers)
		mustResolve(t, leases)
		mustRelease(t, leases)
	}

	if creates, deletes, _ := fake.snapshot(); len(creates) != 1 || len(deletes) != 0 {
		t.Fatalf("two jobs inside ?idle=: %d creates, %d deletes — want one droplet kept warm", len(creates), len(deletes))
	}

	err := registry.Close(context.Background())
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, deletes, live := fake.snapshot(); len(deletes) != 1 || live != 0 {
		t.Errorf("after Close: deletes %v, %d left — want the warm droplet deleted", deletes, live)
	}
}

func TestDORegistryKeyFoldsInImageRegionAndSize(t *testing.T) {
	t.Parallel()

	keys := map[string]bool{}

	for _, raw := range []string{"do://launch/", "do://launch/ubuntu-24-04-x64", "do://launch/?region=sfo3", "do://launch/?size=s-2vcpu-4gb"} {
		worker, err := ParseWorker(raw)
		if err != nil {
			t.Fatalf("ParseWorker(%q): %v", raw, err)
		}

		keys[worker.registryKey()] = true
	}

	if len(keys) != 4 {
		t.Errorf("four machine shapes made %d registry keys, want 4", len(keys))
	}

	plain, _ := ParseWorker("do://launch/")
	spelled, _ := ParseWorker("do://launch/docker-20-04?region=nyc3&size=s-1vcpu-1gb&idle=5m")

	if plain.registryKey() != spelled.registryKey() {
		t.Errorf("the defaults spelled out are a second machine: %q vs %q", plain.registryKey(), spelled.registryKey())
	}
}

func (f *fakeDO) plant(id int, region, image, size, status string, created time.Time, tags ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.droplets[id] = &fakeDroplet{Droplet: godo.Droplet{
		ID: id, Name: "steps-planted", Status: status, Region: &godo.Region{Slug: region}, Image: &godo.Image{Slug: image},
		SizeSlug: size, Tags: tags, Created: created.UTC().Format(time.RFC3339),
	}}
}

// The sweep deletes only what this daemon's stamp names AND a mapping here would have launched: a stamp is a tag, which anyone with tag write can put on any droplet.
func TestDOSweepDeletesOnlyThisDaemonsLaunches(t *testing.T) {
	fake := newFakeDO(t)
	now := time.Now()
	ours := labelOwner + ":" + stamp("me", RungLaunch, now)

	fake.plant(1, "nyc3", "docker-20-04", "s-1vcpu-1gb", "active", now, "steps-pid:1", ours)
	fake.plant(2, "nyc3", "docker-20-04", "s-1vcpu-1gb", "new", now, ours)
	fake.plant(3, "nyc3", "postgres", "s-1vcpu-1gb", "active", now, ours)
	fake.plant(4, "sfo3", "docker-20-04", "s-1vcpu-1gb", "active", now, ours)
	fake.plant(5, "nyc3", "docker-20-04", "s-1vcpu-1gb", "active", now, labelOwner+":"+stamp("other", RungLaunch, now))
	fake.plant(6, "nyc3", "docker-20-04", "s-1vcpu-1gb", "active", now.Add(time.Hour), ours)
	fake.plant(7, "nyc3", "docker-20-04", "s-1vcpu-1gb", "active", now)
	fake.plant(8, "nyc3", "docker-20-04", "s-2vcpu-4gb", "active", now, ours)
	fake.plant(9, "nyc3", "docker-20-04", "s-1vcpu-1gb", "off", now, ours)
	fake.plant(10, "nyc3", "ubuntu-24-04-x64", "s-1vcpu-1gb", "active", now, ours)
	fake.droplets[10].Image.ID = 777

	worker, _ := ParseWorker("do://launch/")
	byID, _ := ParseWorker("do://launch/777")

	err := NewRegistry("me").Sweep(context.Background(), map[string]Worker{"do": worker, "by-id": byID})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	_, deletes, _ := fake.snapshot()
	slices.Sort(deletes)

	if !slices.Equal(deletes, []int{1, 2, 10}) {
		t.Errorf("swept %v, want only 1, 2 and 10 (a slugged image a mapping names by id): this daemon's stamp, this region, a mapped image and size, billing, created when stamped", deletes)
	}
}

// A droplet that existed and then 404s was destroyed out from under the wait, which must not sit out the whole acquisition timeout for it.
func TestDODropletThatVanishesFailsFast(t *testing.T) {
	fake := newFakeDO(t)
	fake.noBoot = true
	fake.vanishAfter = 1

	worker, _ := ParseWorker("do://launch/")

	_, _, err := acquire(context.Background(), worker)
	if err == nil || !strings.Contains(err.Error(), "vanished") {
		t.Fatalf("acquire = %v, want the droplet named as vanished", err)
	}
}

// An image without docker answers ssh and nothing else: the dial says so rather than handing back a worker no step can run on.
func TestDODropletWithoutDockerIsRefused(t *testing.T) {
	bin := t.TempDir()
	mustWrite(t, filepath.Join(bin, "docker"), "#!/bin/sh\nexit 1\n")
	mustWrite(t, filepath.Join(bin, "sleep"), "#!/bin/sh\nexit 0\n")

	for _, name := range []string{"docker", "sleep"} {
		err := os.Chmod(filepath.Join(bin, name), 0o700) //nolint:gosec // a test stub on PATH
		if err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("PATH", bin+":/bin:/usr/bin")

	fake := newFakeDO(t)
	resolved, machine := launchedMachine(t)

	_, err := doSSHClient(context.Background(), resolved)
	if err == nil || !strings.Contains(err.Error(), "docker never answered") {
		t.Fatalf("doSSHClient = %v, want the missing docker named", err)
	}

	if machine.ready.Load() || fake.dials() == 0 {
		t.Errorf("ready = %v after %d dials, want a droplet whose docker never answered left un-ready", machine.ready.Load(), fake.dials())
	}
}

// The docker wait takes no context of its own, so a job ending mid-wait must still end it rather than sit out the window.
func TestDOCancelledDockerWaitEndsAtOnce(t *testing.T) {
	bin := t.TempDir()
	mustWrite(t, filepath.Join(bin, "docker"), "#!/bin/sh\nexit 1\n")
	mustWrite(t, filepath.Join(bin, "sleep"), "#!/bin/sh\nexec /bin/sleep 0.1\n")

	for _, name := range []string{"docker", "sleep"} {
		err := os.Chmod(filepath.Join(bin, name), 0o700) //nolint:gosec // a test stub on PATH
		if err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("PATH", bin+":/bin:/usr/bin")

	newFakeDO(t)
	resolved, machine := launchedMachine(t)
	// Fifteen tries of a tenth of a second: a wait that ignored the deadline would outlast it several times over.
	doReadyTimeout = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	started := time.Now()

	_, err := doSSHClient(ctx, resolved)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("doSSHClient = %v, want the job's own deadline", err)
	}

	if elapsed := time.Since(started); elapsed > time.Second || machine.ready.Load() {
		t.Errorf("returned after %s, ready = %v; want the wait ended by the deadline, un-ready", elapsed, machine.ready.Load())
	}
}
