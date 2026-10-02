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

	list := between(t, page, `id="jobs-list"`, `id="queue-region"`)

	wantAll(t, "build's row", between(t, list, `href="/p/demo/jobs/build"`, "</tr>"),
		`class="st st-queued" href="/p/demo/jobs/build/follow?since=`,
		`title="waiting: the pipeline is paused"`,
		`>queued<span class="visually-hidden">, waiting: the pipeline is paused</span></a>`)

	deploy := between(t, list, `href="/p/demo/jobs/deploy"`, "</tr>")
	if strings.Contains(deploy, "st-queued") {
		t.Errorf("deploy has nothing queued but is marked queued:\n%s", deploy)
	}

	wantAll(t, "build's graph node", between(t, between(t, page, `id="jobs-graph"`, `id="jobs-list"`), `href="/p/demo/jobs/build"`, "</a>"),
		"○ queued")

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
