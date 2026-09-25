package web

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// Every action lives in one bar under the nav, in the same place on every page: the pipeline's pause on the left, what this page's job or run can do on the right. They were spread across a banner, the page head and a sentence beside each button, which read as clutter and hid which button acted on what.

// actionBar is the bar's markup, so an assertion about an action cannot pass on a button elsewhere on the page.
func actionBar(t *testing.T, page string) string {
	t.Helper()

	bar := regexp.MustCompile(`(?s)<div class="actionbar[^"]*"[^>]*>.*?</div></div>`).FindString(page)
	if bar == "" {
		t.Fatalf("page has no action bar:\n%s", page)
	}

	return bar
}

func TestTheBarOffersPauseOnEveryPipelinePage(t *testing.T) {
	t.Parallel()

	server := writableServer(t)

	for _, target := range []string{"/p/demo", "/p/demo/runs", "/p/demo/resources", "/p/demo/jobs/build/detail"} {
		_, page := get(t, server, target)
		bar := actionBar(t, page)

		if !strings.Contains(bar, `action="/p/demo/pause"`) || !strings.Contains(bar, `aria-pressed="false">⏸ Pause</button>`) {
			t.Errorf("%s: the bar does not offer Pause", target)
		}
	}
}

// TestAPausedPipelineIsSaidByTheBarAlone: Concourse turns its top bar blue and says nothing more; the banner, the disabled button and the sentence explaining it were three signals for one fact.
func TestAPausedPipelineIsSaidByTheBarAlone(t *testing.T) {
	t.Parallel()

	server, pipeline := writableServerWithPipeline(t)

	err := pipeline.Store.Pause(t.Context())
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	_, page := get(t, server, "/p/demo/jobs/build/detail")
	bar := actionBar(t, page)

	for _, want := range []string{
		`class="actionbar paused"`,
		`action="/p/demo/unpause"`,
		`aria-pressed="true">▶ Unpause</button>`,
		`disabled title="the pipeline is paused">↻ Trigger</button>`,
	} {
		if !strings.Contains(bar, want) {
			t.Errorf("paused bar is missing %q:\n%s", want, bar)
		}
	}

	if strings.Contains(page, "no polling, no new runs") || strings.Contains(page, "unpause it to trigger") {
		t.Error("the page still explains the pause in prose outside the bar")
	}
}

// TestTheJobPagesBarTriggersTheJob: the tooltip carries what the button does, not a sentence beside it.
func TestTheJobPagesBarTriggersTheJob(t *testing.T) {
	t.Parallel()

	server := writableServer(t)
	_, page := get(t, server, "/p/demo/jobs/build/detail")
	bar := actionBar(t, page)

	for _, want := range []string{
		`action="/p/demo/jobs/build/trigger"`,
		`title="a new run of build, with the newest versions">↻ Trigger</button>`,
	} {
		if !strings.Contains(bar, want) {
			t.Errorf("job page bar is missing %q:\n%s", want, bar)
		}
	}

	if strings.Contains(page, "Re-run") || strings.Contains(bar, "no cache") {
		t.Error("the job page says re-run, which is Retry's word, or still offers a trigger without the cache")
	}
}

func TestAHeldJobsBarOffersRelease(t *testing.T) {
	t.Parallel()

	server, pipeline := writableServerWithPipeline(t)

	_, _, err := pipeline.Store.RecordJobOutcome(t.Context(), "build", false, 1)
	if err != nil {
		t.Fatalf("RecordJobOutcome: %v", err)
	}

	_, page := get(t, server, "/p/demo/jobs/build/detail")
	bar := actionBar(t, page)

	if !strings.Contains(bar, `action="/p/demo/jobs/build/release"`) || !strings.Contains(bar, `>Release</button>`) {
		t.Errorf("held job's bar does not offer Release:\n%s", bar)
	}

	if !strings.Contains(page, `<span class="st st-held">held</span>`) {
		t.Error("held job page does not say it is held")
	}

	if code := post(t, server, "/p/demo/jobs/build/release", nil); code != http.StatusSeeOther {
		t.Fatalf("POST release = %d, want 303", code)
	}

	held, err := pipeline.Store.IsJobPaused(t.Context(), "build")
	if err != nil || held {
		t.Errorf("after Release the job is held = %v (%v), want released", held, err)
	}
}

// TestARunsBarOffersTriggerRetryOrAbort: Retry is fly rerun-build — this build, its own inputs; Trigger is a new run of the job; while the run is live Abort takes Retry's place, as on Concourse's build page.
func TestARunsBarOffersTriggerRetryOrAbort(t *testing.T) {
	t.Parallel()

	server, pipeline := writableServerWithPipeline(t)

	err := pipeline.Store.StartRun(t.Context(), "r1", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	_, page := get(t, server, "/p/demo/runs/r1")
	live := actionBar(t, page)

	if !strings.Contains(live, `action="/p/demo/runs/r1/abort"`) || strings.Contains(live, "Retry") {
		t.Errorf("a live run's bar should offer Abort and no Retry:\n%s", live)
	}

	err = pipeline.Store.FinishRun(t.Context(), "r1", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, page = get(t, server, "/p/demo/runs/r1")
	done := actionBar(t, page)

	trigger := strings.Index(done, "↻ Trigger</button>")
	retry := strings.Index(done, `title="run this build again, with the same inputs">⟲ Retry</button>`)

	if trigger < 0 || retry < 0 || trigger > retry || !strings.Contains(done, `action="/p/demo/runs/r1/rerun"`) {
		t.Errorf("a finished run's bar should offer Trigger, then Retry of this run:\n%s", done)
	}

	if strings.Contains(done, "Abort") {
		t.Error("a finished run's bar still offers Abort")
	}
}

func TestTheFollowPagesBarAbortsTheQueuedRun(t *testing.T) {
	t.Parallel()

	server := writableServer(t)
	_, page := get(t, server, "/p/demo/jobs/build/follow?since=0")

	if bar := actionBar(t, page); !strings.Contains(bar, `action="/p/demo/jobs/build/queued/abort"`) {
		t.Errorf("follow page bar does not abort the queued run:\n%s", bar)
	}
}

// TestAReadOnlyBarOffersNothing: nothing can be done here, so there is nothing to press; the state is still said.
func TestAReadOnlyBarOffersNothing(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	err := pipeline.Store.Pause(t.Context())
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	_, page := get(t, server, "/p/demo/jobs/build/detail")
	bar := actionBar(t, page)

	if strings.Contains(bar, "<form") {
		t.Errorf("a read-only bar offers an action:\n%s", bar)
	}

	if !strings.Contains(bar, `<span class="st st-paused">paused</span>`) {
		t.Error("a read-only bar does not say the pipeline is paused")
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
