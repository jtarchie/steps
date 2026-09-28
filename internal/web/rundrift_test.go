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
		{"config and content", after, "hash-new", 1, []string{"configuration changed", "/p/demo/config/" + before, "1 step changed content"}, []string{"no step's content moved"}},
		{"config only", after, "hash-old", 1, []string{"configuration changed", "no step's content moved"}, []string{`class="note chg"`}},
		{"content only", before, "hash-new", 1, []string{"1 step changed content"}, []string{"configuration changed"}},
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

// A failed step now publishes the hash it failed under, so the diff compares
// rows it used to skip — and each of these is a way that comparison could
// mark or claim something the runs do not say.
func TestDriftComparesOnlyWhatBothRunsCanShow(t *testing.T) {
	t.Parallel()

	before, after := strings.Repeat("1", 40), strings.Repeat("2", 40)

	finished := func(id int64, name, kind, status, hash string) []store.RunEventRow {
		index := 0
		if kind == "hook" {
			index = -1
		}

		return []store.RunEventRow{
			{Type: events.TypeStepStarted, StepIndex: index, StepName: name, StepKind: kind, StepID: id},
			{Type: events.TypeStepFinished, StepIndex: index, StepName: name, StepKind: kind, StepID: id, Status: status, Hash: hash},
		}
	}

	skipped := []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "build", StepKind: "task", StepID: 1},
		{Type: events.TypeStepSkipped, StepIndex: 0, StepName: "build", StepKind: "task", StepID: 1, Status: "skipped", Text: "when: guard was false"},
	}

	cases := []struct {
		name   string
		redSHA string
		green  []store.RunEventRow
		red    []store.RunEventRow
		want   []string
		absent []string
	}{
		{
			name:   "the passing run's row was a when: skip",
			redSHA: after,
			green:  skipped,
			red:    finished(1, "build", "task", "failed", "hash-now"),
			want:   []string{"configuration changed"},
			absent: []string{`class="note chg"`, "no step's content moved"},
		},
		{
			name:   "the failed row carries no hash",
			redSHA: after,
			green:  finished(1, "build", "task", "succeeded", "hash-old"),
			red:    finished(1, "build", "task", "failed", ""),
			want:   []string{"configuration changed"},
			absent: []string{`class="note chg"`, "no step's content moved"},
		},
		{
			name:   "members sharing a name",
			redSHA: before,
			green:  append(finished(1, "reviewer", "agent", "succeeded", "hash-a"), finished(2, "reviewer", "agent", "succeeded", "hash-b")...),
			red:    append(finished(1, "reviewer", "agent", "failed", "hash-a"), finished(2, "reviewer", "agent", "succeeded", "hash-b")...),
			absent: []string{`class="note chg"`, "changed content"},
		},
		{
			name:   "a hook only a red run has",
			redSHA: before,
			green:  finished(1, "build", "task", "succeeded", "hash-old"),
			red:    append(finished(1, "build", "task", "failed", "hash-old"), finished(2, "on_failure · task explain", "hook", "succeeded", "")...),
			absent: []string{`class="note chg"`, "changed content"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server, pipeline := testPipeline(t)
			seedRuns(t, pipeline, before, tc.redSHA, tc.green, tc.red, before, after)

			_, body := get(t, server, "/p/demo/runs/red")

			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("the page lacks %q:\n%s", want, body)
				}
			}

			for _, absent := range tc.absent {
				if strings.Contains(body, absent) {
					t.Errorf("the page carries %q:\n%s", absent, body)
				}
			}
		})
	}
}

// seedRuns records a green run and then a red one of job build, each with the
// events given, under the given revisions.
func seedRuns(t *testing.T, pipeline *Pipeline, greenSHA, redSHA string, green, red []store.RunEventRow, revisions ...string) {
	t.Helper()

	ctx := context.Background()

	for _, revision := range revisions {
		err := pipeline.Store.RecordRevision(ctx, revision, "jobs: []", nil)
		if err != nil {
			t.Fatalf("RecordRevision: %v", err)
		}
	}

	for _, run := range []struct {
		id, sha, status string
		rows            []store.RunEventRow
	}{
		{"green", greenSHA, "succeeded", green},
		{"red", redSHA, "failed", red},
	} {
		err := pipeline.Store.StartRun(ctx, run.id, "build", "", run.sha)
		if err != nil {
			t.Fatalf("StartRun %s: %v", run.id, err)
		}

		appendEvents(t, pipeline.Store, run.id, run.rows)

		err = pipeline.Store.FinishRun(ctx, run.id, run.status)
		if err != nil {
			t.Fatalf("FinishRun %s: %v", run.id, err)
		}
	}
}

// nodes is last-write-wins per hash, so a failed row whose content a green
// run shares finds THAT run's result under its hash — an answer drawn on the
// step that failed to give one.
func TestAFailedRowDrawsNoNodeResult(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	seedRuns(t, pipeline, "", "", nil, finished1("reviewer", "failed", "hash-shared"))
	mustRecordResult(t, pipeline, "hash-shared", map[string]any{"response": "an answer from a green run"})

	_, body := get(t, server, "/p/demo/runs/red")

	if !strings.Contains(body, `/nodes/hash-shared`) {
		t.Fatalf("the failed row does not link its node; this test guards nothing:\n%s", body)
	}

	if strings.Contains(body, "an answer from a green run") {
		t.Errorf("the failed row shows another run's result:\n%s", body)
	}
}

func finished1(name, status, hash string) []store.RunEventRow {
	return []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: name, StepKind: "agent", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: name, StepKind: "agent", StepID: 1, Status: status, Hash: hash},
	}
}

// Failed rows link their node now, and "every run listed reused it" is a
// cache hit's explanation, not a failure's.
func TestNodePageExplainsSkipsOnlyForASuccess(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	for hash, status := range map[string]string{"hash-green": "succeeded", "hash-red": "failed"} {
		err := pipeline.Store.RecordNode(t.Context(), store.NodeRecord{Hash: hash, Kind: "task", Resource: "build"}, "build", status, nil, nil)
		if err != nil {
			t.Fatalf("RecordNode: %v", err)
		}
	}

	if _, body := get(t, server, "/p/demo/nodes/hash-green"); !strings.Contains(body, "Why was this skipped?") {
		t.Errorf("a succeeded node lost its cache receipt:\n%s", body)
	}

	if _, body := get(t, server, "/p/demo/nodes/hash-red"); strings.Contains(body, "Why was this skipped?") {
		t.Errorf("a failed node is explained as a cache hit:\n%s", body)
	}
}
