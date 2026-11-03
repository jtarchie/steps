package pipeline

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/jtarchie/steps/internal/config"
)

// ValidateWorkerPlacement refuses a job whose steps name workers this
// invocation cannot supply.
//
// Before anything runs, and deliberately not a fallback to local execution: a
// step that says it needs a GPU box, quietly running on a laptop instead, is
// the kind of promise-it-cannot-keep that network: without image: is refused
// for. A pipeline that has to run without workers says so by not tagging.
func ValidateWorkerPlacement(ctx context.Context, cfg *config.Config, job *config.Job) error {
	workers := workersFrom(ctx)

	missing := map[string][]string{}

	err := job.VisitSteps(func(label string, step *config.Step) error {
		// The step's own tag, and — for a get or put — the RESOURCE's, which
		// is where its check runs even when the step overrides it for the
		// fetch. Both are dialled, so both have to be mapped.
		for _, tag := range stepPlacementTags(cfg, *step) {
			worker, ok := workers[tag]
			if !ok {
				missing[tag] = append(missing[tag], label)

				continue
			}

			// Before any step runs, and before an acquisition rung launches a billed machine to discover it.
			checkErr := worker.ImageCheck(cfg.PlacedImage(*step) != "")
			if checkErr != nil {
				return fmt.Errorf("%s on --worker %s: %w", label, tag, checkErr)
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("job %q: %w", job.Name, err)
	}

	if len(missing) == 0 {
		return nil
	}

	tags := slices.Sorted(maps.Keys(missing))

	return fmt.Errorf("job %q: no worker is registered for %s (%s) — map it with --worker %s=ssh://user@host, or remove the tag",
		job.Name, pluralTag(len(tags)), describeMissing(missing, tags), tags[0])
}

// ValidateExclusivePools refuses every job that touches two or more capped exclusive pools: one job holding a and waiting for b while another holds b and waits for a never ends, and nothing would ever say why.
func ValidateExclusivePools(ctx context.Context, cfg *config.Config) error {
	workers := workersFrom(ctx)

	var refusals []error

	for i := range cfg.Jobs {
		job := &cfg.Jobs[i]
		pools := map[string][]string{}

		_ = job.VisitSteps(func(_ string, step *config.Step) error {
			for _, tag := range stepPlacementTags(cfg, *step) {
				pool := workers[tag].CappedPool()
				if pool != "" && !slices.Contains(pools[pool], tag) {
					pools[pool] = append(pools[pool], tag)
				}
			}

			return nil
		})

		if len(pools) < 2 {
			continue
		}

		tags := []string{}
		for _, named := range pools {
			tags = append(tags, named...)
		}

		sort.Strings(tags)

		refusals = append(refusals, fmt.Errorf("job %q takes a machine from %d capped exclusive pools (tags %s): two such jobs can each hold one pool and wait forever for the other's — put the job's steps on one pool, or drop max= from all but one",
			job.Name, len(pools), strings.Join(tags, ", ")))
	}

	return errors.Join(refusals...)
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
