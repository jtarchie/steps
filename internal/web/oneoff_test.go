package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// The run reads are a wire format a CLI of another version decodes, so their keys are pinned here rather than following store field names, and the daemon's workspace path never leaves it.
func TestRunReadsAnswerAStableWireFormat(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	const workspace = "/daemon/host/secret-workspace"

	err := pipeline.Store.StartRun(ctx, store.NewRun{ID: "run-1", JobName: "build", Workspace: workspace, CreatedBy: "user:ann"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = pipeline.Store.AppendRunEvent(ctx, store.RunEventRow{
		RunID: "run-1", Type: "step.finished", StepIndex: 2, StepName: "test", StepKind: "task", StepID: 3, ParentStepID: 1,
		Status: "succeeded", Hash: "h", Text: "ok", Name: "n", Detail: "d", DurationMS: 5, Worker: "w", At: time.Unix(1, 0),
	})
	if err != nil {
		t.Fatalf("AppendRunEvent: %v", err)
	}

	err = pipeline.Store.FinishRun(ctx, "run-1", store.RunSucceeded)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	const run = `{"ConfigSHA":"","CreatedBy":"user:ann","FinishedAt":"TIME","ID":"run-1","JobName":"build","ParentRunID":"","RerunOf":"","StartedAt":"TIME","Status":"succeeded"}`

	for path, want := range map[string]string{
		"/api/pipelines/demo/runs?job=build":    "[" + run + "]",
		"/api/pipelines/demo/runs/run-1":        run,
		"/api/pipelines/demo/runs/run-1/events": `[{"At":"TIME","Detail":"d","DurationMS":5,"Hash":"h","Name":"n","ParentStepID":1,"RunID":"run-1","Seq":1,"Status":"succeeded","StepID":3,"StepIndex":2,"StepKind":"task","StepName":"test","Text":"ok","Type":"step.finished","Worker":"w"}]`,
	} {
		status, body := get(t, server, path)
		if status != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, status, body)
		}

		if strings.Contains(body, workspace) {
			t.Errorf("GET %s hands out the daemon's workspace path: %s", path, body)
		}

		got := normalizeTimes(t, body)
		if got != want {
			t.Errorf("GET %s\n got %s\nwant %s", path, got, want)
		}
	}
}

// normalizeTimes re-encodes an answer with sorted keys and every timestamp replaced, since those are the clock's and not the format's.
func normalizeTimes(t *testing.T, body string) string {
	t.Helper()

	var value any

	err := json.Unmarshal([]byte(body), &value)
	if err != nil {
		t.Fatalf("not JSON: %v: %s", err, body)
	}

	objects, isList := value.([]any)
	if !isList {
		objects = []any{value}
	}

	for _, object := range objects {
		fields, _ := object.(map[string]any)
		for _, key := range []string{"StartedAt", "FinishedAt", "At"} {
			if stamp, found := fields[key].(string); found && stamp != "" {
				fields[key] = "TIME"
			}
		}
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	return string(encoded)
}
