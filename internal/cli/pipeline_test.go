package cli

// What `steps pipeline` sends, and what it shows the person about to deploy it.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/web"
)

// A name is CONCATENATED into the request path, so anything URL-significant in it names a different pipeline than the one that was typed — and for destroy that is somebody else's history.
func TestAVerbRefusesANameThatWouldAddressAnotherPipeline(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"prod#old", "prod?x=1", "prod/../other", "prod bad", "..", ""} {
		_, err := namedPipeline(name, "destroy")
		if err == nil {
			t.Errorf("steps pipeline destroy accepted %q", name)
		}
	}

	got, err := namedPipeline("prod-2.web_a", "destroy")
	if err != nil {
		t.Fatalf("an ordinary name was refused: %v", err)
	}

	if got != "prod-2.web_a" {
		t.Errorf("namedPipeline gave %q; want the name it was handed", got)
	}
}

// Both or neither is ambiguous about what to stop, so it is refused before anything reaches the daemon — port 1 answers nothing, so a request that went out fails differently.
func TestRunsAbortNamesExactlyOneThingToStop(t *testing.T) {
	t.Parallel()

	for _, cmd := range []RunsAbortCmd{
		{PipelineNameFlag: PipelineNameFlag{Pipeline: "app"}},
		{PipelineNameFlag: PipelineNameFlag{Pipeline: "app"}, RunID: "RUN", Queued: "build"},
	} {
		cmd.Target = "http://127.0.0.1:1"

		err := cmd.Run()
		if err == nil || !strings.Contains(err.Error(), "not both") {
			t.Errorf("run %q queued %q = %v, want refused before anything is sent", cmd.RunID, cmd.Queued, err)
		}
	}
}

// The re-read advice is about a set that raced another; on a refused abort it sends somebody to re-read a configuration nothing is wrong with.
func TestOnlyASetConflictSaysToReRead(t *testing.T) {
	t.Parallel()

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"no"}`))
	}))
	t.Cleanup(daemon.Close)

	client := newDaemonClient(daemon.URL)

	_, err := client.set("app", web.SetRequest{Source: "jobs: []"})
	if err == nil || !strings.Contains(err.Error(), "re-read") {
		t.Errorf("a refused set = %v, want the advice to re-read", err)
	}

	err = client.post("/api/pipelines/app/runs/RUN/abort", http.StatusAccepted)
	if err == nil || strings.Contains(err.Error(), "re-read") {
		t.Errorf("a refused abort = %v, want no advice to re-read", err)
	}
}

// The refusal is decided on the daemon and read at the terminal, so the test that matters crosses the wire: an unbound body, or echo's envelope printed raw, reaches a person as noise instead of what to fix.
func TestARefusalTravelsBackToTheTerminalThatAsked(t *testing.T) {
	t.Parallel()

	held := servingDaemon(t)
	held.server.SetManager(held)

	daemon := httptest.NewServer(held.server.Handler())
	t.Cleanup(daemon.Close)

	client := newDaemonClient(daemon.URL)

	result, err := client.set("app", web.SetRequest{Source: idlePipeline})
	if err != nil || !result.Created {
		t.Fatalf("an acceptable set = %+v, %v; want it created", result, err)
	}

	_, err = client.set("broken", web.SetRequest{Source: "jobs: [ {name: "})
	if err == nil {
		t.Fatal("a set that does not parse was accepted")
	}

	if msg := err.Error(); !strings.Contains(msg, web.ErrRefused.Error()) || strings.Contains(msg, `"message"`) {
		t.Errorf("the terminal was told %q; want the daemon's refusal, unwrapped", msg)
	}
}

// A daemon that took the request and has not answered is still working on it — a rename waiting out a cancelled build's ensure hooks, say — and "start one" sends somebody to start a second daemon beside it.
func TestASlowDaemonIsNotCalledUnreachable(t *testing.T) {
	t.Parallel()

	answer := make(chan struct{})

	daemon := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-answer
	}))

	t.Cleanup(func() {
		close(answer)
		daemon.Close()
	})

	client := newDaemonClient(daemon.URL)
	client.http.Timeout = 50 * time.Millisecond

	err := client.rename("app", "renamed")
	if err == nil || strings.Contains(err.Error(), "start one") || !strings.Contains(err.Error(), "may still be applying") {
		t.Errorf("a daemon that did not answer in time = %v, want it reported busy rather than down", err)
	}

	err = newDaemonClient("http://127.0.0.1:1").rename("app", "renamed")
	if err == nil || !strings.Contains(err.Error(), "start one") {
		t.Errorf("a daemon nothing answers for = %v, want the advice to start one", err)
	}
}

// The proof of the above: the refused shapes are the ones net/http silently re-points at a different path.
func TestTheRefusedNamesAreTheOnesThatMoveTheRequest(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]string{
		"prod#old":  "/api/pipelines/prod",
		"prod?x=1":  "/api/pipelines/prod",
		"prod-2.we": "/api/pipelines/prod-2.we",
	} {
		req, err := http.NewRequest(http.MethodDelete, "http://127.0.0.1:8088/api/pipelines/"+name, nil) //nolint:noctx // parsing only; never sent
		if err != nil {
			t.Fatalf("NewRequest %q: %v", name, err)
		}

		if req.URL.Path != want {
			t.Errorf("%q addressed %q; want %q", name, req.URL.Path, want)
		}
	}
}

// An include is as much of a deploy as the YAML: a run_file: decides what a step executes, so a set that only moved one must not be confirmed against a pipeline reported as unchanged.
func TestTheDiffShownBeforeApplyingNamesAChangedInclude(t *testing.T) {
	t.Parallel()

	same := "jobs:\n- name: deploy\n  plan:\n  - task: go\n    run_file: ci/deploy.sh\n"

	if got := diffSource(same, same); got != "" {
		t.Errorf("an unchanged pipeline file printed %q; want nothing", got)
	}

	got := diffIncludes(
		map[string]string{"ci/deploy.sh": "kubectl apply -f staging/", "ci/old.sh": "gone"},
		map[string]string{"ci/deploy.sh": "kubectl apply -f prod/", "ci/new.sh": "added"},
	)

	for _, want := range []string{"~ ci/deploy.sh", "+ ci/new.sh", "- ci/old.sh"} {
		if !strings.Contains(got, want) {
			t.Errorf("the include diff does not say %q:\n%s", want, got)
		}
	}

	if diffIncludes(map[string]string{"a": "1"}, map[string]string{"a": "1"}) != "" {
		t.Error("an unchanged include was reported as a change")
	}
}

// A broken pipeline is part of what the daemon holds, so the list says so and why — as a report that succeeds, since /api/pipelines is what a script reads.
//
// Not t.Parallel(): captureStdout swaps os.Stdout.
func TestPipelineListShowsABrokenPipelineAndWhy(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"gone","sha":"0123456789abcdef","jobs":0,"broken":"workspace: root is not writable"}]`))
	}))
	t.Cleanup(daemon.Close)

	var err error

	out := captureStdout(t, func() { err = (&PipelineListCmd{TargetFlags{Target: daemon.URL}}).Run() })
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if !strings.Contains(out, "broken") || !strings.Contains(out, "gone is not being served: workspace: root is not writable") {
		t.Errorf("list printed:\n%s\nwant the broken state and its reason", out)
	}
}

// The set that repairs a broken pipeline is usually the same YAML again, and "is new to this daemon; it will be created" is wrong on both counts.
//
// Not t.Parallel(): it swaps os.Stdin and os.Stdout.
func TestSetConfirmsARepairAsARepair(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	_ = writer.Close()

	stdin := os.Stdin
	os.Stdin = reader

	t.Cleanup(func() {
		os.Stdin = stdin
		_ = reader.Close()
	})

	out := captureStdout(t, func() {
		_ = (&PipelineSetCmd{}).confirm("app", web.PipelineConfig{}, false, "workspace: root is not writable", idlePipeline, nil)
	})

	if !strings.Contains(out, "app is not being served (workspace: root is not writable); this set will repair it") || strings.Contains(out, "new to this daemon") {
		t.Errorf("confirm printed:\n%s\nwant it called a repair", out)
	}
}

// A repair is a set like any other, so it is compare-and-set against what the daemon holds: another set landing between this one's read and its upload is refused rather than silently overwritten.
//
// Not t.Parallel(): brokenOnRestart's restart runs with the environment as the test left it.
func TestARepairIsCompareAndSet(t *testing.T) {
	isolateLogins(t)

	restarted, root := brokenOnRestart(t)
	restarted.server.SetManager(restarted)

	daemon := httptest.NewServer(restarted.server.Handler())
	t.Cleanup(daemon.Close)

	client := newDaemonClient(daemon.URL)

	current, _, broken, err := client.get("app")
	if err != nil || broken == "" {
		t.Fatalf("get = %q, %v; want app held broken", broken, err)
	}

	err = os.Chmod(root, 0o700) //nolint:gosec // the root is the test's own
	if err != nil {
		t.Fatal(err)
	}

	_, err = restarted.Set(t.Context(), "app", web.SetRequest{Source: onRoot(root, "", "echo theirs")})
	if err != nil {
		t.Fatalf("the other repair: %v", err)
	}

	_, err = client.set("app", web.SetRequest{Source: onRoot(root, "", "echo mine"), ExpectSHA: current.SHA})
	if err == nil {
		t.Error("a repair read before another set overwrote it, uncompared")
	}
}

// A verb the daemon turned away has to fail the command, or `steps pipeline destroy typo && echo gone` reports a destroy that never happened; not parallel, it captures stdout.
func TestAVerbTheDaemonRefusesFailsTheCommand(t *testing.T) {
	isolateLogins(t)

	held := servingDaemon(t)
	held.server.SetManager(held)

	daemon := httptest.NewServer(held.server.Handler())
	t.Cleanup(daemon.Close)

	setPipeline(t, held, "app", idlePipeline)

	target := TargetFlags{Target: daemon.URL}
	ghost := PipelineNameFlag{Pipeline: "ghost"}

	for verb, cmd := range map[string]interface{ Run() error }{
		"destroy": &PipelineDestroyCmd{TargetFlags: target, PipelineNameFlag: ghost, NonInteractive: true},
		"pause":   &PipelinePauseCmd{TargetFlags: target, PipelineNameFlag: ghost},
		"unpause": &PipelineUnpauseCmd{TargetFlags: target, PipelineNameFlag: ghost},
		"abort":   &RunsAbortCmd{TargetFlags: target, PipelineNameFlag: PipelineNameFlag{Pipeline: "app"}, RunID: "NOSUCHRUN"},
	} {
		var err error

		out := captureStdout(t, func() { err = cmd.Run() })
		if err == nil {
			t.Errorf("%s of something the daemon does not have succeeded, printing %q", verb, out)
		}
	}

	var err error

	out := captureStdout(t, func() {
		err = (&PipelineDestroyCmd{TargetFlags: target, PipelineNameFlag: PipelineNameFlag{Pipeline: "app"}, NonInteractive: true}).Run()
	})
	if err != nil || !strings.Contains(out, "destroyed: app") {
		t.Errorf("destroy app = %v, printing %q; want it destroyed and said so", err, out)
	}
}
