package cli

// A pipeline has ONE identity, and every subsystem that scopes anything by
// pipeline must spell it the same way.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// TestSetupRefusesAConfigLoadedUnderADifferentIdentity is the backstop for
// the next call site.
//
// Four commands load a pipeline and open its state, and each has to resolve
// the identity the same way. `jobs resume` did not: it loaded with the file
// name default while opening the store under the --name override, so the
// Config and the store disagreed on a command whose whole job is to write to
// that store. Making it a checked invariant means a fifth command that
// forgets is told, instead of quietly having two identities the way the
// first four did.
func TestSetupRefusesAConfigLoadedUnderADifferentIdentity(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := flagFixture(t)

	// A Config that took the file-name default while --name says otherwise:
	// exactly what a call site that forgot to resolve would produce.
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	flags := StateFlags{
		DB:   DB(filepath.Join(dir, "shared.db")),
		Name: map[string]string{"prod": path},
	}

	_, _, cleanup, err := setup(cfg, path, flags, ExecFlags{}) //nolint:exhaustruct // no execution flags are read on this path
	if err == nil {
		t.Cleanup(cleanup)
		t.Fatal("setup accepted a Config whose identity is not the one its state is scoped to")
	}

	if !strings.Contains(err.Error(), "prod") {
		t.Errorf("error = %v, want it to name the identity the state uses", err)
	}
}

// TestJobsResumeAnswersForThePipelineItWasNamed pins what `-p` moved.
//
// `jobs resume` writes to a pipeline's state and checks the job name against
// the configuration that pipeline is SET to, so both halves have to agree
// about which pipeline is meant. It used to derive one identity from a file
// path and another from --name, which is the split #94 describes; there is no
// path here any more, and the name it is given is the only answer either half
// can reach.
func TestJobsResumeAnswersForThePipelineItWasNamed(t *testing.T) {
	state := setPipelineInto(t, "prod")
	pauseJobIn(t, state, "prod", "build")

	var err error

	out := captureStdout(t, func() {
		err = Run([]string{"jobs", "resume", "build", "-p", "prod", "--db", state})
	})

	if err != nil {
		t.Fatalf("jobs resume: %v", err)
	}

	if !strings.Contains(out, "resumed: build") {
		t.Errorf("output does not say what was resumed:\n%s", out)
	}

	reopened, err := sqlite.OpenStore(state, "prod")
	if err != nil {
		t.Fatalf("reopen state store: %v", err)
	}

	defer func() { _ = reopened.Close() }()

	paused, err := reopened.IsJobPaused(t.Context(), "build")
	if err != nil {
		t.Fatalf("IsJobPaused: %v", err)
	}

	if paused {
		t.Error("the job is still paused, so resume wrote somewhere else")
	}
}

// TestJobsResumeRefusesAJobTheServedConfigDoesNotHave: the name is checked
// against the configuration the daemon is serving, so a typo is a refusal
// rather than a no-op that reports success.
func TestJobsResumeRefusesAJobTheServedConfigDoesNotHave(t *testing.T) {
	state := setPipelineInto(t, "prod")
	pauseJobIn(t, state, "prod", "build")

	err := Run([]string{"jobs", "resume", "buidl", "-p", "prod", "--db", state})
	if err == nil {
		t.Fatal("resuming a job that is not paused was reported as done")
	}

	// Named alongside what IS paused: the mistake this catches is a name that
	// is nearly right, and the fix is almost always visible in the list.
	if !strings.Contains(err.Error(), "build") {
		t.Errorf("the refusal does not say which jobs are paused: %v", err)
	}
}

// setPipelineInto records a configuration under name and makes it current,
// which is what a `steps pipeline set` leaves behind for a local command to
// read.
func setPipelineInto(t *testing.T, name string) string {
	t.Helper()

	path := flagFixture(t)
	state := filepath.Join(t.TempDir(), "shared.db")

	st, err := sqlite.OpenStore(state, name)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	cfg, err := config.Load(path, name, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	err = st.RecordRevision(t.Context(), cfg.Revision.SHA, cfg.Revision.Source, cfg.Revision.Includes)
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	err = st.SetCurrentRevision(t.Context(), cfg.Revision.SHA, path)
	if err != nil {
		t.Fatalf("SetCurrentRevision: %v", err)
	}

	err = st.Close()
	if err != nil {
		t.Fatalf("close state store: %v", err)
	}

	return state
}

// pauseJobIn leaves a job where the circuit breaker would have left it.
func pauseJobIn(t *testing.T, state, name, job string) {
	t.Helper()

	st, err := sqlite.OpenStore(state, name)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	const limit = 3

	for range limit {
		_, _, err = st.RecordJobOutcome(t.Context(), job, false, limit)
		if err != nil {
			t.Fatalf("RecordJobOutcome: %v", err)
		}
	}

	err = st.Close()
	if err != nil {
		t.Fatalf("close state store: %v", err)
	}
}

// TestPipelineNameAgreesWithTheConfigsOwn covers the arrangement the identity
// rests on: `steps run pipeline.yml` derives a default name here while
// config.Load stamps one on the Config, and a second copy of that is how the
// identity split the first time.
func TestPipelineNameAgreesWithTheConfigsOwn(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"app.yml", "./app.yml", "infra/deploy.yml", "/abs/infra/deploy.yaml",
		"no-extension", "dotted.name.yml",
	} {
		if PipelineName(path) != config.Slugify(path) {
			t.Errorf("%q names the pipeline %q for the CLI and %q for the Config",
				path, PipelineName(path), config.Slugify(path))
		}
	}
}

// --name is typed with whatever spelling the operator used, usually relative, so both sides are made absolute or a relative override silently falls back to the filename.
func TestANameOverrideMatchesARelativePath(t *testing.T) {
	t.Parallel()

	abs, err := filepath.Abs("ci/app.yml")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ pipeline, override string }{
		{"ci/app.yml", "ci/app.yml"},
		{"ci/app.yml", abs},
		{abs, "./ci/app.yml"},
	} {
		if got := resolvePipelineName(tc.pipeline, map[string]string{"infra": tc.override}); got != "infra" {
			t.Errorf("--name infra=%s for %s named it %q", tc.override, tc.pipeline, got)
		}
	}
}
