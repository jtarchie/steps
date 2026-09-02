package main

// Hot-reloading the pipeline file.
//
// `steps web` read its YAML once, at startup: an edit did nothing until the
// process was restarted, which drops whatever was running. These drive the
// watcher from the outside — edit the file, and assert the daemon is serving
// the new configuration and recording which one each run executed.

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/web"
)

// reloadPipeline writes a pipeline whose job names are the caller's, so an
// edit is visible on the pages the daemon serves.
func reloadPipeline(t *testing.T, path string, jobs ...string) {
	t.Helper()

	body := &strings.Builder{}
	body.WriteString("jobs:\n")

	for _, job := range jobs {
		body.WriteString("- name: " + job + "\n  plan:\n  - task: compile\n    inputs: []\n    run: echo " + job + "\n")
	}

	writePipelineFile(t, path, body.String())
}

// servedRevision is the configuration the daemon has recorded as current —
// what a run started now would pin.
func servedRevision(t *testing.T, target *web.Pipeline) string {
	t.Helper()

	// Pinned with what the daemon is serving right now, which is what a job
	// admitted at this moment would be handed — the run row is written from
	// the CONFIG, not from anything the store remembers on its own.
	err := target.Store.StartRun(t.Context(),
		"probe-"+time.Now().Format("150405.000000000"), "build", "/tmp/ws",
		target.Config().Revision.SHA)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	rows, err := target.Store.ListRuns(t.Context(), "build", 1)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(rows) == 0 {
		t.Fatal("no run to read a revision from")
	}

	return rows[0].ConfigSHA
}

// TestReloadServesTheEditedConfig is the headline: save the file, and the
// daemon serves what it now says without being restarted — and a run started
// afterwards pins the new configuration while the earlier one keeps naming
// what it actually executed.
func TestReloadServesTheEditedConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	reloadPipeline(t, path, "build")

	server, target := webServerFor(t, path)
	watcher := newConfigWatcher(target, VarFlags{}, HistoryFlags{})

	before := servedRevision(t, target)

	reloadPipeline(t, path, "build", "deploy")

	swapped, err := watcher.check(t.Context())
	if err != nil {
		t.Fatalf("check after a valid edit: %v", err)
	}

	if !swapped {
		t.Fatal("an edited file did not swap the configuration")
	}

	// The serving surface, which is the half a restart used to be needed for.
	code, board := webGet(t, server, "/p/"+target.Slug)
	if code != 200 {
		t.Fatalf("jobs board = %d", code)
	}

	if !strings.Contains(board, "deploy") {
		t.Errorf("the board does not list the job the edit added:\n%s", board)
	}

	// And the store half: what a run started now would say it executed.
	after := servedRevision(t, target)
	if after == before {
		t.Errorf("a run after the swap pinned the configuration from before it (%s)", after)
	}

	// An unchanged file swaps nothing, however often it is checked: a daemon
	// that minted a revision per poll would grow the database with time
	// rather than with change.
	swapped, err = watcher.check(t.Context())
	if err != nil {
		t.Fatalf("check with no edit: %v", err)
	}

	if swapped {
		t.Error("an unchanged file reported a swap")
	}
}

// TestReloadSweepsTheConfigurationItSuperseded: an operator iterating on a
// pipeline with the daemon watching mints a multi-kilobyte row per distinct
// save, and none of those saves has to run anything. Leaving them for a run
// prune means keeping every autosave until some job passes run_history:,
// which is not a bound at all.
func TestReloadSweepsTheConfigurationItSuperseded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	reloadPipeline(t, path, "build")

	target := webPipelineWithVars(t, path, VarFlags{})
	watcher := newConfigWatcher(target, VarFlags{}, HistoryFlags{})

	superseded := target.Config().Revision.SHA

	reloadPipeline(t, path, "build", "deploy")

	swapped, err := watcher.check(t.Context())
	if err != nil || !swapped {
		t.Fatalf("check = (%v, %v), want a swap", swapped, err)
	}

	_, found, err := target.Store.FindRevision(t.Context(), superseded)
	if err != nil {
		t.Fatalf("FindRevision: %v", err)
	}

	if found {
		t.Error("the configuration the swap replaced is still stored, though nothing ever ran under it")
	}

	// And the one now being served is kept, which is the exemption the sweep
	// leans on: the next run admitted will name it.
	_, found, err = target.Store.FindRevision(t.Context(), target.Config().Revision.SHA)
	if err != nil {
		t.Fatalf("FindRevision: %v", err)
	}

	if !found {
		t.Error("the swap swept the configuration it had just started serving")
	}
}

// TestReloadKeepsAConfigurationSomethingRan is the other side of it: a sweep
// that reclaimed rows a run still points at would break the run page's whole
// promise, and the foreign key would refuse it anyway.
func TestReloadKeepsAConfigurationSomethingRan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	reloadPipeline(t, path, "build")

	target := webPipelineWithVars(t, path, VarFlags{})
	watcher := newConfigWatcher(target, VarFlags{}, HistoryFlags{})

	ran := target.Config().Revision.SHA

	err := target.Store.StartRun(t.Context(), "run-one", "build", "/tmp/ws", ran)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	reloadPipeline(t, path, "build", "deploy")

	_, err = watcher.check(t.Context())
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	_, found, err := target.Store.FindRevision(t.Context(), ran)
	if err != nil {
		t.Fatalf("FindRevision: %v", err)
	}

	if !found {
		t.Error("the swap swept a configuration a recorded run says it executed")
	}
}

// TestReloadHoldsTheOldConfigWhenTheEditIsInvalid pins the gate: a file that
// does not load must not take the daemon down with it, and the previous
// configuration keeps serving until the file is fixed.
func TestReloadHoldsTheOldConfigWhenTheEditIsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	reloadPipeline(t, path, "build")

	server, target := webServerFor(t, path)
	watcher := newConfigWatcher(target, VarFlags{}, HistoryFlags{})

	before := servedRevision(t, target)

	// A job whose plan names a resource nothing declares: it parses, and
	// static validation refuses it. Exactly what the gate is for — a swap
	// that only checked the YAML parsed would take this one.
	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - get: nowhere
`)

	swapped, err := watcher.check(t.Context())
	if err == nil {
		t.Fatal("an invalid edit was accepted silently")
	}

	if swapped {
		t.Error("an invalid edit swapped the configuration")
	}

	// Still serving what it was.
	code, board := webGet(t, server, "/p/"+target.Slug)
	if code != 200 {
		t.Fatalf("jobs board = %d", code)
	}

	if !strings.Contains(board, "build") {
		t.Errorf("the held configuration is not being served:\n%s", board)
	}

	if after := servedRevision(t, target); after != before {
		t.Errorf("a run after the refused edit pinned %s, want the held %s", after, before)
	}

	// And the reader is told, rather than left reading a page that silently
	// disagrees with the file on disk.
	if !strings.Contains(board, "nowhere") {
		t.Errorf("the page does not say why the file on disk is not being served:\n%s", board)
	}
}

// TestReloadHoldsAConfigThatWouldNotRun is the gate being deeper than the
// parse, which is the whole difference between the chosen bar and a
// syntax-only one. Both edits below LOAD: one describes a step consuming an
// artifact nothing produces, the other names an API key this machine does not
// have. A swap that took either would serve a configuration whose first run
// dies — the failure the gate exists to keep out of the daemon.
func TestReloadHoldsAConfigThatWouldNotRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	reloadPipeline(t, path, "build")

	_, target := webServerFor(t, path)
	watcher := newConfigWatcher(target, VarFlags{}, HistoryFlags{})

	before := servedRevision(t, target)

	for name, body := range map[string]string{
		"a step consuming an artifact nothing produces": `
jobs:
- name: build
  plan:
  - task: compile
    inputs: [nothing-makes-this]
    run: echo built
`,
		"an api key this machine does not have": `
agents:
- name: reviewer
  source:
    model: openai/gpt-4o
    api_key_env: STEPS_TEST_KEY_THAT_IS_NOT_SET
jobs:
- name: build
  plan:
  - agent: reviewer
    messages: ["hi"]
`,
	} {
		writePipelineFile(t, path, body)

		swapped, err := watcher.check(t.Context())
		if err == nil {
			t.Errorf("%s: accepted", name)
		}

		if swapped {
			t.Errorf("%s: swapped in", name)
		}

		if after := servedRevision(t, target); after != before {
			t.Errorf("%s: the served configuration moved to %s", name, after)
		}
	}
}

// TestReloadRefusesAWorkspaceThisMachineCannotProvide: the provider is built
// and validated once, at startup, so an edited `workspace:` used to be
// accepted by a gate that never looked at it — including one this machine
// cannot provide at all, which then rendered on every page while runs went on
// using the boot provider.
func TestReloadRefusesAWorkspaceThisMachineCannotProvide(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	reloadPipeline(t, path, "build")

	_, target := webServerFor(t, path)
	watcher := newConfigWatcher(target, VarFlags{}, HistoryFlags{})

	before := target.Config().Revision.SHA

	// btrfs over a directory that is not a btrfs filesystem: refused on
	// Linux by the provider and on every other platform by the platform.
	writePipelineFile(t, path, `
workspace:
  strategy: btrfs
  root: `+filepath.Join(dir, "not-btrfs")+`
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run: echo built
`)

	swapped, err := watcher.check(t.Context())
	if err == nil {
		t.Fatal("a workspace this machine cannot provide was accepted")
	}

	if swapped {
		t.Error("the unusable workspace was swapped in")
	}

	if got := target.Config().Revision.SHA; got != before {
		t.Errorf("the served configuration moved to %s", got)
	}

	if !strings.Contains(target.Held(), "workspace") {
		t.Errorf("the page does not say the workspace is the problem: %q", target.Held())
	}
}

// TestReloadSaysAChangedWorkspaceNeedsARestart: the gate validates an edited
// workspace: and does not adopt it, because the provider handed to every run
// is built once. Saying so is the difference between a stated limitation and
// a silent one.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestReloadSaysAChangedWorkspaceNeedsARestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	reloadPipeline(t, path, "build")

	_, target := webServerFor(t, path)
	watcher := newConfigWatcher(target, VarFlags{}, HistoryFlags{})

	writePipelineFile(t, path, `
workspace:
  strategy: copy
  root: `+filepath.Join(dir, "elsewhere")+`
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run: echo built
`)

	var (
		swapped bool
		err     error
	)

	out := captureStdout(t, func() {
		swapped, err = watcher.check(t.Context())
	})

	if err != nil || !swapped {
		t.Fatalf("check = (%v, %v), want a swap", swapped, err)
	}

	if !strings.Contains(out, "restart to run under it") {
		t.Errorf("a changed workspace: said nothing about the provider still being the boot one:\n%s", out)
	}
}

// TestReloadFollowsAnIncludedFile is the answer to what the CONFIG column was
// getting wrong.
//
// A run_file: or a system_file: decides what a step executes, and the
// revision used to be a hash of the pipeline YAML alone — taken before the
// loader resolved the includes. So editing ci/build.sh changed every run of
// that job while `steps runs` showed the same configuration for both, a
// failed run's page said nothing had changed, and the /config/:sha page
// served a source whose run_file: line pointed at bytes nobody recorded. The
// feature's headline question answered no for the edit that broke the build.
func TestReloadFollowsAnIncludedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")
	script := filepath.Join(dir, "build.sh")

	writePipelineFile(t, script, "echo one\n")
	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run_file: build.sh
`)

	target := webPipelineWithVars(t, path, VarFlags{})
	watcher := newConfigWatcher(target, VarFlags{}, HistoryFlags{})

	before := target.Config().Revision.SHA

	// The pipeline file is untouched; only the script it names changes.
	writePipelineFile(t, script, "echo two\n")

	swapped, err := watcher.check(t.Context())
	if err != nil {
		t.Fatalf("check after an include edit: %v", err)
	}

	if !swapped {
		t.Fatal("an edited include did not change the configuration")
	}

	if got := target.Config().Revision.SHA; got == before {
		t.Errorf("the revision is unchanged (%s) though the step now runs different code", got)
	}

	// And the served configuration is the edited one: a swap that reported a
	// change and served the old parse would be worse than not swapping.
	if got := target.Config().Jobs[0].Plan[0].Run; !strings.Contains(got, "two") {
		t.Errorf("the served configuration still runs %q", got)
	}
}

// TestReloadIsQuietWhenAnIncludeIsRewrittenUnchanged: the hash is over the
// include's CONTENT, so an editor that rewrites a file byte-identically is
// not a change — the same promise the pipeline file itself makes.
func TestReloadIsQuietWhenAnIncludeIsRewrittenUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")
	script := filepath.Join(dir, "build.sh")

	writePipelineFile(t, script, "echo one\n")
	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run_file: build.sh
`)

	target := webPipelineWithVars(t, path, VarFlags{})
	watcher := newConfigWatcher(target, VarFlags{}, HistoryFlags{})

	writePipelineFile(t, script, "echo one\n")

	swapped, err := watcher.check(t.Context())
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	if swapped {
		t.Error("rewriting an include with the same bytes reported a change")
	}
}

// TestReloadNoticesTwoIncludesSwappingContents: a pipeline with several
// includes is one configuration, and moving code between two of them changes
// what both steps run.
//
// It does NOT pin the paths being in the digest — the bodies are folded in
// sorted-path order, so the concatenation alone already tells this case
// apart. Nothing reachable distinguishes the two; see withIncludes.
func TestReloadNoticesTwoIncludesSwappingContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")
	first := filepath.Join(dir, "first.sh")
	second := filepath.Join(dir, "second.sh")

	writePipelineFile(t, first, "echo one\n")
	writePipelineFile(t, second, "echo two\n")
	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: one
    inputs: []
    run_file: first.sh
  - task: two
    inputs: []
    run_file: second.sh
`)

	target := webPipelineWithVars(t, path, VarFlags{})
	watcher := newConfigWatcher(target, VarFlags{}, HistoryFlags{})

	before := target.Config().Revision.SHA

	// The same two bodies, now behind the other name — so every step runs
	// something different and the bytes on disk, taken as a set, are equal.
	writePipelineFile(t, first, "echo two\n")
	writePipelineFile(t, second, "echo one\n")

	swapped, err := watcher.check(t.Context())
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	if !swapped || target.Config().Revision.SHA == before {
		t.Error("two includes exchanging contents was not a change")
	}
}

// TestReloadWatchesTheVarsFile pins the watch set: ((var)) substitution
// happens BEFORE the parse, so a vars file is part of the configuration —
// watching only the YAML would serve one thing while the operator's files say
// another, with nothing reporting the difference.
func TestReloadWatchesTheVarsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")
	varsPath := filepath.Join(dir, "vars.yml")

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run: echo ((greeting))
`)
	writePipelineFile(t, varsPath, "greeting: hello\n")

	target := webPipelineWithVars(t, path, VarFlags{VarsFile: varsPath})
	watcher := newConfigWatcher(target, VarFlags{VarsFile: varsPath}, HistoryFlags{})

	before := servedRevision(t, target)

	writePipelineFile(t, varsPath, "greeting: goodbye\n")

	swapped, err := watcher.check(t.Context())
	if err != nil {
		t.Fatalf("check after a vars edit: %v", err)
	}

	if !swapped {
		t.Fatal("an edited vars file did not swap the configuration")
	}

	if after := servedRevision(t, target); after == before {
		t.Errorf("a vars edit left the configuration at %s", after)
	}
}

// TestReloadStartsPollingATriggerAnEditAdded is the seam between the reload
// and the poll loop, and it replaces a stated limitation rather than
// documenting one.
//
// Whether a pipeline had anything to poll used to be decided once, at
// startup, so a pipeline that gained its FIRST `trigger: true` get was served
// with the new configuration and checked by nothing until a restart. The
// decision is now the loop's, taken per configuration.
//
// Driven through the real daemon: this is exactly the kind of claim that
// passes when asserted against a watcher and a poller separately.
func TestReloadStartsPollingATriggerAnEditAdded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")
	versions := filepath.Join(dir, "versions.json")
	log := filepath.Join(dir, "ran.log")

	writePipelineFile(t, versions, `[{"n":"1"}]`)

	// Nothing to poll: one job, run by hand.
	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run: echo built
`)

	served := startWeb(t, []string{path}, "--interval", "100ms")
	defer served.stop(t)

	// The edit that gives it something to check.
	writePipelineFile(t, path, `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: cat `+versions+`
    in: "true"
resources:
- name: items
  type: feed
  source: {}
jobs:
- name: build
  plan:
  - get: items
    trigger: true
  - task: compile
    inputs: [items]
    run: echo built >> `+log+`
`)

	// The run the poller enqueued and the drain executed — end to end, from a
	// file that was saved while the daemon was already running.
	waitForFile(t, log)
}

// TestWatchStopsWithItsContext is the loop around check: it applies edits on
// its own, and it lets go when the daemon does. Without the second half the
// root package's goleak TestMain fails, which is the point of asserting it
// rather than trusting it.
func TestWatchStopsWithItsContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	reloadPipeline(t, path, "build")

	server, target := webServerFor(t, path)
	watcher := newConfigWatcher(target, VarFlags{}, HistoryFlags{})

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})

	go func() {
		defer close(done)

		watcher.Watch(ctx, 10*time.Millisecond)
	}()

	reloadPipeline(t, path, "build", "deploy")

	waitForPage(t, server, target, "deploy")

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch loop outlived its context")
	}
}

// TestWatchKeepsGoingAfterABadSave is the other half of the loop: a file left
// broken must not stop the watcher, or fixing it would need the restart the
// whole feature exists to avoid.
func TestWatchKeepsGoingAfterABadSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	reloadPipeline(t, path, "build")

	server, target := webServerFor(t, path)
	watcher := newConfigWatcher(target, VarFlags{}, HistoryFlags{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		watcher.Watch(ctx, 10*time.Millisecond)
	}()

	writePipelineFile(t, path, "jobs: [ this is not a job ]\n")
	waitForPage(t, server, target, "the file on disk is not being served")

	// Fixed, by the same loop that refused it.
	reloadPipeline(t, path, "build", "deploy")
	waitForPage(t, server, target, "deploy")

	if held := target.Held(); held != "" {
		t.Errorf("a good save left the complaint standing: %s", held)
	}

	cancel()
	<-done
}

// waitForPage polls the pipeline page until it says what the caller expects,
// which is how a loop driven by a ticker is asserted without reaching into
// its timing.
func waitForPage(t *testing.T, server *web.Server, target *web.Pipeline, want string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if strings.Contains(secondReturn(webGet(t, server, "/p/"+target.Slug)), want) {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("the page never said %q", want)
}

// secondReturn is webGet's body, for a loop that only cares about the body.
func secondReturn(_ int, body string) string { return body }

// TestWebActuallyWatchesTheFileItWasStartedWith crosses the seam the rest of
// this file stops short of: every test above drives configWatcher.check by
// hand, so deleting the one line in WebCmd.serve that starts a watcher at all
// shipped a daemon that never reloads with the whole suite green.
//
// Through run(["web", ...]) — the real command, a real listener, a real edit —
// because that line is the only thing between a working watcher and a
// feature nobody has.
func TestWebActuallyWatchesTheFileItWasStartedWith(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	reloadPipeline(t, path, "build")

	served := startWeb(t, []string{path})
	defer served.stop(t)

	reloadPipeline(t, path, "build", "deploy")

	waitForServedPage(t, served, "/p/"+web.Slugify(path), "deploy")
}

// TestReloadKeepsTheRetentionLimitsTheCommandLineAsked is the flag a swap
// used to eat.
//
// --run-history is applied to the Config the daemon parsed, in place, and
// nothing else ever applied it — so a watcher that replaced that object
// reverted the limit to the built-in default, silently, at the first tick,
// with or without an edit. The daemon then kept 100 runs of history on a box
// whose operator had asked for a handful.
func TestReloadKeepsTheRetentionLimitsTheCommandLineAsked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	reloadPipeline(t, path, "build")

	const asked = 5

	target := webPipelineWithVars(t, path, VarFlags{})
	history := HistoryFlags{RunHistory: asked, VersionHistory: asked}
	history.Apply(target.Config())

	watcher := newConfigWatcher(target, VarFlags{}, history)

	// The unchanged file first: this is the tick that fires one second after
	// every startup, edit or no edit.
	_, err := watcher.check(t.Context())
	if err != nil {
		t.Fatalf("check with no edit: %v", err)
	}

	if got := target.Config().RunHistoryLimit(); got != asked {
		t.Errorf("an unedited file reset the run history limit to %d, want %d", got, asked)
	}

	// And an actual edit, which parses a Config the flags were never applied
	// to at all.
	reloadPipeline(t, path, "build", "deploy")

	swapped, err := watcher.check(t.Context())
	if err != nil {
		t.Fatalf("check after an edit: %v", err)
	}

	if !swapped {
		t.Fatal("the edit did not swap")
	}

	if got := target.Config().RunHistoryLimit(); got != asked {
		t.Errorf("the swapped configuration reports a run history limit of %d, want %d", got, asked)
	}

	if got := target.Config().VersionHistoryLimit(); got != asked {
		t.Errorf("the swapped configuration reports a version history limit of %d, want %d", got, asked)
	}
}

// TestReloadResyncsTheQueueLimits is the other half of "the swap is immediate
// for the next job the queue admits": ClaimNextJob decides in SQL, from
// tables mirrored out of the configuration, so a swap that left them alone
// served a page promising a serial: the queue went on ignoring.
func TestReloadResyncsTheQueueLimits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	reloadPipeline(t, path, "build", "deploy")

	target := webPipelineWithVars(t, path, VarFlags{})
	web.PrepareQueue(t.Context(), target)

	watcher := newConfigWatcher(target, VarFlags{}, HistoryFlags{})

	// The edit an operator makes when two jobs must stop overlapping.
	writePipelineFile(t, path, "jobs:\n"+
		"- name: build\n  serial_groups: [release]\n  plan:\n  - task: compile\n    inputs: []\n    run: echo build\n"+
		"- name: deploy\n  serial_groups: [release]\n  plan:\n  - task: ship\n    inputs: []\n    run: echo deploy\n")

	swapped, err := watcher.check(t.Context())
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	if !swapped {
		t.Fatal("the edit did not swap")
	}

	// Asserted through ADMISSION rather than through the mirrored table: two
	// jobs the edit put in one serial group must not be claimed at once,
	// which is the behaviour the operator asked for and the only thing the
	// mirror is for.
	for _, jobName := range []string{"build", "deploy"} {
		err = target.Store.EnqueueJob(t.Context(), jobName, "test")
		if err != nil {
			t.Fatalf("EnqueueJob: %v", err)
		}
	}

	claimed := 0

	for range 2 {
		_, _, ok, err := target.Store.ClaimNextJob(t.Context())
		if err != nil {
			t.Fatalf("ClaimNextJob: %v", err)
		}

		if ok {
			claimed++
		}
	}

	if claimed != 1 {
		t.Errorf("a serial group added by an edit admitted %d builds at once, want 1", claimed)
	}
}

// waitForServedPage polls a real backgrounded daemon's page until it says
// what the caller expects.
func waitForServedPage(t *testing.T, served *webProcess, path, want string) {
	t.Helper()

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(15 * time.Second)

	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+served.addr+path, nil)
		if err != nil {
			t.Fatal(err)
		}

		resp, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if strings.Contains(string(body), want) {
				return
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("the served page %s never said %q", path, want)
}

// TestReloadAdoptsASaveThatDeletesAnInclude: the cheap probe re-reads the
// includes the SERVED configuration resolved, which is a guess about a file
// the new pipeline may no longer name. Treating that read's failure as a
// verdict wedged the daemon: the save was held, so the configuration never
// swapped, so the stale include list was re-read and failed again — every
// tick, until a restart, for a pipeline that loads perfectly.
func TestReloadAdoptsASaveThatDeletesAnInclude(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")
	script := filepath.Join(dir, "build.sh")

	writePipelineFile(t, script, "echo one\n")
	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run_file: build.sh
`)

	target := webPipelineWithVars(t, path, VarFlags{})
	watcher := newConfigWatcher(target, VarFlags{}, HistoryFlags{})

	// The save an operator actually makes: inline the script, and delete it.
	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run: echo one
`)

	err := os.Remove(script)
	if err != nil {
		t.Fatal(err)
	}

	swapped, err := watcher.check(t.Context())
	if err != nil {
		t.Fatalf("check after an include was deleted: %v", err)
	}

	if !swapped {
		t.Fatal("the daemon refused a pipeline that no longer includes the deleted file")
	}

	if held := target.Held(); held != "" {
		t.Errorf("the pages complain about a file the pipeline no longer names: %s", held)
	}

	if got := target.Config().Revision.Includes; len(got) != 0 {
		t.Errorf("the served configuration still lists includes %v", got)
	}
}

// TestReloadDoesNotSweepTheLiveWorkspaceRoot: validating an edited workspace:
// builds a second provider, and a provider's Validate() sweeps every build
// directory under its root with no ownership check. Pointed at a durable
// root: — the one this daemon is already building in — that deletes the tree
// of whatever is running, once a second for as long as the block stays
// unusable.
func TestReloadDoesNotSweepTheLiveWorkspaceRoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")
	root := filepath.Join(dir, "ws")

	// A build directory of the shape the sweep looks for, with a run's work
	// inside it.
	live := filepath.Join(root, "b-live-build")

	err := os.MkdirAll(live, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	writePipelineFile(t, filepath.Join(live, "artifact"), "in flight\n")

	writePipelineFile(t, path, `
workspace:
  strategy: copy
  root: `+root+`
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run: echo one
`)

	target := webPipelineWithVars(t, path, VarFlags{})
	watcher := newConfigWatcher(target, VarFlags{}, HistoryFlags{})

	// Any other field of the block moving is enough: the root stays the one
	// the daemon is using.
	writePipelineFile(t, path, `
workspace:
  strategy: copy
  root: `+root+`
  cache:
    resources: true
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run: echo one
`)

	_, err = watcher.check(t.Context())
	if err != nil {
		t.Fatalf("check after a workspace edit: %v", err)
	}

	_, err = os.Stat(filepath.Join(live, "artifact"))
	if err != nil {
		t.Errorf("validating an edited workspace: deleted the build in flight under the same root: %v", err)
	}
}
