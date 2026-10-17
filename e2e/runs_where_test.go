package e2e

// What `steps runs --where` says, and about which run.
//
// The rendering itself is web.PlacementView's, tested once in internal/web —
// the browser and the terminal draw these rows through the same type. What is
// only testable here is what the CLI does AROUND those rows: which run it
// reports on, and the marker a terminal has no colour for.

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store"
)

// whereFixture writes a pipeline and a state database beside it, with the
// caller's rows already in it. No pipeline is executed: `steps runs` reads the
// database and never loads the YAML, and every case here is about a run that
// cannot be produced by running one.
func whereFixture(t *testing.T, record func(context.Context, store.Store)) string {
	t.Helper()

	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: compile
    run: "true"
`)

	st := openStoreFor(t, path)

	var err error

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
	path := whereFixture(t, func(ctx context.Context, st store.Store) {
		err := st.StartRun(ctx, store.NewRun{ID: "mid-flight", JobName: "build"})
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
	})

	var err error

	out := captureStdout(t, func() {
		err = cli.Run(append([]string{"runs", "where", "never-happened"}, readArgs(path)...))
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
		err = cli.Run(append([]string{"runs", "where", "mid-flight"}, readArgs(path)...))
	})
	if err != nil {
		t.Fatalf("steps runs --where --run: %v", err)
	}

	if strings.Contains(out, "ran every step") {
		t.Errorf("--where spoke in the past tense about a running run:\n%s", out)
	}
}

// TestWhereNamesWhoTheStepRanAs: the run page showed the uid:gid a step ran as and the terminal did not. A worker that named none prints "-", never an invented 0:0, which would read as root.
func TestWhereNamesWhoTheStepRanAs(t *testing.T) {
	root := 0
	hash := strings.Repeat("e", 64)
	other := strings.Repeat("f", 64)

	path := whereFixture(t, func(ctx context.Context, st store.Store) {
		recordPlacedRun(ctx, t, st, "as-who", []store.Placement{
			{StepName: "on-ssh", NodeHash: hash, Address: "ssh://box", UID: &root, GID: &root},
			{StepName: "in-docker", NodeHash: other, Address: "docker+ssh://box", Image: "alpine:3"},
		})
	})

	var err error

	out := captureStdout(t, func() {
		err = cli.Run(append([]string{"runs", "where", "as-who"}, readArgs(path)...))
	})
	if err != nil {
		t.Fatalf("steps runs where: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "IDENTITY") {
		t.Fatalf("steps runs where printed:\n%s\nwant a header with IDENTITY and two rows", out)
	}

	if fields := strings.Fields(lines[1]); !strings.HasPrefix(lines[1], "on-ssh") || !slices.Contains(fields, "0:0") {
		t.Errorf("the ssh row does not name uid:gid 0:0:\n%s", out)
	}

	if fields := strings.Fields(lines[2]); !strings.HasPrefix(lines[2], "in-docker") || !slices.Contains(fields, "-") || slices.Contains(fields, "0:0") {
		t.Errorf("the docker row does not say it named no identity:\n%s", out)
	}
}

// recordPlacedRun records a finished run of build whose steps ran where placements say, one step per placement.
func recordPlacedRun(ctx context.Context, t *testing.T, st store.Store, runID string, placements []store.Placement) {
	t.Helper()

	err := st.StartRun(ctx, store.NewRun{ID: runID, JobName: "build"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	for i, placement := range placements {
		err = st.RecordNode(ctx, store.NodeRecord{
			Hash: placement.NodeHash, Kind: "task", StepIndex: i, Resource: placement.StepName,
			Content: map[string]any{"body": placement.StepName},
		}, "build", "succeeded", nil, nil)
		if err != nil {
			t.Fatalf("RecordNode: %v", err)
		}

		placement.RunID, placement.StepIndex, placement.JobName = runID, i, "build"
		placement.Slot, placement.Tag, placement.GOOS, placement.GOARCH = placement.NodeHash, "box", "linux", "arm64"

		err = st.RecordPlacement(ctx, placement)
		if err != nil {
			t.Fatalf("RecordPlacement: %v", err)
		}
	}

	err = st.FinishRun(ctx, runID, "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
}
