package store

// What a build costs on disk. The retention, interning and cascade work in
// this package was motivated by a real database (a Slack-bot pipeline polling
// overnight), and every claim about it is measured here rather than reasoned
// about: sqlite's dbstat virtual table reports the actual page bytes each
// table and index occupies, so "this change shrank the footprint" is an
// assertion instead of an argument.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The synthetic build below reproduces the SHAPE and the BYTE SIZES of the
// pipeline that motivated this work — six steps whose per-node content ranges
// from a 218-byte shell task to a 2.7KB agent step carrying its whole prompt
// and tool definitions, twelve run events, one agent-usage row and one
// transcript. The numbers matter: the whole point of interning content is that
// the big fields are byte-identical across builds while a version field of a
// few dozen bytes is not, and a generator with uniformly tiny content would
// make an interning win look like a rounding error.
const (
	// agentPromptBytes is the size of the one field that dominates a node's
	// content and never varies between builds.
	agentPromptBytes = 2_300
	// exprScriptBytes is the templated shell/expr body a get and a put each
	// carry — also identical build to build.
	exprScriptBytes = 400
	// transcriptBytes is one agent conversation's persisted transcript. Small
	// next to a real one on purpose: the test asserts the CAP, and a generator
	// that already exceeded it could not tell a working cap from a broken one.
	transcriptBytes = 8_000
)

// syntheticStep is one step of the modelled plan.
//
// versioned is the field that makes the measurement honest, and the whole reason
// interning pays: only a get's content holds the version it fetched. An agent's
// content is its prompt and tool definitions, a task's is its script, a put's is
// its expr body — none of which mention a version, so all three are
// byte-identical in every build. Verified against the database that motivated
// this: 12 nodes, 7 distinct contents, and the two that differed were both gets
// of the resource that had moved.
type syntheticStep struct {
	kind, name string
	invariant  int
	result     int
	versioned  bool
}

var syntheticPlan = []syntheticStep{
	{"get", "mentions", exprScriptBytes, 0, true},
	// Two checkouts that only move when someone pushes: stable across a burst of
	// builds, which is the common case and must not be modelled as if every
	// build refetched something new.
	{"get", "backerkit", exprScriptBytes, 0, false},
	{"get", "brain", exprScriptBytes, 0, false},
	{"agent", "responder", agentPromptBytes, 3_700, false},
	{"task", "address", 218, 0, false},
	{"put", "reply", exprScriptBytes, 50, false},
}

// syntheticStepRecords writes the per-step rows that hang off a node: the
// resume record and the pair of events a step publishes.
func syntheticStepRecords(
	ctx context.Context, t *testing.T, store *Store,
	runID string, index int, step syntheticStep, hash string,
) {
	t.Helper()

	err := store.RecordRunStep(ctx, runID, index, step.name)
	if err != nil {
		t.Fatalf("RecordRunStep: %v", err)
	}

	for _, eventType := range []string{"step_started", "step_finished"} {
		err = store.AppendRunEvent(ctx, RunEventRow{
			RunID: runID, Type: eventType, StepIndex: index,
			StepName: step.name, StepKind: step.kind, Status: "succeeded",
			Hash: hash, DurationMS: 1_234, At: time.Now(),
		})
		if err != nil {
			t.Fatalf("AppendRunEvent: %v", err)
		}
	}
}

// syntheticBuild writes one build's worth of rows: a run, its steps, its
// six-node chain, its events, its usage and its transcript. Deliberately
// written through the public Store methods rather than raw SQL — the footprint
// being measured is the one production code actually produces, including every
// column those methods fill in.
// runIDFor is a synthetic run's id, unique across the pipelines that may share
// one state file. runs.id is a global primary key — two pipelines minting the
// same id would upsert onto one row rather than record two runs — which is why
// production ids are random (pipeline.NewRunID) and why this one carries the
// pipeline.
func runIDFor(store *Store, build int) string {
	if store.pipelineID == 1 {
		return fmt.Sprintf("RUN%05d", build)
	}

	return fmt.Sprintf("RUN%d-%05d", store.pipelineID, build)
}

func syntheticBuild(ctx context.Context, t *testing.T, store *Store, jobName string, build int) {
	t.Helper()

	runID := runIDFor(store, build)
	version := fmt.Sprintf(`{"channel":"C0BQ88M07NV","ts":"17869%05d.021829"}`, build)

	// Every build is backdated to its own minute, because a test that writes
	// them all inside one wall-clock second cannot tell a working retention pass
	// from a broken one: nodes are stamped to the second, and the floor a prune
	// compares against is the oldest retained run's start. The first version of
	// this generator did write them all at once, and it reported a footprint
	// growing 3.4x under a cap while claiming the cap was applied.
	defer func() { backdateBuild(ctx, t, store, runID, build) }()

	err := store.StartRun(ctx, runID, jobName, "/tmp/steps-ws-"+runID+"/b-"+runID+"-1-"+jobName)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// The chain is linear and each node's parent is the one before it, which is
	// what a plan of plain steps produces.
	parent := ""
	hashes := make([]string, 0, 6)

	for index, step := range syntheticPlan {
		hash := fmt.Sprintf("%064x", int(store.pipelineID)*1_000_000+build*100+index)

		// The invariant part: a prompt, an expr script, a tool list. Identical in
		// every build, which is what interning collapses.
		content := map[string]any{"body": strings.Repeat("p", step.invariant)}
		if step.versioned {
			content["version"] = version
		}

		var result map[string]any
		if step.result > 0 {
			result = map[string]any{"output": strings.Repeat("r", step.result)}
		}

		err = store.RecordNode(ctx, NodeRecord{
			Hash: hash, ParentHash: parent, Kind: step.kind,
			StepIndex: index, Resource: step.name, Content: content,
		}, jobName, "succeeded", result, nil)
		if err != nil {
			t.Fatalf("RecordNode: %v", err)
		}

		hashes = append(hashes, hash)
		parent = hash

		syntheticStepRecords(ctx, t, store, runID, index, step, hash)
	}

	err = store.RecordAgentUsage(ctx, AgentUsage{
		RunID: runID, StepIndex: 3, StepName: "responder", JobName: jobName,
		NodeHash: hashes[3], ModelReq: "haiku",
		Prompt: 1_014_963, Completion: 4_140, Total: 1_019_103,
		FinishReason: "stop", DurationMS: 54_456,
	})
	if err != nil {
		t.Fatalf("RecordAgentUsage: %v", err)
	}

	// One placed step per build, so the measured footprint is a build that
	// used a worker — the case that costs the most to record.
	instance := "i-0123456789abcdef0"
	uid, gid := 0, 0

	err = store.RecordPlacement(ctx, Placement{
		RunID: runID, StepIndex: 1, StepName: "unit", JobName: jobName,
		NodeHash: hashes[1], Slot: hashes[1], Tag: "linux-arm64",
		Address: "aws://" + instance, InstanceID: &instance,
		GOOS: "linux", GOARCH: "arm64",
		Workdir: "/var/tmp/steps/001-90319-83c7c0781e2ab799/work",
		FSType:  "btrfs", FSFree: 41_083_355_136,
		UID: &uid, GID: &gid,
		Image: "public.ecr.aws/docker/library/golang:1.25", BytesSent: 67_108_864,
	})
	if err != nil {
		t.Fatalf("RecordPlacement: %v", err)
	}

	syntheticQuestion(ctx, t, store, runID, jobName)

	err = store.SaveNodeTranscript(ctx, hashes[3], transcriptJSON(transcriptBytes))
	if err != nil {
		t.Fatalf("SaveNodeTranscript: %v", err)
	}

	err = store.FinishRun(ctx, runID, "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
}

// syntheticQuestion records one answered question per build, so the measured
// footprint prices a build whose agent asked its end user something — and so
// the orphan sweep has a table with rows in it to ask about.
func syntheticQuestion(ctx context.Context, t *testing.T, store *Store, runID, jobName string) {
	t.Helper()

	question, _, err := store.AskQuestion(ctx, Question{
		RunID: runID, JobName: jobName, AgentName: "responder",
		Question: "Is this release a major or a minor bump?",
		Options:  []string{"major", "minor", "patch"},
		Default:  "patch",
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	err = store.AnswerQuestion(ctx, question.ID, "minor", "jtarchie")
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
}

// buildEpoch is the wall-clock instant build 1 is backdated to; each later
// build is one minute after it. Fixed rather than relative to now, so a failure
// names the same timestamps every run.
var buildEpoch = time.Date(2026, time.August, 16, 0, 0, 0, 0, time.UTC)

// backdateBuild rewrites one build's timestamps so a database of N builds looks
// like it accumulated over N minutes.
//
// Written as direct UPDATEs rather than by making the store's clock injectable:
// a package-level clock variable is shared state, and these tests run in
// parallel. The columns are stamped in the same two formats production uses —
// whole seconds for nodes, nanoseconds for runs — because the mismatch between
// them is precisely what pruneNodes has to survive.
func backdateBuild(ctx context.Context, t *testing.T, store *Store, runID string, build int) {
	t.Helper()

	at := buildEpoch.Add(time.Duration(build) * time.Minute)

	for _, update := range []struct {
		query string
		args  []any
	}{
		{`UPDATE runs SET started_at = ?, finished_at = ? WHERE id = ?`,
			[]any{at.Format(time.RFC3339Nano), at.Add(time.Second).Format(time.RFC3339Nano), runID}},
		{`UPDATE run_events SET created_at = ? WHERE run_id = ?`,
			[]any{at.Format(time.RFC3339Nano), runID}},
		{`UPDATE nodes SET created_at = ?
		    WHERE pipeline_id = ? AND hash IN (SELECT hash FROM run_events WHERE run_id = ?)`,
			[]any{at.Format(time.RFC3339), store.pipelineID, runID}},
	} {
		_, err := store.db.ExecContext(ctx, update.query, update.args...)
		if err != nil {
			t.Fatalf("backdate %s: %v", runID, err)
		}
	}
}

// transcriptJSON builds a transcript of roughly the requested size, in the
// shape internal/agent persists (a JSON array of typed events).
func transcriptJSON(size int) string {
	events := []map[string]string{{"type": "text", "text": strings.Repeat("t", size)}}

	encoded, err := json.Marshal(events)
	if err != nil {
		panic(err)
	}

	return string(encoded)
}

// tableBytes is what dbstat reports each table and index occupies, in bytes.
//
// Read through a fresh connection with a checkpoint first: dbstat sees the
// database FILE, so uncheckpointed WAL pages would be invisible and every
// measurement would read low by however much was still in the log.
func tableBytes(ctx context.Context, t *testing.T, store *Store) map[string]int64 {
	t.Helper()

	_, err := store.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	if err != nil {
		t.Fatalf("wal_checkpoint: %v", err)
	}

	rows, err := store.db.QueryContext(ctx,
		`SELECT name, SUM(pgsize) FROM dbstat GROUP BY name`)
	if err != nil {
		t.Fatalf("dbstat: %v", err)
	}

	defer func() { _ = rows.Close() }()

	sizes := map[string]int64{}

	for rows.Next() {
		var (
			name  string
			bytes int64
		)

		err = rows.Scan(&name, &bytes)
		if err != nil {
			t.Fatalf("dbstat: %v", err)
		}

		sizes[name] = bytes
	}

	err = rows.Err()
	if err != nil {
		t.Fatalf("dbstat: %v", err)
	}

	return sizes
}

// logFootprint prints the per-table byte table, largest first, so a run of
// this test is also the report behind any claim about what shrank.
func logFootprint(t *testing.T, label string, sizes map[string]int64) {
	t.Helper()

	names := make([]string, 0, len(sizes))
	for name := range sizes {
		names = append(names, name)
	}

	sort.Slice(names, func(i, j int) bool { return sizes[names[i]] > sizes[names[j]] })

	var total int64

	report := &strings.Builder{}

	for _, name := range names {
		total += sizes[name]

		if sizes[name] >= 4_096 {
			fmt.Fprintf(report, "\n  %-32s %9d", name, sizes[name])
		}
	}

	t.Logf("%s: %d bytes total (tables/indexes over one page)%s", label, total, report)
}

func totalBytes(sizes map[string]int64) int64 {
	var total int64
	for _, size := range sizes {
		total += size
	}

	return total
}

func countRows(ctx context.Context, t *testing.T, store *Store, table string) int {
	t.Helper()

	var count int

	// The table name is a literal from this test, never caller input.
	err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count)
	if err != nil {
		t.Fatalf("count %s: %v", table, err)
	}

	return count
}

// TestFootprintPerBuildIsBounded is the headline measurement: with retention
// on, a database that has run hundreds of builds is no bigger than one that
// has run the cap.
//
// It is the assertion the whole retention change exists to make true. Before
// it, resource_versions was the only table in the schema with a prune path and
// every run-scoped table grew forever — a pipeline answering a hundred Slack
// mentions a day accumulated about 1.3MB a day, monotonically, and nothing
// ever gave a byte back.
func TestFootprintPerBuildIsBounded(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	// Both measurement points are past the point where the CACHE has filled, which
	// is what makes comparing them meaningful. Retention bounds two things with two
	// different caps — the retained runs, and a larger allowance of cached nodes
	// beyond them (see retention.go) — so the footprint is flat only once the
	// second one has saturated. Measuring at 10 and 60 builds compared a database
	// whose cache was a third full against one whose cache was full, and read the
	// difference as a retention failure.
	const (
		keep      = 10
		saturated = 40
		builds    = 100
		jobName   = "answer-mention"
	)

	buildAndPrune := func(from, to int) {
		for build := from; build <= to; build++ {
			syntheticBuild(ctx, t, store, jobName, build)

			err := store.PruneRuns(ctx, jobName, keep, "")
			if err != nil {
				t.Fatalf("PruneRuns: %v", err)
			}
		}
	}

	buildAndPrune(1, saturated)

	atCap := tableBytes(ctx, t, store)
	logFootprint(t, fmt.Sprintf("after %d builds (cap %d)", saturated, keep), atCap)

	buildAndPrune(saturated+1, builds)

	atSteadyState := tableBytes(ctx, t, store)
	logFootprint(t, fmt.Sprintf("after %d builds (cap %d)", builds, keep), atSteadyState)

	// Six times the builds, and the file must not have grown like it. A little
	// slack for sqlite's own page-allocation lumpiness, not for a per-build
	// leak: growing with builds at all would blow well past this.
	if grown := float64(totalBytes(atSteadyState)) / float64(totalBytes(atCap)); grown > 1.15 {
		t.Errorf("footprint grew %.2fx from %d builds to %d under a cap of %d; retention is not bounding it",
			grown, saturated, builds, keep)
	}

	// And the caches are at their caps rather than still growing, which is the
	// mechanism behind the flatness above.
	for _, bounded := range []struct {
		table string
		limit int
	}{
		{"nodes", keep * nodesPerRetainedRun},
		{"job_runs", keep * chainsPerRetainedRun},
	} {
		if got := countRows(ctx, t, store, bounded.table); got > bounded.limit {
			t.Errorf("%s = %d rows after %d builds, want at most %d", bounded.table, got, builds, bounded.limit)
		}
	}

	// And the reason it did not grow is that the old rows are gone, not that
	// they compressed well.
	if got := countRows(ctx, t, store, "runs"); got != keep {
		t.Errorf("runs = %d rows, want %d — the cap is not being applied", got, keep)
	}

	for _, table := range []string{"run_events", "run_steps", "agent_usage", "run_placements", "node_transcripts", "nodes"} {
		if countRows(ctx, t, store, table) == 0 {
			t.Errorf("%s is empty; the prune took the retained builds with it", table)
		}
	}
}

// TestFootprintNoOrphansSurviveAPrune is the cascade, stated as the property
// it exists to guarantee: after retention runs, nothing references a row that
// is no longer there.
//
// Every one of these was a missing REFERENCES clause. The repo's rule is that
// a column pointing at another table's key declares one with an explicit ON
// DELETE, because without it a delete leaves a silently-orphaned row rather
// than taking its dependents with it — and with nine tables and no cascade,
// retention could not have been written as a DELETE at all.
func TestFootprintNoOrphansSurviveAPrune(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	for build := 1; build <= 12; build++ {
		syntheticBuild(ctx, t, store, "answer-mention", build)
	}

	err := store.PruneRuns(ctx, "answer-mention", 3, "")
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}

	for _, orphan := range []struct{ what, query string }{
		{"run_events rows whose run is gone",
			`SELECT COUNT(*) FROM run_events e WHERE NOT EXISTS (SELECT 1 FROM runs r WHERE r.id = e.run_id)`},
		{"run_steps rows whose run is gone",
			`SELECT COUNT(*) FROM run_steps s WHERE NOT EXISTS (SELECT 1 FROM runs r WHERE r.id = s.run_id)`},
		{"agent_usage rows whose run is gone",
			`SELECT COUNT(*) FROM agent_usage a WHERE NOT EXISTS (SELECT 1 FROM runs r WHERE r.id = a.run_id)`},
		{"agent_usage rows whose node is gone",
			`SELECT COUNT(*) FROM agent_usage a WHERE NOT EXISTS (SELECT 1 FROM nodes n WHERE n.hash = a.node_hash)`},
		{"questions rows whose run is gone",
			`SELECT COUNT(*) FROM questions q WHERE NOT EXISTS (SELECT 1 FROM runs r WHERE r.id = q.run_id)`},
		{"run_placements rows whose run is gone",
			`SELECT COUNT(*) FROM run_placements p WHERE NOT EXISTS (SELECT 1 FROM runs r WHERE r.id = p.run_id)`},
		{"run_placements rows whose node is gone",
			`SELECT COUNT(*) FROM run_placements p WHERE NOT EXISTS (SELECT 1 FROM nodes n WHERE n.hash = p.node_hash)`},
		{"node_transcripts rows whose node is gone",
			`SELECT COUNT(*) FROM node_transcripts t WHERE NOT EXISTS (SELECT 1 FROM nodes n WHERE n.hash = t.hash)`},
		// Only universal because syntheticPlan is a LINEAR chain, where every
		// parent is a step that recorded itself. A plan containing a do: block has
		// children naming a hash no row ever held — the block records nothing of
		// its own — so this is an assertion about retention, not an invariant of
		// the table. TestRetentionKeepsTheDatabaseInternallyConsistent covers the
		// container case through sqlite's own foreign_key_check.
		{"nodes whose parent is gone but still named",
			`SELECT COUNT(*) FROM nodes c WHERE c.parent_hash IS NOT NULL
			   AND NOT EXISTS (SELECT 1 FROM nodes p WHERE p.hash = c.parent_hash)`},
		{"node_content rows no node points at",
			`SELECT COUNT(*) FROM node_content c WHERE NOT EXISTS (SELECT 1 FROM nodes n WHERE n.content_hash = c.content_hash)`},
	} {
		var count int

		err = store.db.QueryRowContext(ctx, orphan.query).Scan(&count)
		if err != nil {
			t.Fatalf("%s: %v", orphan.what, err)
		}

		if count != 0 {
			t.Errorf("%s: %d", orphan.what, count)
		}
	}
}

// TestFootprintInternedContentDoesNotRepeat measures the interning directly:
// a step's content is mostly its prompt and its expr scripts, byte-identical
// in every build, and storing one copy per build was the single largest column
// in the database.
//
// Asserted as a ratio against the one-build case rather than an absolute size,
// so it keeps meaning if the synthetic build's shape changes.
func TestFootprintInternedContentDoesNotRepeat(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	const builds = 40

	for build := 1; build <= builds; build++ {
		syntheticBuild(ctx, t, store, "answer-mention", build)
	}

	var (
		distinct int
		rows     int
	)

	err := store.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM node_content), (SELECT COUNT(*) FROM nodes)`).Scan(&distinct, &rows)
	if err != nil {
		t.Fatalf("count content: %v", err)
	}

	// One get moves per build; the other five steps' content never changes. So
	// a database of N builds should hold about N distinct contents, not 6N — a
	// third is a generous ceiling that still fails loudly if nothing is shared.
	if distinct*3 >= rows {
		t.Errorf("node_content has %d rows for %d nodes; identical content is not being shared", distinct, rows)
	}

	sizes := tableBytes(ctx, t, store)
	logFootprint(t, fmt.Sprintf("%d builds, interned content", builds), sizes)
}

// TestFootprintTranscriptIsCapped: a transcript is the largest single value the
// schema stores, and until now the only bound on one was per tool RESULT. A
// conversation has unboundedly many turns, so a long agent step could write
// megabytes into one row.
func TestFootprintTranscriptIsCapped(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	err := store.RecordNode(ctx, NodeRecord{
		Hash: strings.Repeat("a", 64), Kind: "agent", Resource: "responder",
		Content: map[string]any{"body": "x"},
	}, "job", "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("RecordNode: %v", err)
	}

	err = store.SaveNodeTranscript(ctx, strings.Repeat("a", 64),
		transcriptJSON(MaxTranscriptBytes*3))
	if err != nil {
		t.Fatalf("SaveNodeTranscript: %v", err)
	}

	var stored int

	err = store.db.QueryRowContext(ctx,
		`SELECT LENGTH(transcript) FROM node_transcripts`).Scan(&stored)
	if err != nil {
		t.Fatalf("read transcript length: %v", err)
	}

	if stored > MaxTranscriptBytes {
		t.Errorf("stored transcript is %d bytes, want at most %d", stored, MaxTranscriptBytes)
	}

	// Truncated, not dropped: the head of a long conversation is where the task
	// and the first decisions are, so a reader still gets the part that
	// explains what the step was doing.
	transcript, ok, err := store.NodeTranscript(ctx, strings.Repeat("a", 64))
	if err != nil || !ok {
		t.Fatalf("NodeTranscript: ok=%v err=%v", ok, err)
	}

	if transcript == "" {
		t.Error("a transcript over the cap was stored as nothing at all")
	}

	assertRendersAsTranscript(t, transcript)
}

// assertRendersAsTranscript is the assertion the cap test was missing: a stored
// transcript has to still PARSE.
//
// The first truncation sliced the JSON array at a byte offset and appended a
// plain-text notice, so json.Unmarshal in the renderer failed and every over-cap
// transcript displayed as nothing at all — while a non-empty check passed.
// "Stored something" and "stored something readable" are different claims, and
// only the second one is worth anything.
func assertRendersAsTranscript(t *testing.T, transcript string) {
	t.Helper()

	var events []map[string]any

	err := json.Unmarshal([]byte(transcript), &events)
	if err != nil {
		t.Fatalf("a truncated transcript does not parse as JSON, so nothing can render it: %v", err)
	}

	if len(events) == 0 {
		t.Fatal("a truncated transcript parsed to no events")
	}

	// The last entry says the conversation continued, so a reader who reaches the
	// end is not left thinking the step simply stopped there.
	if last, _ := events[len(events)-1]["text"].(string); !strings.Contains(last, "truncated") {
		t.Errorf("the final event is %q, want a truncation notice", last)
	}
}

// TestTranscriptUnderTheCapIsStoredVerbatim: the common case must be untouched.
// A truncation path that rewrites every transcript would be a silent change to
// what a node page shows for conversations that were never too big.
func TestTranscriptUnderTheCapIsStoredVerbatim(t *testing.T) {
	t.Parallel()

	original := transcriptJSON(1_024)

	if got := truncateTranscript(original); got != original {
		t.Errorf("a transcript under the cap was rewritten:\n got %d bytes\nwant %d", len(got), len(original))
	}
}

// TestTranscriptTruncationSurvivesNonJSON: the cap is a storage bound, not a
// validator. Something that is not a JSON array still has to come back under the
// limit rather than panic or grow.
func TestTranscriptTruncationSurvivesNonJSON(t *testing.T) {
	t.Parallel()

	// Multi-byte runes on purpose: a byte-offset cut can split one in half, which
	// is how the first version could produce invalid UTF-8 as well as invalid JSON.
	got := truncateTranscript(strings.Repeat("日本語", MaxTranscriptBytes))

	if len(got) > MaxTranscriptBytes {
		t.Errorf("non-JSON transcript stored %d bytes, want at most %d", len(got), MaxTranscriptBytes)
	}

	if !utf8.ValidString(got) {
		t.Error("truncation split a multi-byte rune, leaving invalid UTF-8")
	}
}

// TestFootprintPruneKeepsTheNewestRuns pins WHICH runs survive. A retention
// pass that kept an arbitrary N would satisfy every byte assertion above and
// be useless: the run someone wants to look at is the one that just failed.
func TestFootprintPruneKeepsTheNewestRuns(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	for build := 1; build <= 8; build++ {
		syntheticBuild(ctx, t, store, "answer-mention", build)
	}

	// A second job, to prove the cap is per job rather than global — one busy
	// job must not evict a quiet one's only run.
	syntheticBuild(ctx, t, store, "other", 99)

	err := store.PruneRuns(ctx, "answer-mention", 3, "")
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}

	kept, err := store.ListRuns(ctx, "answer-mention", 100)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	got := make([]string, 0, len(kept))
	for _, run := range kept {
		got = append(got, run.ID)
	}

	sort.Strings(got)

	want := []string{"RUN00006", "RUN00007", "RUN00008"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("kept %v, want %v", got, want)
	}

	other, err := store.ListRuns(ctx, "other", 100)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(other) != 1 {
		t.Errorf("the other job kept %d runs, want 1 — the cap is global, not per job", len(other))
	}
}

// TestFootprintPruneIsSafeOnAnEmptyDatabase: retention runs on a schedule, so
// it meets a database with nothing in it constantly.
func TestFootprintPruneIsSafeOnAnEmptyDatabase(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	err := store.PruneRuns(ctx, "nothing-here", 10, "")
	if err != nil {
		t.Fatalf("PruneRuns on an empty database: %v", err)
	}

	// Zero means no limit, the convention every other cap in this repo uses.
	syntheticBuild(ctx, t, store, "job", 1)

	err = store.PruneRuns(ctx, "job", 0, "")
	if err != nil {
		t.Fatalf("PruneRuns with no cap: %v", err)
	}

	if got := countRows(ctx, t, store, "runs"); got != 1 {
		t.Errorf("runs = %d after an uncapped prune, want 1 — zero must mean no limit", got)
	}
}

// TestFootprintForeignKeysAreDeclared holds the repo's own rule against the
// schema: every column that references another table's key declares it, so a
// delete cascades instead of leaving an orphan. Named columns rather than a
// count, so adding a table does not silently satisfy it.
func TestFootprintForeignKeysAreDeclared(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	for _, want := range []struct{ table, column, target, onDelete string }{
		{"run_events", "run_id", "runs", "CASCADE"},
		{"run_steps", "run_id", "runs", "CASCADE"},
		{"agent_usage", "run_id", "runs", "CASCADE"},
		{"questions", "run_id", "runs", "CASCADE"},
		{"agent_usage", "node_hash", "nodes", "CASCADE"},
		{"run_placements", "run_id", "runs", "CASCADE"},
		{"run_placements", "node_hash", "nodes", "CASCADE"},
		{"node_transcripts", "hash", "nodes", "CASCADE"},
		{"nodes", "content_hash", "node_content", "RESTRICT"},
		{"runs", "parent_run_id", "runs", "SET NULL"},
		{"job_versions", "resource_name", "resource_versions", "CASCADE"},
		// Every pipeline-scoped table cascades off the pipelines row, which is
		// what makes forgetting a pipeline one DELETE rather than fourteen.
		// The run-scoped tables are absent on purpose: they reach the pipeline
		// through runs and cascade with it.
		{"nodes", "pipeline_id", "pipelines", "CASCADE"},
		{"job_runs", "pipeline_id", "pipelines", "CASCADE"},
		{"runs", "pipeline_id", "pipelines", "CASCADE"},
		{"resource_checks", "pipeline_id", "pipelines", "CASCADE"},
		{"resource_versions", "pipeline_id", "pipelines", "CASCADE"},
		{"job_version_cursor", "pipeline_id", "pipelines", "CASCADE"},
		{"trigger_queue", "pipeline_id", "pipelines", "CASCADE"},
		{"approvals", "pipeline_id", "pipelines", "CASCADE"},
		{"job_concurrency", "pipeline_id", "pipelines", "CASCADE"},
		{"job_serial_groups", "pipeline_id", "pipelines", "CASCADE"},
		{"job_breaker", "pipeline_id", "pipelines", "CASCADE"},
	} {
		if !hasForeignKey(ctx, t, store, want.table, want.column, want.target, want.onDelete) {
			t.Errorf("%s.%s does not declare REFERENCES %s ... ON DELETE %s",
				want.table, want.column, want.target, want.onDelete)
		}
	}

	// And the one deliberate omission, asserted so it stays deliberate. A
	// container node (in_parallel, race, across, ensemble, do, try) is recorded
	// after the branches that already hashed under it, so a child legitimately
	// precedes its parent — adding this constraint makes every block step fail
	// to record, which is how it was found. pruneNodes nulls dangling links
	// instead.
	if declaresForeignKey(ctx, t, store, "nodes", "parent_hash") {
		t.Error("nodes.parent_hash declares a foreign key; a container node is recorded " +
			"after its branches, so the child exists before the parent and every " +
			"in_parallel/race/ensemble/try step will fail to record")
	}

	// The other deliberate omission. A chain ending in a do:/in_parallel: block
	// has a leaf hash no node row describes — the block records nothing of its
	// own — and cascading this away when a node is reaped would make retention
	// re-run work that succeeded. Aged out by pruneJobRuns instead.
	if declaresForeignKey(ctx, t, store, "job_runs", "root_hash") {
		t.Error("job_runs.root_hash declares a foreign key; a chain whose leaf is a " +
			"container block has no node to point at, and cascading the skip index " +
			"off node retention silently re-runs succeeded work")
	}

	// And the third, for the same shape of reason as nodes.parent_hash: a
	// container publishes its step_finished AFTER the steps that ran inside
	// it, so the child's row precedes the parent's. Self-referential within
	// one table besides, where a cascade would take a whole subtree out on a
	// pass that meant to trim one row. Both rows die with their run either
	// way, via run_id.
	if declaresForeignKey(ctx, t, store, "run_events", "parent_step_id") {
		t.Error("run_events.parent_step_id declares a foreign key; a container's own " +
			"event is written after its children's, so the parent row does not exist " +
			"yet when the child is recorded")
	}
}

func hasForeignKey(ctx context.Context, t *testing.T, store *Store, table, column, target, onDelete string) bool {
	t.Helper()

	// table is a literal from the table above, never caller input.
	rows, err := store.db.QueryContext(ctx, `PRAGMA foreign_key_list(`+table+`)`)
	if err != nil {
		t.Fatalf("foreign_key_list(%s): %v", table, err)
	}

	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			id, seq                    int
			gotTarget, from            string
			gotUpdate, gotDelete, kind string
			toCol                      sql.NullString
		)

		err = rows.Scan(&id, &seq, &gotTarget, &from, &toCol, &gotUpdate, &gotDelete, &kind)
		if err != nil {
			t.Fatalf("foreign_key_list(%s): %v", table, err)
		}

		if from == column && gotTarget == target && gotDelete == onDelete {
			return true
		}
	}

	err = rows.Err()
	if err != nil {
		t.Fatalf("foreign_key_list(%s): %v", table, err)
	}

	return false
}

// declaresForeignKey reports whether a column declares ANY foreign key, whatever
// its target or its ON DELETE action.
//
// Distinct from hasForeignKey on purpose. The negative assertions above are
// "this column must reference nothing", and expressing that as
// !hasForeignKey(..., "SET NULL") only rules out one spelling: adding
// `REFERENCES nodes(hash) ON DELETE CASCADE` to nodes.parent_hash left the test
// green while every in_parallel/race/ensemble/try step failed to record, which is
// the whole regression the assertion exists to catch.
func declaresForeignKey(ctx context.Context, t *testing.T, store *Store, table, column string) bool {
	t.Helper()

	// table is a literal from the caller in this file, never external input.
	rows, err := store.db.QueryContext(ctx, `PRAGMA foreign_key_list(`+table+`)`)
	if err != nil {
		t.Fatalf("foreign_key_list(%s): %v", table, err)
	}

	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			id, seq                     int
			target, from                string
			onUpdate, onDelete, matchOn string
			toCol                       sql.NullString
		)

		err = rows.Scan(&id, &seq, &target, &from, &toCol, &onUpdate, &onDelete, &matchOn)
		if err != nil {
			t.Fatalf("foreign_key_list(%s): %v", table, err)
		}

		if from == column {
			return true
		}
	}

	err = rows.Err()
	if err != nil {
		t.Fatalf("foreign_key_list(%s): %v", table, err)
	}

	return false
}

// TestPruneKeepsTheRunItWasCalledFrom is the resume safety property, and it was
// a real bug: a resumed run reaped itself at the end of its own build.
//
// StartRun upserts without touching started_at — correctly, since that is when
// the run started — so resuming an old run leaves it the OLDEST row. Retention
// then deleted it, cascading away the run_steps a further resume reads and the
// agent_usage a budget continues from, and FindRun answered "no run recorded":
// permanently unresumable, caused by the resume itself.
func TestPruneKeepsTheRunItWasCalledFrom(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	for build := 1; build <= 4; build++ {
		syntheticBuild(ctx, t, store, "job", build)
	}

	// Resume the oldest run, the way `steps run --resume` does.
	const resumed = "RUN00001"

	err := store.ResumeRun(ctx, resumed, "/tmp/ws")
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}

	err = store.PruneRuns(ctx, "job", 3, resumed)
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}

	_, err = store.FindRun(ctx, resumed)
	if err != nil {
		t.Errorf("the run being resumed was deleted by its own prune: %v", err)
	}

	// Its resume record has to survive with it, or the resume restarts from zero.
	steps, err := store.CompletedRunSteps(ctx, resumed)
	if err != nil {
		t.Fatalf("CompletedRunSteps: %v", err)
	}

	if len(steps) == 0 {
		t.Error("the resumed run kept its row but lost its completed steps to the cascade")
	}
}

// TestPruneSpareseARunStillInFlight: a second build of one job (max_in_flight, or
// a watch alongside a browser trigger) must not be reaped by the one that
// finishes first — its own later event and usage inserts would then fail the
// foreign keys those tables now declare.
func TestPruneSparesARunStillInFlight(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	for build := 1; build <= 4; build++ {
		syntheticBuild(ctx, t, store, "job", build)
	}

	// An older run that is still going: started before the others, never finished.
	err := store.StartRun(ctx, "INFLIGHT", "job", "/tmp/ws")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	_, err = store.db.ExecContext(ctx,
		`UPDATE runs SET started_at = ? WHERE id = 'INFLIGHT'`,
		buildEpoch.Format(sortableNano))
	if err != nil {
		t.Fatalf("backdate: %v", err)
	}

	err = store.PruneRuns(ctx, "job", 2, "")
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}

	_, err = store.FindRun(ctx, "INFLIGHT")
	if err != nil {
		t.Errorf("a run still in flight was deleted by another build's prune: %v", err)
	}
}

// TestRunOrderIsTimeOrder pins the stored timestamp format against the trap that
// made retention able to delete the NEWER of two runs.
//
// time.RFC3339Nano trims trailing zeros, so as text '…:00.5Z' sorts after
// '…:00.51Z' and a whole second sorts after every fraction within it. Everything
// here orders these columns as TEXT, and retention DELETES by that order.
func TestRunOrderIsTimeOrder(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)

	// Deliberately the values that break under a trimming layout: a whole second,
	// a single-digit fraction, and a longer fraction just after it.
	ordered := []time.Time{
		base,
		base.Add(50 * time.Millisecond),
		base.Add(500 * time.Millisecond),
		base.Add(500*time.Millisecond + time.Microsecond),
		base.Add(510 * time.Millisecond),
	}

	for i := 1; i < len(ordered); i++ {
		earlier := ordered[i-1].Format(sortableNano)
		later := ordered[i].Format(sortableNano)

		if earlier >= later {
			t.Errorf("%s does not sort before %s, so ORDER BY started_at is not time order",
				earlier, later)
		}
	}

	// And it is still a timestamp the readers can parse back.
	if got := parseTimestamp(ordered[2].Format(sortableNano)); !got.Equal(ordered[2]) {
		t.Errorf("parseTimestamp round trip = %v, want %v", got, ordered[2])
	}
}

// TestFootprintSharedDatabaseCostsWhatItHolds is the measurement for --state:
// several pipelines in one file must cost about what they cost apart, and each
// one's retention must bound its own rows and only its own.
//
// The failure it is aimed at is an unscoped index or an unscoped prune. A
// pipeline column added to a key without the query that reads it being narrowed
// turns every scan into a full-table one and every cap into a shared cap — and
// neither shows up as a wrong answer until a second pipeline exists to be wrong
// about. Growth well past the pipeline count is what that looks like on disk.
func TestFootprintSharedDatabaseCostsWhatItHolds(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")

	const (
		keep      = 10
		builds    = 40
		pipelines = 3
		jobName   = "answer-mention"
	)

	// The same job name and the same plan in every pipeline, deliberately: it
	// is the case where nothing but pipeline_id tells the rows apart.
	alone := mustOpenStore(t, filepath.Join(t.TempDir(), "alone.db"))

	buildAndPrune(ctx, t, alone, jobName, builds, keep)

	oneAlone := totalBytes(tableBytes(ctx, t, alone))
	logFootprint(t, "one pipeline in its own file", tableBytes(ctx, t, alone))

	err := alone.Close()
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	shared := make([]*Store, 0, pipelines)

	for i := range pipelines {
		store, err := OpenStore(path, fmt.Sprintf("pipeline-%d", i))
		if err != nil {
			t.Fatalf("OpenStore: %v", err)
		}

		defer func() { _ = store.Close() }()

		shared = append(shared, store)
	}

	for _, store := range shared {
		buildAndPrune(ctx, t, store, jobName, builds, keep)
	}

	sizes := tableBytes(ctx, t, shared[0])
	logFootprint(t, fmt.Sprintf("%d pipelines in one file", pipelines), sizes)

	// Interning is shared across pipelines (node_content is keyed by a hash OF
	// the content), and these three plans are byte-identical, so the shared
	// file should come in UNDER the naive multiple rather than over it. The
	// ceiling is what catches an unscoped prune or index; the generous floor
	// only catches a pipeline whose rows went missing entirely.
	ratio := float64(totalBytes(sizes)) / float64(oneAlone)
	if ratio > pipelines*1.15 {
		t.Errorf("%d pipelines in one file cost %.2fx one pipeline alone; something is not scoped",
			pipelines, ratio)
	}

	if ratio < 1.0 {
		t.Errorf("%d pipelines cost %.2fx one; rows are being lost across pipelines", pipelines, ratio)
	}

	assertEachPipelineAtItsOwnCap(ctx, t, shared, jobName, keep)
}

// buildAndPrune runs `builds` synthetic builds through one store, pruning to
// `keep` after each, which is what a real build does at the end of RunJob.
func buildAndPrune(ctx context.Context, t *testing.T, store *Store, jobName string, builds, keep int) {
	t.Helper()

	for build := 1; build <= builds; build++ {
		syntheticBuild(ctx, t, store, jobName, build)

		err := store.PruneRuns(ctx, jobName, keep, "")
		if err != nil {
			t.Fatalf("PruneRuns: %v", err)
		}
	}
}

// assertEachPipelineAtItsOwnCap checks that retention bounded every pipeline
// separately. A cap applied globally would leave the file at `keep` runs in
// total rather than `keep` per pipeline, and each pipeline would see a
// fraction of its own history.
func assertEachPipelineAtItsOwnCap(ctx context.Context, t *testing.T, shared []*Store, jobName string, keep int) {
	t.Helper()

	for i, store := range shared {
		runs, err := store.ListRuns(ctx, jobName, keep*len(shared))
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}

		if len(runs) != keep {
			t.Errorf("pipeline-%d kept %d runs, want %d — retention is not per pipeline", i, len(runs), keep)
		}
	}

	want := keep * len(shared)
	if got := countRows(ctx, t, shared[0], "runs"); got != want {
		t.Errorf("the file holds %d runs, want %d — one pipeline's prune reached another's", got, want)
	}
}

// recordFailedStep writes what a step that FAILED leaves behind: its node, and
// the step_finished event carrying no hash at all — which is the whole reason
// an events-only exemption could not see it.
func recordFailedStep(
	ctx context.Context, t *testing.T, store *Store,
	runID, jobName string, index int, hash, name, kind string,
) {
	t.Helper()

	err := store.RecordNode(ctx, NodeRecord{
		Hash: hash, Kind: kind, StepIndex: index,
		Resource: name, Content: map[string]any{"body": name},
	}, jobName, "failed", nil, errors.New("boom"))
	if err != nil {
		t.Fatalf("RecordNode %s: %v", name, err)
	}

	err = store.AppendRunEvent(ctx, RunEventRow{
		RunID: runID, Type: "step_finished", StepIndex: index,
		StepName: name, StepKind: kind, Status: "failed",
		Hash: "", At: time.Now(),
	})
	if err != nil {
		t.Fatalf("AppendRunEvent %s: %v", name, err)
	}
}

// fillNodeCache records later builds' cache entries, which is the pressure that
// makes pruneNodes delete anything at all.
func fillNodeCache(ctx context.Context, t *testing.T, store *Store, jobName string, count int) {
	t.Helper()

	for i := range count {
		err := store.RecordNode(ctx, NodeRecord{
			Hash: hashOf(i + 1), Kind: "task", StepIndex: 0,
			Resource: "later", Content: map[string]any{"body": i},
		}, jobName, "succeeded", nil, nil)
		if err != nil {
			t.Fatalf("RecordNode filler %d: %v", i, err)
		}
	}
}

// TestPruneKeepsWhatASurvivingRunPointsAt is record LOSS rather than orphan
// prevention, and it runs the other way round from the cascade tests above.
//
// run_placements and agent_usage both cascade off nodes, and the NODE cap is a
// different bound from the RUN cap — twenty cached nodes per retained run, of
// which a busy job burns through far more than it keeps runs. pruneNodes
// exempted only the hashes a surviving run's EVENTS name, and a failed step
// publishes step_finished with an empty hash after its placement and its usage
// row are already written under the real one. So a retained run kept its green
// records and lost exactly its red ones — backwards from what someone debugging
// a placed step or paying for a failed agent needs.
func TestPruneKeepsWhatASurvivingRunPointsAt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	const (
		runID   = "FAILED01"
		jobName = "build"
		keep    = 1
	)

	err := store.StartRun(ctx, runID, jobName, "/tmp/ws")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// Two failed steps, so sabotaging either exemption shows up on its own table
	// rather than being carried by the other's node.
	placedHash, agentHash := hashOf(900_001), hashOf(900_002)

	recordFailedStep(ctx, t, store, runID, jobName, 0, placedHash, "unit", "task")
	recordFailedStep(ctx, t, store, runID, jobName, 1, agentHash, "review", "agent")

	err = store.RecordPlacement(ctx, Placement{
		RunID: runID, StepIndex: 0, StepName: "unit", JobName: jobName, NodeHash: placedHash, Slot: placedHash,
		Tag: "spot", Address: "aws://i-0123456789abcdef0", GOOS: "linux", GOARCH: "arm64",
		Workdir: "/var/tmp/w", FSType: "btrfs", FSFree: 1 << 30, BytesSent: 4_096,
	})
	if err != nil {
		t.Fatalf("RecordPlacement: %v", err)
	}

	err = store.RecordAgentUsage(ctx, AgentUsage{
		RunID: runID, StepIndex: 1, StepName: "review", JobName: jobName, NodeHash: agentHash,
		ModelReq: "haiku", Prompt: 1_000, Completion: 100, Total: 1_100,
		FinishReason: "error", DurationMS: 900,
	})
	if err != nil {
		t.Fatalf("RecordAgentUsage: %v", err)
	}

	fillNodeCache(ctx, t, store, jobName, keep*nodesPerRetainedRun+5)

	err = store.PruneRuns(ctx, jobName, keep, runID)
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}

	_, err = store.FindRun(ctx, runID)
	if err != nil {
		t.Fatalf("the run itself was reaped, so this proves nothing about its records: %v", err)
	}

	placements, err := store.RunPlacements(ctx, runID)
	if err != nil {
		t.Fatalf("RunPlacements: %v", err)
	}

	if len(placements) != 1 {
		t.Errorf("a retained run reports %d placements, want 1 — the node cap reaped the record of where its failed step ran",
			len(placements))
	}

	usage, err := store.RunUsage(ctx, runID)
	if err != nil {
		t.Fatalf("RunUsage: %v", err)
	}

	if len(usage) != 1 {
		t.Errorf("a retained run reports %d usage rows, want 1 — the node cap reaped what its failed agent step spent",
			len(usage))
	}
}

// TestPruneStillWorksBesideAHookPlacement is the trap a nullable column sets
// for an anti-join.
//
// node_hash became nullable so a tagged HOOK could record the machine it
// acquired: a hook is deliberately never merkle-hashed, so it has no node to
// be keyed on. The node prune exempts anything a placement still points at,
// and it asks with `hash NOT IN (SELECT node_hash FROM run_placements ...)`.
//
// SQL answers `x NOT IN (...)` with UNKNOWN — never TRUE — the moment the
// subquery yields a single NULL. So one tagged hook, anywhere in the
// pipeline's history, makes that clause match no rows at all and the node
// cache stops being pruned FOREVER. It is the exact bound footprint_test.go
// exists to measure, defeated silently: nothing errors, nothing warns, the
// table just grows.
func TestPruneStillWorksBesideAHookPlacement(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	const (
		runID   = "HOOKRUN1"
		jobName = "build"
		keep    = 1
	)

	err := store.StartRun(ctx, runID, jobName, "/tmp/ws")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// A tagged hook: a real machine, billed, with no node of its own. Slot is
	// its scope label precisely because there is no hash to key on.
	err = store.RecordPlacement(ctx, Placement{
		RunID: runID, StepIndex: 0, StepName: "build", JobName: jobName,
		Slot: `step 0 (task "build") (on_failure hook)`,
		Tag:  "spot", Address: "aws://i-0123456789abcdef0", GOOS: "linux", GOARCH: "arm64",
		Workdir: "/var/tmp/w", FSType: "ext4", FSFree: 1 << 30, BytesSent: 512,
	})
	if err != nil {
		t.Fatalf("RecordPlacement: %v", err)
	}

	overflow := keep*nodesPerRetainedRun + 40
	fillNodeCache(ctx, t, store, jobName, overflow)

	before := countNodes(ctx, t, store, jobName)

	err = store.PruneRuns(ctx, jobName, keep, runID)
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}

	after := countNodes(ctx, t, store, jobName)

	if after >= before {
		t.Errorf("nodes went %d -> %d beside a hook placement; the cache is no longer bounded at all", before, after)
	}

	if after > keep*nodesPerRetainedRun {
		t.Errorf("nodes = %d after pruning, want no more than the cap of %d", after, keep*nodesPerRetainedRun)
	}
}

// countNodes reports how many cache entries a job currently holds.
func countNodes(ctx context.Context, t *testing.T, store *Store, jobName string) int {
	t.Helper()

	var count int

	err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE pipeline_id = ? AND job_name = ?`, store.pipelineID, jobName).Scan(&count)
	if err != nil {
		t.Fatalf("counting nodes: %v", err)
	}

	return count
}
