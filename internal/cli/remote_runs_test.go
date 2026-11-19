package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// A daemon from before the wire types sent untagged store rows, Workspace included; a follower of this version must still read every field it sent.
func TestRemoteRunsReadsADaemonOfTheEarlierWireFormat(t *testing.T) {
	t.Parallel()

	const run = `{"ID":"run-1","JobName":"build","Workspace":"/daemon/ws","Status":"succeeded","StartedAt":"2026-01-02T03:04:05Z","FinishedAt":"2026-01-02T03:04:06Z","ParentRunID":"p","ConfigSHA":"c","RerunOf":"r","CreatedBy":"user:ann"}`

	answers := map[string]string{
		"/api/pipelines/demo/runs/run-1":        run,
		"/api/pipelines/demo/runs":              "[" + run + "]",
		"/api/pipelines/demo/runs/run-1/events": `[{"Seq":4,"RunID":"run-1","Type":"step.finished","StepIndex":2,"StepName":"test","StepKind":"task","StepID":3,"ParentStepID":1,"Status":"succeeded","Hash":"h","Text":"ok","Name":"n","Detail":"d","DurationMS":5,"Worker":"w","At":"2026-01-02T03:04:05Z"}]`,
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(answers[r.URL.Path]))
	}))
	t.Cleanup(server.Close)

	client, err := newDaemonClient(server.URL)
	if err != nil {
		t.Fatalf("newDaemonClient: %v", err)
	}

	remote := remoteRuns{client: client, pipeline: "demo"}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	wantRun := store.RunRow{
		ID: "run-1", JobName: "build", Status: store.RunSucceeded, StartedAt: at, FinishedAt: at.Add(time.Second),
		ParentRunID: "p", ConfigSHA: "c", RerunOf: "r", CreatedBy: "user:ann",
	}

	found, _, err := remote.FindRunRow(t.Context(), "run-1")
	if err != nil || found != wantRun {
		t.Errorf("FindRunRow = %+v, %v; want %+v", found, err, wantRun)
	}

	newest, _, err := remote.newest(t.Context(), "build")
	if err != nil || newest != wantRun {
		t.Errorf("newest = %+v, %v; want %+v", newest, err, wantRun)
	}

	rows, err := remote.RunEvents(t.Context(), "run-1", 0, 10)
	wantEvent := store.RunEventRow{
		Seq: 4, RunID: "run-1", Type: "step.finished", StepIndex: 2, StepName: "test", StepKind: "task", StepID: 3, ParentStepID: 1,
		Status: "succeeded", Hash: "h", Text: "ok", Name: "n", Detail: "d", DurationMS: 5, Worker: "w", At: at,
	}

	if err != nil || len(rows) != 1 || rows[0] != wantEvent {
		t.Errorf("RunEvents = %+v, %v; want [%+v]", rows, err, wantEvent)
	}
}

// Each segment is escaped on its own, so a run id or job name cannot add a segment, a query or a fragment to the route.
func TestPipelinePathEscapesEverySegment(t *testing.T) {
	t.Parallel()

	got := pipelinePath("demo", "runs", "a b/c?d#e%f", "abort")

	const want = "/api/pipelines/demo/runs/a%20b%2Fc%3Fd%23e%25f/abort"
	if got != want {
		t.Errorf("pipelinePath = %s, want %s", got, want)
	}
}
