package web

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
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

// TestAgoCompactIsOneUnitWithASpokenTwin: the strip's label is one coarse
// unit so twenty runs fit on a line, and "5d" is not something a screen
// reader can be trusted to say — so every short form has a spelled-out twin,
// pluralized, that is what the chip is announced as.
func TestAgoCompactIsOneUnitWithASpokenTwin(t *testing.T) {
	t.Parallel()

	for _, want := range []struct {
		elapsed       time.Duration
		short, spoken string
	}{
		{-time.Second, "now", "just now"},
		{0, "now", "just now"},
		{999 * time.Millisecond, "now", "just now"},
		{time.Second, "1s", "1 second ago"},
		{59 * time.Second, "59s", "59 seconds ago"},
		{time.Minute, "1m", "1 minute ago"},
		{21*time.Minute + 26*time.Second, "21m", "21 minutes ago"},
		{59*time.Minute + 59*time.Second, "59m", "59 minutes ago"},
		{time.Hour, "1h", "1 hour ago"},
		{22*time.Hour + 55*time.Minute, "22h", "22 hours ago"},
		{24 * time.Hour, "1d", "1 day ago"},
		{120*time.Hour + 11*time.Minute, "5d", "5 days ago"},
	} {
		short, spoken := agoCompact(want.elapsed)
		if short != want.short || spoken != want.spoken {
			t.Errorf("agoCompact(%s) = %q, %q; want %q, %q", want.elapsed, short, spoken, want.short, want.spoken)
		}
	}
}

var compactTimeTag = regexp.MustCompile(`<time datetime="[^"]+" data-ago-compact><span aria-hidden="true">(?:now|\d+[smhd])</span><span class="visually-hidden">(?:just now|\d+ (?:second|minute|hour|day)s? ago)</span></time>`)

// TestRunPageStripIsAnnouncedAsWords: a chip's status is a CSS glyph and its
// time an abbreviation, neither of which a screen reader reads reliably. The
// chip therefore carries the status WORD and the spelled-out time as hidden
// text, hides the abbreviation from assistive tech so it is not read twice,
// and sits in a list so "20 items" is announced before the first one.
func TestRunPageStripIsAnnouncedAsWords(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	startFinishedRun(t, pipeline, "run-a", "build", "failed")

	_, body := get(t, server, "/p/demo/runs/run-a")
	strip := stripOf(t, body)

	for want, cost := range map[string]string{
		`<ol class="striplist" id="strip-runs" role="list">`: "runs are not a list a screen reader can count",
		`<span class="visually-hidden">failed, </span>`:      "the status is only a glyph to a screen reader",
	} {
		if !strings.Contains(strip, want) {
			t.Errorf("%s: missing %s in %s", cost, want, strip)
		}
	}

	// Matched by shape rather than as "now": the run started when the test
	// did, and a loaded machine can put a second between that and the render.
	if !compactTimeTag.MatchString(strip) {
		t.Errorf("the time is not a machine-readable instant the ticker can find, an abbreviation hidden from assistive tech, and its spoken twin: %s", strip)
	}

	if strings.Contains(strip, " ago</time>") {
		t.Error("the strip still renders the long relative time it was shortened from")
	}
}
