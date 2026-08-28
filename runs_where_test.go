package main

// What `steps runs --where` says, and about which run.
//
// The rendering itself is web.PlacementView's, tested once in internal/web —
// the browser and the terminal draw these rows through the same type. What is
// only testable here is what the CLI does AROUND those rows: which run it
// reports on, and the marker a terminal has no colour for.

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// whereFixture writes a pipeline and a state database beside it, with the
// caller's rows already in it. No pipeline is executed: `steps runs` reads the
// database and never loads the YAML, and every case here is about a run that
// cannot be produced by running one.
func whereFixture(t *testing.T, record func(context.Context, *store.Store)) string {
	t.Helper()

	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: compile
    run: "true"
`)

	st, err := store.OpenStore(statePath(path, ""), pipelineName(path))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	record(context.Background(), st)

	err = st.Close()
	if err != nil {
		t.Fatalf("close state store: %v", err)
	}

	return path
}

// TestWhereWillNotVouchForARunItDoesNotHave.
//
// RunPlacements is pipeline-scoped, so a typo — or a run belonging to another
// pipeline in a shared state file — reads back as zero rows, exactly like a
// run that placed nothing. Reporting that as "ran every step on this machine"
// is a positive claim about a run this pipeline has never seen, made with
// exit 0.
func TestWhereWillNotVouchForARunItDoesNotHave(t *testing.T) {
	path := whereFixture(t, func(ctx context.Context, st *store.Store) {
		err := st.StartRun(ctx, "mid-flight", "build", "")
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
	})

	var err error

	out := captureStdout(t, func() {
		err = run([]string{"runs", path, "--where", "--run", "never-happened"})
	})
	if err != nil {
		t.Fatalf("steps runs --where --run: %v", err)
	}

	if strings.Contains(out, "ran every step") {
		t.Errorf("--where vouched for a run this pipeline does not have:\n%s", out)
	}

	if !strings.Contains(out, "never-happened") {
		t.Errorf("--where does not say which run it could not find:\n%s", out)
	}

	// A run still in flight records its placements as its steps finish, so
	// the same past-tense claim is just as wrong about one that exists.
	out = captureStdout(t, func() {
		err = run([]string{"runs", path, "--where", "--run", "mid-flight"})
	})
	if err != nil {
		t.Fatalf("steps runs --where --run: %v", err)
	}

	if strings.Contains(out, "ran every step") {
		t.Errorf("--where spoke in the past tense about a running run:\n%s", out)
	}
}

// TestWhereMarksAMemoryWorkdir is the drift this renderer was merged to end:
// the run page warned about a tmpfs workdir in the colour it warns about
// everything, and the terminal — where someone debugging a placed step looks
// FIRST — printed it as an ordinary disk.
//
// tmpfs on a worker is RAM: the pushed binary and the step's tree spend the
// machine's memory, and a reboot loses both.
func TestWhereMarksAMemoryWorkdir(t *testing.T) {
	hash := strings.Repeat("d", 64)

	path := whereFixture(t, func(ctx context.Context, st *store.Store) {
		err := st.StartRun(ctx, "in-memory", "build", "")
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}

		err = st.RecordNode(ctx, store.NodeRecord{
			Hash: hash, Kind: "task", StepIndex: 0, Resource: "compile",
			Content: map[string]any{"body": "x"},
		}, "build", "succeeded", nil, nil)
		if err != nil {
			t.Fatalf("RecordNode: %v", err)
		}

		err = st.RecordPlacement(ctx, store.Placement{
			RunID: "in-memory", StepIndex: 0, StepName: "compile", JobName: "build",
			NodeHash: hash, Slot: hash, Tag: "gpu", Address: "ssh://box",
			GOOS: "linux", GOARCH: "arm64",
			Workdir: "/tmp/steps/work", FSType: "tmpfs", FSFree: 848 << 20,
			BytesSent: 4096,
		})
		if err != nil {
			t.Fatalf("RecordPlacement: %v", err)
		}

		err = st.FinishRun(ctx, "in-memory", "succeeded")
		if err != nil {
			t.Fatalf("FinishRun: %v", err)
		}
	})

	var err error

	out := captureStdout(t, func() {
		err = run([]string{"runs", path, "--where", "--run", "in-memory"})
	})
	if err != nil {
		t.Fatalf("steps runs --where --run: %v", err)
	}

	if !strings.Contains(out, "tmpfs (848.0 MiB free) [RAM]") {
		t.Errorf("--where reports a tmpfs workdir as an ordinary disk:\n%s", out)
	}

	if !strings.Contains(out, "memory, not disk") {
		t.Errorf("--where marks the row and never says what the marker means:\n%s", out)
	}
}
