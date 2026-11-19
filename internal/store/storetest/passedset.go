package storetest

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// passAs records one upstream build: each resource=version pair green under build for job.
func passAs(t *testing.T, st store.Store, job, build string, pairs ...string) {
	t.Helper()

	passes := make([]store.Pass, 0, len(pairs)/2)

	for i := 0; i+1 < len(pairs); i += 2 {
		encoded, err := store.EncodeVersion(map[string]any{"n": pairs[i+1]})
		if err != nil {
			t.Fatal(err)
		}

		passes = append(passes, store.Pass{JobName: job, Resource: pairs[i], Version: encoded, BuildID: build})
	}

	err := st.RecordPassedVersions(ctxFor(t), passes)
	if err != nil {
		t.Fatalf("RecordPassedVersions: %v", err)
	}
}

// passedSetNames is PassedSet's answer as resource=n pairs, or "none".
func passedSetNames(t *testing.T, st store.Store, constraints map[string][]string) string {
	t.Helper()

	set, found, err := st.PassedSet(ctxFor(t), constraints)
	if err != nil {
		t.Fatalf("PassedSet(%v): %v", constraints, err)
	}

	if !found {
		return "none"
	}

	named := map[string]string{}

	for resource, version := range set {
		decoded, err := store.DecodeVersion(version)
		if err != nil {
			t.Fatal(err)
		}

		named[resource] = fmt.Sprint(decoded["n"])
	}

	return fmt.Sprint(named)
}

// TestPassedSetIsTheNewestBuildHoldingEveryResource is docs/infra.md's fan-in: r2 and c2 are each the newest green version and never passed together, so the set is the newest build holding both — not the newest of each.
func (s suite) TestPassedSetIsTheNewestBuildHoldingEveryResource(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	other := s.open(t, "other")

	recordN(t, st, "repo", "r1", "r2", "r3")
	recordN(t, st, "config", "c1", "c2", "c3")

	passAs(t, st, "upstream", "build-1", "repo", "r2", "config", "c1")
	passAs(t, st, "upstream", "build-2", "repo", "r1", "config", "c2")
	passAs(t, st, "upstream", "build-3", "repo", "r3")
	passAs(t, st, "upstream", "build-5", "config", "c3")
	passAs(t, st, "upstream", "", "repo", "r4", "config", "c4")
	recordN(t, other, "config", "c3", "c4", "c5")
	passAs(t, other, "upstream", "build-4", "repo", "r3", "config", "c5")

	both := map[string][]string{"repo": {"upstream"}, "config": {"upstream"}}

	if got, want := passedSetNames(t, st, both), "map[config:c2 repo:r1]"; got != want {
		t.Errorf("PassedSet = %s, want %s: the newest build that held both, not a build holding one, a row with no build, or another pipeline's", got, want)
	}

	if got, want := passedSetNames(t, st, map[string][]string{"repo": {"upstream"}}), "map[repo:r3]"; got != want {
		t.Errorf("PassedSet(repo) = %s, want %s: one resource is its own coherent set", got, want)
	}

	if got, want := passedSetNames(t, st, nil), "map[]"; got != want {
		t.Errorf("PassedSet(nil) = %s, want %s: no constraint is vacuously met", got, want)
	}

	if got, want := passedSetNames(t, st, map[string][]string{"repo": {"never-ran"}}), "none"; got != want {
		t.Errorf("PassedSet(never-ran) = %s, want %s", got, want)
	}
}

// TestPassedSetOrdersBuildsByTheirVersions: a green rerun of an old build must not jump ahead of the newer version (docs/conformance.md's rerun row), and a build that both fetched and put a resource offers the newer, the one it put.
func (s suite) TestPassedSetOrdersBuildsByTheirVersions(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	recordN(t, st, "items", "1", "2", "3")

	passAs(t, st, "upstream", "build-1", "items", "1", "items", "3")
	passAs(t, st, "upstream", "build-2", "items", "2")

	if got, want := passedSetNames(t, st, map[string][]string{"items": {"upstream"}}), "map[items:3]"; got != want {
		t.Errorf("PassedSet = %s, want %s: the newest version passed, though a later build passed an older one", got, want)
	}
}

// TestPassedSetAgreesAcrossUpstreamJobs: passed: [unit, vet] is both, so unit's newest build is passed over when vet never passed its repo, and an older unit build vet agrees with is taken.
func (s suite) TestPassedSetAgreesAcrossUpstreamJobs(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	recordN(t, st, "repo", "r1", "r2")
	recordN(t, st, "config", "c1", "c2")

	passAs(t, st, "unit", "u-1", "repo", "r2", "config", "c1")
	passAs(t, st, "unit", "u-2", "repo", "r1", "config", "c2")
	passAs(t, st, "vet", "v-1", "repo", "r2")

	constraints := map[string][]string{"repo": {"unit", "vet"}, "config": {"unit"}}

	if got, want := passedSetNames(t, st, constraints), "map[config:c1 repo:r2]"; got != want {
		t.Errorf("PassedSet = %s, want %s", got, want)
	}

	if got, want := passedSetNames(t, st, map[string][]string{"config": {"vet"}}), "none"; got != want {
		t.Errorf("PassedSet(config by vet) = %s, want %s", got, want)
	}
}

// TestPassedSetForgetsWhatAFailedBranchChose: gamma never passed c2, so beta's choice of it must be undone before alpha's older build is tried, or the right set is judged against a version no longer chosen.
func (s suite) TestPassedSetForgetsWhatAFailedBranchChose(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	recordN(t, st, "repo", "r1", "r2")
	recordN(t, st, "config", "c1", "c2")
	recordN(t, st, "image", "i1", "i2")

	passAs(t, st, "alpha", "a-1", "image", "i1", "repo", "r1")
	passAs(t, st, "alpha", "a-2", "image", "i2", "repo", "r2")
	passAs(t, st, "beta", "b-1", "repo", "r1", "config", "c1")
	passAs(t, st, "beta", "b-2", "repo", "r2", "config", "c2")
	passAs(t, st, "gamma", "g-1", "config", "c1")

	constraints := map[string][]string{"repo": {"alpha", "beta"}, "config": {"beta", "gamma"}, "image": {"alpha"}}

	if got, want := passedSetNames(t, st, constraints), "map[config:c1 image:i1 repo:r1]"; got != want {
		t.Errorf("PassedSet = %s, want %s", got, want)
	}
}

// TestPassedSetPagesThroughOlderBuilds: the one build a second job agrees with can be older than a page of the first job's builds.
func (s suite) TestPassedSetPagesThroughOlderBuilds(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	builds := store.PassedBuildsPage + 8
	names := make([]string, 0, builds)

	for i := 1; i <= builds; i++ {
		names = append(names, strconv.Itoa(i))
	}

	recordN(t, st, "items", names...)

	for _, name := range names {
		passAs(t, st, "unit", "u-"+name, "items", name)
	}

	passAs(t, st, "vet", "v-1", "items", "1")

	if got, want := passedSetNames(t, st, map[string][]string{"items": {"unit", "vet"}}), "map[items:1]"; got != want {
		t.Errorf("PassedSet = %s, want %s: the oldest of %d unit builds is the only one vet passed", got, want, builds)
	}
}

// TestNewestVersionIsWhatACheckFiledLast: a version only a run used is not one a check reported, and another pipeline's history is not this one's.
func (s suite) TestNewestVersionIsWhatACheckFiledLast(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	other := s.open(t, "other")
	ctx := ctxFor(t)

	_, found, err := st.NewestVersion(ctx, "items")
	if err != nil || found {
		t.Fatalf("NewestVersion of nothing = %v, %v; want not found", found, err)
	}

	recordN(t, st, "items", "1", "2")
	recordN(t, other, "items", "3")
	passAs(t, st, "unit", "u-1", "items", "9")

	newest, found, err := st.NewestVersion(ctx, "items")
	if err != nil || !found || newest != `{"n":"2"}` {
		t.Errorf("NewestVersion = %s, %v, %v; want the last version this pipeline's check filed", newest, found, err)
	}
}
