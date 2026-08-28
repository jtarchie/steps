package shell

import (
	"github.com/jtarchie/steps/internal/dockerapi"
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

// TestOwnerPID covers the label a sweep attributes a container by.
//
// It used to parse a formatted `docker ps` row, so half these cases were
// about the SHAPE of that line — a missing second field, a stray third. The
// labels arrive structured now, so what is left is the only question that was
// ever really being asked: is this a pid worth checking? Anything else is
// skipped rather than swept, because an unattributable container is exactly
// the one not to remove on a guess.
func TestOwnerPID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		label   string
		wantPID int
		wantOK  bool
	}{
		{"an ordinary pid", "4242", 4242, true},
		{"whitespace around it", " 4242 ", 4242, true},
		{"no label at all", "", 0, false},
		{"not a number", "notapid", 0, false},
		{"zero", "0", 0, false},
		{"negative", "-1", 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pid, ok := ownerPID(tc.label)
			if ok != tc.wantOK || pid != tc.wantPID {
				t.Errorf("ownerPID(%q) = (%d, %v), want (%d, %v)", tc.label, pid, ok, tc.wantPID, tc.wantOK)
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
//
// A daemon that does not answer rather than a missing binary, which is what
// "no docker" means now that nothing is spawned.
func TestSweepOrphanedContainersToleratesNoDocker(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")

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
	client, err := dockerapi.New("tcp://127.0.0.1:1")
	if err != nil {
		t.Fatalf("dockerapi.New: %v", err)
	}

	t.Cleanup(func() { _ = client.Close() })

	orphans := listOrphanedContainers(t.Context(), client)
	if len(orphans) != 0 {
		t.Errorf("a sweep aimed at a daemon that does not exist listed %d containers — it is reading the local one", len(orphans))
	}
}
