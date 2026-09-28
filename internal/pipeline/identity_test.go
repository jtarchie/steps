package pipeline

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/workspace"
)

// TestGetStepLogsSayWhichGet covers the one step kind that never reaches
// runNonGetStep, and so never picked up the index/kind/step fields every
// other kind gets.
//
// The symptom was invisible in a one-get job and useless in a real one: with
// several get steps, `job.step.finished` said only which run and job it
// belonged to, so "which resource just failed" had no answer in the log at
// all.
func TestGetStepLogsSayWhichGet(t *testing.T) {
	// Not t.Parallel(): mutates slog's default logger.
	cfg, job, st, provider := twoGetFixture(t)
	defer func() { _ = st.Close() }()
	defer func() { _ = provider.Close() }()

	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(events.LogHandler(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))))
	t.Cleanup(func() { slog.SetDefault(prev) })

	err := RunJob(context.Background(), cfg, job, nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	out := buf.String()

	var (
		sawFirst  bool
		sawSecond bool
	)

	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "job.step.finished") || logField(line, "kind") != "get" {
			continue
		}

		switch logField(line, "step") {
		case "alpha":
			sawFirst = true
		case "beta":
			sawSecond = true
		}
	}

	if !sawFirst || !sawSecond {
		t.Errorf("get steps did not both name themselves in the log (alpha=%v beta=%v):\n%s", sawFirst, sawSecond, out)
	}
}

// TestGetStepIdentityStopsAtTheGet is the other half, and the reason
// fanOutGet stamps a scoped context instead of reassigning its own: a
// triggered build runs the REMAINDER of the plan, and if it inherited the
// get's identity every later step would report itself as that get.
func TestGetStepIdentityStopsAtTheGet(t *testing.T) {
	// Not t.Parallel(): mutates slog's default logger.
	cfg, job, st, provider := triggeredBuildFixture(t)
	defer func() { _ = st.Close() }()
	defer func() { _ = provider.Close() }()

	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(events.LogHandler(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))))
	t.Cleanup(func() { slog.SetDefault(prev) })

	err := RunJob(context.Background(), cfg, job, nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.Contains(line, "job.step.finished") || logField(line, "step") != "compile" {
			continue
		}

		if got := logField(line, "kind"); got != "task" {
			t.Errorf("the task after a get reported kind=%q, want task — it inherited the get's identity: %s", got, line)
		}
	}
}

// captureRun runs the fixture's job with slog's default captured the way the
// CLI installs it, and returns what it logged.
func captureRun(t *testing.T, pipeline string) string {
	t.Helper()

	cfg, job, st, provider := fixtureFrom(t, pipeline)
	defer func() { _ = st.Close() }()
	defer func() { _ = provider.Close() }()

	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(events.LogHandler(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))))
	t.Cleanup(func() { slog.SetDefault(prev) })

	err := RunJob(context.Background(), cfg, job, nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	return buf.String()
}

// TestAnAliasedGetNamesItsResource: a get: whose resource: differs logs both,
// since the step name alone does not say which resource's check ran.
func TestAnAliasedGetNamesItsResource(t *testing.T) {
	// Not t.Parallel(): mutates slog's default logger.
	out := captureRun(t, `
resource_types:
  - name: dummy
    config:
      check: 'echo ''[{"ref":"v1"}]'''
      in: "true"

resources:
  - name: alpha
    type: dummy
    source: {key: a}

jobs:
  - name: build
    plan:
      - get: src
        resource: alpha
`)

	// resource.fetched too: a first get fetches inside the build it fans out
	// into, whose context deliberately does not carry the get's identity.
	for _, msg := range []string{"msg=job.step.finished", "msg=resource.fetched"} {
		line := ""

		for candidate := range strings.SplitSeq(out, "\n") {
			if strings.Contains(candidate, msg+" ") {
				line = candidate

				break
			}
		}

		if logField(line, "step") != "src" || logField(line, "resource") != "alpha" {
			t.Errorf("%s logged step=%q resource=%q, want src/alpha: %q", msg, logField(line, "step"), logField(line, "resource"), line)
		}
	}
}

// TestAHookBodyLogsAsItselfNotItsStep: a hook runs under the context of the
// step it guards, and a task hook on a get used to log as that get — its
// index, kind and resource — the same misfiling withHookIdentity undoes for
// events.
func TestAHookBodyLogsAsItselfNotItsStep(t *testing.T) {
	// Not t.Parallel(): mutates slog's default logger.
	out := captureRun(t, `
resource_types:
  - name: dummy
    config:
      check: 'echo ''[{"ref":"v1"}]'''
      in: "true"

resources:
  - name: alpha
    type: dummy
    source: {key: a}
  - name: beta
    type: dummy
    source: {key: b}

jobs:
  - name: build
    plan:
      - get: alpha
      - get: beta
        on_success:
          task: tell
          run: echo fetched
`)

	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "job.step.finished") && logField(line, "hook") == "on_success" {
			if logField(line, "step") != "tell" || logField(line, "kind") != "task" ||
				logField(line, "resource") != "" || logField(line, "index") != "" {
				t.Errorf("hook body logged as its enclosing step: %s", line)
			}

			return
		}
	}

	t.Fatalf("no hook finished:\n%s", out)
}

// TestJobHookKeepsItsJobAndDropsTheStepIndex pins what a hook body hands to
// another package (see currentStepRef and agent.RunFix).
//
// Both halves were wrong, in opposite directions. A step-level hook inherited
// its enclosing step's plan position, so a fix agent's conversation published
// under a step the hook is not. A job-level hook inherited nothing at all, so
// the same conversation published under an empty job name — a real
// conversation filed under no job.
func TestJobHookKeepsItsJobAndDropsTheStepIndex(t *testing.T) {
	t.Parallel()

	// A job-level hook: nothing has tagged the context at all.
	jobName, index := currentStepRef(withHookIdentity(context.Background(), "deploy", "on_failure"))
	if jobName != "deploy" {
		t.Errorf("job = %q, want deploy — a job-level hook knows its job even with no plan position", jobName)
	}

	if index != -1 {
		t.Errorf("index = %d, want -1 — a hook holds no plan position", index)
	}

	// A step-level hook: the enclosing step tagged it first, and the hook
	// must not keep that index.
	stepCtx := withStepIdentity(context.Background(), "deploy", 3, config.Step{Task: "build"})

	jobName, index = currentStepRef(withHookIdentity(stepCtx, "deploy", "on_failure"))
	if jobName != "deploy" || index != -1 {
		t.Errorf("step-level hook reported (%q, %d), want (deploy, -1) — it inherited the step's plan position", jobName, index)
	}
}

// twoGetFixture is a job with two get steps of the same trivial resource
// type, so a log line naming only "a get" cannot be told from the other one.
func twoGetFixture(t *testing.T) (*config.Config, *config.Job, store.Store, workspace.Provider) {
	t.Helper()

	return fixtureFrom(t, `
resource_types:
  - name: dummy
    config:
      check: 'echo ''[{"ref":"v1"}]'''
      in: "true"

resources:
  - name: alpha
    type: dummy
    source: {key: a}
  - name: beta
    type: dummy
    source: {key: b}

jobs:
  - name: build
    plan:
      - get: alpha
      - get: beta
`)
}

// triggeredBuildFixture is a triggering get followed by a task, so the task
// runs inside the build the get fans out into.
func triggeredBuildFixture(t *testing.T) (*config.Config, *config.Job, store.Store, workspace.Provider) {
	t.Helper()

	return fixtureFrom(t, `
resource_types:
  - name: dummy
    config:
      check: 'echo ''[{"ref":"v1"}]'''
      in: "true"

resources:
  - name: alpha
    type: dummy
    source: {key: a}

jobs:
  - name: build
    plan:
      - get: alpha
        trigger: true
      - task: compile
        run: "true"
`)
}

// The job is always named build: every fixture here is a one-job pipeline
// written for the test that uses it, so a name to pass was flexibility no
// caller ever wanted.
func fixtureFrom(t *testing.T, pipeline string) (*config.Config, *config.Job, store.Store, workspace.Provider) {
	const jobName = "build"

	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "pipe.yml")

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	st, err := sqlite.OpenStore(filepath.Join(dir, ".steps", "state.db"), "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	job, err := cfg.FindJob(jobName)
	if err != nil {
		t.Fatal(err)
	}

	return cfg, job, st, provider
}
