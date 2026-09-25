package web

import (
	"net/http"
	"strings"
	"testing"
)

// The trigger buttons said Trigger, Re-run (forced) and Trigger new run for one route, and what each did lived in a tooltip. One verb now, and what it does is on the page (.design/run-actions-and-states).

// TestJobPageSaysWhatEachTriggerDoes: both buttons carry their consequence as visible text, and nothing says re-run — that word is reserved for re-running a run with its own inputs (#146).
func TestJobPageSaysWhatEachTriggerDoes(t *testing.T) {
	t.Parallel()

	server := writableServer(t)
	_, page := get(t, server, "/p/demo/jobs/build/detail")

	for _, want := range []string{
		`>▶ Trigger new run</button>`,
		"builds the newest versions it has not built; unchanged steps are skipped",
		`>▶ Trigger new run without cache</button>`,
		"runs every step even if unchanged; still only versions not yet built",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("job page is missing %q", want)
		}
	}

	if strings.Contains(page, "Re-run") || strings.Contains(page, `title="Re-runs`) {
		t.Error("job page still says re-run, or explains a button only in a tooltip")
	}

	if strings.Contains(page, "disabled") {
		t.Error("a trigger is disabled on a pipeline that is not paused")
	}
}

// TestAPausedPipelinesTriggersAreDisabledWithTheReason: the server refuses the POST with a 409, which reached a person as an error page after they pressed a button that looked live.
func TestAPausedPipelinesTriggersAreDisabledWithTheReason(t *testing.T) {
	t.Parallel()

	server, pipeline := writableServerWithPipeline(t)

	err := pipeline.Store.Pause(t.Context())
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	err = pipeline.Store.StartRun(t.Context(), "r1", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	for _, target := range []string{"/p/demo/jobs/build/detail", "/p/demo/runs/r1"} {
		_, page := get(t, server, target)

		if n := strings.Count(page, `disabled aria-describedby="trigger-why"`); n == 0 {
			t.Errorf("%s: no trigger is disabled on a paused pipeline", target)
		}

		if !strings.Contains(page, `id="trigger-why">the pipeline is paused; unpause it to trigger`) {
			t.Errorf("%s: a disabled trigger does not say why", target)
		}
	}
}

// TestAHeldJobsHeadOffersReleaseAndKeepsTrigger: held is a job state, so it is said at the job; a manual trigger still runs it (the breaker holds only automatic triggers), so Trigger stays live and says so.
func TestAHeldJobsHeadOffersReleaseAndKeepsTrigger(t *testing.T) {
	t.Parallel()

	server, pipeline := writableServerWithPipeline(t)

	_, _, err := pipeline.Store.RecordJobOutcome(t.Context(), "build", false, 1)
	if err != nil {
		t.Fatalf("RecordJobOutcome: %v", err)
	}

	_, page := get(t, server, "/p/demo/jobs/build/detail")

	for _, want := range []string{
		`<span class="st st-held">held</span> after 1 failure`,
		"will not trigger on new versions",
		`action="/p/demo/jobs/build/release"`,
		`>Release</button>`,
		"resets the failure count; new versions trigger it again",
		"runs now; held only stops automatic triggers",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("held job page is missing %q", want)
		}
	}

	if strings.Contains(page, "disabled") {
		t.Error("a held job's trigger is disabled, but a manual trigger runs it")
	}

	if code := post(t, server, "/p/demo/jobs/build/release", nil); code != http.StatusSeeOther {
		t.Fatalf("POST release = %d, want 303", code)
	}

	held, err := pipeline.Store.IsJobPaused(t.Context(), "build")
	if err != nil || held {
		t.Errorf("after Release the job is held = %v (%v), want released", held, err)
	}
}

// TestReadOnlyJobPageSuggestsNoLocalCommand: it used to say "Trigger with steps run <file> --job", which runs the job on the reader's own machine rather than this server — and no command queues on a daemon.
func TestReadOnlyJobPageSuggestsNoLocalCommand(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)
	_, page := get(t, server, "/p/demo/jobs/build/detail")

	if !strings.Contains(page, "This server is read-only; runs start only from its triggers.") {
		t.Error("read-only job page does not say how runs start")
	}

	if strings.Contains(page, "steps run ") {
		t.Error("read-only job page still suggests a command that runs the job locally")
	}
}

// TestRunPageTriggerNamesItsJobAndItsVersions: the one button on a failed run read as "run this again", while it builds whatever the job would take now.
func TestRunPageTriggerNamesItsJobAndItsVersions(t *testing.T) {
	t.Parallel()

	server, pipeline := writableServerWithPipeline(t)

	err := pipeline.Store.StartRun(t.Context(), "r1", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	_, page := get(t, server, "/p/demo/runs/r1")

	abort := strings.Index(page, "■ Abort</button>")
	trigger := strings.Index(page, ">▶ Trigger new run of build</button>")

	if abort < 0 || trigger < 0 || abort > trigger {
		t.Errorf("run page actions = abort at %d, trigger at %d: want Abort first, then the trigger naming its job", abort, trigger)
	}

	if !strings.Contains(page, "newest versions, not this run's") {
		t.Error("run page trigger does not say which versions it builds")
	}
}

func writableServer(t *testing.T) *Server {
	t.Helper()

	server, _ := writableServerWithPipeline(t)

	return server
}

func writableServerWithPipeline(t *testing.T) (*Server, *Pipeline) {
	t.Helper()

	_, pipeline := testPipeline(t)

	server, err := New([]*Pipeline{pipeline}, &enqueueRecorder{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return server, pipeline
}

// TestThePauseControlSaysUntilUnpaused: it said "until resumed", and resume is --resume <run>'s word; the button that undoes a pause says Unpause.
func TestThePauseControlSaysUntilUnpaused(t *testing.T) {
	t.Parallel()

	server := writableServer(t)
	_, page := get(t, server, "/p/demo")

	if !strings.Contains(page, "stops polling, admission and triggering until unpaused") {
		t.Error("the pause control does not say what undoes it")
	}
}
