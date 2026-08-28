package shell

// The contract a container step keeps, asked of a real daemon.
//
// Every other test in this package asserts the ARGV this package builds, which
// is only a proxy: it proves what `docker` was told, never what the daemon
// did with it. That proxy is also exactly what a move off the docker CLI
// deletes, so a suite made of it would go green through a port that changed
// the behaviour underneath. These tests ask the daemon instead — what the
// container was actually configured with, and what a command inside it can
// actually see — so they mean the same thing before and after such a change.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// daemonView is the daemon's own account of a container, as `docker inspect`
// reports it. Only the fields this package sets are named: the point is to
// compare what was asked for against what the daemon recorded, not to mirror
// the API.
type daemonView struct {
	Name   string `json:"Name"`
	Config struct {
		Image      string            `json:"Image"`
		Cmd        []string          `json:"Cmd"`
		Env        []string          `json:"Env"`
		User       string            `json:"User"`
		WorkingDir string            `json:"WorkingDir"`
		Labels     map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		Binds       []string `json:"Binds"`
		NetworkMode string   `json:"NetworkMode"`
		Privileged  bool     `json:"Privileged"`
		CPUShares   int64    `json:"CpuShares"`
		Memory      int64    `json:"Memory"`
		Init        *bool    `json:"Init"`
		AutoRemove  bool     `json:"AutoRemove"`
	} `json:"HostConfig"`
}

// inspectOurContainer returns the daemon's view of the one container this
// process currently owns.
//
// Found by the ownership labels rather than by name because the name is minted
// inside the session and never leaves it — which is the same reason the sweep
// has to work this way. Fails rather than skips when there is no such
// container: the caller has just run a command in one.
func inspectOurContainer(t *testing.T) daemonView {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	//nolint:gosec // fixed argv but for this process's own pid
	out, err := exec.CommandContext(ctx, "docker", "ps", "--all", "--no-trunc", "--quiet",
		"--filter", "label="+dockerOwnerLabel+"=steps",
		"--filter", "label="+dockerPIDLabel+"="+strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Fatalf("listing this process's containers: %v", err)
	}

	ids := strings.Fields(string(out))
	if len(ids) != 1 {
		t.Fatalf("found %d containers labelled for this process, want exactly 1", len(ids))
	}

	//nolint:gosec // the id came from the daemon
	raw, err := exec.CommandContext(ctx, "docker", "inspect", ids[0]).Output()
	if err != nil {
		t.Fatalf("inspecting %s: %v", ids[0], err)
	}

	var views []daemonView

	err = json.Unmarshal(raw, &views)
	if err != nil {
		t.Fatalf("decoding docker inspect: %v", err)
	}

	if len(views) != 1 {
		t.Fatalf("docker inspect returned %d entries, want 1", len(views))
	}

	return views[0]
}

// TestContractSessionContainerConfiguration is the whole flag block, asked of
// the daemon at once.
//
// One container rather than a test per knob: each assertion costs nothing once
// it is started, and the thing worth pinning is that EVERY setting a pipeline
// can write survives the trip — a port that dropped one would otherwise be
// caught only by whichever knob happened to have its own test.
func TestContractSessionContainerConfiguration(t *testing.T) {
	requireDocker(t)

	dir := mountableTempDir(t)

	t.Setenv("STEPS_TEST_CONTRACT_SET", "visible")
	_ = os.Unsetenv("STEPS_TEST_CONTRACT_UNSET")

	runner, err := NewRunner(RunnerSpec{
		Image:       testImage,
		Cwd:         dir,
		Env:         []string{"STEPS_TEST_CONTRACT_SET", "STEPS_TEST_CONTRACT_UNSET"},
		EnvValues:   map[string]string{"STEPS_TEST_CONTRACT_SUPPLIED": "from-the-caller"},
		User:        "1234:5678",
		Network:     "none",
		Privileged:  true,
		CPUShares:   512,
		MemoryBytes: 64 << 20,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	// Starts the container; its own result is not what is under test.
	_, _, _, err = runner.RunCaptureFull(context.Background(), "true")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	view := inspectOurContainer(t)

	resolved, err := ResolveMountPath(dir)
	if err != nil {
		t.Fatalf("ResolveMountPath: %v", err)
	}

	assertContainerLimits(t, view)
	assertContainerLifecycle(t, view)
	assertContainerPlacement(t, view, resolved)
	assertContainerEnv(t, view)
}

// assertContainerLimits checks the knobs a pipeline sets to bound or place a
// container.
func assertContainerLimits(t *testing.T, view daemonView) {
	t.Helper()

	if view.Config.User != "1234:5678" {
		t.Errorf("User = %q, want the configured user", view.Config.User)
	}

	if view.HostConfig.NetworkMode != "none" {
		t.Errorf("NetworkMode = %q, want none", view.HostConfig.NetworkMode)
	}

	if !view.HostConfig.Privileged {
		t.Error("Privileged = false, want the container privileged")
	}

	if view.HostConfig.CPUShares != 512 {
		t.Errorf("CpuShares = %d, want 512", view.HostConfig.CPUShares)
	}

	if view.HostConfig.Memory != 64<<20 {
		t.Errorf("Memory = %d, want %d", view.HostConfig.Memory, 64<<20)
	}
}

// assertContainerLifecycle checks the two settings that decide what the
// container does on its own — reap its orphans, and survive its own death long
// enough to be diagnosed.
func assertContainerLifecycle(t *testing.T, view daemonView) {
	t.Helper()

	// --init supplies a real PID 1, without which an exec's orphans have
	// nothing to reap them for the container's whole life.
	if view.HostConfig.Init == nil || !*view.HostConfig.Init {
		t.Errorf("Init = %v, want the container to have a real PID 1", view.HostConfig.Init)
	}

	// Not self-removing, deliberately: a container that removes itself takes
	// the postmortem checkAlive reads with it.
	if view.HostConfig.AutoRemove {
		t.Error("AutoRemove = true, want the session container to outlive its own death so it can be diagnosed")
	}
}

// assertContainerPlacement checks where the container's work happens and whose
// it is.
func assertContainerPlacement(t *testing.T, view daemonView, resolved string) {
	t.Helper()

	wantBind := resolved + ":" + resolved
	if len(view.HostConfig.Binds) != 1 || view.HostConfig.Binds[0] != wantBind {
		t.Errorf("Binds = %v, want exactly [%s]", view.HostConfig.Binds, wantBind)
	}

	if view.Config.WorkingDir != resolved {
		t.Errorf("WorkingDir = %q, want %q", view.Config.WorkingDir, resolved)
	}

	if view.Config.Labels[dockerOwnerLabel] != "steps" {
		t.Errorf("owner label = %q, want steps", view.Config.Labels[dockerOwnerLabel])
	}

	if view.Config.Labels[dockerPIDLabel] != strconv.Itoa(os.Getpid()) {
		t.Errorf("pid label = %q, want this process's pid", view.Config.Labels[dockerPIDLabel])
	}

	if view.Config.Labels[dockerHostLabel] != ownerHostname() {
		t.Errorf("host label = %q, want this machine's hostname", view.Config.Labels[dockerHostLabel])
	}
}

// assertContainerEnv checks which variables reached the container, and — the
// half that matters more — which did not.
func assertContainerEnv(t *testing.T, view daemonView) {
	t.Helper()

	// A bare name with no '=' is docker's spelling of "forward this from the
	// client, which did not have it" — the DAEMON drops such an entry when it
	// builds the process environment, so it is recorded here as forwarded but
	// valueless rather than as a variable the container will see.
	got := map[string]string{}
	valueless := map[string]bool{}

	for _, entry := range view.Config.Env {
		name, value, hasValue := strings.Cut(entry, "=")
		if !hasValue {
			valueless[name] = true

			continue
		}

		got[name] = value
	}

	if got["STEPS_TEST_CONTRACT_SET"] != "visible" {
		t.Errorf("STEPS_TEST_CONTRACT_SET = %q, want the value from this process's environment", got["STEPS_TEST_CONTRACT_SET"])
	}

	if got["STEPS_TEST_CONTRACT_SUPPLIED"] != "from-the-caller" {
		t.Errorf("STEPS_TEST_CONTRACT_SUPPLIED = %q, want the caller-supplied value", got["STEPS_TEST_CONTRACT_SUPPLIED"])
	}

	// A named variable this process does not have never acquires a value.
	// Whether the daemon records the bare name is an implementation detail of
	// how it was asked; carrying a value would not be, and neither would
	// inventing an empty one — see the sibling test for what the container
	// actually sees.
	if value, present := got["STEPS_TEST_CONTRACT_UNSET"]; present {
		t.Errorf("STEPS_TEST_CONTRACT_UNSET = %q, want a variable this process does not have to carry no value", value)
	}

	// The env: allowlist is the whole boundary: nothing the pipeline did not
	// name gets in, however ordinary the variable looks.
	if _, present := got["HOME"]; present {
		t.Error("HOME reached the container; only the pipeline's env: names should")
	}

	if valueless["HOME"] {
		t.Error("HOME was forwarded to the container; only the pipeline's env: names should be")
	}
}

// TestContractEnvValueIsReadableInsideTheContainer is the same question asked
// from inside, because the daemon's record and the process's environment are
// not the same claim.
func TestContractEnvValueIsReadableInsideTheContainer(t *testing.T) {
	requireDocker(t)

	t.Setenv("STEPS_TEST_CONTRACT_READ", "read-me")
	_ = os.Unsetenv("STEPS_TEST_CONTRACT_ABSENT")

	runner, err := NewRunner(RunnerSpec{
		Image:     testImage,
		Cwd:       mountableTempDir(t),
		Env:       []string{"STEPS_TEST_CONTRACT_READ", "STEPS_TEST_CONTRACT_ABSENT"},
		EnvValues: map[string]string{"STEPS_TEST_CONTRACT_WORKER": "worker-1"},
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	stdout, _, _, err := runner.RunCaptureFull(context.Background(),
		`echo "[$STEPS_TEST_CONTRACT_READ][$STEPS_TEST_CONTRACT_WORKER]"`)
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	if strings.TrimSpace(stdout) != "[read-me][worker-1]" {
		t.Errorf("stdout = %q, want both variables readable by a command in the container", stdout)
	}

	// The name this process does not have must not exist in the container at
	// all. Set-but-empty is a different answer to a script that tests for the
	// variable's presence, and naming an optional variable is exactly what
	// env: is for.
	absent, _, _, err := runner.RunCaptureFull(context.Background(),
		`if [ -z "${STEPS_TEST_CONTRACT_ABSENT+set}" ]; then echo absent; else echo present; fi`)
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	if strings.TrimSpace(absent) != "absent" {
		t.Errorf("an env: name this process does not have was %s in the container, want absent", strings.TrimSpace(absent))
	}
}

// TestContractBadImageSurfacesAsData pins the classification a bad image gets,
// against a daemon rather than a fake.
//
// This is load-bearing twice over. An agent must see it as ordinary
// tool-result data rather than a crashed step, and a task must classify it as
// a FAILURE rather than an infrastructure error — both of which follow from
// "RunCaptureFull returns an exit code and no Go error" and "the underlying
// error satisfies IsExitError". The existing tests for it drive a fake docker
// on PATH, so they pin the shape of a script's exit status, not the daemon's.
func TestContractBadImageSurfacesAsData(t *testing.T) {
	requireDocker(t)

	runner, err := NewRunner(RunnerSpec{
		Image: "steps-test-no-such-image:definitely-not-here",
		Cwd:   mountableTempDir(t),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	stdout, stderr, exitCode, err := runner.RunCaptureFull(context.Background(), "true")
	if err != nil {
		t.Fatalf("RunCaptureFull returned a Go error for a bad image: %v", err)
	}

	if exitCode == 0 {
		t.Errorf("exitCode = 0, want a nonzero status for an image that does not exist")
	}

	if stdout != "" {
		t.Errorf("stdout = %q, want nothing from a container that never started", stdout)
	}

	if stderr == "" {
		t.Error("stderr was empty; the daemon's explanation is the only thing that says WHY the step failed")
	}
}

// TestContractBadImageIsAnExitError is the other half: the same failure,
// reached through Run, has to classify as the step's own verdict.
func TestContractBadImageIsAnExitError(t *testing.T) {
	requireDocker(t)

	runner, err := NewRunner(RunnerSpec{
		Image: "steps-test-no-such-image:definitely-not-here",
		Cwd:   mountableTempDir(t),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	runErr := runner.Run(context.Background(), "true")
	if runErr == nil {
		t.Fatal("Run succeeded against an image that does not exist")
	}

	if !IsExitError(runErr) {
		t.Errorf("IsExitError(%v) = false, want a bad image to read as the step failing rather than the machinery breaking", runErr)
	}
}

// TestContractStartFailureIsSticky pins that a bad image is diagnosed once.
//
// An agent conversation issues dozens of commands through one runner, and a
// session that re-attempted the start each time would ask the daemon for the
// same missing image dozens of times — which is a re-PULL against a registry
// in the case that matters.
func TestContractStartFailureIsSticky(t *testing.T) {
	requireDocker(t)

	runner, err := NewRunner(RunnerSpec{
		Image: "steps-test-no-such-image:definitely-not-here",
		Cwd:   mountableTempDir(t),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	first := time.Now()
	_, _, _, _ = runner.RunCaptureFull(context.Background(), "true")
	firstElapsed := time.Since(first)

	second := time.Now()

	_, stderr, exitCode, err := runner.RunCaptureFull(context.Background(), "true")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	secondElapsed := time.Since(second)

	if exitCode == 0 || stderr == "" {
		t.Errorf("the second command reported exit %d / stderr %q, want the first failure repeated verbatim", exitCode, stderr)
	}

	// The remembered failure is returned without touching the daemon, so it
	// cannot plausibly take as long as asking did.
	if secondElapsed > firstElapsed {
		t.Errorf("second attempt took %s against the first's %s, want the failure remembered rather than re-asked", secondElapsed, firstElapsed)
	}
}

// TestContractSweepRemovesOnlyADeadOwnersContainer pins the sweep against a
// real daemon, in both directions.
//
// The existing coverage checks the argv of a `docker ps` and the parsing of
// its output; neither can catch a filter that the daemon reads differently
// from how it was meant. Both directions are asserted because a sweep that
// removes everything passes a one-sided test and destroys a concurrent run.
func TestContractSweepRemovesOnlyADeadOwnersContainer(t *testing.T) {
	requireDocker(t)

	// A pid that cannot be running: the sweep's whole rule is "the owning
	// process is gone". Chosen far above any plausible live pid rather than
	// picked from a table, so it cannot collide with this test's own process.
	const deadPID = 4194303

	orphan := startLabelledContainer(t, strconv.Itoa(deadPID))
	live := startLabelledContainer(t, strconv.Itoa(os.Getpid()))

	SweepOrphanedContainers(context.Background(), "")

	if containerExists(t, orphan) {
		t.Error("a container whose owning process is gone survived the sweep")
	}

	if !containerExists(t, live) {
		t.Error("a container owned by a LIVE process was swept — a concurrent run's step just lost its container")
	}
}

// startLabelledContainer runs a container wearing this tool's ownership
// labels and the given owner pid, returning its name.
func startLabelledContainer(t *testing.T, pid string) string {
	t.Helper()

	name, err := NewContainerName()
	if err != nil {
		t.Fatalf("NewContainerName: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	//nolint:gosec // every value is generated by this test
	cmd := exec.CommandContext(ctx, "docker", "run", "-d", "--name", name,
		"--label", dockerOwnerLabel+"=steps",
		"--label", dockerPIDLabel+"="+pid,
		"--label", dockerHostLabel+"="+ownerHostname(),
		"--", testImage, "sh", "-c", "sleep 120")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("starting a labelled container: %v\n%s", err, out)
	}

	t.Cleanup(func() { RemoveContainer(context.Background(), name) })

	return name
}

// containerExists reports whether the daemon still has a container by name.
func containerExists(t *testing.T, name string) bool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	//nolint:gosec // name was generated by this test
	err := exec.CommandContext(ctx, "docker", "inspect", "--type", "container", name).Run()

	var exitErr *exec.ExitError

	if errors.As(err, &exitErr) {
		return false
	}

	if err != nil {
		t.Fatalf("asking whether %s exists: %v", name, err)
	}

	return true
}

// TestContractPrepareImagesSkipsAPresentImage pins that a warm daemon costs no
// network.
//
// The check is what keeps a locally-BUILT image working — one that exists in
// no registry is found, so nothing is pulled — and it is the difference
// between a warm run adding milliseconds and a warm run hitting a registry
// once per image per job.
func TestContractPrepareImagesSkipsAPresentImage(t *testing.T) {
	requireDocker(t)

	requireImagePresent(t, testImage)

	announced := captureStdout(t, func() {
		err := PrepareImages(context.Background(), []string{testImage})
		if err != nil {
			t.Errorf("PrepareImages: %v", err)
		}
	})

	// The announcement, not the clock. Elapsed time cannot tell "checked and
	// skipped" from "pulled again" once the layers are cached — a re-pull of a
	// warm image returns in milliseconds, so a timing bound passes for the
	// behaviour it was written to catch.
	if strings.Contains(announced, "pulling image") {
		t.Errorf("PrepareImages announced %q for an image already on the daemon, want it skipped", strings.TrimSpace(announced))
	}
}

// TestContractPrepareImagesReportsAMissingImage pins that a name no registry
// can answer for fails startup rather than the first step that needs it.
func TestContractPrepareImagesReportsAMissingImage(t *testing.T) {
	requireDocker(t)

	err := PrepareImages(context.Background(), []string{"steps-test-no-such-image:definitely-not-here"})
	if err == nil {
		t.Fatal("PrepareImages succeeded for an image that cannot be pulled")
	}

	if !strings.Contains(err.Error(), "steps-test-no-such-image") {
		t.Errorf("error = %v, want it to name the image that could not be pulled", err)
	}
}

// requireImagePresent skips unless the daemon already has image, so a test
// about not pulling never becomes a test that pulls.
func requireImagePresent(t *testing.T, image string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	//nolint:gosec // image is a constant in this package
	err := exec.CommandContext(ctx, "docker", "image", "inspect", "--", image).Run()
	if err != nil {
		t.Skipf("%s is not on the daemon; this test is about NOT pulling", image)
	}
}
