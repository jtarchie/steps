package storetest

// One state file holds several pipelines, and every row says which one it
// belongs to: what each facet must keep to the pipeline its handle names.

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// TestTheOpenerSharesOneDatabase holds the contract on OpenFunc itself.
//
// Several tests below are about two pipelines in ONE database not seeing each
// other's rows. Every one of them passes for free against an opener that hands
// out a separate database per name — the isolation they assert would be the
// filesystem's rather than the query's. This is the one test that fails there,
// so the rest mean what they say.
func (s suite) TestTheOpenerSharesOneDatabase(t *testing.T) {
	t.Parallel()

	web := s.open(t, "web")
	infra := s.open(t, "infra")

	if web.Pipeline() != "web" || infra.Pipeline() != "infra" {
		t.Fatalf("the handles are scoped to %q and %q, want web and infra", web.Pipeline(), infra.Pipeline())
	}

	if web.Description() != infra.Description() {
		t.Fatalf("the handles are backed by %q and %q, want one database — Description is stable, so two of them means the scoping tests below assert nothing", web.Description(), infra.Description())
	}

	// The description is a promise; this is the storage. Two private
	// databases can answer to one description (`:memory:` does), and the
	// scoping tests would then assert nothing.
	rows, err := web.Reader().Pipelines(context.Background())
	if err != nil {
		t.Fatalf("Pipelines through web: %v", err)
	}

	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}

	if !slices.Contains(names, "infra") {
		t.Fatalf("web's database holds pipelines %q, want infra among them — the descriptions matched but the storage did not", names)
	}
}

// TestCheckErrorsAreScopedToOnePipeline: two pipelines in one state file both
// name a resource `repo`, and the badge on one must not count the other's.
func (s suite) TestCheckErrorsAreScopedToOnePipeline(t *testing.T) {
	t.Parallel()

	web, infra := s.open(t, "web"), s.open(t, "infra")

	err := web.RecordCheckError(t.Context(), "repo", "web is broken")
	if err != nil {
		t.Fatal(err)
	}

	if names := checkErrorNames(t, infra); len(names) != 0 {
		t.Errorf("infra sees web's check errors: %v", names)
	}

	if names := checkErrorNames(t, web); len(names) != 1 {
		t.Errorf("web lost its own check error: %v", names)
	}
}

// TestTheCurrentRevisionIsScopedToItsPipeline: a state file holds several, and one daemon row answering with another's configuration would serve the wrong pipeline entirely.
func (s suite) TestTheCurrentRevisionIsScopedToItsPipeline(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	mine := s.open(t, "test")
	theirs := s.open(t, "other")

	setRevision(t, theirs, "sha-theirs", 1, nil)

	_, found, err := mine.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision: %v", err)
	}

	if found {
		t.Error("one pipeline reports the configuration another pipeline was set with")
	}
}

// TestPauseIsScopedToItsPipeline: a shared state file would otherwise stop every pipeline in it when one was paused.
func (s suite) TestPauseIsScopedToItsPipeline(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	mine := s.open(t, "test")
	theirs := s.open(t, "other")

	err := theirs.Pause(ctx)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	paused, err := mine.Paused(ctx)
	if err != nil {
		t.Fatalf("Paused: %v", err)
	}

	if paused {
		t.Error("pausing one pipeline paused another sharing the state file")
	}
}

// TestAskQuestionRefusesARunFromAnotherPipeline: a run id alone does not
// scope a question. In a shared state file a sibling pipeline's run exists,
// and a row filed against it would be one this handle could never list back.
func (s suite) TestAskQuestionRefusesARunFromAnotherPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	mine := s.open(t, "mine")
	theirs := s.open(t, "theirs")

	mustStartRuns(ctx, t, theirs, "build", "their-run")

	_, _, err := mine.AskQuestion(ctx, store.Question{
		RunID: "their-run", JobName: "build", AgentName: "writer", Question: "Which bump?",
	})
	if err == nil {
		t.Fatal("a question was recorded against another pipeline's run")
	}

	if errors.Is(err, store.ErrQuestionNotPending) {
		t.Fatalf("refused as %v, which is the wrong reason", err)
	}

	pending, err := theirs.Questions(ctx, true, 0)
	if err != nil {
		t.Fatalf("Questions: %v", err)
	}

	if len(pending) != 0 {
		t.Errorf("the owning pipeline lists %d question(s) it never asked", len(pending))
	}
}

// TestMemoriesBelongToTheirPipeline: two pipelines in one database, each with
// a user U1, are two different people.
func (s suite) TestMemoriesBelongToTheirPipeline(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	web := s.open(t, "web")
	infra := s.open(t, "infra")

	theirs := remember(ctx, t, web, "U1", "web's fact", 0)

	if got := memoryTexts(ctx, t, infra, "U1"); len(got) != 0 {
		t.Errorf("infra sees %v, want nothing of web's", got)
	}

	forgot, err := infra.Forget(ctx, "U1", theirs.ID)
	if err != nil || forgot {
		t.Errorf("infra forgetting web's entry: forgot=%v err=%v, want false", forgot, err)
	}

	gone, err := infra.ForgetScope(ctx, "U1")
	if err != nil || gone != 0 {
		t.Errorf("infra forgetting its U1: gone=%d err=%v, want 0", gone, err)
	}

	scopes, err := infra.MemoryScopes(ctx)
	if err != nil || len(scopes) != 0 {
		t.Errorf("infra's scopes = %+v err=%v, want none", scopes, err)
	}

	if got := memoryTexts(ctx, t, web, "U1"); len(got) != 1 {
		t.Errorf("web's U1 = %v, want its entry", got)
	}
}

// TestAMemoryNamesOnlyItsOwnPipelinesRun: a sibling's run id satisfies the
// foreign key, and a memory claiming it would point at a run this pipeline
// cannot show.
func (s suite) TestAMemoryNamesOnlyItsOwnPipelinesRun(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	web := s.open(t, "web")
	infra := s.open(t, "infra")

	mustStartRuns(ctx, t, infra, "build", "infra-run")

	memory, _, err := web.Remember(ctx, store.Memory{Scope: "U1", Text: "likes go", RunID: "infra-run"}, 0)
	if err != nil {
		t.Fatal(err)
	}

	if memory.RunID != "" {
		t.Errorf("RunID = %q, want nothing: the run is another pipeline's", memory.RunID)
	}
}

// TestPlacementsAreScopedToTheirPipeline holds the repo's rule that a query in
// this package without a pipeline_id predicate is a bug. One state file may
// hold several pipelines, and run ids are minted per pipeline, so an unscoped
// read hands one pipeline's report another's machines.
func (s suite) TestPlacementsAreScopedToTheirPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	web := s.open(t, "web")

	infra := s.open(t, "infra")

	// The same run id in both pipelines, which is what makes this a real
	// collision rather than a coincidence: ids are minted per pipeline.
	placeAt(ctx, t, web, store.Placement{
		RunID: "SHARED01", StepName: "web-step", JobName: "build", NodeHash: hashOf(1),
		Tag: "web", Address: "ssh://web", GOOS: "linux", GOARCH: "amd64",
		Workdir: "/tmp/w",
	})
	placeAt(ctx, t, infra, store.Placement{
		RunID: "SHARED01", StepName: "infra-step", JobName: "build", NodeHash: hashOf(2),
		Tag: "infra", Address: "ssh://infra", GOOS: "linux", GOARCH: "amd64",
		Workdir: "/tmp/i",
	})

	if got := onlyPlacement(ctx, t, web, "SHARED01").StepName; got != "web-step" {
		t.Errorf("web's run reported step %q — it is reading another pipeline's placements", got)
	}

	if got := onlyPlacement(ctx, t, infra, "SHARED01").StepName; got != "infra-step" {
		t.Errorf("infra's run reported step %q — it is reading another pipeline's placements", got)
	}
}

// TestPlacementKeyIsScopedToItsPipeline holds the same rule one level down: it
// is the KEY that has to carry the pipeline, not only the read.
//
// Defense in depth since StartRun stopped upserting — a run id now names a row
// in exactly one pipeline — kept because the repo rule says a key carries the
// pipeline and because that should not rest on one statement in one function.
//
// merkle.HashNode folds kind, content and parent but NOT the pipeline, so two
// pipelines each running a job named build over a byte-identical task produce
// the same node hash — which is exactly why nodes is keyed (pipeline_id, hash).
// Keyed on (run_id, node_hash) alone, the second pipeline's upsert
// lands on the first's row and rewrites every column except the one saying
// whose it is: one pipeline then reports the other's machine, and the other
// reports nothing at all.
func (s suite) TestPlacementKeyIsScopedToItsPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	web := s.open(t, "web")

	infra := s.open(t, "infra")

	// Every part of the key the two can agree on by accident: one job name, one
	// content hash, one run id.
	const (
		runID   = "SHARED01"
		jobName = "build"
	)

	placeAt(ctx, t, web, store.Placement{
		RunID: runID, StepName: "unit", JobName: jobName, NodeHash: hashOf(7),
		Tag: "web", Address: "ssh://web", GOOS: "linux", GOARCH: "amd64",
		Workdir: "/tmp/web",
	})
	placeAt(ctx, t, infra, store.Placement{
		RunID: runID, StepName: "unit", JobName: jobName, NodeHash: hashOf(7),
		Tag: "infra", Address: "ssh://infra", GOOS: "linux", GOARCH: "amd64",
		Workdir: "/tmp/infra",
	})

	if got := onlyPlacement(ctx, t, web, runID).Address; got != "ssh://web" {
		t.Errorf("web's run ran on %q, want ssh://web — the other pipeline's upsert landed on its row", got)
	}

	if got := onlyPlacement(ctx, t, infra, runID).Address; got != "ssh://infra" {
		t.Errorf("infra's run ran on %q, want ssh://infra", got)
	}
}

// TestPendingQuestionsAreScopedToTheirPipeline holds the scoping rule against
// a shared state file: a question belongs to one pipeline, reached through its
// run, and the other pipeline in the same database must not see it.
func (s suite) TestPendingQuestionsAreScopedToTheirPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	mine := s.open(t, "test")

	theirs := s.open(t, "other")

	err := mine.StartRun(ctx, store.NewRun{ID: "run-1", JobName: "release-note", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	question, _ := askBump(t, mine, "run-1")

	pending, err := theirs.Questions(ctx, true, 0)
	if err != nil {
		t.Fatalf("Questions: %v", err)
	}

	if len(pending) != 0 {
		t.Errorf("the other pipeline saw %d of this one's questions", len(pending))
	}

	err = theirs.AnswerQuestion(ctx, question.ID, "minor", "stranger")
	if err == nil {
		t.Error("the other pipeline answered this one's question")
	}

	if mineStill, _ := mine.QuestionStatus(ctx, question.ID); mineStill.Status != store.QuestionPending {
		t.Errorf("this pipeline's question was resolved from outside: %q", mineStill.Status)
	}
}

// TestAbortingAQueuedJobIsScopedToItsPipeline: two pipelines in one file each with a job named build.
func (s suite) TestAbortingAQueuedJobIsScopedToItsPipeline(t *testing.T) {
	t.Parallel()

	mine := s.open(t, "mine")
	theirs := s.open(t, "theirs")

	mustEnqueueJob(t, mine, "build", "manual")
	mustEnqueueJob(t, theirs, "build", "manual")
	mustAbortQueued(t, mine)

	if !mustClaimNext(t, theirs) {
		t.Fatal("aborting one pipeline's queued build dropped another's")
	}
}

// TestCompletedRunStepsAreScopedToTheirPipeline pins the read --resume trusts.
//
// run_steps carries no pipeline column of its own, so this read reaches the
// pipeline only through runs. Unscoped, a pipeline asking about a run id it
// does not own reads the OWNER's finished steps as its own and skips work it
// never did — which for the resume index means skipping work, not just
// misreporting it.
//
// Defense in depth since StartRun stopped upserting: a run id now names a row
// in exactly one pipeline, so no honest resume can ask this question. The
// predicate stays because the repo rule is categorical about it.
func (s suite) TestCompletedRunStepsAreScopedToTheirPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	web := s.open(t, "web")
	infra := s.open(t, "infra")

	const shared = "SHARED01"

	err := web.StartRun(ctx, store.NewRun{ID: shared, JobName: "build", Workspace: "/tmp/web"})
	if err != nil {
		t.Fatalf("StartRun web: %v", err)
	}

	err = web.RecordRunStep(ctx, shared, store.RunStep{BuildID: shared + "#0", Index: 0, Name: "compile"})
	if err != nil {
		t.Fatalf("RecordRunStep web: %v", err)
	}

	// infra records no run of its own: runs.id is global and StartRun refuses
	// an id another pipeline holds. What is being asked is narrower and is the
	// query's own contract — infra reading a run id it does not own must see
	// nothing, rather than the other pipeline's finished steps.
	done, err := infra.CompletedRunSteps(ctx, shared)
	if err != nil {
		t.Fatalf("CompletedRunSteps infra: %v", err)
	}

	if len(done) != 0 {
		t.Fatalf("infra sees %d completed steps of a run it never finished: %v", len(done), done)
	}
}

// TestRunEventsAreScopedToTheirPipeline: run_events reaches its pipeline
// only through runs, like run_steps, and the Events facet can be held on its
// own — with no FindRunRow to ask first, the read has to refuse the other
// pipeline's run by itself.
func (s suite) TestRunEventsAreScopedToTheirPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	web := s.open(t, "web")
	infra := s.open(t, "infra")

	const shared = "SHARED02"

	err := web.StartRun(ctx, store.NewRun{ID: shared, JobName: "build", Workspace: "/tmp/web"})
	if err != nil {
		t.Fatalf("StartRun web: %v", err)
	}

	err = web.AppendRunEvent(ctx, store.RunEventRow{RunID: shared, Type: "step_started", StepName: "compile", At: time.Now()})
	if err != nil {
		t.Fatalf("AppendRunEvent web: %v", err)
	}

	err = web.RecordRunInput(ctx, shared, store.RunInput{Input: "repo", Resource: "repo", Version: `{"ref":"abc"}`})
	if err != nil {
		t.Fatalf("RecordRunInput web: %v", err)
	}

	events, err := infra.RunEvents(ctx, shared, 0, 0)
	if err != nil {
		t.Fatalf("RunEvents infra: %v", err)
	}

	if len(events) != 0 {
		t.Fatalf("infra sees %d event(s) of a run it never ran: %+v", len(events), events)
	}

	inputs, err := infra.RunInputs(ctx, shared)
	if err != nil {
		t.Fatalf("RunInputs infra: %v", err)
	}

	if len(inputs) != 0 {
		t.Fatalf("infra sees the inputs of a run it never created: %v", inputs)
	}
}

// TestFindRevisionIsScopedToItsPipeline: a state file may hold several
// pipelines, and a hash identifies bytes rather than a pipeline — so an
// unscoped lookup would serve one pipeline's page a configuration another
// pipeline ran, which is the shape of bug every query in this package carries
// a pipeline_id predicate to prevent.
func (s suite) TestFindRevisionIsScopedToItsPipeline(t *testing.T) {
	t.Parallel()

	mine := s.open(t, "test")

	theirs := s.open(t, "other")

	err := theirs.RecordRevision(ctxFor(t), "sha-theirs", pipelineSource(1), nil)
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	_, found, err := mine.FindRevision(ctxFor(t), "sha-theirs")
	if err != nil {
		t.Fatalf("FindRevision: %v", err)
	}

	if found {
		t.Error("one pipeline read a configuration another pipeline recorded")
	}

	// And its own is still found, so the scoping is a predicate rather than a
	// lookup that never works.
	err = mine.RecordRevision(ctxFor(t), "sha-mine", pipelineSource(2), nil)
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	revision, found, err := mine.FindRevision(ctxFor(t), "sha-mine")
	if err != nil || !found {
		t.Fatalf("FindRevision of its own configuration = (%v, %v, %v)", revision.SHA, found, err)
	}

	if revision.Source != pipelineSource(2) {
		t.Error("FindRevision returned a configuration that is not the one recorded")
	}
}

// TestStartRunRefusesAnIDHeldByAnotherPipeline: one state file may hold
// several pipelines and runs.id is global across all of them, so the refusal
// has to be global too. A pipeline-scoped check would hand the second
// pipeline a row belonging to the first.
func (s suite) TestStartRunRefusesAnIDHeldByAnotherPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	web := s.open(t, "web")

	infra := s.open(t, "infra")

	err := web.StartRun(ctx, store.NewRun{ID: "SHARED01", JobName: "build", Workspace: "/tmp/web"})
	if err != nil {
		t.Fatalf("StartRun web: %v", err)
	}

	err = infra.StartRun(ctx, store.NewRun{ID: "SHARED01", JobName: "build", Workspace: "/tmp/infra"})
	if !errors.Is(err, store.ErrRunExists) {
		t.Fatalf("minting an id another pipeline holds returned %v, want store.ErrRunExists", err)
	}
}

// TestResumeRunIsScopedToItsPipeline holds the repo rule on the new query: a
// run id names a row in ONE pipeline, and reaching another pipeline's run
// would put a foreign row back in flight under this pipeline's name.
func (s suite) TestResumeRunIsScopedToItsPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	web := s.open(t, "web")

	infra := s.open(t, "infra")

	err := web.StartRun(ctx, store.NewRun{ID: "WEBRUN01", JobName: "build", Workspace: "/tmp/web"})
	if err != nil {
		t.Fatalf("StartRun web: %v", err)
	}

	err = infra.ResumeRun(ctx, "WEBRUN01", "/tmp/infra", "")
	if !errors.Is(err, store.ErrNoSuchRun) {
		t.Fatalf("resuming another pipeline's run returned %v, want store.ErrNoSuchRun", err)
	}
}

// TestStepBlobsAreScopedToThePipeline holds the standing rule: two pipelines
// sharing a state file never see each other's rows, even for an identical
// action key.
func (s suite) TestStepBlobsAreScopedToThePipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	first := s.open(t, "app")

	second := s.open(t, "infra")

	err := first.RecordStepBlobs(ctx, "same-key", map[string]string{"out": "app-digest"})
	if err != nil {
		t.Fatalf("RecordStepBlobs: %v", err)
	}

	got, err := second.StepBlobs(ctx, "same-key")
	if err != nil || len(got) != 0 {
		t.Fatalf("the other pipeline sees %v, %v; want nothing", got, err)
	}
}

// TestAgentUsageKeyIsScopedToItsPipeline is the same collision run_placements
// had, on the table that holds money.
//
// merkle.HashNode folds kind, content and parent but NOT the pipeline, so two
// Defense in depth since StartRun stopped upserting: a run id now names a row
// in exactly one pipeline, so the collision below is no longer reachable
// through the API. The key still carries the pipeline, because the repo rule
// says it must and because that guarantee should not rest on one statement in
// one function.
//
// pipelines each with a job named build over a byte-identical agent step
// produce the same node hash. Keyed on (run_id, node_hash) alone the second pipeline's
// insert conflicts with the first's and takes the ACCUMULATING branch — which
// is correct for a retried step and catastrophic across pipelines: the tokens
// are added to a row belonging to someone else, so one pipeline is billed for
// work it never ran and the other reports nothing at all.
func (s suite) TestAgentUsageKeyIsScopedToItsPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	web := s.open(t, "web")
	infra := s.open(t, "infra")

	const (
		runID   = "SHARED01"
		jobName = "build"
	)

	recordSpend(ctx, t, web, runID, jobName, "web-review", 1_000)
	recordSpend(ctx, t, infra, runID, jobName, "infra-review", 7)

	got := onlyUsage(ctx, t, web, runID)
	if got.StepName != "web-review" {
		t.Errorf("web's run reports step %q — it is reading another pipeline's spend", got.StepName)
	}

	if got.Total != 1_000 {
		t.Errorf("web's run spent %d tokens, want 1000 — another pipeline's tokens were added to its row", got.Total)
	}

	got = onlyUsage(ctx, t, infra, runID)
	if got.StepName != "infra-review" {
		t.Errorf("infra's run reports step %q", got.StepName)
	}

	if got.Total != 7 {
		t.Errorf("infra's run spent %d tokens, want 7", got.Total)
	}
}
