package cli

// A pipeline has ONE identity, and every subsystem that scopes anything by
// pipeline must spell it the same way.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/web"
)

// TestLoadedPipelineCarriesTheIdentityTheRestOfTheProcessUses is the seam.
//
// The slug and the Config were assigned four lines apart in WebCmd.load and
// never compared: `w.Load(path)` stamped one identity from the path and
// `resolvePipelineName(path, w.Name)` computed another from the --name map,
// and the pieces downstream picked whichever was nearest. The store, the
// /p/<slug> route and run_events took the slug; the agent pin scope took the
// Config. So --name moved two of them and not the third, and the pin log
// lines an operator correlates against a run record named a different string
// than the record does.
//
// Asserted across the boundary rather than on either half, because both
// halves were correct on their own the whole time.
func TestLoadedPipelineCarriesTheIdentityTheRestOfTheProcessUses(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := flagFixture(t)

	cmd := &WebCmd{ //nolint:exhaustruct // the identity flags are what is under test
		Pipeline: []string{path},
		StateFlags: StateFlags{
			State: filepath.Join(dir, "shared.db"),
			// The flag that used to move only some of them.
			Name: map[string]string{"prod": path},
		},
	}

	pipelines, _, cleanup, err := cmd.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	t.Cleanup(cleanup)

	if len(pipelines) != 1 {
		t.Fatalf("loaded %d pipelines, want 1", len(pipelines))
	}

	loaded := pipelines[0]

	if loaded.Slug != "prod" {
		t.Fatalf("slug = %q, want the --name override", loaded.Slug)
	}

	if loaded.Config().Name != loaded.Slug {
		t.Errorf("the Config calls this pipeline %q while everything else calls it %q — an agent pin scoped by the first cannot be joined to a run record written under the second",
			loaded.Config().Name, loaded.Slug)
	}
}

// TestPipelineIdentityDefaultsToTheSlugWithoutAnOverride pins the other half:
// the default has to agree too, or --name would be the only way to get one
// identity and every pipeline without one would still have two.
func TestPipelineIdentityDefaultsToTheSlugWithoutAnOverride(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := flagFixture(t)

	cmd := &WebCmd{ //nolint:exhaustruct // the identity flags are what is under test
		Pipeline:   []string{path},
		StateFlags: StateFlags{State: filepath.Join(dir, "shared.db")}, //nolint:exhaustruct // no --name is the case under test
	}

	pipelines, _, cleanup, err := cmd.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	t.Cleanup(cleanup)

	if pipelines[0].Config().Name != pipelines[0].Slug {
		t.Errorf("Config name %q, slug %q", pipelines[0].Config().Name, pipelines[0].Slug)
	}
}

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
		State: filepath.Join(dir, "shared.db"),
		Name:  map[string]string{"prod": path},
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

// TestJobsResumeHonoursTheNameOverride is the command the invariant caught,
// asserted end to end so the fix cannot be reverted quietly.
//
// `jobs resume` writes to a pipeline's state, so it has to agree with the
// state about which pipeline that is. It loaded the Config under the file
// name while opening the store under --name, which is precisely the split
// #94 describes — invisible until something else keyed by the Config's
// identity (an agent pin) had to be joined to a row written under the
// store's.
func TestJobsResumeHonoursTheNameOverride(t *testing.T) {
	path := flagFixture(t)
	state := filepath.Join(t.TempDir(), "shared.db")

	st, err := store.OpenStore(state, "prod")
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	const limit = 3

	for range limit {
		_, _, err = st.RecordJobOutcome(t.Context(), "build", false, limit)
		if err != nil {
			t.Fatalf("RecordJobOutcome: %v", err)
		}
	}

	err = st.Close()
	if err != nil {
		t.Fatalf("close state store: %v", err)
	}

	out := captureStdout(t, func() {
		err = Run([]string{"jobs", "resume", path, "build", "--state", state, "--name", "prod=" + path})
	})

	if err != nil {
		t.Fatalf("jobs resume under --name: %v", err)
	}

	if !strings.Contains(out, "resumed: build") {
		t.Errorf("output does not say what was resumed:\n%s", out)
	}

	reopened, err := store.OpenStore(state, "prod")
	if err != nil {
		t.Fatalf("reopen state store: %v", err)
	}

	defer func() { _ = reopened.Close() }()

	paused, err := reopened.IsJobPaused(t.Context(), "build")
	if err != nil {
		t.Fatalf("IsJobPaused: %v", err)
	}

	if paused {
		t.Error("the job under the --name identity is still paused, so resume wrote somewhere else")
	}
}

// TestWebRefusesTwoPipelinesClaimingOneName is the guarantee that makes the
// identity safe to be a NAME rather than a path.
//
// The pin scope, the store rows and the /p/<slug> route are all keyed by it,
// so two pipelines answering to one name would share all three — the agent
// pin collision pinScope was added to prevent, plus a state file where two
// pipelines' runs interleave under one identity. A path could never collide
// and a name can, so the check that names are distinct stops being a
// convenience about URLs and becomes the thing that holds the scope apart.
// `steps web` is the only mode that serves several pipelines from one
// process, which is why it is the only place this has to hold.
func TestWebRefusesTwoPipelinesClaimingOneName(t *testing.T) {
	t.Parallel()

	// Two files that are genuinely different pipelines and genuinely share a
	// base name — the ordinary infra/pipeline.yml and app/pipeline.yml shape,
	// not a contrived one.
	first := flagFixture(t)
	second := flagFixture(t)

	err := Run([]string{"web", first, second, "--listen", "127.0.0.1:1"})
	if err == nil {
		t.Fatal("web served two pipelines under one name: their agent pins, their run records and their /p/<slug> routes would all be shared")
	}

	// The refusal an operator can act on, naming both files and the --name
	// that settles it. A second layer inside the server also rejects a
	// duplicate slug, so this asserts the ACTIONABLE one rather than merely
	// that something failed.
	if !strings.Contains(err.Error(), "both named") || !strings.Contains(err.Error(), "--name") {
		t.Errorf("error = %v, want the refusal that names both files and how to settle it", err)
	}

	// And --name is the way out, which is the whole reason the flag exists.
	err = Run([]string{
		"web", first, second, "--listen", "127.0.0.1:1",
		"--name", "app=" + first, "--name", "infra=" + second,
	})
	if err == nil || strings.Contains(err.Error(), "both named") {
		t.Errorf("with --name distinguishing them web still refused on names: %v", err)
	}
}

// TestTheTwoSlugifiersAgree covers the arrangement the above rests on.
//
// web.Slugify is what the /p/<slug> route is built from and config.Slugify is
// what a Config defaults its name to; a second, drifting copy is exactly how
// the identity split in the first place, so web.Slugify delegates. This
// asserts the RESULTS match rather than that there is one definition — a
// reintroduced copy that happens to agree passes here, and only stops passing
// once it drifts, which is the moment that matters.
func TestTheTwoSlugifiersAgree(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"app.yml", "./app.yml", "infra/deploy.yml", "/abs/infra/deploy.yaml",
		"no-extension", "dotted.name.yml",
	} {
		if web.Slugify(path) != config.Slugify(path) {
			t.Errorf("%q slugifies to %q for the UI and %q for the Config",
				path, web.Slugify(path), config.Slugify(path))
		}
	}
}
