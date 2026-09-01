package venue

// The gcp:// venue against a fake control plane and a real sshd in this
// process: only Google itself is replaced. The dial's SSH handshake, the
// binary push, the shim, and the tree round trip all run for real — the
// relay protocol has its own tests one package over, and the seam here
// (iapOpen) is a TCP connection standing in for the whole tunnel.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"

	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/venue/iapdial"
)

// fakeGCE is the control plane: instance lifecycle, metadata, and guest
// attributes, scripted per test.
type fakeGCE struct {
	t *testing.T

	mu       sync.Mutex
	statuses []string
	inserts  []string
	starts   []string
	stops    []string
	deletes  []string
	sshKeys  []string

	hostKeys           map[string]string
	attributesDelay    int
	attributesDisabled bool
	insertErr          error
}

func (f *fakeGCE) InsertFromTemplate(_ context.Context, _, _, name, template string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.insertErr != nil {
		return f.insertErr
	}

	f.inserts = append(f.inserts, template+"->"+name)

	return nil
}

func (f *fakeGCE) Start(_ context.Context, _, _, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.starts = append(f.starts, name)

	return nil
}

func (f *fakeGCE) Stop(_ context.Context, _, _, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.stops = append(f.stops, name)

	return nil
}

func (f *fakeGCE) Delete(_ context.Context, _, _, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.deletes = append(f.deletes, name)

	return nil
}

func (f *fakeGCE) Status(_ context.Context, _, _, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.statuses) == 0 {
		return "RUNNING", nil
	}

	next := f.statuses[0]
	if len(f.statuses) > 1 {
		f.statuses = f.statuses[1:]
	}

	if next == "notfound" {
		return "", fmt.Errorf("%w: scripted", errGCENotFound)
	}

	return next, nil
}

func (f *fakeGCE) AddSSHKey(_ context.Context, _, _, _, entry string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.sshKeys = append(f.sshKeys, entry)

	return nil
}

func (f *fakeGCE) GuestAttributes(_ context.Context, _, _, name, _ string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.attributesDisabled {
		return nil, fmt.Errorf("%w: %s", errGuestAttributesDisabled, name)
	}

	if f.attributesDelay > 0 {
		f.attributesDelay--

		return map[string]string{}, nil
	}

	return f.hostKeys, nil
}

// newGCPSSHD is the worker's sshd: the shared harness, configured to accept
// the process's own gcp:// identity — the key dialGCP mints and installs.
func newGCPSSHD(t *testing.T) *testSSHD {
	t.Helper()

	dir := t.TempDir()
	server := &testSSHD{Root: filepath.Join(dir, "worker")}

	err := os.MkdirAll(server.Root, 0o750)
	if err != nil {
		t.Fatalf("making the worker root: %v", err)
	}

	hostSigner, hostPub, _ := generateKey(t)

	clientKey, err := gcpKey()
	if err != nil {
		t.Fatalf("gcpKey: %v", err)
	}

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if meta.User() != gcpUser {
				return nil, fmt.Errorf("unexpected user %q", meta.User())
			}

			if string(key.Marshal()) != string(clientKey.PublicKey().Marshal()) {
				return nil, errors.New("unknown key")
			}

			return &ssh.Permissions{}, nil
		},
	}
	config.AddHostKey(hostSigner)

	var listenConfig net.ListenConfig

	server.listener, err = listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	server.HostKey = hostPub

	go server.accept(config)

	t.Cleanup(func() {
		server.closed.Store(true)
		_ = server.listener.Close()
		server.conns.Wait()
	})

	return server
}

// hostKeyAttributes is the sshd's host key in the shape the guest agent
// publishes: the hostkeys/ namespace, keyed by algorithm.
func hostKeyAttributes(t *testing.T, pub ssh.PublicKey) map[string]string {
	t.Helper()

	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))

	keyType, value, found := strings.Cut(line, " ")
	if !found {
		t.Fatalf("unsplittable host key %q", line)
	}

	return map[string]string{keyType: value}
}

// seamGCP points gcp:// dials at the fake control plane and the local sshd,
// with only Google itself replaced.
func seamGCP(t *testing.T, fake *fakeGCE, sshd *testSSHD) {
	t.Helper()

	fake.t = t

	if fake.hostKeys == nil && sshd != nil {
		fake.hostKeys = hostKeyAttributes(t, sshd.HostKey)
	}

	previousFor, previousToken, previousOpen := gceFor, gcpToken, iapOpen

	gceFor = func(context.Context, Worker) (gceAPI, error) { return fake, nil }
	gcpToken = func(context.Context) (string, error) { return "test-token", nil }
	iapOpen = func(ctx context.Context, _ iapdial.Target, token string) (net.Conn, error) {
		if token != "test-token" {
			return nil, fmt.Errorf("the dial carried token %q", token)
		}

		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", sshd.listener.Addr().String())
		if err != nil {
			return nil, fmt.Errorf("reaching the test sshd: %w", err)
		}

		return conn, nil
	}

	// The install cache would otherwise let one test's key write satisfy the
	// next test's assertion.
	gcpInstalled.Range(func(key, _ any) bool {
		gcpInstalled.Delete(key)

		return true
	})

	t.Cleanup(func() { gceFor, gcpToken, iapOpen = previousFor, previousToken, previousOpen })
}

// shrinkGCPWaits makes the boot waits testable: the branches worth proving
// are that the dial polls at all, not that it can sit out four minutes.
func shrinkGCPWaits(t *testing.T) {
	t.Helper()

	previousTimeout, previousPoll := gcpReadyTimeout, gcpReadyPoll
	gcpReadyTimeout, gcpReadyPoll = 5*time.Second, 20*time.Millisecond

	t.Cleanup(func() { gcpReadyTimeout, gcpReadyPoll = previousTimeout, previousPoll })
}

// localGCPWorker builds a spec pointing at a gcp:// worker served by the
// local sshd, pushing this test binary as the shim.
func localGCPWorker(t *testing.T, sshd *testSSHD, cwd string, outputs ...string) shell.RunnerSpec {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}

	return shell.RunnerSpec{
		Cwd:    cwd,
		Worker: "gcp://worker-1" + sshd.Root + "?project=test-project&zone=us-central1-a&binary=" + self,
		Fetch:  outputs,
	}
}

// TestVenueRunsAStepOnAGCPWorker is the feature: a step placed on a gcp://
// worker is reached through the tunnel seam, authenticated with a minted
// key against a host key the instance attested, and gives its outputs back —
// the same contract every other venue meets.
func TestVenueRunsAStepOnAGCPWorker(t *testing.T) {
	shrinkGCPWaits(t)

	sshd := newGCPSSHD(t)
	// One empty answer first: a machine that has not finished booting has
	// published nothing, and the dial must wait rather than fail.
	fake := &fakeGCE{attributesDelay: 1}
	seamGCP(t, fake, sshd)

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner := newLocalRunner(t, localGCPWorker(t, sshd, cwd, "out"))

	err := runner.Run(context.Background(), "cat data/seed.txt > out/report.txt")
	if err != nil {
		t.Fatalf("Run on a gcp:// worker: %v", err)
	}

	got := mustRead(t, filepath.Join(cwd, "out", "report.txt"))
	if got != "seed\n" {
		t.Errorf("out/report.txt = %q, want %q", got, "seed\n")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.sshKeys) != 1 {
		t.Fatalf("the dial installed %d SSH keys, want 1", len(fake.sshKeys))
	}

	// The expiring form, so the guest agent removes the grant later.
	entry := fake.sshKeys[0]
	if !strings.HasPrefix(entry, gcpUser+":ssh-ed25519 ") || !strings.Contains(entry, `google-ssh {"userName":"steps","expireOn":`) {
		t.Errorf("installed key is not a google-ssh expiring entry: %q", entry)
	}
}

// TestGCPSecondSessionReusesTheInstalledKey pins that the metadata write
// happens once per instance, not once per step.
func TestGCPSecondSessionReusesTheInstalledKey(t *testing.T) {
	shrinkGCPWaits(t)

	sshd := newGCPSSHD(t)
	fake := &fakeGCE{}
	seamGCP(t, fake, sshd)

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")

	spec := localGCPWorker(t, sshd, cwd)

	for range 2 {
		runner := newLocalRunner(t, spec)

		err := runner.Run(context.Background(), "true")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}

		_ = runner.Close()
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.sshKeys) != 1 {
		t.Errorf("two sessions installed %d SSH keys, want the second to reuse the first", len(fake.sshKeys))
	}
}

// TestGCPDialWaitsOutABootingSSHD pins the retry on the relay's "could not
// reach the backend": for a machine acquired seconds ago it means "not yet",
// and failing on it would make every launch a race with systemd.
func TestGCPDialWaitsOutABootingSSHD(t *testing.T) {
	shrinkGCPWaits(t)

	sshd := newGCPSSHD(t)
	fake := &fakeGCE{}
	seamGCP(t, fake, sshd)

	var refusals sync.Mutex

	remaining := 2
	innerOpen := iapOpen
	iapOpen = func(ctx context.Context, target iapdial.Target, token string) (net.Conn, error) {
		refusals.Lock()

		if remaining > 0 {
			remaining--
			refusals.Unlock()

			return nil, fmt.Errorf("scripted: %w", iapdial.ErrBackendNotReached)
		}

		refusals.Unlock()

		return innerOpen(ctx, target, token)
	}

	cwd := t.TempDir()
	runner := newLocalRunner(t, localGCPWorker(t, sshd, cwd))

	err := runner.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("Run after a booting sshd: %v", err)
	}
}

// TestGCPRefusesWhenGuestAttributesAreDisabled pins the error an operator
// hits when the template never set enable-guest-attributes: the fix is named,
// and nothing was dialed against an unverifiable host.
func TestGCPRefusesWhenGuestAttributesAreDisabled(t *testing.T) {
	shrinkGCPWaits(t)

	sshd := newGCPSSHD(t)
	fake := &fakeGCE{attributesDisabled: true}
	seamGCP(t, fake, sshd)

	runner := newLocalRunner(t, localGCPWorker(t, sshd, t.TempDir()))

	err := runner.Run(context.Background(), "true")
	if err == nil {
		t.Fatal("a worker with no attestable host key was accepted")
	}

	if !strings.Contains(err.Error(), "enable-guest-attributes") || !strings.Contains(err.Error(), "?hostkey=") {
		t.Errorf("error = %v, want both fixes named", err)
	}
}

// TestGCPHostKeyPinBypassesGuestAttributes pins that ?hostkey= is a complete
// answer: no attribute read happens, and a matching key connects.
func TestGCPHostKeyPinBypassesGuestAttributes(t *testing.T) {
	shrinkGCPWaits(t)

	sshd := newGCPSSHD(t)
	// Disabled attributes prove the pin path never asks for them.
	fake := &fakeGCE{attributesDisabled: true}
	seamGCP(t, fake, sshd)

	cwd := t.TempDir()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}

	// Escaped, because a SHA256 fingerprint's base64 can contain '+', which
	// query decoding would otherwise read as a space.
	pin := url.QueryEscape(ssh.FingerprintSHA256(sshd.HostKey))
	runner := newLocalRunner(t, shell.RunnerSpec{
		Cwd: cwd,
		Worker: "gcp://worker-1" + sshd.Root +
			"?project=test-project&zone=us-central1-a&hostkey=" + pin + "&binary=" + self,
	})

	err = runner.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("Run with a pinned host key: %v", err)
	}
}

// TestGCPWrongHostKeyIsRefused pins the verification itself: a machine
// offering a key the instance never attested is not connected to.
func TestGCPWrongHostKeyIsRefused(t *testing.T) {
	shrinkGCPWaits(t)

	sshd := newGCPSSHD(t)

	// Attest a DIFFERENT key than the one the sshd actually offers.
	_, otherPub, _ := generateKey(t)
	fake := &fakeGCE{hostKeys: hostKeyAttributes(t, otherPub)}
	seamGCP(t, fake, sshd)

	runner := newLocalRunner(t, localGCPWorker(t, sshd, t.TempDir()))

	err := runner.Run(context.Background(), "true")
	if err == nil {
		t.Fatal("a host offering an unattested key was accepted")
	}

	if !strings.Contains(err.Error(), "guest attributes") {
		t.Errorf("error = %v, want the attestation mismatch named", err)
	}
}

// TestGCPLaunchRungAcquiresAndDeletes pins the launch rung's whole life:
// created from the template, resolved to a static worker whose rebuilt URL
// still carries the connection options, and deleted at release.
func TestGCPLaunchRungAcquiresAndDeletes(t *testing.T) {
	fake := &fakeGCE{}
	seamGCP(t, fake, nil)

	worker, err := ParseWorker("gcp://launch/steps-workers/var/tmp?project=test-project&zone=us-central1-a")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"gpu": worker})

	resolved, err := leases.Resolve(context.Background(), "gpu")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	assertResolvedGCPWorker(t, resolved)

	fake.mu.Lock()

	if len(fake.inserts) != 1 || !strings.HasPrefix(fake.inserts[0], "steps-workers->") {
		t.Errorf("inserts = %v, want one from the template", fake.inserts)
	}

	fake.mu.Unlock()

	err = leases.ReleaseAll(context.Background())
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.deletes) != 1 || fake.deletes[0] != resolved.Instance {
		t.Errorf("deletes = %v, want the launched instance deleted", fake.deletes)
	}
}

// assertResolvedGCPWorker checks the launched worker's rebuilt URL: it is
// what the pipeline hands the runner as a STRING, so it must re-parse into
// the same machine with its project and zone — this is the seam a resolved
// worker crosses.
func assertResolvedGCPWorker(t *testing.T, resolved Worker) {
	t.Helper()

	if !strings.HasPrefix(resolved.Instance, "steps-") {
		t.Errorf("launched instance %q, want a steps- name", resolved.Instance)
	}

	reparsed, err := ParseWorker(resolved.URL)
	if err != nil {
		t.Fatalf("re-parsing the resolved URL %q: %v", resolved.URL, err)
	}

	if reparsed.Instance != resolved.Instance || reparsed.Project != "test-project" ||
		reparsed.Zone != "us-central1-a" || reparsed.Root != "/var/tmp" {
		t.Errorf("re-parsed worker = %+v, want the launched instance in test-project/us-central1-a under /var/tmp", reparsed)
	}
}

// TestGCPParkedRungStartsAndStops pins the stopped rung, including the
// replica lag: a just-started instance still reads TERMINATED — Compute
// Engine's word for parked — and the wait must see through it.
func TestGCPParkedRungStartsAndStops(t *testing.T) {
	fake := &fakeGCE{statuses: []string{"TERMINATED", "RUNNING"}}
	seamGCP(t, fake, nil)

	worker, err := ParseWorker("gcp://stopped/worker-1?project=test-project&zone=us-central1-a")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"gpu": worker})

	resolved, err := leases.Resolve(context.Background(), "gpu")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if resolved.Instance != "worker-1" || resolved.Rung != RungStatic {
		t.Errorf("resolved = %+v, want worker-1 as a static worker", resolved)
	}

	err = leases.ReleaseAll(context.Background())
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.starts) != 1 || len(fake.stops) != 1 {
		t.Errorf("starts = %v, stops = %v — want one of each", fake.starts, fake.stops)
	}
}

// TestGCPParkedFailedAcquisitionStopsTheInstance pins the parked rung's
// cleanup: a start that never reached RUNNING still parks the machine again,
// because nothing later will — the lease records no release on a failed
// acquire.
func TestGCPParkedFailedAcquisitionStopsTheInstance(t *testing.T) {
	fake := &fakeGCE{statuses: []string{"SUSPENDED"}}
	seamGCP(t, fake, nil)

	worker, err := ParseWorker("gcp://stopped/worker-1?project=test-project&zone=us-central1-a")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	_, _, err = acquire(context.Background(), worker)
	if err == nil {
		t.Fatal("an instance that went to SUSPENDED was acquired")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.stops) != 1 || fake.stops[0] != "worker-1" {
		t.Errorf("stops = %v, want the unusable instance parked again", fake.stops)
	}
}

// TestMergeSSHKey pins the metadata read-modify-write's pure half: a fresh
// item is created, an existing one is appended to, and a key already present
// reports nothing to write — which is what makes the retry loop converge.
func TestMergeSSHKey(t *testing.T) {
	t.Parallel()

	metadata := &compute.Metadata{}

	if !mergeSSHKey(metadata, "steps:key-one") {
		t.Fatal("merging into empty metadata reported no change")
	}

	if len(metadata.Items) != 1 || metadata.Items[0].Key != "ssh-keys" || *metadata.Items[0].Value != "steps:key-one" {
		t.Fatalf("metadata after first merge = %+v", metadata.Items)
	}

	if !mergeSSHKey(metadata, "steps:key-two") {
		t.Fatal("merging a second key reported no change")
	}

	if *metadata.Items[0].Value != "steps:key-one\nsteps:key-two" {
		t.Fatalf("ssh-keys after append = %q, want both lines", *metadata.Items[0].Value)
	}

	if mergeSSHKey(metadata, "steps:key-one") {
		t.Fatal("re-merging an existing key claimed a change — the caller would loop on writes")
	}
}

// TestIsGoogleAPIStatus pins the classification by status code rather than
// message text — the same by-the-contract rule notYetVisible holds for EC2.
func TestIsGoogleAPIStatus(t *testing.T) {
	t.Parallel()

	notFound := fmt.Errorf("wrapped: %w", &googleapi.Error{Code: 404, Message: "not found"})

	if !isGoogleAPIStatus(notFound, 404) {
		t.Error("a wrapped 404 was not recognized")
	}

	if isGoogleAPIStatus(notFound, 403) {
		t.Error("a 404 answered for 403")
	}

	if isGoogleAPIStatus(errors.New("plain"), 404) {
		t.Error("a plain error answered for an API status")
	}
}

// TestGCPFailedAcquisitionDeletesTheInstance pins the cleanup: a machine
// that went the wrong way mid-acquisition is deleted, not stranded billing.
func TestGCPFailedAcquisitionDeletesTheInstance(t *testing.T) {
	fake := &fakeGCE{statuses: []string{"STOPPING"}}
	seamGCP(t, fake, nil)

	worker, err := ParseWorker("gcp://launch/steps-workers?project=test-project&zone=us-central1-a")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	_, _, err = acquire(context.Background(), worker)
	if err == nil {
		t.Fatal("an instance that went to STOPPING was acquired")
	}

	if !strings.Contains(err.Error(), "STOPPING") {
		t.Errorf("error = %v, want the terminal status named", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.deletes) != 1 {
		t.Errorf("deletes = %v, want the unusable instance deleted", fake.deletes)
	}
}

// TestGCPLaunchToleratesReplicaLag pins the 404 window after an accepted
// insert: "does not exist" before the first sighting means "not yet".
func TestGCPLaunchToleratesReplicaLag(t *testing.T) {
	fake := &fakeGCE{statuses: []string{"notfound", "RUNNING"}}
	seamGCP(t, fake, nil)

	worker, err := ParseWorker("gcp://launch/steps-workers?project=test-project&zone=us-central1-a")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	resolved, release, err := acquire(context.Background(), worker)
	if err != nil {
		t.Fatalf("acquire through the lag: %v", err)
	}

	_ = release(context.Background(), true)

	if resolved.Rung != RungStatic {
		t.Errorf("resolved rung = %q, want static", resolved.Rung)
	}
}

// TestParseGCPWorker pins the static form: an instance with its root,
// project and zone, and the address the run record keeps.
func TestParseGCPWorker(t *testing.T) {
	t.Parallel()

	worker, err := ParseWorker("gcp://worker-1/var/tmp/steps?project=my-proj&zone=us-central1-a")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	if worker.Scheme != SchemeGCP || worker.Instance != "worker-1" ||
		worker.Root != "/var/tmp/steps" || worker.Project != "my-proj" || worker.Zone != "us-central1-a" {
		t.Errorf("worker = %+v, want the instance with its root, project and zone", worker)
	}

	if got := worker.Address(); got != "gcp://worker-1/var/tmp/steps" {
		t.Errorf("Address = %q, want the machine without its options", got)
	}
}

// TestParseGCPStoppedRung pins the parked form and its idle window.
func TestParseGCPStoppedRung(t *testing.T) {
	t.Parallel()

	stopped, err := ParseWorker("gcp://stopped/worker-1?zone=us-central1-a&idle=5m")
	if err != nil {
		t.Fatalf("ParseWorker stopped: %v", err)
	}

	if stopped.Rung != RungStopped || stopped.Instance != "worker-1" || stopped.Idle != 5*time.Minute {
		t.Errorf("stopped = %+v, want the parked rung with its idle window", stopped)
	}

	if got := stopped.Address(); got != "gcp://stopped/worker-1" {
		t.Errorf("Address = %q, want the rung spelled", got)
	}
}

// TestParseGCPLaunchRung pins the template form.
func TestParseGCPLaunchRung(t *testing.T) {
	t.Parallel()

	launch, err := ParseWorker("gcp://launch/steps-workers?zone=us-central1-a")
	if err != nil {
		t.Fatalf("ParseWorker launch: %v", err)
	}

	if launch.Rung != RungLaunch || launch.Template != "steps-workers" {
		t.Errorf("launch = %+v, want the template rung", launch)
	}

	if got := launch.Address(); got != "gcp://launch/steps-workers" {
		t.Errorf("Address = %q, want the rung spelled", got)
	}
}

// TestParseGCPWorkerRefusals pins every mapping the grammar must refuse when
// it is read — each either a typo with money attached or an option that
// belongs to a different scheme.
func TestParseGCPWorkerRefusals(t *testing.T) {
	t.Parallel()

	refusals := map[string]string{
		"gcp://user@worker-1?zone=z":                    "username cannot be chosen",
		"gcp://Worker_1?zone=us-central1-a":             "cannot name an instance",
		"gcp://stopped?zone=us-central1-a":              "needs something to acquire",
		"gcp://launch?zone=us-central1-a":               "needs something to acquire",
		"gcp://worker-1?zone=z&version=2":               "version= does not describe a gcp worker",
		"gcp://launch/tmpl?zone=z&capacity=spot":        "capacity= does not describe a gcp worker",
		"gcp://worker-1?zone=z&shim=/usr/bin/steps":     "shim= does not describe a gcp worker",
		"gcp://worker-1?zone=z&region=us-east-1":        "region= does not describe a gcp worker",
		"gcp://worker-1?zone=z&identity=/tmp/key":       "identity= does not describe a gcp worker",
		"gcp://worker-1?zone=z&known_hosts=/tmp/kh":     "known_hosts= does not describe a gcp worker",
		"gcp://worker-1?zone=z&ssh_config=none":         "ssh_config= does not describe a gcp worker",
		"gcp://worker-1?zone=z&idle=5m":                 "not on the stopped rung",
		"gcp://launch/steps-workers?zone=z&idle=5m":     "not on the stopped rung",
		"gcp://worker-1?zone=z&hostkey=SHA256:tooshort": "must be an OpenSSH SHA256 fingerprint",
	}

	for mapping, want := range refusals {
		_, err := ParseWorker(mapping)
		if err == nil {
			t.Errorf("ParseWorker(%q) accepted a mapping that cannot work", mapping)

			continue
		}

		if !strings.Contains(err.Error(), want) {
			t.Errorf("ParseWorker(%q) = %v, want it to say %q", mapping, err, want)
		}
	}
}

// TestGCPLocationResolution pins where the project and zone come from: the
// mapping, then the environment gcloud honors, and for the zone nothing else.
func TestGCPLocationResolution(t *testing.T) {
	worker, err := ParseWorker("gcp://worker-1?project=from-url&zone=zone-from-url")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	project, zone, err := gcpLocation(context.Background(), worker)
	if err != nil || project != "from-url" || zone != "zone-from-url" {
		t.Errorf("gcpLocation = %q, %q, %v — want the mapping's own answers", project, zone, err)
	}
}

// TestGCPLocationFromEnvironment pins the fallbacks: the variables gcloud
// itself honors, and the refusal when no zone can be found anywhere.
func TestGCPLocationFromEnvironment(t *testing.T) {
	bare, err := ParseWorker("gcp://worker-1")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	t.Setenv("CLOUDSDK_COMPUTE_ZONE", "env-zone")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "env-project")

	project, zone, err := gcpLocation(context.Background(), bare)
	if err != nil || project != "env-project" || zone != "env-zone" {
		t.Errorf("gcpLocation = %q, %q, %v — want the environment's answers", project, zone, err)
	}

	t.Setenv("CLOUDSDK_COMPUTE_ZONE", "")

	_, _, err = gcpLocation(context.Background(), bare)
	if err == nil || !strings.Contains(err.Error(), "?zone=") {
		t.Errorf("gcpLocation with no zone = %v, want the fix named", err)
	}
}

// TestGCPProjectFallbackChain pins where the project comes from when the
// mapping and GOOGLE_CLOUD_PROJECT are silent: the gcloud-config variable,
// then the credentials' own project, then a refusal that names the fix.
func TestGCPProjectFallbackChain(t *testing.T) {
	bare, err := ParseWorker("gcp://worker-1")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	t.Setenv("CLOUDSDK_COMPUTE_ZONE", "env-zone")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("CLOUDSDK_CORE_PROJECT", "cloudsdk-project")

	project, _, err := gcpLocation(context.Background(), bare)
	if err != nil || project != "cloudsdk-project" {
		t.Errorf("gcpLocation = %q, %v — want the CLOUDSDK project", project, err)
	}

	t.Setenv("CLOUDSDK_CORE_PROJECT", "")

	previous := gcpDefaultProject
	gcpDefaultProject = func(context.Context) (string, error) { return "adc-project", nil }

	t.Cleanup(func() { gcpDefaultProject = previous })

	project, _, err = gcpLocation(context.Background(), bare)
	if err != nil || project != "adc-project" {
		t.Errorf("gcpLocation = %q, %v — want the ADC project", project, err)
	}

	gcpDefaultProject = func(context.Context) (string, error) { return "", nil }

	_, _, err = gcpLocation(context.Background(), bare)
	if err == nil || !strings.Contains(err.Error(), "?project=") {
		t.Errorf("gcpLocation with no project = %v, want the fix named", err)
	}
}

// TestGCPPlacementCheck pins the pre-run refusal of a dial that can never
// work: a non-Linux orchestrator's own binary cannot run on a GCE instance.
func TestGCPPlacementCheck(t *testing.T) {
	t.Parallel()

	bare, err := ParseWorker("gcp://worker-1?zone=us-central1-a")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	withBinary, err := ParseWorker("gcp://worker-1?zone=us-central1-a&binary=/tmp/steps-linux-amd64")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	err = withBinary.PlacementCheck(false)
	if err != nil {
		t.Errorf("PlacementCheck with ?binary= = %v, want accepted", err)
	}

	// The refusal is a fact about the orchestrator's own OS, so the test
	// asserts whichever side of it this machine is on.
	err = bare.PlacementCheck(false)
	if runtime.GOOS == "linux" {
		if err != nil {
			t.Errorf("PlacementCheck on linux = %v, want this binary accepted", err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "?binary=") {
		t.Errorf("PlacementCheck off linux = %v, want the fix named", err)
	}
}
