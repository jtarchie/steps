package web

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// Drift since the last green is ONE line under the strip, whatever moved: two lines saying "the config changed" and "these steps changed" were two answers to the one question a red run opens with, and each cost the transcript a row.
func TestDriftSinceTheLastPassIsOneLine(t *testing.T) {
	t.Parallel()

	before, after := strings.Repeat("1", 40), strings.Repeat("2", 40)

	cases := []struct {
		name      string
		sha       string
		hash      string
		wantLines int
		want      []string
		absent    []string
	}{
		{"config and content", after, "hash-new", 1, []string{"configuration changed", "/p/demo/config/" + before, `<span class="chg">build</span>`, "changed content"}, []string{"no step's content moved"}},
		{"config only", after, "hash-old", 1, []string{"configuration changed", "no step's content moved"}, []string{`class="chg"`}},
		{"content only", before, "hash-new", 1, []string{`<span class="chg">build</span>`, "changed content"}, []string{"configuration changed"}},
		{"neither", before, "hash-old", 0, nil, []string{"configuration changed", "changed content"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server, pipeline := testPipeline(t)
			seedDrift(t, pipeline, before, after, tc.sha, tc.hash)

			_, body := get(t, server, "/p/demo/runs/red")

			if got := strings.Count(body, `class="diffnote"`); got != tc.wantLines {
				t.Errorf("%d drift lines, want %d:\n%s", got, tc.wantLines, body)
			}

			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("the drift line lacks %q:\n%s", want, body)
				}
			}

			for _, absent := range tc.absent {
				if strings.Contains(body, absent) {
					t.Errorf("the drift line carries %q, which nothing here moved", absent)
				}
			}
		})
	}
}

// seedDrift records a green run of one step under the first revision, then a red run of the same step under the given revision and content hash.
func seedDrift(t *testing.T, pipeline *Pipeline, before, after, sha, hash string) {
	t.Helper()

	ctx := context.Background()

	for _, revision := range []string{before, after} {
		err := pipeline.Store.RecordRevision(ctx, revision, "jobs: []", nil)
		if err != nil {
			t.Fatalf("RecordRevision: %v", err)
		}
	}

	for _, run := range []struct{ id, sha, hash, status string }{
		{"green", before, "hash-old", "succeeded"},
		{"red", sha, hash, "failed"},
	} {
		err := pipeline.Store.StartRun(ctx, run.id, "build", "", run.sha)
		if err != nil {
			t.Fatalf("StartRun %s: %v", run.id, err)
		}

		appendEvents(t, pipeline.Store, run.id, []store.RunEventRow{
			{Type: events.TypeStepStarted, StepIndex: 0, StepName: "build", StepKind: "task", StepID: 1},
			{Type: events.TypeStepFinished, StepIndex: 0, StepName: "build", StepKind: "task", StepID: 1, Status: run.status, Hash: run.hash},
		})

		err = pipeline.Store.FinishRun(ctx, run.id, run.status)
		if err != nil {
			t.Fatalf("FinishRun %s: %v", run.id, err)
		}
	}
}

// The keys drive the transcript, so the hint sits on its top edge — not as a row of its own between the head and the first step.
func TestTheKeyHintSitsOnTheTranscript(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	startFinishedRun(t, pipeline, "run-keys", "build", "succeeded")

	_, body := get(t, server, "/p/demo/runs/run-keys")

	head := strings.Index(body, `class="transcripthead"`)
	hint := strings.Index(body, `class="keyhint"`)
	transcript := strings.Index(body, `id="transcript"`)

	if head < 0 || hint < head || transcript < hint {
		t.Fatalf("the hint is not on the transcript's head (head %d, hint %d, transcript %d)", head, hint, transcript)
	}

	if between := body[hint:transcript]; strings.Contains(between, "<p") || strings.Contains(between, "<section") {
		t.Errorf("something sits between the hint and the transcript:\n%s", between)
	}
}
