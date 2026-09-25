package web

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// stripOf cuts the run strip out of a run page, so an assertion about a chip
// cannot be satisfied by the same link appearing in the crumbs or the body.
func stripOf(t *testing.T, body string) string {
	t.Helper()

	open := strings.Index(body, `id="run-strip"`)
	if open < 0 {
		t.Fatalf("run page has no #run-strip: %s", body)
	}

	shut := strings.Index(body[open:], "</nav>")
	if shut < 0 {
		t.Fatal("#run-strip never closes")
	}

	return body[open : open+shut]
}

// TestRunPageStripListsTheJobsRuns: the strip is this JOB's recent runs,
// newest first, with the run on the page marked — so hopping between runs of
// one job never leaves the transcript, and the detail page is one link away.
func TestRunPageStripListsTheJobsRuns(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	startFinishedRun(t, pipeline, "run-a", "build", "succeeded")
	startFinishedRun(t, pipeline, "run-b", "build", "failed")
	startFinishedRun(t, pipeline, "run-c", "build", "succeeded")
	startFinishedRun(t, pipeline, "run-d", "deploy", "succeeded")

	_, body := get(t, server, "/p/demo/runs/run-b")
	strip := stripOf(t, body)

	a, b, c := strings.Index(strip, `href="/p/demo/runs/run-a"`), strings.Index(strip, `href="/p/demo/runs/run-b"`), strings.Index(strip, `href="/p/demo/runs/run-c"`)
	if a < 0 || b < 0 || c < 0 {
		t.Fatalf("strip does not link every run of the job: %s", strip)
	}

	if c > b || b > a {
		t.Errorf("strip is not newest first: c=%d b=%d a=%d", c, b, a)
	}

	if strings.Contains(strip, "run-d") {
		t.Error("strip carries another job's run")
	}

	for want, cost := range map[string]string{
		`href="/p/demo/runs/run-b" aria-current="page"`: "the run on the page is not marked current",
		`class="st st-failed"`:                          "a failed run's chip does not carry the status vocabulary",
		`title="[run-c] · `:                             "a chip's title does not carry the id a hover needs",
		`href="/p/demo/jobs/build/detail"`:              "strip does not lead to the job's detail page",
		`hx-select="#run-strip"`:                        "strip is not a live region of its own",
	} {
		if !strings.Contains(strip, want) {
			t.Errorf("%s: missing %s", cost, want)
		}
	}
}

// TestRunPageStripShowsAQueuedTriggerAsItsFollowLink: a trigger that has not
// become a run yet is the newest thing about the job, and the follow page is
// what turns into that run — so the chip carries the queue row's own since.
func TestRunPageStripShowsAQueuedTriggerAsItsFollowLink(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	startFinishedRun(t, pipeline, "run-1", "build", "succeeded")

	ctx := context.Background()

	err := pipeline.Store.EnqueueJob(ctx, "build", "test")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	queue, err := pipeline.Store.ListTriggerQueue(ctx, 1)
	if err != nil || len(queue) != 1 {
		t.Fatalf("ListTriggerQueue: %v (%d rows)", err, len(queue))
	}

	_, body := get(t, server, "/p/demo/runs/run-1")
	strip := stripOf(t, body)

	want := `href="/p/demo/jobs/build/follow?since=` + strconv.FormatInt(queue[0].EnqueuedAt.UnixMilli(), 10) + `">queued</a>`
	if !strings.Contains(strip, want) {
		t.Errorf("strip has no queued chip %s: %s", want, strip)
	}

	queued, current := strings.Index(strip, "queued</a>"), strings.Index(strip, `href="/p/demo/runs/run-1"`)
	if queued > current {
		t.Error("the queued chip is not the newest thing on the strip")
	}
}

// TestRunPageStripDropsTheQueuedChipOnceItsRunStarts: a claimed queue row
// stays "running" for the whole build, so reading the queue alone would show
// a queued chip beside the run it became. The chip means "no run yet", and
// only a run started since the row was enqueued can say otherwise.
func TestRunPageStripDropsTheQueuedChipOnceItsRunStarts(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	startFinishedRun(t, pipeline, "run-1", "build", "succeeded")

	_, _, ok := enqueueAndClaim(t, pipeline.Store, "build")
	if !ok {
		t.Fatal("the queued build was not claimed")
	}

	_, body := get(t, server, "/p/demo/runs/run-1")
	if !strings.Contains(stripOf(t, body), "queued</a>") {
		t.Fatal("a claimed row with no run yet is not shown as queued")
	}

	err := pipeline.Store.StartRun(context.Background(), "run-2", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	_, body = get(t, server, "/p/demo/runs/run-1")
	strip := stripOf(t, body)

	if strings.Contains(strip, "queued</a>") {
		t.Errorf("the queued chip outlived the run it became: %s", strip)
	}

	if !strings.Contains(strip, `href="/p/demo/runs/run-2"`) {
		t.Error("the run the row became is not on the strip")
	}
}
