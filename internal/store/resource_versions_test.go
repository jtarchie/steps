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

	// The oldest version has been passed by a job.
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

	passed, err := store.PassedVersions(ctx, "build", 10)
	if err != nil {
		t.Fatal(err)
	}

	for _, row := range passed {
		if row.Version == oldest {
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

	_, err = store.RecordVersionOrder(ctx, "items", version)
	if err != nil {
		t.Fatalf("filing a version nothing had checked: %v", err)
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

// TestTheMarkCannotDevelopHoles is why the cursor is a high-water mark and
// not a set of consumed versions.
//
// A set has to be capped or it grows forever, and a capped set forgets its
// oldest members while the versions they name are still offered — so they
// read as unbuilt and run again, which is the repetition the cursor exists to
// prevent. That was a live bug the moment history became configurable: ask to
// remember more versions than the consumed cap, and the difference came back
// round for a second build.
//
// A mark has no members to forget. Whatever the history limit, everything at
// or below it is done.
func TestTheMarkCannotDevelopHoles(t *testing.T) {
	t.Parallel()

	store := newHistoryStore(t)
	ctx := context.Background()

	// Far more versions than any set-based cap would have kept.
	const total = 5000

	versions := make([]map[string]any, 0, total)
	for i := range total {
		versions = append(versions, map[string]any{"n": strconv.Itoa(i)})
	}

	err := store.RecordVersions(ctx, "items", versions, total)
	if err != nil {
		t.Fatal(err)
	}

	orders, err := store.VersionOrders(ctx, "items")
	if err != nil {
		t.Fatal(err)
	}

	if len(orders) != total {
		t.Fatalf("history holds %d versions, want %d", len(orders), total)
	}

	// The job builds all of them, which is one row rather than 5000.
	err = store.RecordConsumedMark(ctx, "build", "items", highestOrder(orders))
	if err != nil {
		t.Fatal(err)
	}

	mark, err := store.ConsumedMark(ctx, "build", "items")
	if err != nil {
		t.Fatal(err)
	}

	unbuilt := 0

	for _, order := range orders {
		if order > mark {
			unbuilt++
		}
	}

	if unbuilt != 0 {
		t.Errorf("%d of %d already-built versions read as unbuilt; they would all run again",
			unbuilt, total)
	}
}

func highestOrder(orders map[string]int64) int64 {
	var highest int64

	for _, order := range orders {
		if order > highest {
			highest = order
		}
	}

	return highest
}
