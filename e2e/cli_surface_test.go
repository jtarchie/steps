package e2e

// The shape of the command line itself: which verbs exist, and whether a flag
// a command DECLARES is one it actually applies.
//
// The second half is not a hypothetical. --version-history was declared on
// `run` and threaded nowhere, so it silently did nothing there and worked
// only under the watcher — a flag that reads as configured while binding
// nothing.
// Sharing a flag between commands via an embed makes that failure cheaper to
// repeat (one declaration, many commands, each of which must still apply it),
// so the embeds arrive with the tests that would catch it.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store"
)

// TestTopLevelCommandsAreTheDocumentedSet pins the CLI's verb list.
//
// A consolidation that means to remove commands should have to say so here,
// in one place, rather than being counted by hand from a help dump — and a
// command added by accident (an embed pulling in a struct with a cmd tag, a
// copy-paste) is otherwise invisible until somebody reads the help.
func TestTopLevelCommandsAreTheDocumentedSet(t *testing.T) {
	t.Parallel()

	want := map[string]bool{
		"Run": true, "Test": true, "Validate": true,
		"Runs": true, "Plan": true, "MCP": true,
		"Jobs": true, "Approvals": true, "Questions": true,
		"Web": true, "Docs": true,
	}

	got := map[string]bool{}
	grammar := reflect.TypeOf(cli.CLI{})

	for index := range grammar.NumField() {
		field := grammar.Field(index)

		_, isCmd := field.Tag.Lookup("cmd")
		if !isCmd {
			continue
		}

		// _shim is hidden on purpose: it is the remote half of a placed step,
		// not a verb anybody types.
		if _, hidden := field.Tag.Lookup("hidden"); hidden {
			continue
		}

		got[field.Name] = true
	}

	for name := range want {
		if !got[name] {
			t.Errorf("command %s is gone; update this list if that was deliberate", name)
		}
	}

	for name := range got {
		if !want[name] {
			t.Errorf("command %s appeared without being listed here", name)
		}
	}
}

// flagFixture writes a pipeline that any command can be pointed at.
func flagFixture(t *testing.T) string {
	t.Helper()

	return writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run: "true"
`)
}

// TestWorkerFlagAppliesWhereverItIsDeclared.
//
// --worker is declared by four commands and parsed in one place, which is the
// arrangement that lets a command declare it and never call the parser. An
// unparseable worker URL is the cheapest proof that each of them does: the
// command must refuse before it does any work, and a command that never
// applied the flag would sail past it.
func TestWorkerFlagAppliesWhereverItIsDeclared(t *testing.T) {
	t.Parallel()

	path := flagFixture(t)

	for _, args := range [][]string{
		{"run", path, "--job", "build"},
		{"test", path},
		// Port 1 is unbindable as an ordinary user, so a regression here
		// fails in a second rather than serving until the test binary's own
		// timeout — which is how this case behaved when it was first
		// sabotaged to check it worked.
		{"web", path, "--listen", "127.0.0.1:1"},
	} {
		t.Run(args[0], func(t *testing.T) {
			err := cli.Run(append(args, "--worker", "gpu=nope://x"))
			if err == nil {
				t.Fatalf("%s accepted an unparseable --worker, so it never applied the flag", args[0])
			}

			if !strings.Contains(err.Error(), "worker") {
				t.Errorf("%s failed for some other reason: %v", args[0], err)
			}
		})
	}
}

// TestVarsFileAppliesWhereverItIsDeclared is the same proof for the other
// widely shared pair: seven commands declare --var/--vars-file, and all seven
// load the pipeline through the one loader that reads it.
func TestVarsFileAppliesWhereverItIsDeclared(t *testing.T) {
	t.Parallel()

	path := flagFixture(t)
	missing := filepath.Join(t.TempDir(), "absent.yml")

	for _, args := range [][]string{
		{"run", path, "--job", "build"},
		{"test", path},
		{"validate", path},
		{"validate", path, "--live", "--job", "build"},
		{"plan", path, "--job", "build"},
		{"web", path},
	} {
		t.Run(args[0], func(t *testing.T) {
			err := cli.Run(append(args, "--vars-file", missing))
			if err == nil {
				t.Fatalf("%s accepted a --vars-file that does not exist, so it never read one", args[0])
			}

			if !strings.Contains(err.Error(), "vars file") {
				t.Errorf("%s failed for some other reason: %v", args[0], err)
			}
		})
	}
}

// TestGroupedVerbsKeepTheirBareForm.
//
// approvals/approve/reject and questions/answer were one feature spread over
// three top-level verbs each, and `jobs --resume` was a listing that turned
// into a write when you passed a flag. Grouping them costs nothing at the
// prompt only if the bare listing still works — which is what `list` being
// the default subcommand buys, and what would silently regress if a later
// change moved the default.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestGroupedVerbsKeepTheirBareForm(t *testing.T) {
	path := flagFixture(t)

	for _, group := range []struct {
		bare []string
		full []string
		says string
	}{
		{[]string{"approvals", path}, []string{"approvals", "list", path}, "no approvals are waiting"},
		{[]string{"questions", path}, []string{"questions", "list", path}, "no questions are waiting"},
		{[]string{"jobs", path}, []string{"jobs", "list", path}, "no jobs are paused"},
	} {
		t.Run(group.bare[0], func(t *testing.T) {
			for _, args := range [][]string{group.bare, group.full} {
				var err error

				out := captureStdout(t, func() { err = cli.Run(args) })
				if err != nil {
					t.Fatalf("%v: %v", args, err)
				}

				if !strings.Contains(out, group.says) {
					t.Errorf("%v printed %q, want %q", args, out, group.says)
				}
			}
		})
	}
}

// TestRetiredVerbsAreGone: the point of a group is that there is one way in.
// A stale top-level verb that still works is a second grammar to document and
// to keep behaving the same.
func TestRetiredVerbsAreGone(t *testing.T) {
	t.Parallel()

	path := flagFixture(t)

	for _, args := range [][]string{
		{"approve", path, "1"},
		{"reject", path, "1"},
		{"answer", path, "1", "yes"},
		{"jobs", path, "--resume", "build"},
	} {
		t.Run(strings.Join(args[:1], " "), func(t *testing.T) {
			err := cli.Run(args)
			if err == nil {
				t.Fatalf("%v still parses; the old spelling was meant to be gone", args)
			}

			// The grammar must reject it, not the command: an `approve` that
			// still exists and merely fails to find approval 1 is an error
			// too, and is exactly the state this test is here to catch.
			if !strings.Contains(err.Error(), "could not parse flags") {
				t.Errorf("%v was rejected by the command rather than by the grammar: %v", args, err)
			}
		})
	}
}

// TestJobsResumeClearsTheBreaker.
//
// `jobs --resume <name>` was a listing command that wrote when you passed it
// a flag. The subcommand does the same work; this is the proof it does it —
// which the flag form never had, so the mutation was covered by nothing at
// the CLI level at all.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestJobsResumeClearsTheBreaker(t *testing.T) {
	path := flagFixture(t)

	pauseJob(t, path, "build")

	var err error

	out := captureStdout(t, func() {
		err = cli.Run([]string{"jobs", "resume", path, "build"})
	})

	if err != nil {
		t.Fatalf("jobs resume: %v", err)
	}

	if !strings.Contains(out, "resumed: build") {
		t.Errorf("output does not say what was resumed:\n%s", out)
	}

	if jobPaused(t, path, "build") {
		t.Error("the job is still paused, so resume resumed nothing")
	}
}

// pauseJob leaves a job in the state the trigger circuit breaker would
// have left it in: enough consecutive failures to trip the limit.
func pauseJob(t *testing.T, path, job string) {
	t.Helper()

	st, err := store.OpenStore(cli.StatePath(path, ""), cli.PipelineName(path))
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

	if !jobPaused(t, path, job) {
		t.Fatal("fixture did not pause the job")
	}
}

// jobPaused reads the breaker back through a fresh handle, so the answer is
// what landed on disk rather than what a live handle remembers.
func jobPaused(t *testing.T, path, job string) bool {
	t.Helper()

	st, err := store.OpenStore(cli.StatePath(path, ""), cli.PipelineName(path))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	paused, err := st.IsJobPaused(t.Context(), job)
	if err != nil {
		t.Fatalf("IsJobPaused: %v", err)
	}

	return paused
}

// TestJobsResumeRefusesAJobThePipelineDoesNotHave: the name is checked
// against the pipeline, so a typo is a refusal rather than a no-op that
// reports success.
func TestJobsResumeRefusesAJobThePipelineDoesNotHave(t *testing.T) {
	t.Parallel()

	err := cli.Run([]string{"jobs", "resume", flagFixture(t), "buidl"})
	if err == nil {
		t.Fatal("resuming a job the pipeline does not declare was reported as done")
	}
}

// TestRunsCostTakesTheRunAsAnArgument.
//
// --run implied --cost, which is a flag that changes which view runs rather
// than configuring one — the same smell as `jobs --resume`. As a positional
// the deeper view is what you typed, and the old spelling has to be gone for
// there to be one grammar.
func TestRunsCostTakesTheRunAsAnArgument(t *testing.T) {
	t.Parallel()

	path := flagFixture(t)

	err := cli.Run([]string{"runs", "cost", path, "--run", "SOMERUN"})
	if err == nil {
		t.Fatal("--run still parses; the run id is a positional now")
	}

	if !strings.Contains(err.Error(), "could not parse flags") {
		t.Errorf("--run was rejected by the command rather than by the grammar: %v", err)
	}
}

// TestListingsDoNotMintThePipelineTheyWereAskedAbout.
//
// The read-that-writes fix reached `steps runs` and stopped there: approvals,
// questions and jobs still opened their store the writer's way, which
// registers whatever name it is handed. That was worse than the original bug
// it mirrored — one listing against a shared state file minted the phantom
// row that made `steps runs` STOP refusing the same typo, so the fix undid
// itself the moment an operator asked a second question.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestListingsDoNotMintThePipelineTheyWereAskedAbout(t *testing.T) {
	state, first, _ := sharedRunsFixture(t)
	stranger := filepath.Join(filepath.Dir(first), "stranger.yml")

	writePipelineFile(t, stranger, `
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run: "true"
`)

	for _, args := range [][]string{
		{"approvals", stranger, "--state", state},
		{"questions", stranger, "--state", state},
		{"jobs", stranger, "--state", state},
	} {
		t.Run(args[0], func(t *testing.T) {
			var err error

			_ = captureStdout(t, func() { err = cli.Run(args) })
			if err == nil {
				t.Fatalf("%v answered for a pipeline the file has never heard of", args)
			}
		})
	}

	// And the file still holds only what actually ran, which is the property
	// a phantom row destroys for every later command.
	listing := captureStdout(t, func() {
		err := cli.Run([]string{"runs", "--state", state})
		if err != nil {
			t.Errorf("runs --state: %v", err)
		}
	})

	if strings.Contains(listing, "stranger") {
		t.Errorf("a listing registered the pipeline it was asked about:\n%s", listing)
	}
}

// TestListingsAnswerBeforeAStateFileExists: on a fresh checkout there is no
// database, and "nothing is waiting" is the answer — not an error, and not a
// .steps/ directory created by asking.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestListingsAnswerBeforeAStateFileExists(t *testing.T) {
	path := flagFixture(t)

	for _, probe := range []struct {
		verb string
		says string
	}{
		{"approvals", "no approvals are waiting"},
		{"questions", "no questions are waiting"},
		{"jobs", "no jobs are paused"},
	} {
		t.Run(probe.verb, func(t *testing.T) {
			var err error

			out := captureStdout(t, func() { err = cli.Run([]string{probe.verb, path}) })
			if err != nil {
				t.Fatalf("%s on a fresh checkout: %v", probe.verb, err)
			}

			if !strings.Contains(out, probe.says) {
				t.Errorf("%s printed %q, want %q", probe.verb, out, probe.says)
			}
		})
	}

	if fileExists(cli.StatePath(path, "")) {
		t.Error("a listing created the state database it was asked about")
	}
}

// TestReadingADatabaseBeingCreated.
//
// A writer creates the file and stamps the schema into it as two steps, so a
// reader can arrive in between — racing the first-ever `steps run` on a fresh
// checkout is all it takes. It used to answer "the state database was written
// by a different version of steps … delete the file", which is wrong and is
// the worst possible advice: it is the operator's brand new database, being
// written by the run they just started.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestReadingADatabaseBeingCreated(t *testing.T) {
	path := flagFixture(t)
	state := cli.StatePath(path, "")

	err := os.MkdirAll(filepath.Dir(state), 0o750)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// An empty file is exactly what the window looks like from outside.
	err = os.WriteFile(state, nil, 0o600)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, probe := range []struct {
		args []string
		says string
	}{
		{[]string{"runs", "list", path}, "no runs recorded yet"},
		{[]string{"approvals", path}, "no approvals are waiting"},
		{[]string{"questions", path}, "no questions are waiting"},
		{[]string{"jobs", path}, "no jobs are paused"},
		{[]string{"runs", "--state", state}, "no pipelines recorded"},
	} {
		t.Run(strings.Join(probe.args[:2], " "), func(t *testing.T) {
			var runErr error

			out := captureStdout(t, func() { runErr = cli.Run(probe.args) })
			if runErr != nil {
				t.Fatalf("%v: %v", probe.args, runErr)
			}

			if !strings.Contains(out, probe.says) {
				t.Errorf("%v printed %q, want %q", probe.args, out, probe.says)
			}
		})
	}
}

// TestSuccessfulRunClearsTheBreaker.
//
// Running the job by hand is the natural way to confirm a fix, so a manual
// run that succeeds takes the job out of the paused state — otherwise every
// recovery needs a second command nobody remembers. The line that does it had
// no test at all: mutation testing could delete the clearing and every suite
// stayed green, which makes this the THIRD place this one feature was found
// unproven (inert in the daemon, unwired in the one-shot, unasserted here).
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestSuccessfulRunClearsTheBreaker(t *testing.T) {
	path := flagFixture(t)

	pauseJob(t, path, "build")

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if jobPaused(t, path, "build") {
		t.Error("a successful manual run left the job paused; the fix cannot be confirmed by running it")
	}
}

// TestFailedRunLeavesTheBreakerAlone is the other half: only SUCCESS clears
// it. A mutant that cleared on every outcome would make the breaker
// unreachable through the one command most likely to be run against a broken
// job.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestFailedRunLeavesTheBreakerAlone(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run: exit 1
`)

	pauseJob(t, path, "build")

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err == nil {
		t.Fatal("the failing job reported success")
	}

	if !jobPaused(t, path, "build") {
		t.Error("a failed manual run cleared the breaker")
	}
}

// TestStateNoteOnlyCarriesAFlagThatWasGiven.
//
// The follow-up command a listing prints has to name the same database the
// listing read, or it sends the reader to the pipeline's default path and
// tells them nothing is there. The other direction matters too, and is what a
// mutant walked through: with no --state given, appending an empty flag emits
// `steps runs cost pipeline.yml <run> --state ` — a command that does not
// parse.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestStateNoteOnlyCarriesAFlagThatWasGiven(t *testing.T) {
	path := costFixture(t)

	var err error

	out := captureStdout(t, func() { err = cli.Run([]string{"runs", "cost", path}) })
	if err != nil {
		t.Fatalf("runs cost: %v", err)
	}

	if strings.Contains(out, "--state") {
		t.Errorf("the hint names a --state nobody passed:\n%s", out)
	}

	state := cli.StatePath(path, "")

	out = captureStdout(t, func() { err = cli.Run([]string{"runs", "cost", path, "--state", state}) })
	if err != nil {
		t.Fatalf("runs cost --state: %v", err)
	}

	if !strings.Contains(out, "--state "+state) {
		t.Errorf("the hint drops the --state the reader is using:\n%s", out)
	}
}
