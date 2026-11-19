package storetest

import (
	"slices"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// hostileWrite stores hostile text through one write and reads back every field it landed in.
type hostileWrite struct {
	name  string
	write func(t *testing.T, st store.Store) []string
}

// TestEveryFreeTextWriteSurvivesHostileText: text.go's three sites are where a NUL first cost a row; any other write a person, a model or a script fills is the same hole, so every one is listed.
func (s suite) TestEveryFreeTextWriteSurvivesHostileText(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	for _, site := range hostileWrites() {
		t.Run(site.name, func(t *testing.T) {
			for _, got := range site.write(t, st) {
				assertReadable(t, site.name, got)
			}
		})
	}
}

func must(t *testing.T, what string, err error) {
	t.Helper()

	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func hostileWrites() []hostileWrite {
	return slices.Concat(hostileDecisionWrites(), hostileRunWrites(), hostileQueueWrites())
}

func hostileDecisionWrites() []hostileWrite {
	return []hostileWrite{
		{"approval", func(t *testing.T, st store.Store) []string {
			ctx := ctxFor(t)
			id, err := st.RequestApproval(ctx, "deploy", hostile)
			must(t, "RequestApproval", err)

			must(t, "DecideApproval", st.DecideApproval(ctx, id, store.ApprovalRejected, hostile, hostile))

			got, err := st.ApprovalStatus(ctx, id)
			must(t, "ApprovalStatus", err)

			return []string{got.Message, got.DecidedBy, got.Reason}
		}},
		{"approval for a step", func(t *testing.T, st store.Store) []string {
			ctx := ctxFor(t)
			seedRuns(t, st, "deploy", "approval-run")

			id, err := st.RequestApprovalFor(ctx, "approval-run", hostile, "deploy", hostile)
			must(t, "RequestApprovalFor", err)

			again, err := st.RequestApprovalFor(ctx, "approval-run", hostile, "deploy", hostile)
			must(t, "RequestApprovalFor again", err)

			if again != id {
				t.Errorf("the same step's second request made approval %d, want %d", again, id)
			}

			got, err := st.ApprovalStatus(ctx, id)
			must(t, "ApprovalStatus", err)

			return []string{got.Message}
		}},
		{"answered question", func(t *testing.T, st store.Store) []string {
			ctx := ctxFor(t)
			seedRuns(t, st, "release", "answer-run")

			asked, _, err := st.AskQuestion(ctx, store.Question{
				RunID: "answer-run", JobName: "release", AgentName: hostile, Question: hostile,
				Options: []string{hostile}, Default: hostile,
			})
			must(t, "AskQuestion", err)

			must(t, "AnswerQuestion", st.AnswerQuestion(ctx, asked.ID, hostile, hostile))

			got, err := st.QuestionStatus(ctx, asked.ID)
			must(t, "QuestionStatus", err)

			return []string{got.AgentName, got.Question, got.Options[0], got.Default, got.Answer, got.AnsweredBy}
		}},
		{"closed question", func(t *testing.T, st store.Store) []string {
			ctx := ctxFor(t)
			seedRuns(t, st, "release", "close-run")

			asked, _, err := st.AskQuestion(ctx, store.Question{RunID: "close-run", JobName: "release", AgentName: "writer", Question: "which?"})
			must(t, "AskQuestion", err)

			must(t, "CloseQuestion", st.CloseQuestion(ctx, asked.ID, store.QuestionExpired, hostile, hostile))

			got, err := st.QuestionStatus(ctx, asked.ID)
			must(t, "QuestionStatus", err)

			return []string{got.Answer, got.AnsweredBy}
		}},
		{"check error", func(t *testing.T, st store.Store) []string {
			must(t, "RecordCheckError", st.RecordCheckError(ctxFor(t), "repo", hostile))

			errs, err := st.CheckErrors(ctxFor(t))
			must(t, "CheckErrors", err)

			if len(errs) != 1 {
				t.Fatalf("%d check errors recorded, want 1", len(errs))
			}

			return []string{errs[0].Message}
		}},
		{"activity", func(t *testing.T, st store.Store) []string {
			recordActivity(t, st, store.ActivityEntry{Actor: hostile, Action: hostile, Target: hostile}, 0)

			got := readActivity(t, st, store.ActivityFilter{Limit: 1})
			if len(got) != 1 {
				t.Fatalf("%d activity entries, want 1", len(got))
			}

			return []string{got[0].Actor, got[0].Action, got[0].Target}
		}},
		{"revision", func(t *testing.T, st store.Store) []string {
			ctx := ctxFor(t)
			must(t, "RecordRevision", st.RecordRevision(ctx, "sha-hostile", hostile, nil))

			got, found, err := st.FindRevision(ctx, "sha-hostile")
			if err != nil || !found {
				t.Fatalf("FindRevision: found=%v err=%v", found, err)
			}

			must(t, "SetCurrentRevision", st.SetCurrentRevision(ctx, "sha-hostile", hostile))

			return []string{got.Source, pipelinePath(t, st)}
		}},
		{"source path", func(t *testing.T, st store.Store) []string {
			must(t, "SetSourcePath", st.SetSourcePath(ctxFor(t), hostile))

			return []string{pipelinePath(t, st)}
		}},
	}
}

func hostileRunWrites() []hostileWrite {
	return []hostileWrite{
		{"placement", func(t *testing.T, st store.Store) []string {
			ctx := ctxFor(t)
			instance := hostile

			placeAt(ctx, t, st, store.Placement{
				RunID: "placed-run", StepName: hostile, JobName: "build", NodeHash: hashOf(70),
				Tag: hostile, Address: hostile, InstanceID: &instance, GOOS: hostile, GOARCH: hostile,
				Workdir: hostile, Image: hostile, ImageDigest: hostile,
			})

			got := onlyPlacement(ctx, t, st, "placed-run")

			return []string{
				got.StepName, got.Tag, got.Address, derefOr(got.InstanceID, ""), got.GOOS, got.GOARCH,
				got.Workdir, got.Image, got.ImageDigest,
			}
		}},
		{"run", func(t *testing.T, st store.Store) []string {
			ctx := ctxFor(t)
			must(t, "StartRun", st.StartRun(ctx, store.NewRun{ID: "hostile-run", JobName: "build", Workspace: hostile, CreatedBy: hostile}))

			started, _, err := st.FindRunRow(ctx, "hostile-run")
			must(t, "FindRunRow", err)

			must(t, "FinishRun", st.FinishRun(ctx, "hostile-run", store.RunFailed))
			must(t, "ResumeRun", st.ResumeRun(ctx, "hostile-run", "before resumed "+hostile, ""))

			resumed, _, err := st.FindRunRow(ctx, "hostile-run")
			must(t, "FindRunRow", err)

			return []string{started.Workspace, started.CreatedBy, resumed.Workspace}
		}},
		{"run step", func(t *testing.T, st store.Store) []string {
			ctx := ctxFor(t)
			seedRuns(t, st, "build", "stepped-run")

			must(t, "RecordRunStep", st.RecordRunStep(ctx, "stepped-run", store.RunStep{
				BuildID: "stepped-run#0", Index: 0, Visit: 1, Name: hostile, Outcome: hostile, Hash: hashOf(71),
			}))

			steps, err := st.CompletedRunSteps(ctx, "stepped-run")
			must(t, "CompletedRunSteps", err)

			if len(steps) != 1 {
				t.Fatalf("%d run steps, want 1", len(steps))
			}

			return []string{steps[0].Name, steps[0].Outcome}
		}},
		{"node", func(t *testing.T, st store.Store) []string {
			must(t, "RecordNode", st.RecordNode(ctxFor(t), store.NodeRecord{
				Hash: hashOf(72), Kind: "task", Resource: hostile, Content: map[string]any{"body": hostile},
			}, "node-job", store.NodeSucceeded, map[string]any{"out": hostile}, nil))

			nodes, err := st.ListNodes(ctxFor(t), "node-job", 0)
			must(t, "ListNodes", err)

			if len(nodes) != 1 {
				t.Fatalf("%d nodes, want 1", len(nodes))
			}

			return []string{nodes[0].Resource}
		}},
		{"agent usage", func(t *testing.T, st store.Store) []string {
			ctx := ctxFor(t)
			seedRuns(t, st, "review", "usage-run")

			must(t, "RecordAgentUsage", st.RecordAgentUsage(ctx, store.AgentUsage{
				RunID: "usage-run", StepName: hostile, JobName: "review", Kind: store.UsageHook,
				ModelReq: hostile, ModelServed: hostile, FinishReason: hostile, RawMeta: hostile, Total: 1,
			}))

			usage, err := st.RunUsage(ctx, "usage-run")
			must(t, "RunUsage", err)

			if len(usage) != 1 {
				t.Fatalf("%d usage rows, want 1", len(usage))
			}

			return []string{usage[0].StepName, usage[0].ModelReq, usage[0].ModelServed, usage[0].FinishReason, usage[0].RawMeta}
		}},
	}
}

func hostileQueueWrites() []hostileWrite {
	return []hostileWrite{
		{"queue", func(t *testing.T, st store.Store) []string {
			ctx := ctxFor(t)
			seedRuns(t, st, "rerun", "queued-run")

			must(t, "EnqueueJob", st.EnqueueJob(ctx, "auto", hostile, hostile))
			must(t, "EnqueueManualJob", st.EnqueueManualJob(ctx, "manual", hostile, hostile))
			must(t, "EnqueueRerunJob", st.EnqueueRerunJob(ctx, "rerun", hostile, "queued-run", hostile))
			must(t, "EnqueuePinnedJob", st.EnqueuePinnedJob(ctx, "pinned", hostile, hostile, hostile))

			rows, err := st.ListTriggerQueue(ctx, 0)
			must(t, "ListTriggerQueue", err)

			var reasons []string

			for _, row := range rows {
				if row.Status == store.QueuePending {
					reasons = append(reasons, row.Reason)
				}
			}

			if len(reasons) != 4 {
				t.Fatalf("%d pending rows, want the 4 just queued", len(reasons))
			}

			return reasons
		}},
		{"queued by", claimRequesters},
		{"credential", func(t *testing.T, st store.Store) []string {
			ctx := ctxFor(t)
			must(t, "SaveCredential", st.SaveCredential(ctx, hostile, []byte("sealed")))

			sealed, found, err := st.Credential(ctx, hostile)
			if err != nil || !found || string(sealed) != "sealed" {
				t.Fatalf("Credential = %q, %v, %v; want the sealed bytes back under the same name", sealed, found, err)
			}

			return nil
		}},
		{"event", func(t *testing.T, st store.Store) []string {
			ctx := ctxFor(t)
			seedRuns(t, st, "build", "event-run")

			must(t, "AppendRunEvent", st.AppendRunEvent(ctx, store.RunEventRow{
				RunID: "event-run", Type: "step_note", StepName: hostile, Name: hostile, Worker: hostile, Detail: hostile, At: time.Now(),
			}))

			events := mustRunEvents(ctx, t, st, "event-run", 0, 0)
			if len(events) != 1 {
				t.Fatalf("%d events, want 1", len(events))
			}

			return []string{events[0].StepName, events[0].Name, events[0].Worker, events[0].Detail}
		}},
	}
}

// claimRequesters reads who asked for each queued row through the run its claim starts, which is where requested_by surfaces.
func claimRequesters(t *testing.T, st store.Store) []string {
	t.Helper()

	ctx := ctxFor(t)
	must(t, "SyncJobLimits", st.SyncJobLimits(ctx, nil, map[string]int{"auto": 10, "manual": 10, "rerun": 10, "pinned": 10}))

	var got []string

	for {
		claim, found, err := st.ClaimNextJob(ctx)
		must(t, "ClaimNextJob", err)

		if !found {
			break
		}

		if claim.Pin != "" {
			got = append(got, claim.Pin)
		}

		runID := "by-" + claim.JobName
		must(t, "StartRun", st.StartRun(ctx, store.NewRun{ID: runID, JobName: claim.JobName, Workspace: "/tmp/ws", QueueID: claim.ID}))

		run, _, err := st.FindRunRow(ctx, runID)
		must(t, "FindRunRow", err)

		got = append(got, run.CreatedBy)
	}

	if len(got) != 5 {
		t.Fatalf("read %d requesters and pins, want 4 requesters and 1 pin", len(got))
	}

	return got
}

func pipelinePath(t *testing.T, st store.Store) string {
	t.Helper()

	rows, err := st.Reader().Pipelines(ctxFor(t))
	must(t, "Pipelines", err)

	for _, row := range rows {
		if row.Name == "test" {
			return row.Path
		}
	}

	t.Fatal("the pipeline is not listed")

	return ""
}
