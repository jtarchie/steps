package pipeline

// Turning a step's tags: into the machine it runs on.
//
// The pipeline says what a step needs; the invocation says which machine has
// it. Keeping the mapping out of the pipeline file is what lets the same
// pipeline run on somebody else's fleet, and it is the same split Concourse
// draws between a step's tags: and a worker's advertised ones.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/venue"
)

// workersKey types the context value carrying the tag-to-worker mapping.
type workersKey struct{}

// WithWorkers records the --worker mappings for this invocation, rejecting any
// that cannot be reached as written.
//
// Parsed here rather than at first use so a typo in a worker URL is reported
// before a run starts, alongside every other thing that is wrong with the
// invocation, rather than mid-plan when a step happens to reach for it.
func WithWorkers(ctx context.Context, mappings map[string]string) (context.Context, error) {
	if len(mappings) == 0 {
		return ctx, nil
	}

	workers := make(map[string]venue.Worker, len(mappings))

	for _, tag := range sortedKeys(mappings) {
		worker, err := venue.ParseWorker(mappings[tag])
		if err != nil {
			return nil, fmt.Errorf("--worker %s: %w", tag, err)
		}

		workers[tag] = worker
	}

	return context.WithValue(ctx, workersKey{}, workers), nil
}

func workersFrom(ctx context.Context) map[string]venue.Worker {
	workers, _ := ctx.Value(workersKey{}).(map[string]venue.Worker)

	return workers
}

// workerFor answers which worker a step runs on, or "" for this machine.
func workerFor(ctx context.Context, step config.Step) string {
	tag := placementTag(step)
	if tag == "" {
		return ""
	}

	worker, ok := workersFrom(ctx)[tag]
	if !ok {
		// Unreachable in a real run: ValidateWorkerPlacement refuses an
		// unmapped tag before the first step executes.
		return ""
	}

	return worker.URL
}

// placementTag is the one tag a step carries, if any. A try: wrapper is
// transparent here for the same reason it is everywhere else: the wrapped step
// is the one that runs.
func placementTag(step config.Step) string {
	if step.Try != nil {
		return placementTag(*step.Try)
	}

	if len(step.Tags) == 0 {
		return ""
	}

	return step.Tags[0]
}

// ValidateWorkerPlacement refuses a job whose steps name workers this
// invocation cannot supply.
//
// Before anything runs, and deliberately not a fallback to local execution: a
// step that says it needs a GPU box, quietly running on a laptop instead, is
// the kind of promise-it-cannot-keep that network: without image: is refused
// for. A pipeline that has to run without workers says so by not tagging.
func ValidateWorkerPlacement(ctx context.Context, job *config.Job) error {
	workers := workersFrom(ctx)

	missing := map[string][]string{}

	err := job.VisitSteps(func(label string, step *config.Step) error {
		tag := placementTag(*step)
		if tag == "" {
			return nil
		}

		if _, ok := workers[tag]; !ok {
			missing[tag] = append(missing[tag], label)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("job %q: %w", job.Name, err)
	}

	if len(missing) == 0 {
		return nil
	}

	tags := sortedKeys(missing)

	return fmt.Errorf("job %q: no worker is registered for %s (%s) — map it with --worker %s=ssh://user@host, or remove the tag",
		job.Name, pluralTag(len(tags)), describeMissing(missing, tags), tags[0])
}

func describeMissing(missing map[string][]string, tags []string) string {
	parts := make([]string, 0, len(tags))

	for _, tag := range tags {
		parts = append(parts, fmt.Sprintf("%s on %s", tag, strings.Join(missing[tag], ", ")))
	}

	return strings.Join(parts, "; ")
}

func pluralTag(n int) string {
	if n == 1 {
		return "tag"
	}

	return "tags"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}
