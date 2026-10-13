package storetest

// resource_versions: what steps remembers, in what order, and what goes with
// a version when it is forgotten.
//
// check_errors: why a resource cannot be checked, which is the one thing a
// failing poll used to leave nowhere but a log line.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

func checkErrorNames(t *testing.T, st store.Store) []string {
	t.Helper()

	errs, err := st.CheckErrors(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(errs))
	for _, row := range errs {
		names = append(names, row.Name)
	}

	return names
}

// TestACheckErrorIsRecordedAndCleared: the clear is the half that matters. A
// resource that recovers has to stop being reported, or the surface built on
// this is a light nobody can turn off.
func (s suite) TestACheckErrorIsRecordedAndCleared(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := t.Context()

	err := st.RecordCheckError(ctx, "repo", "check: exit status 128")
	if err != nil {
		t.Fatal(err)
	}

	errs, err := st.CheckErrors(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(errs) != 1 || errs[0].Name != "repo" || errs[0].Message != "check: exit status 128" {
		t.Fatalf("CheckErrors = %+v, want one repo row carrying its message", errs)
	}

	if errs[0].FailedAt.IsZero() {
		t.Error("a recorded check error does not say when it failed")
	}

	err = st.RecordCheckError(ctx, "repo", "")
	if err != nil {
		t.Fatal(err)
	}

	if names := checkErrorNames(t, st); len(names) != 0 {
		t.Errorf("CheckErrors = %v after a clear, want none", names)
	}
}

// TestClearingACheckErrorThatIsNotThereIsFine: every successful poll of every
// healthy resource takes this path, so it has to be a no-op rather than a row
// count somebody checks.
func (s suite) TestClearingACheckErrorThatIsNotThereIsFine(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	err := st.RecordCheckError(t.Context(), "never-failed", "")
	if err != nil {
		t.Fatalf("clearing an absent check error: %v", err)
	}
}

// TestARecheckReplacesTheReasonRatherThanAddingOne: a resource down for an
// hour is one broken resource, not sixty.
func (s suite) TestARecheckReplacesTheReasonRatherThanAddingOne(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := t.Context()

	for _, message := range []string{"dial tcp: connection refused", "exit status 128"} {
		err := st.RecordCheckError(ctx, "repo", message)
		if err != nil {
			t.Fatal(err)
		}
	}

	errs, err := st.CheckErrors(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(errs) != 1 || errs[0].Message != "exit status 128" {
		t.Fatalf("CheckErrors = %+v, want one row holding the newest reason", errs)
	}
}

// TestACheckErrorIsBoundedLikeEveryOtherStoredError: a check reports the whole
// generated shell script when it fails, and a resource down at a one-minute
// poll rewrites that row every minute.
func (s suite) TestACheckErrorIsBoundedLikeEveryOtherStoredError(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := t.Context()

	err := st.RecordCheckError(ctx, "repo", strings.Repeat("x", 4*store.MaxStoredErrorBytes))
	if err != nil {
		t.Fatal(err)
	}

	errs, err := st.CheckErrors(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(errs) != 1 {
		t.Fatalf("CheckErrors = %+v, want one row", errs)
	}

	if len(errs[0].Message) > store.MaxStoredErrorBytes {
		t.Errorf("a stored check error is %d bytes, over the %d cap", len(errs[0].Message), store.MaxStoredErrorBytes)
	}
}

// TestCheckedResourcesListsEachCursor, by name.
func (s suite) TestCheckedResourcesListsEachCursor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	for name, version := range map[string]store.VersionKey{"repo": `{"ref":"b"}`, "image": `{"digest":"a"}`} {
		err := st.RecordCheckedVersion(ctx, name, version)
		if err != nil {
			t.Fatalf("RecordCheckedVersion: %v", err)
		}
	}

	checked, err := st.CheckedResources(ctx)
	if err != nil || len(checked) != 2 || checked[0].Name != "image" || checked[1].Version != `{"ref":"b"}` || checked[0].CheckedAt.IsZero() {
		t.Fatalf("CheckedResources = %+v, %v; want image then repo, each with its version and when", checked, err)
	}
}

// TestHasPassedVersionSetNeedsOneBuild: versions that each passed in
// different builds were never proven together.
func (s suite) TestHasPassedVersionSetNeedsOneBuild(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")
	want := map[string]store.VersionKey{"repo": `{"ref":"a"}`, "image": `{"digest":"b"}`}

	pass := func(resource, build string) {
		t.Helper()

		err := st.RecordPassedVersion(ctx, store.Pass{JobName: "unit", Resource: resource, Version: want[resource], BuildID: build})
		if err != nil {
			t.Fatalf("RecordPassedVersion: %v", err)
		}
	}

	assertPassedSet := func(set map[string]store.VersionKey, expected bool, why string) {
		t.Helper()

		got, err := st.HasPassedVersionSet(ctx, "unit", set)
		if err != nil || got != expected {
			t.Fatalf("HasPassedVersionSet = %v, %v; want %v — %s", got, err, expected, why)
		}
	}

	assertPassedSet(nil, true, "no constraint is vacuously met")

	pass("repo", "b1")
	pass("image", "b2")
	assertPassedSet(want, false, "each passed, in different builds")

	pass("image", "b1")
	assertPassedSet(want, true, "one build passed both")
}

// TestTheCheckedVersionMovesOnlyFromWhatWasRead: a compare-and-set that loses leaves the newer version in place, including when the expectation was that none existed.
func (s suite) TestTheCheckedVersionMovesOnlyFromWhatWasRead(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	for _, step := range []struct {
		expected store.VersionKey
		found    bool
		next     store.VersionKey
		moves    bool
	}{
		{"", false, "a", true},
		{"", false, "b", false},
		{"stale", true, "b", false},
		{"a", true, "b", true},
	} {
		moved, err := st.CompareAndSetCheckedVersion(ctx, store.CheckedSwap{Resource: "push", Expected: step.expected, ExpectedFound: step.found, Next: step.next})
		if err != nil || moved != step.moves {
			t.Errorf("from %q (found=%v) to %q: moved=%v err=%v, want moved=%v", step.expected, step.found, step.next, moved, err, step.moves)
		}
	}

	checked, _, err := st.LastChecked(ctx, "push")
	if err != nil || checked.Version != "b" {
		t.Errorf("checked = %q (err %v), want b", checked.Version, err)
	}
}

// TestPassedVersionsAreNewestFirstWithinASecond: a green build records its
// versions in one burst, so most of a job's rows share a timestamp. A limit
// over a timestamp order alone would hand back an arbitrary subset of the
// tied rows on every page load; insertion order breaks the tie.
func (s suite) TestPassedVersionsAreNewestFirstWithinASecond(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	const recorded = 30

	for i := range recorded {
		encoded, err := store.EncodeVersion(map[string]any{"n": fmt.Sprintf("v%02d", i)})
		if err != nil {
			t.Fatal(err)
		}

		err = st.RecordPassedVersion(ctx, store.Pass{JobName: "build", Resource: "repo", Version: encoded, BuildID: "b1"})
		if err != nil {
			t.Fatalf("RecordPassedVersion %d: %v", i, err)
		}
	}

	all, err := st.PassedVersions(ctx, "build", 0)
	if err != nil {
		t.Fatalf("PassedVersions(0): %v", err)
	}

	if len(all) != recorded {
		t.Fatalf("PassedVersions with limit 0 returned %d rows, want all %d", len(all), recorded)
	}

	page, err := st.PassedVersions(ctx, "build", 25)
	if err != nil {
		t.Fatalf("PassedVersions(25): %v", err)
	}

	if len(page) != 25 {
		t.Fatalf("PassedVersions with limit 25 returned %d rows", len(page))
	}

	for i, row := range page {
		want := store.VersionKey(fmt.Sprintf(`{"n":"v%02d"}`, recorded-1-i))
		if row.Version != want {
			t.Fatalf("PassedVersions[%d] = %s, want %s — the newest recorded comes first, ties or not", i, row.Version, want)
		}
	}
}

func versionNames(t *testing.T, versions []map[string]any) []string {
	t.Helper()

	names := make([]string, 0, len(versions))
	for _, version := range versions {
		names = append(names, fmt.Sprint(version["n"]))
	}

	return names
}

func recordN(t *testing.T, st store.Store, resource string, names ...string) {
	t.Helper()

	versions := make([]map[string]any, 0, len(names))
	for _, name := range names {
		versions = append(versions, map[string]any{"n": name})
	}

	_, err := st.RecordVersions(context.Background(), resource, versions, 0)
	if err != nil {
		t.Fatal(err)
	}
}

// TestRecordVersionOrderAnswersWithTheOrderFiled: a cursor advances to the order this returns, so a zero for a version just filed reads as "consumed nothing" and the same version is built again.
func (s suite) TestRecordVersionOrderAnswersWithTheOrderFiled(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := context.Background()

	recordN(t, st, "items", "1", "2")

	for _, name := range []string{"9", "2", "9"} {
		version, err := store.EncodeVersion(map[string]any{"n": name})
		if err != nil {
			t.Fatal(err)
		}

		got, err := st.RecordVersionOrder(ctx, "items", version)
		if err != nil {
			t.Fatalf("RecordVersionOrder(%s): %v", name, err)
		}

		orders, err := st.VersionOrders(ctx, "items")
		if err != nil {
			t.Fatal(err)
		}

		if want := orders[version]; got != want || got <= 0 {
			t.Errorf("RecordVersionOrder(%s) = %d, want the order it is filed at, %d", name, got, want)
		}
	}
}

// TestRecordVersionsKeepsDiscoveryOrder: a version's place in history is when
// it was first seen, and re-reporting it does not move it. A check that
// returns its whole window every poll is the common case, and it has to be
// idempotent — otherwise the order a job walks would shuffle underneath it.
func (s suite) TestRecordVersionsKeepsDiscoveryOrder(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	recordN(t, st, "items", "1", "2")
	recordN(t, st, "items", "1", "2", "3")
	recordN(t, st, "items", "2", "3")

	versions, err := decodedVersions(t, st, "items")
	if err != nil {
		t.Fatal(err)
	}

	if got := versionNames(t, versions); fmt.Sprint(got) != "[1 2 3]" {
		t.Errorf("versions = %v, want [1 2 3] oldest first", got)
	}
}

// TestResourceVersionsJSONPreservesAWideID pins what the store hands back: the
// stored bytes, not a value that has been through a decode. It is the whole
// reason this read is JSON and DecodeVersion (with its UseNumber) is the
// caller's business — a round trip through float64 turns a wide id into
// exponent notation, and the version goes back out to the API that reported it.
func (s suite) TestResourceVersionsJSONPreservesAWideID(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := context.Background()

	_, err := st.RecordVersions(ctx, "items", []map[string]any{
		{"n": "1"}, {"id": json.Number("1234567890123456789")},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := st.ResourceVersionsJSON(ctx, "items")
	if err != nil {
		t.Fatal(err)
	}

	if len(raw) != 2 {
		t.Fatalf("got %d rows, want 2", len(raw))
	}

	if !strings.Contains(string(raw[1]), "1234567890123456789") {
		t.Errorf("raw row 1 = %q, want the wide id preserved exactly", raw[1])
	}

	decoded, err := store.DecodeVersion(raw[1])
	if err != nil {
		t.Fatal(err)
	}

	if fmt.Sprint(decoded["id"]) != "1234567890123456789" {
		t.Errorf("decoded id = %v, want the wide id intact", decoded["id"])
	}
}

// TestResourceVersionsAreScopedToTheirResource: two resources keep separate
// histories and separate orders.
func (s suite) TestResourceVersionsAreScopedToTheirResource(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	recordN(t, st, "items", "1", "2")
	recordN(t, st, "other", "9")

	versions, err := decodedVersions(t, st, "other")
	if err != nil {
		t.Fatal(err)
	}

	if got := versionNames(t, versions); fmt.Sprint(got) != "[9]" {
		t.Errorf("other = %v, want [9]", got)
	}
}

// TestRecordVersionsKeepsExactDigits: history is read back into templates and
// sent to APIs, so a wide id or a fractional timestamp must survive storage.
func (s suite) TestRecordVersionsKeepsExactDigits(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	_, err := st.RecordVersions(context.Background(), "items", []map[string]any{
		{"id": json.Number("1234567890123456789"), "ts": json.Number("1699887654.001200")},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}

	versions, err := decodedVersions(t, st, "items")
	if err != nil || len(versions) != 1 {
		t.Fatalf("decoded %d versions, err %v; want the one recorded", len(versions), err)
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
func (s suite) TestRecordVersionsPrunesOldestAndCascades(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := context.Background()

	oldest, err := store.EncodeVersion(map[string]any{"n": "1"})
	if err != nil {
		t.Fatal(err)
	}

	recordN(t, st, "items", "1", "2", "3")

	// The oldest version has been passed by a job.
	err = st.RecordPassedVersion(ctx, store.Pass{JobName: "build", Resource: "items", Version: oldest, BuildID: "build-1"})
	if err != nil {
		t.Fatal(err)
	}

	// A cap of two, applied as a fourth version arrives.
	_, err = st.RecordVersions(ctx, "items", []map[string]any{{"n": "4"}}, 2)
	if err != nil {
		t.Fatal(err)
	}

	versions, err := decodedVersions(t, st, "items")
	if err != nil {
		t.Fatal(err)
	}

	if got := versionNames(t, versions); fmt.Sprint(got) != "[3 4]" {
		t.Errorf("versions = %v, want the newest two [3 4]", got)
	}

	passed, err := st.PassedVersions(ctx, "build", 10)
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
func (s suite) TestUsingAVersionIsNotTheSameAsCheckingForIt(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := context.Background()

	version, err := store.EncodeVersion(map[string]any{"n": "7"})
	if err != nil {
		t.Fatal(err)
	}

	_, err = st.RecordVersionOrder(ctx, "items", version)
	if err != nil {
		t.Fatalf("filing a version nothing had checked: %v", err)
	}

	err = st.RecordPassedVersion(ctx, store.Pass{JobName: "build", Resource: "items", Version: version, BuildID: "build-1"})
	if err != nil {
		t.Fatalf("recording a passed version nothing had filed: %v", err)
	}

	versions, err := decodedVersions(t, st, "items")
	if err != nil {
		t.Fatal(err)
	}

	if len(versions) != 0 {
		t.Errorf("history = %v, want none — using a version is not checking for one",
			versionNames(t, versions))
	}

	// A later check filing the same version DOES make it history, so nothing
	// is permanently invisible.
	recordN(t, st, "items", "7")

	versions, err = decodedVersions(t, st, "items")
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
func (s suite) TestTheMarkCannotDevelopHoles(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := context.Background()

	// Far more versions than any set-based cap would have kept.
	const total = 5000

	versions := make([]map[string]any, 0, total)
	for i := range total {
		versions = append(versions, map[string]any{"n": strconv.Itoa(i)})
	}

	_, err := st.RecordVersions(ctx, "items", versions, total)
	if err != nil {
		t.Fatal(err)
	}

	orders, err := st.VersionOrders(ctx, "items")
	if err != nil {
		t.Fatal(err)
	}

	if len(orders) != total {
		t.Fatalf("history holds %d versions, want %d", len(orders), total)
	}

	// The job builds all of them, which is one row rather than 5000.
	err = st.RecordConsumedMark(ctx, "build", "items", highestOrder(orders))
	if err != nil {
		t.Fatal(err)
	}

	mark, err := st.ConsumedMark(ctx, "build", "items")
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

func highestOrder(orders map[store.VersionKey]int64) int64 {
	var highest int64

	for _, order := range orders {
		if order > highest {
			highest = order
		}
	}

	return highest
}

// TestCheckReDiscoveryReMintsARunFiledOrder: a version only a RUN had filed
// sits at whatever order the run minted, which is wrong the moment a check
// reports it — "latest" resolves by highest order, so the run-filed newest
// version would sort below everything the check reported after it, and the
// prune would treat the NEWEST version as the oldest and delete it first.
// The check is discovering it now, so it takes a fresh order now.
func (s suite) TestCheckReDiscoveryReMintsARunFiledOrder(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := context.Background()

	// A manual run files v10 before any check has seen the resource.
	v10 := mustEncode(t, map[string]any{"n": "10"})

	_, err := st.RecordVersionOrder(ctx, "items", v10)
	if err != nil {
		t.Fatal(err)
	}

	// The first check reports v1..v10, oldest first.
	versions := make([]map[string]any, 0, 10)
	for i := 1; i <= 10; i++ {
		versions = append(versions, map[string]any{"n": strconv.Itoa(i)})
	}

	_, err = st.RecordVersions(ctx, "items", versions, 0)
	if err != nil {
		t.Fatal(err)
	}

	history, err := decodedVersions(t, st, "items")
	if err != nil || len(history) == 0 {
		t.Fatalf("decoded %d versions, err %v; want the recorded history", len(history), err)
	}

	if got := fmt.Sprint(history[len(history)-1]["n"]); got != "10" {
		t.Errorf("latest = v%s, want v10 — the run-filed order must be re-minted on discovery", got)
	}
}

// TestSteadyStateReportWritesNothing: the common case is a check re-reporting
// its whole window every poll, and it must cost nothing — no order advance,
// no row rewrite, and no "new versions" signal that would re-trigger jobs.
func (s suite) TestSteadyStateReportWritesNothing(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := context.Background()

	report := []map[string]any{{"n": "1"}, {"n": "2"}, {"n": "3"}}

	added, err := st.RecordVersions(ctx, "items", report, 0)
	if err != nil {
		t.Fatal(err)
	}

	if added != 3 {
		t.Fatalf("first report added %d, want 3", added)
	}

	added, err = st.RecordVersions(ctx, "items", report, 0)
	if err != nil {
		t.Fatal(err)
	}

	if added != 0 {
		t.Errorf("a re-reported window counted as %d new versions, want 0", added)
	}

	orders, err := st.VersionOrders(ctx, "items")
	if err != nil {
		t.Fatal(err)
	}

	if got := highestOrder(orders); got != 3 {
		t.Errorf("highest order = %d after a no-op poll, want 3 — orders must not inflate", got)
	}
}

// TestPruneNeverEatsTheReportedWindow: a version the check still reports is
// still real, whatever the cap says. Pruning it would make the next poll
// "discover" it again at a fresh top order — with a cap smaller than the
// window the table oscillates between halves forever, "latest" flips to an
// old version on alternate polls, and every prune cascades away green
// records so gates re-open and jobs re-fan-out each cycle. The cap bounds
// what has scrolled AWAY; a window larger than the cap is kept whole.
func (s suite) TestPruneNeverEatsTheReportedWindow(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := context.Background()

	window := make([]map[string]any, 0, 20)
	for i := 1; i <= 20; i++ {
		window = append(window, map[string]any{"n": fmt.Sprintf("%02d", i)})
	}

	for poll := 1; poll <= 3; poll++ {
		_, err := st.RecordVersions(ctx, "items", window, 10)
		if err != nil {
			t.Fatal(err)
		}

		history, err := decodedVersions(t, st, "items")
		if err != nil {
			t.Fatal(err)
		}

		if len(history) != 20 {
			t.Fatalf("poll %d: %d rows, want the whole 20-item window", poll, len(history))
		}

		if got := fmt.Sprint(history[len(history)-1]["n"]); got != "20" {
			t.Errorf("poll %d: latest = %s, want 20 — the prune must not oscillate", poll, got)
		}
	}
}

func mustEncode(t *testing.T, version map[string]any) store.VersionKey {
	t.Helper()

	encoded, err := store.EncodeVersion(version)
	if err != nil {
		t.Fatal(err)
	}

	return encoded
}

// decodedVersions reads a resource's check history the way version resolution
// does: the stored JSON, through DecodeVersion.
func decodedVersions(t *testing.T, st store.Store, name string) ([]map[string]any, error) {
	t.Helper()

	encoded, err := st.ResourceVersionsJSON(context.Background(), name)
	if err != nil {
		return nil, fmt.Errorf("could not read the stored versions: %w", err)
	}

	versions, err := store.DecodeVersions(encoded)
	if err != nil {
		return nil, fmt.Errorf("could not decode a stored version: %w", err)
	}

	return versions, nil
}

// TestVersionHistoryZeroMeansNoLimit: the deleted-versions bound, and the
// one value of it that is easy to get wrong.
//
// `version_history: 0` means no limit, the same convention every other cap in
// this repo uses (docs/attempts-timeout.md). The branch that enforces it is
// `if limit < 0`, and widening it to `limit <= 0` restores exactly the bug the
// comment records — zero silently becoming the 1000 default — with the whole
// suite still green. A cap nobody asked for is not a visible failure: it looks
// like a working pipeline until a resource passes a thousand versions and the
// oldest start disappearing.
func (s suite) TestVersionHistoryZeroMeansNoLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	// Filed across several checks, not one. A prune never touches versions the
	// CURRENT check just reported — minReportedOrder is the floor — so a
	// single call of everything is protected in full and would pass under any
	// cap at all. The cap only bites on versions an EARLIER check filed.
	const (
		perCheck = 200
		checks   = 6
		filed    = perCheck * checks
	)

	for check := range checks {
		versions := make([]map[string]any, 0, perCheck)
		for i := range perCheck {
			versions = append(versions, map[string]any{"ref": fmt.Sprintf("v%04d", check*perCheck+i)})
		}

		_, err := st.RecordVersions(ctx, "mentions", versions, 0)
		if err != nil {
			t.Fatalf("RecordVersions check %d: %v", check, err)
		}
	}

	orders, err := st.VersionOrders(ctx, "mentions")
	if err != nil {
		t.Fatalf("VersionOrders: %v", err)
	}

	if len(orders) != filed {
		t.Errorf("kept %d of %d versions under version_history: 0, want all of them — zero means no limit, not the default cap of %d",
			len(orders), filed, store.DefaultResourceVersionCap)
	}
}

// TestGreenVersionsIsTheIntersection: a passed: get chooses among the versions EVERY named upstream built green, in discovery order — not the union, not one upstream's list, and never another pipeline's record of a job with the same name.
func (s suite) TestGreenVersionsIsTheIntersection(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	other := s.open(t, "other")
	ctx := context.Background()

	recordN(t, st, "items", "1", "2", "3", "4")
	recordN(t, other, "items", "1", "2", "3", "4")

	pass := func(st store.Store, job string, names ...string) {
		t.Helper()

		for _, name := range names {
			encoded, err := store.EncodeVersion(map[string]any{"n": name})
			if err != nil {
				t.Fatal(err)
			}

			err = st.RecordPassedVersion(ctx, store.Pass{JobName: job, Resource: "items", Version: encoded, BuildID: "b"})
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	pass(st, "a", "3", "1", "2")
	pass(st, "b", "2", "3", "4")
	pass(other, "a", "4")

	for _, tc := range []struct {
		upstream []string
		want     string
	}{
		{[]string{"a", "b"}, "[2 3]"},
		{[]string{"a"}, "[1 2 3]"},
		{[]string{"a", "a"}, "[1 2 3]"},
		{[]string{"a", "b", "never-ran"}, "[]"},
		{nil, "[1 2 3 4]"},
	} {
		green, err := st.GreenVersions(ctx, "items", tc.upstream)
		if err != nil {
			t.Fatal(err)
		}

		if got := fmt.Sprint(versionNames(t, green)); got != tc.want {
			t.Errorf("GreenVersions(%q) = %s, want %s", tc.upstream, got, tc.want)
		}
	}
}

// TestRunInputsAreKeptPerGet: two gets of one resource in a run are two
// records, each under its own name; and a run is created once, so recording
// a get again is the same row.
func (s suite) TestRunInputsAreKeptPerGet(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "web")

	const run = "PERBUILD02"

	err := st.StartRun(ctx, store.NewRun{ID: run, JobName: "build", Workspace: "/tmp/web"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	for _, input := range []store.RunInput{
		{Input: "code", Resource: "repo", Version: `{"ref":"abc"}`},
		{Input: "baseline", Resource: "repo", Version: `{"ref":"v1"}`},
		{Input: "code", Resource: "repo", Version: `{"ref":"abc"}`},
	} {
		err = st.RecordRunInput(ctx, run, store.RunInput{Input: input.Input, Resource: input.Resource, Version: input.Version})
		if err != nil {
			t.Fatalf("RecordRunInput %+v: %v", input, err)
		}
	}

	inputs, err := st.RunInputs(ctx, run)
	if err != nil {
		t.Fatalf("RunInputs: %v", err)
	}

	got := map[store.RunInput]bool{}
	for _, input := range inputs {
		got[input] = true
	}

	want := []store.RunInput{
		{Input: "code", Resource: "repo", Version: `{"ref":"abc"}`},
		{Input: "baseline", Resource: "repo", Version: `{"ref":"v1"}`},
	}

	if len(inputs) != len(want) {
		t.Fatalf("RunInputs = %+v, want one row per get", inputs)
	}

	for _, input := range want {
		if !got[input] {
			t.Errorf("RunInputs = %+v, missing %+v", inputs, input)
		}
	}
}

// TestVersionRunsAreTheRunsThatTookEachVersion: the resource page asks, per
// version, which runs took it — newest first, so the first row for a version
// is where that version stands now. Scoped like every other read: a second
// pipeline with the same job and resource names must see none of it.
func (s suite) TestVersionRunsAreTheRunsThatTookEachVersion(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	web := s.open(t, "web")
	infra := s.open(t, "infra")

	for _, run := range []struct {
		id      string
		version store.VersionKey
		status  store.RunStatus
	}{
		{"VRUNS01", `{"ref":"v1"}`, "aborted"},
		{"VRUNS02", `{"ref":"v1"}`, "succeeded"},
		{"VRUNS03", `{"ref":"v2"}`, "failed"},
	} {
		recordInputRun(t, web, run.id, "repo", run.version, run.status)

		// started_at orders the answer; one run per tick keeps that order
		// the one this test wrote rather than a tie.
		time.Sleep(2 * time.Millisecond)
	}

	recordInputRun(t, web, "VRUNS04", "other", `{"ref":"v1"}`, "succeeded")

	got, err := web.VersionRuns(ctx, "repo")
	if err != nil {
		t.Fatalf("VersionRuns: %v", err)
	}

	want := []store.VersionRun{
		{Version: `{"ref":"v2"}`, Run: store.RunRow{ID: "VRUNS03", JobName: "build", Status: "failed"}},
		{Version: `{"ref":"v1"}`, Run: store.RunRow{ID: "VRUNS02", JobName: "build", Status: "succeeded"}},
		{Version: `{"ref":"v1"}`, Run: store.RunRow{ID: "VRUNS01", JobName: "build", Status: "aborted"}},
	}

	if len(got) != len(want) {
		t.Fatalf("VersionRuns = %+v, want %d rows, only repo's", got, len(want))
	}

	for i := range want {
		g := store.VersionRun{Version: got[i].Version, Run: store.RunRow{ID: got[i].Run.ID, JobName: got[i].Run.JobName, Status: got[i].Run.Status}}
		if g != want[i] {
			t.Errorf("VersionRuns[%d] = %+v, want %+v", i, g, want[i])
		}
	}

	other, err := infra.VersionRuns(ctx, "repo")
	if err != nil {
		t.Fatalf("VersionRuns infra: %v", err)
	}

	if len(other) != 0 {
		t.Errorf("infra sees another pipeline's runs: %+v", other)
	}
}

// recordInputRun records a finished build run that took version of resource.
func recordInputRun(t *testing.T, st store.Store, runID, resource string, version store.VersionKey, status store.RunStatus) {
	t.Helper()

	ctx := context.Background()

	err := st.StartRun(ctx, store.NewRun{ID: runID, JobName: "build", Workspace: "/tmp/web"})
	if err != nil {
		t.Fatalf("StartRun %s: %v", runID, err)
	}

	err = st.RecordRunInput(ctx, runID, store.RunInput{Input: resource, Resource: resource, Version: version})
	if err != nil {
		t.Fatalf("RecordRunInput %s: %v", runID, err)
	}

	err = st.FinishRun(ctx, runID, status)
	if err != nil {
		t.Fatalf("FinishRun %s: %v", runID, err)
	}
}

func assertLastCheckedVersion(t *testing.T, st store.Store, resourceName string, wantFound bool, wantVersion store.VersionKey) {
	t.Helper()

	last, found, err := st.LastChecked(context.Background(), resourceName)
	if err != nil {
		t.Fatalf("LastChecked(%q): %v", resourceName, err)
	}

	if found != wantFound || (found && last.Version != wantVersion) {
		t.Fatalf("LastChecked(%q) = (%q, %v), want (%q, %v)", resourceName, last.Version, found, wantVersion, wantFound)
	}
}

func (s suite) TestStoreCheckedVersionRoundTrip(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	ctx := context.Background()

	assertLastCheckedVersion(t, st, "thing", false, "")

	err := st.RecordCheckedVersion(ctx, "thing", `{"ref":"v1"}`)
	if err != nil {
		t.Fatalf("RecordCheckedVersion: %v", err)
	}

	assertLastCheckedVersion(t, st, "thing", true, `{"ref":"v1"}`)

	// Upsert: recording a new version for the same resource replaces it.
	err = st.RecordCheckedVersion(ctx, "thing", `{"ref":"v2"}`)
	if err != nil {
		t.Fatalf("RecordCheckedVersion (upsert): %v", err)
	}

	assertLastCheckedVersion(t, st, "thing", true, `{"ref":"v2"}`)
}

// TestConsumedMarkRoundTrip covers the get: version: every cursor: how far a
// job has fanned out, per (job, resource).
func (s suite) TestConsumedMarkRoundTrip(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := context.Background()

	mark, err := st.ConsumedMark(ctx, "answer", "mentions")
	if err != nil {
		t.Fatalf("ConsumedMark: %v", err)
	}

	if mark != 0 {
		t.Fatalf("a fresh store reports mark %d, want 0 — nothing taken", mark)
	}

	err = st.RecordConsumedMark(ctx, "answer", "mentions", 5)
	if err != nil {
		t.Fatalf("RecordConsumedMark: %v", err)
	}

	// Re-recording is a no-op, not an error: a resumed or replayed run must
	// not fail on work it already did.
	err = st.RecordConsumedMark(ctx, "answer", "mentions", 5)
	if err != nil {
		t.Fatalf("RecordConsumedMark (again): %v", err)
	}

	// And it only moves forward. A backfilled version reaching a job that has
	// already moved past it must not rewind the mark and hand back everything
	// in between.
	err = st.RecordConsumedMark(ctx, "answer", "mentions", 2)
	if err != nil {
		t.Fatalf("RecordConsumedMark (older): %v", err)
	}

	mark, err = st.ConsumedMark(ctx, "answer", "mentions")
	if err != nil {
		t.Fatalf("ConsumedMark: %v", err)
	}

	if mark != 5 {
		t.Errorf("mark = %d, want 5 — an older version must not rewind it", mark)
	}

	// Scoped per job and per resource — another job has taken nothing.
	other, err := st.ConsumedMark(ctx, "other-job", "mentions")
	if err != nil {
		t.Fatalf("ConsumedMark: %v", err)
	}

	if other != 0 {
		t.Errorf("another job reports mark %d; the cursor is per job", other)
	}
}
