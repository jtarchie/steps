package web

import (
	"context"
	"strings"
	"testing"
)

// TestTheJobsBoardSaysWhatIsQueuedAndWhy: a queued run was only visible in
// the queue table below the board, and nothing on the page said why it had
// not started — the answer lived on the follow page, a click away per job.
func TestTheJobsBoardSaysWhatIsQueuedAndWhy(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-1", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = pipeline.Store.FinishRun(ctx, "run-1", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	err = pipeline.Store.Pause(ctx)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	err = pipeline.Store.EnqueueJob(ctx, "build", "new version")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	_, page := get(t, server, "/p/demo")

	svg := dagSVG(t, page)

	wantAll(t, "build's graph node", dagNodeMarkup(t, svg, "job:build"),
		"○ queued",
		"<title>latest run took ",
		"a run is queued, waiting: the pipeline is paused</title>")

	if deploy := dagNodeMarkup(t, svg, "job:deploy"); strings.Contains(deploy, "queued") {
		t.Errorf("deploy has nothing queued but is marked queued:\n%s", deploy)
	}

	wantAll(t, "the queue table", between(t, page, `id="queue-region"`, "</table>"),
		"<th>Waiting on</th>", "the pipeline is paused")
}

func wantAll(t *testing.T, what, markup string, wants ...string) {
	t.Helper()

	for _, want := range wants {
		if !strings.Contains(markup, want) {
			t.Errorf("%s lacks %q:\n%s", what, want, markup)
		}
	}
}

// TestTheQueueTableBlamesMaxRuns: the daemon-wide limit is the one reason no
// pipeline's own state can show, so it has to reach the board through the
// server rather than the store.
func TestTheQueueTableBlamesMaxRuns(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)

	server, err := New([]*Pipeline{pipeline}, stubRunner{}, WithCapacity(fullCapacity{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = pipeline.Store.EnqueueJob(context.Background(), "build", "new version")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	_, page := get(t, server, "/p/demo")

	queue := between(t, page, `id="queue-region"`, "</table>")
	if !strings.Contains(queue, "the server&#39;s --max-runs (1) is full") {
		t.Errorf("the queue table does not blame --max-runs:\n%s", queue)
	}
}

type fullCapacity struct{}

func (fullCapacity) RunCapacity() (int, int) { return 1, 1 }
