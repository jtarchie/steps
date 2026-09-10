package cli

// What `steps pipeline` sends, and what it shows the person about to deploy it.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
