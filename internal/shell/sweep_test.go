package shell

import (
	"os"
	"slices"
	"strconv"
	"testing"
)

// TestDockerStartArgsCarriesOwnershipLabels is what makes an orphan
// recoverable at all: a SIGKILLed run leaves a container and nothing else, so
// everything the next run needs to identify it has to already be on it.
func TestDockerStartArgsCarriesOwnershipLabels(t *testing.T) {
	t.Parallel()

	args := dockerStartArgs("alpine", "steps-abc", "", nil, "", "", false, 0, 0)

	want := map[string]string{
		dockerOwnerLabel: "steps",
		dockerPIDLabel:   strconv.Itoa(os.Getpid()),
		dockerHostLabel:  ownerHostname(),
	}

	for key, value := range want {
		if !slices.Contains(args, key+"="+value) {
			t.Errorf("args = %v, want a --label %s=%s", args, key, value)
		}
	}

	// Before the -- separator, or docker reads them as the container's argv.
	sep := slices.Index(args, "--")
	if last := slices.Index(args[sep:], "--label"); last >= 0 {
		t.Errorf("args = %v, want every --label before the -- separator", args)
	}
}

func TestParseSweepLine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		line    string
		wantID  string
		wantPID int
		wantOK  bool
	}{
		{"ordinary row", "abc123 4242", "abc123", 4242, true},
		{"missing pid label", "abc123", "", 0, false},
		{"empty", "", "", 0, false},
		{"non-numeric pid", "abc123 notapid", "", 0, false},
		{"zero pid", "abc123 0", "", 0, false},
		{"negative pid", "abc123 -1", "", 0, false},
		{"extra fields", "abc123 42 stray", "", 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			id, pid, ok := parseSweepLine(tc.line)
			if ok != tc.wantOK || id != tc.wantID || pid != tc.wantPID {
				t.Errorf("parseSweepLine(%q) = (%q, %d, %v), want (%q, %d, %v)",
					tc.line, id, pid, ok, tc.wantID, tc.wantPID, tc.wantOK)
			}
		})
	}
}

// TestProcessAliveRecognizesThisProcess is the load-bearing half: a false
// negative here sweeps a container out from under a RUNNING step, which is far
// worse than leaving an orphan for one more run.
func TestProcessAliveRecognizesThisProcess(t *testing.T) {
	t.Parallel()

	if !processAlive(os.Getpid()) {
		t.Error("processAlive said this very process is dead")
	}

	if !processAlive(1) {
		t.Error("processAlive said pid 1 is dead; a permission error must count as alive")
	}
}

// TestProcessAliveRejectsAnImpossiblePid uses a pid above the kernel's
// maximum, which cannot be in use and cannot be reused underneath the test.
func TestProcessAliveRejectsAnImpossiblePid(t *testing.T) {
	t.Parallel()

	if processAlive(1 << 30) {
		t.Error("processAlive said an impossible pid is alive")
	}
}

// TestSweepOrphanedContainersToleratesNoDocker pins that the sweep can never
// be the reason a run fails: it is tidying, and tidying is not the job.
func TestSweepOrphanedContainersToleratesNoDocker(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // docker cannot be found

	SweepOrphanedContainers(t.Context(), "")
}

// TestSweepHonoursTheDaemonItIsGiven is the half the venue's seam test relies
// on: a host that is named is a host that is used.
//
// Without it the venue could hand the worker's socket across correctly and the
// sweep would still be tidying this machine — the parameter accepted, ignored,
// and the leak untouched.
func TestSweepHonoursTheDaemonItIsGiven(t *testing.T) {
	requireDocker(t)

	// A daemon that is not there. Honoured, the listing fails and answers
	// nothing; ignored, this reaches the real local daemon and answers about
	// containers it was never asked about.
	orphans := listOrphanedContainers(t.Context(), "unix:///nonexistent/steps-sweep-test.sock")
	if len(orphans) != 0 {
		t.Errorf("a sweep aimed at a daemon that does not exist listed %d containers — it is reading the local one", len(orphans))
	}
}
