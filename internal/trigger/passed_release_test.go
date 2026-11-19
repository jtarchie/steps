package trigger

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// pass records one green build of job: each resource=version pair, the version an object {key: value} keyed by the resource's first letter.
func pass(t *testing.T, st store.Store, job, build string, pairs ...string) {
	t.Helper()

	passes := make([]store.Pass, 0, len(pairs)/2)

	for i := 0; i+1 < len(pairs); i += 2 {
		encoded, err := store.EncodeVersion(map[string]any{pairs[i][:1]: pairs[i+1]})
		if err != nil {
			t.Fatal(err)
		}

		passes = append(passes, store.Pass{JobName: job, Resource: pairs[i], Version: encoded, BuildID: build})
	}

	err := st.RecordPassedVersions(t.Context(), passes)
	if err != nil {
		t.Fatalf("RecordPassedVersions: %v", err)
	}
}

func feedType(name, path string) config.ResourceType {
	return config.ResourceType{Name: name, Config: config.ResourceTypeConfig{Check: "cat " + path, In: "true"}}
}

// TestPassedNeverReleasesAJobWithNoTriggerGet: Concourse schedules a job only for a trigger: true input, so a job whose gets are all passed: is started by a person, never by its upstream going green. ship, beside it, is the control.
func TestPassedNeverReleasesAJobWithNoTriggerGet(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")
	writeVersions(t, versions, `[{"r":"v1"}]`)

	cfg := &config.Config{
		ResourceTypes: []config.ResourceType{feedType("listing", versions)},
		Resources:     []config.Resource{{Name: "repo", Type: "listing"}},
		Jobs: []config.Job{
			{Name: "test", Plan: []config.Step{{Get: "repo", Trigger: true}}},
			{Name: "ship", Plan: []config.Step{{Get: "repo", Trigger: true, Passed: []string{"test"}}}},
			{Name: "audit", Plan: []config.Step{{Get: "repo", Passed: []string{"test"}}}},
		},
	}

	st := mustOpenStore(t, dir)

	_, err := pollOnce(t.Context(), cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	pass(t, st, "test", "b1", "repo", "v1")

	enqueued, err := pollOnce(t.Context(), cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	if got := strings.Join(enqueued, ","); got != "ship" {
		t.Errorf("enqueued = %q, want only ship: audit has no trigger: true get", got)
	}
}

// fanInPipeline is docs/infra.md's fan-in, with its history filed in discovery order r1, r2 and c1, c2.
func fanInPipeline(t *testing.T, dir string, st store.Store) *config.Config {
	t.Helper()

	repo, cfgFeed := filepath.Join(dir, "repo.json"), filepath.Join(dir, "config.json")
	writeVersions(t, repo, `[{"r":"r1"},{"r":"r2"}]`)
	writeVersions(t, cfgFeed, `[{"c":"c1"},{"c":"c2"}]`)

	for resource, versions := range map[string][]map[string]any{
		"repo":   {{"r": "r1"}, {"r": "r2"}},
		"config": {{"c": "c1"}, {"c": "c2"}},
	} {
		_, err := st.RecordVersions(t.Context(), resource, versions, 0)
		if err != nil {
			t.Fatal(err)
		}

		encoded, err := store.EncodeVersion(versions[len(versions)-1])
		if err != nil {
			t.Fatal(err)
		}

		err = st.RecordCheckedVersion(t.Context(), resource, encoded)
		if err != nil {
			t.Fatal(err)
		}
	}

	return &config.Config{
		ResourceTypes: []config.ResourceType{feedType("repo-feed", repo), feedType("config-feed", cfgFeed)},
		Resources:     []config.Resource{{Name: "repo", Type: "repo-feed"}, {Name: "config", Type: "config-feed"}},
		Jobs: []config.Job{
			{Name: "upstream", Plan: []config.Step{{Get: "repo"}, {Get: "config"}}},
			{Name: "deploy", Plan: []config.Step{
				{Get: "repo", Trigger: true, Passed: []string{"upstream"}},
				{Get: "config", Passed: []string{"upstream"}},
			}},
		},
	}
}

// TestPassedReleasesTheFanInSetThatPassedTogether: judging each resource's newest green version held deploy forever on r2+c2; r1+c2 passed together and is released, once.
func TestPassedReleasesTheFanInSetThatPassedTogether(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	st := mustOpenStore(t, dir)
	cfg := fanInPipeline(t, dir, st)

	pass(t, st, "upstream", "build-1", "repo", "r2", "config", "c1")
	pass(t, st, "upstream", "build-2", "repo", "r1", "config", "c2")

	enqueued, err := pollOnce(t.Context(), cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	if !contains(enqueued, "deploy") {
		t.Fatalf("enqueued = %v, want deploy released for r1+c2, which passed together in build-2", enqueued)
	}

	pass(t, st, "deploy", "deploy-1", "repo", "r1", "config", "c2")

	enqueued, err = pollOnce(t.Context(), cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	if contains(enqueued, "deploy") {
		t.Errorf("enqueued = %v, want deploy left alone: it built r1+c2", enqueued)
	}
}

// TestPassedJudgesAlreadyRanOnTriggerInputs: a newer artifacts passing build is no reason to deploy again, because artifacts carries no trigger: — Concourse compares a job's trigger inputs only.
func TestPassedJudgesAlreadyRanOnTriggerInputs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	repoVersions, artifactVersions := filepath.Join(dir, "repo.json"), filepath.Join(dir, "artifacts.json")
	writeVersions(t, repoVersions, `[{"r":"v1"}]`)
	writeVersions(t, artifactVersions, `[{"a":"1"},{"a":"2"}]`)

	cfg := &config.Config{
		ResourceTypes: []config.ResourceType{feedType("repo-listing", repoVersions), feedType("artifact-listing", artifactVersions)},
		Resources:     []config.Resource{{Name: "repo", Type: "repo-listing"}, {Name: "artifacts", Type: "artifact-listing"}},
		Jobs: []config.Job{
			{Name: "build", Plan: []config.Step{{Get: "artifacts", Trigger: true}}},
			{Name: "deploy", Plan: []config.Step{
				{Get: "repo", Trigger: true},
				{Get: "artifacts", Passed: []string{"build"}},
			}},
		},
	}

	st := mustOpenStore(t, dir)

	_, err := pollOnce(t.Context(), cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	pass(t, st, "build", "b1", "artifacts", "1")
	pass(t, st, "deploy", "d1", "repo", "v1", "artifacts", "1")
	pass(t, st, "build", "b2", "artifacts", "2")

	enqueued, err := pollOnce(t.Context(), cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	if contains(enqueued, "deploy") {
		t.Errorf("enqueued = %v, want deploy left alone: it built repo v1, its only trigger input", enqueued)
	}
}

// TestPassedJudgesEachTriggerInputOnItsOwn: Concourse builds when some trigger input is a first occurrence (versions_db.go's IsFirstOccurrence, per input), so v2+c2 is no news when each was already built, if never together.
func TestPassedJudgesEachTriggerInputOnItsOwn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	repoVersions, configVersions := filepath.Join(dir, "repo.json"), filepath.Join(dir, "config.json")
	artifactVersions := filepath.Join(dir, "artifacts.json")
	writeVersions(t, repoVersions, `[{"r":"v1"},{"r":"v2"}]`)
	writeVersions(t, configVersions, `[{"c":"c1"},{"c":"c2"}]`)
	writeVersions(t, artifactVersions, `[{"a":"1"}]`)

	cfg := &config.Config{
		ResourceTypes: []config.ResourceType{
			feedType("repo-listing", repoVersions), feedType("config-listing", configVersions), feedType("artifact-listing", artifactVersions),
		},
		Resources: []config.Resource{{Name: "repo", Type: "repo-listing"}, {Name: "config", Type: "config-listing"}, {Name: "artifacts", Type: "artifact-listing"}},
		Jobs: []config.Job{
			{Name: "build", Plan: []config.Step{{Get: "artifacts", Trigger: true}}},
			{Name: "deploy", Plan: []config.Step{
				{Get: "repo", Trigger: true},
				{Get: "config", Trigger: true},
				{Get: "artifacts", Passed: []string{"build"}},
			}},
		},
	}

	st := mustOpenStore(t, dir)

	_, err := pollOnce(t.Context(), cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	pass(t, st, "build", "b1", "artifacts", "1")
	pass(t, st, "deploy", "d1", "repo", "v1", "config", "c2", "artifacts", "1")
	pass(t, st, "deploy", "d2", "repo", "v2", "config", "c1", "artifacts", "1")

	enqueued, err := pollOnce(t.Context(), cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	if contains(enqueued, "deploy") {
		t.Errorf("enqueued = %v, want deploy left alone: it has built repo v2 and config c2, each", enqueued)
	}
}

// TestPassedHoldsAJobWhoseTriggerInputHasNoVersion: Concourse cannot resolve an input with no version, so it builds nothing; releasing here would send the job's get to a check that has never answered.
func TestPassedHoldsAJobWhoseTriggerInputHasNoVersion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	artifactVersions := filepath.Join(dir, "artifacts.json")
	writeVersions(t, artifactVersions, `[{"a":"1"}]`)

	cfg := &config.Config{
		ResourceTypes: []config.ResourceType{
			feedType("repo-listing", filepath.Join(dir, "never-written.json")),
			feedType("artifact-listing", artifactVersions),
		},
		Resources: []config.Resource{{Name: "repo", Type: "repo-listing"}, {Name: "artifacts", Type: "artifact-listing"}},
		Jobs: []config.Job{
			{Name: "build", Plan: []config.Step{{Get: "artifacts", Trigger: true}}},
			{Name: "deploy", Plan: []config.Step{
				{Get: "repo", Trigger: true},
				{Get: "artifacts", Passed: []string{"build"}},
			}},
		},
	}

	st := mustOpenStore(t, dir)
	pass(t, st, "build", "b1", "artifacts", "1")

	enqueued, _ := pollOnce(t.Context(), cfg, st)

	if contains(enqueued, "deploy") {
		t.Errorf("enqueued = %v, want deploy held: its trigger input repo has never had a version", enqueued)
	}
}

// panickingStore is a store whose resource lookups panic for one resource, the way a built-in type's check can.
type panickingStore struct {
	PollStore

	resource string
}

func (p panickingStore) LastChecked(ctx context.Context, name string) (store.CheckedResource, bool, error) {
	if name == p.resource {
		panic("a built-in check blew up")
	}

	return p.PollStore.LastChecked(ctx, name) //nolint:wrapcheck // a test double passing through
}

// TestAPanickingCheckIsACheckFailure: a panic in one resource's turn used to unwind the poller's goroutine and the daemon with it; it is that resource's check failure instead, and the others are still polled.
func TestAPanickingCheckIsACheckFailure(t *testing.T) {
	logs := captureLog(t)

	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")
	writeVersions(t, versions, `[{"r":"v1"}]`)

	cfg := &config.Config{
		ResourceTypes: []config.ResourceType{feedType("listing", versions)},
		Resources:     []config.Resource{{Name: "boom", Type: "listing"}, {Name: "fine", Type: "listing"}},
		Jobs: []config.Job{
			{Name: "watch", Plan: []config.Step{{Get: "boom", Trigger: true}, {Get: "fine", Trigger: true}}},
		},
	}

	st := mustOpenStore(t, dir)

	_, err := pollOnce(t.Context(), cfg, panickingStore{PollStore: st, resource: "boom"})
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "panic") {
		t.Errorf("pollOnce error = %v, want boom's panic as its failure", err)
	}

	if reason := checkErrorFor(t, st, "boom"); !strings.Contains(reason, "panic") {
		t.Errorf("boom's recorded check error = %q, want the panic", reason)
	}

	_, found, err := st.LastChecked(t.Context(), "fine")
	if err != nil || !found {
		t.Errorf("fine was not polled after boom panicked: found=%v err=%v", found, err)
	}

	if line := logLine(logs.String(), "trigger.check_panic"); !strings.Contains(line, "goroutine") {
		t.Errorf("no trigger.check_panic line carrying the stack in:\n%s", logs)
	}
}
