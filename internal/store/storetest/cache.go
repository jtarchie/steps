package storetest

// The merkle skip index and the transcripts hung off it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jtarchie/steps/internal/store"
)

// TestHasNodeSucceededIsPerJobAndStatus: an across: cell asks about itself,
// and only a green node of its own job answers yes.
func (s suite) TestHasNodeSucceededIsPerJobAndStatus(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	mustRecordNode(t, st, "build", hashOf(1))

	err := st.RecordNode(ctx, store.NodeRecord{Hash: hashOf(2), Kind: "task", Content: map[string]any{"n": 2}},
		"build", "failed", nil, errors.New("boom"))
	if err != nil {
		t.Fatalf("RecordNode: %v", err)
	}

	for _, probe := range []struct {
		job, hash string
		want      bool
	}{
		{"build", hashOf(1), true},
		{"other", hashOf(1), false},
		{"build", hashOf(2), false},
		{"build", hashOf(3), false},
	} {
		got, err := st.HasNodeSucceeded(ctx, probe.job, probe.hash)
		if err != nil || got != probe.want {
			t.Errorf("HasNodeSucceeded(%q, …%s) = %v, %v; want %v", probe.job, probe.hash[60:], got, err, probe.want)
		}
	}
}

func (s suite) TestListNodes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	record := store.NodeRecord{
		Hash: "node-1", ParentHash: "", Kind: "task", StepIndex: 0,
		Resource: "compile", Content: map[string]any{"run": "make"},
	}

	err := st.RecordNode(ctx, record, "build", "failed", nil, errors.New("boom"))
	if err != nil {
		t.Fatal(err)
	}

	rows, err := st.ListNodes(ctx, "build", 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 1 {
		t.Fatalf("got %d nodes, want 1", len(rows))
	}

	row := rows[0]
	if row.Kind != "task" || row.Resource != "compile" {
		t.Errorf("got kind=%q resource=%q, want task/compile", row.Kind, row.Resource)
	}

	if row.Status != "failed" || row.Error != "boom" {
		t.Errorf("got status=%q error=%q, want failed/boom", row.Status, row.Error)
	}
}

func (s suite) TestListNodesOnAnEmptyStore(t *testing.T) {
	t.Parallel()

	rows, err := s.open(t, "test").ListNodes(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("listing an empty store should not error: %v", err)
	}

	if len(rows) != 0 {
		t.Errorf("got %d rows from an empty store, want 0", len(rows))
	}
}

func (s suite) TestListNodesHonoursItsLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	for _, hash := range []string{"n1", "n2", "n3"} {
		mustRecordNode(t, st, "build", hash)
	}

	all, err := st.ListNodes(ctx, "build", 0)
	if err != nil {
		t.Fatalf("ListNodes(0): %v", err)
	}

	if len(all) != 3 {
		t.Fatalf("ListNodes with limit 0 returned %d nodes, want all 3", len(all))
	}

	two, err := st.ListNodes(ctx, "build", 2)
	if err != nil {
		t.Fatalf("ListNodes(2): %v", err)
	}

	if len(two) != 2 {
		t.Fatalf("ListNodes with limit 2 returned %d nodes", len(two))
	}
}

// TestRecordNodeStoresWhatItWasGiven covers three inversions mutation testing
// found nothing asserting: whether a parent hash is stored or nulled, whether
// a step's result is marshalled at all, and whether the marshalled bytes are
// stored or nulled.
//
// Each of the three is a one-character change — == for !=, != for == — that
// swaps a value for NULL or NULL for a value, and every one of them survived
// the whole suite. The reason they matter is the same reason nodes.parent_hash
// is a deliberate non-foreign-key: the chain is what the cache is read back
// along, and a node whose parent is NULL is a chain that starts over.
func (s suite) TestRecordNodeStoresWhatItWasGiven(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	root := strings.Repeat("a", 64)
	child := strings.Repeat("b", 64)

	err := st.RecordNode(ctx, store.NodeRecord{
		Hash: root, Kind: "get", StepIndex: 0, Resource: "mentions",
		Content: map[string]any{"body": "the first step of the chain"},
	}, "build", "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("RecordNode root: %v", err)
	}

	err = st.RecordNode(ctx, store.NodeRecord{
		Hash: child, ParentHash: root, Kind: "task", StepIndex: 1, Resource: "compile",
		Content: map[string]any{"body": "the second"},
	}, "build", "succeeded", map[string]any{"output": "built"}, nil)
	if err != nil {
		t.Fatalf("RecordNode child: %v", err)
	}

	rows, err := st.NodesByHash(ctx, []string{root, child})
	if err != nil {
		t.Fatalf("NodesByHash: %v", err)
	}

	// The chain, which is the whole reason the column exists.
	if got := rows[child].ParentHash; got != root {
		t.Errorf("child's parent = %q, want the root hash — the chain does not link", got)
	}

	// And a root really has none, so the two are distinguishable. Empty here
	// is the NULL sqlite stores; a hash would mean a chain that loops.
	if got := rows[root].ParentHash; got != "" {
		t.Errorf("root's parent = %q, want nothing", got)
	}

	// A result that was produced is a result that is readable: this is what a
	// cache HIT replays instead of re-running the step.
	if got := rows[child].Result; !strings.Contains(got, "built") {
		t.Errorf("child's result = %q, want what the step produced", got)
	}

	if got := rows[root].Result; got != "" {
		t.Errorf("root's result = %q, want nothing — it produced none", got)
	}
}

// mustRecordNode records a minimal node under a hash, so a row that references
// it has something to point at.
//
// agent_usage.node_hash and node_transcripts.hash are foreign keys into nodes,
// which is what lets retention delete a node and take its dependents with it.
// job_runs.root_hash deliberately is NOT one (see the schema, and the negative
// assertion in footprint_test.go) — but a job_runs row still names a chain leaf,
// and seeding the node it names keeps these fixtures shaped like the runs that
// produce them.
//
// The node is recorded as SUCCEEDED, which is a cache-hit state: a test about
// HasNodeSucceeded or across-cell memoization should record its own nodes rather
// than inherit this one.
func mustRecordNode(t *testing.T, st store.Store, jobName, hash string) {
	t.Helper()

	err := st.RecordNode(context.Background(), store.NodeRecord{
		Hash: hash, Kind: "task", Resource: "step", Content: map[string]any{"hash": hash},
	}, jobName, "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("RecordNode(%q, %q): %v", jobName, hash, err)
	}
}

func mustRecordChainSucceeded(t *testing.T, st store.Store, jobName, rootHash string) {
	t.Helper()

	mustRecordNode(t, st, jobName, rootHash)

	err := st.RecordChainSucceeded(context.Background(), jobName, rootHash)
	if err != nil {
		t.Fatalf("RecordChainSucceeded(%q, %q): %v", jobName, rootHash, err)
	}
}

func mustForgetChain(t *testing.T, st store.Store, jobName, rootHash string) {
	t.Helper()

	err := st.ForgetChain(context.Background(), jobName, rootHash)
	if err != nil {
		t.Fatalf("ForgetChain(%q, %q): %v", jobName, rootHash, err)
	}
}

func (s suite) TestStoreHasSucceededBatch(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	mustRecordChainSucceeded(t, st, "job", "hash1")
	mustForgetChain(t, st, "job", "hash2")
	mustRecordChainSucceeded(t, st, "other-job", "hash1")

	got, err := st.HasSucceededBatch(context.Background(), "job", []string{"hash1", "hash2", "hash3"})
	if err != nil {
		t.Fatalf("HasSucceededBatch: %v", err)
	}

	want := map[string]bool{"hash1": true}
	if len(got) != len(want) || got["hash1"] != want["hash1"] {
		t.Errorf("HasSucceededBatch = %v, want a map with only hash1=true (hash2 failed, hash3 unknown, other-job's hash1 is a different job)", got)
	}

	got, err = st.HasSucceededBatch(context.Background(), "job", nil)
	if err != nil {
		t.Fatalf("HasSucceededBatch(nil): %v", err)
	}

	if len(got) != 0 {
		t.Errorf("HasSucceededBatch(nil) = %v, want an empty map", got)
	}
}

// TestStoreHasSucceededBatchManyHashes exercises the chunked IN (...) query
// path with more root hashes than fit in a single chunk, confirming it
// neither errors nor drops any of the seeded succeeded rows.
func (s suite) TestStoreHasSucceededBatchManyHashes(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	const n = 1500

	hashes := make([]string, n)

	for i := range n {
		hash := fmt.Sprintf("hash-%d", i)
		hashes[i] = hash

		mustRecordChainSucceeded(t, st, "job", hash)
	}

	got, err := st.HasSucceededBatch(context.Background(), "job", hashes)
	if err != nil {
		t.Fatalf("HasSucceededBatch: %v", err)
	}

	if len(got) != n {
		t.Fatalf("HasSucceededBatch returned %d entries, want %d", len(got), n)
	}

	for _, hash := range hashes {
		if !got[hash] {
			t.Errorf("HasSucceededBatch[%q] = false, want true", hash)
		}
	}
}

func (s suite) TestStoreRecordNode(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	ctx := context.Background()

	node := store.NodeRecord{
		Hash:       "abc",
		ParentHash: "",
		Kind:       "get",
		StepIndex:  0,
		Resource:   "thing",
		Content:    map[string]any{"source": map[string]any{"key": "v1"}},
	}

	err := st.RecordNode(ctx, node, "job", "succeeded", map[string]any{"ref": "v1"}, nil)
	if err != nil {
		t.Fatalf("RecordNode: %v", err)
	}

	// Recording the same hash again (an upsert) should not error.
	err = st.RecordNode(ctx, node, "job", "succeeded", map[string]any{"ref": "v1"}, nil)
	if err != nil {
		t.Fatalf("RecordNode (upsert): %v", err)
	}
}

// TestNodeTranscriptRoundTrip covers the transcript store: absent before any
// save, returned verbatim after, and replaced (not duplicated) on a re-save
// under the same hash — the same replace-on-re-record shape nodes has.
func (s suite) TestNodeTranscriptRoundTrip(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	ctx := context.Background()

	_, ok, err := st.NodeTranscript(ctx, "abc123")
	if err != nil {
		t.Fatalf("NodeTranscript (empty): %v", err)
	}

	if ok {
		t.Fatal("expected no transcript before any save")
	}

	mustRecordNode(t, st, "build", "abc123")

	err = st.SaveNodeTranscript(ctx, "abc123", `[{"type":"text","text":"hi"}]`)
	if err != nil {
		t.Fatalf("SaveNodeTranscript: %v", err)
	}

	got, ok, err := st.NodeTranscript(ctx, "abc123")
	if err != nil || !ok {
		t.Fatalf("NodeTranscript: ok=%v err=%v", ok, err)
	}

	if got != `[{"type":"text","text":"hi"}]` {
		t.Errorf("transcript = %q", got)
	}
}

// TestNodeTranscriptReplace pins that a re-save under the same hash replaces
// the row rather than duplicating or erroring — the same replace-on-re-record
// shape nodes has.
func (s suite) TestNodeTranscriptReplace(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	ctx := context.Background()

	mustRecordNode(t, st, "build", "abc123")

	for _, transcript := range []string{`[{"type":"text","text":"hi"}]`, `[{"type":"text","text":"replaced"}]`} {
		err := st.SaveNodeTranscript(ctx, "abc123", transcript)
		if err != nil {
			t.Fatalf("SaveNodeTranscript: %v", err)
		}
	}

	got, ok, err := st.NodeTranscript(ctx, "abc123")
	if err != nil || !ok {
		t.Fatalf("NodeTranscript: ok=%v err=%v", ok, err)
	}

	if got != `[{"type":"text","text":"replaced"}]` {
		t.Errorf("replaced transcript = %q", got)
	}
}

// TestAFailedRerunUngreensTheChain: --force re-runs a chain the index holds
// as green. If that run fails, the next unforced run must not skip it on the
// strength of the older success.
func (s suite) TestAFailedRerunUngreensTheChain(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	mustRecordChainSucceeded(t, st, "job", "hash1")
	mustForgetChain(t, st, "job", "hash1")

	got, err := st.HasSucceededBatch(ctx, "job", []string{"hash1"})
	if err != nil {
		t.Fatalf("HasSucceededBatch: %v", err)
	}

	if got["hash1"] {
		t.Fatal("a chain whose latest run failed is still reported as succeeded")
	}
}

// TestFailedChainsDoNotCrowdOutGreenOnes: the index is capped by count, and a
// job can fail through fresh content every run — a flaky agent, a moving
// input. Each failure used to be a row, so a run of failures pushed the green
// chains out of the cap and the next rerun redid work that had succeeded.
// A failure is a removal, never a row, so however many there are the cap
// only ever bounds chains worth keeping.
func (s suite) TestFailedChainsDoNotCrowdOutGreenOnes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	green := []string{"green-1", "green-2", "green-3"}
	for _, hash := range green {
		mustRecordChainSucceeded(t, st, "build", hash)
	}

	// Far past any multiple of a one-run history a driver could choose.
	for i := range 200 {
		mustForgetChain(t, st, "build", fmt.Sprintf("failed-%03d", i))
	}

	err := st.Prune(ctx, store.Retention{JobName: "build", Runs: 1}, "")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	got, err := st.HasSucceededBatch(ctx, "build", green)
	if err != nil {
		t.Fatalf("HasSucceededBatch: %v", err)
	}

	for _, hash := range green {
		if !got[hash] {
			t.Errorf("green chain %q was evicted by failed ones — the next run redoes work that succeeded", hash)
		}
	}
}

// TranscriptJSON is a transcript of one text event carrying size bytes of
// text — the shape a cap test needs, exported so a driver's own footprint
// measurement can store the same thing.
func TranscriptJSON(size int) string {
	events := []map[string]string{{"type": "text", "text": strings.Repeat("t", size)}}

	encoded, err := json.Marshal(events)
	if err != nil {
		panic(err)
	}

	return string(encoded)
}

// TestNodeTranscriptIsCapped: a transcript is the largest single value a
// driver stores, and a conversation has unboundedly many turns. Over the cap
// it is truncated, not dropped — the head is where the task and the first
// decisions are — and what remains is still JSON.
func (s suite) TestNodeTranscriptIsCapped(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	mustRecordNode(t, st, "job", "agent-node")

	err := st.SaveNodeTranscript(ctx, "agent-node", TranscriptJSON(store.MaxTranscriptBytes*3))
	if err != nil {
		t.Fatalf("SaveNodeTranscript: %v", err)
	}

	transcript, ok, err := st.NodeTranscript(ctx, "agent-node")
	if err != nil || !ok {
		t.Fatalf("NodeTranscript: ok=%v err=%v", ok, err)
	}

	if transcript == "" {
		t.Fatal("a transcript over the cap was stored as nothing at all")
	}

	if len(transcript) > store.MaxTranscriptBytes {
		t.Errorf("stored transcript is %d bytes, want at most %d", len(transcript), store.MaxTranscriptBytes)
	}

	if !json.Valid([]byte(transcript)) {
		t.Error("the truncated transcript is not valid JSON")
	}
}

func roundTripTranscript(t *testing.T, st store.Store, hash, transcript string) string {
	t.Helper()

	ctx := context.Background()

	mustRecordNode(t, st, "job", hash)

	err := st.SaveNodeTranscript(ctx, hash, transcript)
	if err != nil {
		t.Fatalf("SaveNodeTranscript: %v", err)
	}

	stored, ok, err := st.NodeTranscript(ctx, hash)
	if err != nil || !ok {
		t.Fatalf("NodeTranscript: ok=%v err=%v", ok, err)
	}

	return stored
}

// TestATranscriptAtTheCapIsStoredVerbatim: a truncation path that rewrites transcripts that were never too big silently changes what a node page shows.
func (s suite) TestATranscriptAtTheCapIsStoredVerbatim(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	exactly := TranscriptJSON(store.MaxTranscriptBytes - len(TranscriptJSON(0)))

	for i, original := range []string{TranscriptJSON(1_024), exactly} {
		if got := roundTripTranscript(t, st, hashOf(i+1), original); got != original {
			t.Errorf("a transcript of %d bytes, at most the cap, was stored as %d", len(original), len(got))
		}
	}
}

// TestTranscriptTruncationKeepsAsMuchAsFits: the head kept is the longest run of whole events the cap has room for beside the notice. A budget off by a few bytes passes every check that only asks for "under the cap".
func (s suite) TestTranscriptTruncationKeepsAsMuchAsFits(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	// One-byte events after the first, so each one more costs exactly two; two widths of the first, so one of them fills the cap to the byte.
	for i, first := range []string{"1", "12"} {
		events := []string{first}
		for len(events) < store.MaxTranscriptBytes {
			events = append(events, "1")
		}

		got := roundTripTranscript(t, st, hashOf(i+1), "["+strings.Join(events, ",")+"]")

		var parsed []json.RawMessage

		err := json.Unmarshal([]byte(got), &parsed)
		if err != nil || !strings.HasPrefix(got, "["+first+",1,") || !strings.Contains(string(parsed[len(parsed)-1]), "truncated") {
			t.Fatalf("first event %s: truncated to %.60q…, want the head, then the notice, as one array", first, got)
		}

		if len(got) > store.MaxTranscriptBytes || len(got) < store.MaxTranscriptBytes-1 {
			t.Errorf("first event %s: kept %d bytes, want %d or one less — one more event costs two", first, len(got), store.MaxTranscriptBytes)
		}
	}
}

// TestTranscriptTruncationSurvivesNonJSON: the cap is a storage bound, not a validator, and a byte-offset cut must not split a multi-byte rune.
func (s suite) TestTranscriptTruncationSurvivesNonJSON(t *testing.T) {
	t.Parallel()

	got := roundTripTranscript(t, s.open(t, "test"), hashOf(1), strings.Repeat("日本語", store.MaxTranscriptBytes))

	if len(got) > store.MaxTranscriptBytes || len(got) < store.MaxTranscriptBytes-(utf8.UTFMax-1) {
		t.Errorf("non-JSON transcript stored %d bytes, want the longest whole-rune prefix of the %d cap", len(got), store.MaxTranscriptBytes)
	}

	if !utf8.ValidString(got) {
		t.Error("truncation split a multi-byte rune, leaving invalid UTF-8")
	}
}
