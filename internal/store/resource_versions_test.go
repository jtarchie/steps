package store

// resource_versions: what steps remembers, in what order, and what goes with
// a version when it is forgotten.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
)

func versionNames(t *testing.T, versions []map[string]any) []string {
	t.Helper()

	names := make([]string, 0, len(versions))
	for _, version := range versions {
		names = append(names, fmt.Sprint(version["n"]))
	}

	return names
}

func newHistoryStore(t *testing.T) *Store {
	t.Helper()

	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	t.Cleanup(func() { _ = store.Close() })

	return store
}

func recordN(t *testing.T, store *Store, resource string, names ...string) {
	t.Helper()

	versions := make([]map[string]any, 0, len(names))
	for _, name := range names {
		versions = append(versions, map[string]any{"n": name})
	}

	err := store.RecordVersions(context.Background(), resource, versions, 0)
	if err != nil {
		t.Fatal(err)
	}
}

// TestRecordVersionsKeepsDiscoveryOrder: a version's place in history is when
// it was first seen, and re-reporting it does not move it. A check that
// returns its whole window every poll is the common case, and it has to be
// idempotent — otherwise the order a job walks would shuffle underneath it.
func TestRecordVersionsKeepsDiscoveryOrder(t *testing.T) {
	t.Parallel()

	store := newHistoryStore(t)

	recordN(t, store, "items", "1", "2")
	recordN(t, store, "items", "1", "2", "3")
	recordN(t, store, "items", "2", "3")

	versions, err := store.ResourceVersions(context.Background(), "items")
	if err != nil {
		t.Fatal(err)
	}

	if got := versionNames(t, versions); fmt.Sprint(got) != "[1 2 3]" {
		t.Errorf("versions = %v, want [1 2 3] oldest first", got)
	}
}

// TestResourceVersionsAreScopedToTheirResource: two resources keep separate
// histories and separate orders.
func TestResourceVersionsAreScopedToTheirResource(t *testing.T) {
	t.Parallel()

	store := newHistoryStore(t)

	recordN(t, store, "items", "1", "2")
	recordN(t, store, "other", "9")

	versions, err := store.ResourceVersions(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}

	if got := versionNames(t, versions); fmt.Sprint(got) != "[9]" {
		t.Errorf("other = %v, want [9]", got)
	}
}

// TestRecordVersionsKeepsExactDigits: history is read back into templates and
// sent to APIs, so a wide id or a fractional timestamp must survive storage.
func TestRecordVersionsKeepsExactDigits(t *testing.T) {
	t.Parallel()

	store := newHistoryStore(t)

	err := store.RecordVersions(context.Background(), "items", []map[string]any{
		{"id": json.Number("1234567890123456789"), "ts": json.Number("1699887654.001200")},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}

	versions, err := store.ResourceVersions(context.Background(), "items")
	if err != nil {
		t.Fatal(err)
	}

	if got := fmt.Sprint(versions[0]["id"]); got != "1234567890123456789" {
		t.Errorf("id = %s, want the digits as written", got)
	}

	if got := fmt.Sprint(versions[0]["ts"]); got != "1699887654.001200" {
		t.Errorf("ts = %s, want the digits as written", got)
	}
}

// TestRecordVersionsPrunesOldestAndCascades is the retention rule and its
// price in one test.
//
// The cap keeps the NEWEST, because a job behind by more than the cap has
// bigger problems than which versions it lost. What goes with a pruned
// version is the point: its consumed row and its green record, by foreign
// key, so nothing is left pointing at a version that no longer exists — and
// so a `passed:` gate stops clearing for it, which is correct and is also
// why the cap is configurable.
func TestRecordVersionsPrunesOldestAndCascades(t *testing.T) {
	t.Parallel()

	store := newHistoryStore(t)
	ctx := context.Background()

	oldest, err := EncodeVersion(map[string]any{"n": "1"})
	if err != nil {
		t.Fatal(err)
	}

	recordN(t, store, "items", "1", "2", "3")

	// The oldest version has been both taken and passed by a job.
	err = store.RecordConsumedVersion(ctx, "build", "items", oldest, 0)
	if err != nil {
		t.Fatal(err)
	}

	err = store.RecordPassedVersion(ctx, "build", "items", oldest, "build-1")
	if err != nil {
		t.Fatal(err)
	}

	// A cap of two, applied as a fourth version arrives.
	err = store.RecordVersions(ctx, "items", []map[string]any{{"n": "4"}}, 2)
	if err != nil {
		t.Fatal(err)
	}

	versions, err := store.ResourceVersions(ctx, "items")
	if err != nil {
		t.Fatal(err)
	}

	if got := versionNames(t, versions); fmt.Sprint(got) != "[3 4]" {
		t.Errorf("versions = %v, want the newest two [3 4]", got)
	}

	assertNoTrace(t, store, oldest)
}

// assertNoTrace fails if anything still points at a pruned version.
func assertNoTrace(t *testing.T, store *Store, version string) {
	t.Helper()

	ctx := context.Background()

	consumed, err := store.ConsumedVersions(ctx, "build", "items")
	if err != nil {
		t.Fatal(err)
	}

	if consumed[version] {
		t.Error("the pruned version is still marked consumed; the cascade left an orphan")
	}

	passed, err := store.PassedVersions(ctx, "build", 10)
	if err != nil {
		t.Fatal(err)
	}

	for _, row := range passed {
		if row.Version == version {
			t.Error("the pruned version is still recorded as passed; the cascade left an orphan")
		}
	}
}

// TestUsingAVersionIsNotTheSameAsCheckingForIt covers the distinction the
// whole lookup rests on.
//
// A job that resolved its own versions — every `steps run` — records what it
// took against versions no check ever filed. The foreign keys mean such a row
// needs a parent, so one is created; but that parent is NOT history. It says
// a version was used, not what else exists.
//
// Conflating them breaks the next run: one remembered version would be
// returned as though it were the resource's whole history, and the check that
// would have found the others never runs. That is a real failure, not a
// hypothetical — it is what TestConformanceGetVersionEveryTakesEachVersionOnce
// caught.
func TestUsingAVersionIsNotTheSameAsCheckingForIt(t *testing.T) {
	t.Parallel()

	store := newHistoryStore(t)
	ctx := context.Background()

	version, err := EncodeVersion(map[string]any{"n": "7"})
	if err != nil {
		t.Fatal(err)
	}

	err = store.RecordConsumedVersion(ctx, "build", "items", version, 0)
	if err != nil {
		t.Fatalf("recording a consumed version nothing had filed: %v", err)
	}

	err = store.RecordPassedVersion(ctx, "build", "items", version, "build-1")
	if err != nil {
		t.Fatalf("recording a passed version nothing had filed: %v", err)
	}

	versions, err := store.ResourceVersions(ctx, "items")
	if err != nil {
		t.Fatal(err)
	}

	if len(versions) != 0 {
		t.Errorf("history = %v, want none — using a version is not checking for one",
			versionNames(t, versions))
	}

	// A later check filing the same version DOES make it history, so nothing
	// is permanently invisible.
	recordN(t, store, "items", "7")

	versions, err = store.ResourceVersions(ctx, "items")
	if err != nil {
		t.Fatal(err)
	}

	if got := versionNames(t, versions); fmt.Sprint(got) != "[7]" {
		t.Errorf("history = %v, want [7] once a check reported it", got)
	}
}

// TestConsumedRecordsOutliveTheHistoryTheyFilter is the interaction between
// two caps that used to be set independently.
//
// The consumed set exists to stop a job rebuilding a version, and it is
// bounded so it cannot grow forever. History is bounded too, and became
// CONFIGURABLE — so a pipeline asking to remember more versions than the
// consumed bound would lose the record of having built the oldest of them
// while still being offered them. They read as unbuilt and get built again,
// which is the exact repetition the consumed set exists to prevent.
//
// So the bounds are one bound: forget a version and forget that it was taken,
// in that order or not at all.
func TestConsumedRecordsOutliveTheHistoryTheyFilter(t *testing.T) {
	t.Parallel()

	store := newHistoryStore(t)
	ctx := context.Background()

	// More versions than the consumed floor, remembered on purpose.
	const total = consumedVersionCap + 200

	versions := make([]map[string]any, 0, total)
	for i := range total {
		versions = append(versions, map[string]any{"n": strconv.Itoa(i)})
	}

	err := store.RecordVersions(ctx, "items", versions, total)
	if err != nil {
		t.Fatal(err)
	}

	for _, version := range versions {
		encoded := mustEncode(t, version)

		err = store.RecordConsumedVersion(ctx, "build", "items", encoded, total)
		if err != nil {
			t.Fatal(err)
		}
	}

	history, err := store.ResourceVersions(ctx, "items")
	if err != nil {
		t.Fatal(err)
	}

	consumed, err := store.ConsumedVersions(ctx, "build", "items")
	if err != nil {
		t.Fatal(err)
	}

	unbuilt := 0

	for _, version := range history {
		if !consumed[mustEncode(t, version)] {
			unbuilt++
		}
	}

	if unbuilt != 0 {
		t.Errorf("%d of %d already-built versions read as unbuilt; they would all run again",
			unbuilt, len(history))
	}
}

func mustEncode(t *testing.T, version map[string]any) string {
	t.Helper()

	encoded, err := EncodeVersion(version)
	if err != nil {
		t.Fatal(err)
	}

	return encoded
}
