package pipeline

// Which configuration a run says it executed.
//
// The revision used to live on the store HANDLE and the configuration
// travelled as a pointer, and the two are read minutes apart: RunJob takes
// the config, then validates placement, acquires leases, pulls images and
// preflights before its row is written. A reload anywhere in that window made
// the run record a configuration it never ran — and the run page then says
// "the configuration changed" about a run where nothing moved.

import (
	"context"
	"testing"
)

// TestRunRecordsTheConfigItWasHanded is the seam: the daemon has already
// loaded a newer configuration, and a job still executing the older one
// records the older one.
//
// The swap is arranged BEFORE the run rather than raced against it, which is
// the same defect without the timing: whether the row comes from the config
// the job was handed or from the newest thing the handle has seen is exactly
// what separates a correct record from a confident lie.
func TestRunRecordsTheConfigItWasHanded(t *testing.T) {
	t.Parallel()

	cfg, job, st, provider := fixtureFrom(t, `
jobs:
  - name: build
    plan:
      - task: work
        inputs: []
        run: echo built
`)

	defer func() { _ = st.Close() }()
	defer func() { _ = provider.Close() }()

	err := st.RecordRevision(t.Context(), cfg.Revision.SHA, cfg.Revision.Source)
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	// The reload: a different configuration is now the newest this pipeline
	// has loaded. The job below is still the one that was queued against the
	// old one.
	err = st.RecordRevision(t.Context(), "sha-swapped-in", "jobs: []\n")
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	err = RunJob(context.Background(), cfg, job, nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	rows, err := st.ListRuns(t.Context(), "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("got %d runs, want 1", len(rows))
	}

	if rows[0].ConfigSHA != cfg.Revision.SHA {
		t.Errorf("the run recorded configuration %q, but executed %q", rows[0].ConfigSHA, cfg.Revision.SHA)
	}
}

// TestRunRecordsAConfigASweepAlreadyReclaimed is the other half of the same
// window, and the one that survives an edit landing MID-BUILD.
//
// A run's row is written after placement, leases, image pulls and preflight —
// long after the job was handed its configuration. A reload in that window
// adopts a newer one and sweeps every revision no run references yet, which is
// exactly what this one still is. The sha alone then resolves to nothing, and
// the run that was executing across the edit is the only one to record no
// configuration at all.
func TestRunRecordsAConfigASweepAlreadyReclaimed(t *testing.T) {
	t.Parallel()

	cfg, job, st, provider := fixtureFrom(t, `
jobs:
  - name: build
    plan:
      - task: work
        inputs: []
        run: echo built
`)

	defer func() { _ = st.Close() }()
	defer func() { _ = provider.Close() }()

	// The daemon reloads: a newer configuration is interned, and the sweep
	// reclaims the one this job is holding because nothing has run under it.
	err := st.RecordRevision(t.Context(), "sha-swapped-in", "jobs: []\n")
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	err = st.PruneRevisions(t.Context())
	if err != nil {
		t.Fatalf("PruneRevisions: %v", err)
	}

	err = RunJob(context.Background(), cfg, job, nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	rows, err := st.ListRuns(t.Context(), "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("got %d runs, want 1", len(rows))
	}

	if rows[0].ConfigSHA != cfg.Revision.SHA {
		t.Errorf("the run recorded configuration %q, but executed %q", rows[0].ConfigSHA, cfg.Revision.SHA)
	}
}
