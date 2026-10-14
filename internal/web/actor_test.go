package web

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestTheRunHeaderSaysWhoStartedIt: a person reads in the foreground, anything automatic and any unverified name in the dim tone, and a run recorded before anyone was says nothing rather than "unknown".
func TestTheRunHeaderSaysWhoStartedIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		by, want string
	}{
		{"local:ops", `by <span class="actor" title="local:ops">ops (local)</span>`},
		{"os:jt", `by <span class="actor quiet" title="os:jt">jt (this machine, unverified)</span>`},
		{"check:repo", `by <span class="actor quiet" title="check:repo">new version of repo</span>`},
	} {
		server, pipeline := testPipeline(t)

		err := pipeline.Store.StartRun(context.Background(), store.NewRun{ID: "run-by", JobName: "build", Workspace: t.TempDir(), CreatedBy: tc.by})
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}

		_, body := get(t, server, "/p/demo/runs/run-by")
		if !strings.Contains(body, tc.want) {
			t.Errorf("created by %q: the header does not carry %s", tc.by, tc.want)
		}
	}

	server, pipeline := testPipeline(t)
	startFinishedRun(t, pipeline, "run-old", "build", "succeeded")

	_, body := get(t, server, "/p/demo/runs/run-old")
	if strings.Contains(body, `class="actor`) || strings.Contains(body, " by ") {
		t.Error("a run nobody recorded a starter for still names one")
	}
}

// TestARerunLinksWhatItRetried: the retry says what it repeated before it says who asked.
func TestARerunLinksWhatItRetried(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	startFinishedRun(t, pipeline, "ORIGINALRUN", "build", "failed")

	err := pipeline.Store.StartRun(ctx, store.NewRun{ID: "RETRY", JobName: "build", Workspace: t.TempDir(), CreatedBy: "local:ops"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = pipeline.Store.RecordRunRerun(ctx, "RETRY", "ORIGINALRUN")
	if err != nil {
		t.Fatalf("RecordRunRerun: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/RETRY")
	if !strings.Contains(body, `rerun of <a class="hash" href="/p/demo/runs/ORIGINALRUN">#`) || !strings.Contains(body, `by <span class="actor" title="local:ops">`) {
		t.Errorf("a rerun's header does not link its original and its requester: %s", body)
	}
}
