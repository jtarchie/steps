package pipeline

// steps clear-task-cache: Concourse's fly clear-task-cache, here and on the workers a step is placed on.

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/venue"
	"github.com/jtarchie/steps/internal/workspace"
)

// ClearedCache is one cache a clear reached, or a Note on where none is kept.
type ClearedCache struct {
	// Path is the cache's cleaned path; empty for a Note about every path at Where.
	Path  string
	Where string
	Found bool
	Note  string
	Err   error
}

// ClearTaskCaches empties the caches of the task or agent step named step in job, on this machine and on every worker it is placed on; cachePath, when set, picks one. Refusals come back before anything is removed, so a refused clear removes nothing; per-cache failures are each result's Err.
func ClearTaskCaches(ctx context.Context, cfg *config.Config, jobName, step, cachePath string, start bool) ([]ClearedCache, error) {
	// ponytail: run from a terminal against the file and its --worker mappings only; a steps web route, clearing with the daemon's own pipelines and workers, is the upgrade.
	paths, tags, err := stepCaches(cfg, jobName, step, cachePath)
	if err != nil {
		return nil, err
	}

	workers := map[string]string{}
	missing := map[string][]string{}

	for _, tag := range tags {
		worker, ok := workersFrom(ctx)[tag]
		if !ok {
			missing[tag] = append(missing[tag], fmt.Sprintf("step %q", step))

			continue
		}

		if worker.Rung == venue.RungStopped && !start {
			return nil, fmt.Errorf("--worker %s: %w (pass --start to start it, clear, and stop it again)", tag, venue.ErrParked)
		}

		workers[tag] = worker.URL
	}

	if len(missing) > 0 {
		names := sortedKeys(missing)

		return nil, fmt.Errorf("job %q: no worker is registered for %s (%s) — map it with --worker %s=ssh://user@host, or the cache there stays",
			jobName, pluralTag(len(names)), describeMissing(missing, names), names[0])
	}

	keyed := workspace.TaskCacheKeys(cfg.Name, jobName, step, cachesOf(paths))
	keys := make([]string, 0, len(paths))

	for _, p := range paths {
		keys = append(keys, keyed[p])
	}

	cleared, err := clearLocal(cfg.Workspace, paths, keys)
	if err != nil {
		return cleared, err
	}

	for _, tag := range sortedKeys(workers) {
		cleared = append(cleared, clearOnWorker(ctx, tag, workers[tag], paths, keys, start)...)
	}

	return cleared, nil
}

func clearLocal(ws *config.WorkspaceConfig, paths, keys []string) ([]ClearedCache, error) {
	const here = "this machine"

	if ws == nil || ws.Root == "" {
		return []ClearedCache{{Where: here, Note: "no workspace.root is set, so no cache outlives its run"}}, nil
	}

	found, err := workspace.ClearTaskCaches(ws, keys)
	if err != nil {
		return nil, fmt.Errorf("clearing the caches under %s: %w", ws.Root, err)
	}

	cleared := make([]ClearedCache, 0, len(paths))

	for i, p := range paths {
		cleared = append(cleared, ClearedCache{Path: p, Where: here, Found: found[keys[i]]})
	}

	return cleared, nil
}

func clearOnWorker(ctx context.Context, tag, url string, paths, keys []string, start bool) []ClearedCache {
	where := "worker " + tag

	removed, err := venue.ClearTaskCaches(ctx, url, keys, start)
	if errors.Is(err, venue.ErrKeepsNoCache) {
		return []ClearedCache{{Where: where, Note: "nothing to clear: " + err.Error()}}
	}

	if err != nil {
		return []ClearedCache{{Where: where, Err: err}}
	}

	cleared := make([]ClearedCache, 0, len(removed))

	for i, r := range removed {
		cleared = append(cleared, ClearedCache{Path: paths[i], Where: where, Found: r.Found, Err: r.Err})
	}

	return cleared
}

// stepCaches finds every step of the job the cache key names step (its task: or agent:), and returns their cache paths, the one asked for when cachePath is set, and the tags they are placed by.
func stepCaches(cfg *config.Config, jobName, step, cachePath string) ([]string, []string, error) {
	job, err := cfg.FindJob(jobName)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // FindJob names the job and lists the others
	}

	found := matchedSteps{cfg: cfg, step: step}

	err = job.VisitSteps(found.visit)
	if err != nil {
		return nil, nil, fmt.Errorf("job %q: %w", jobName, err)
	}

	if !found.matched {
		return nil, nil, fmt.Errorf("job %q has no task or agent step named %q (it has: %s)", jobName, step, strings.Join(slices.Compact(slices.Sorted(slices.Values(found.names))), ", "))
	}

	if len(found.paths) == 0 {
		return nil, nil, fmt.Errorf("job %q: step %q declares no caches", jobName, step)
	}

	if cachePath == "" {
		return found.paths, found.tags, nil
	}

	want := path.Clean(cachePath)
	if !slices.Contains(found.paths, want) {
		return nil, nil, fmt.Errorf("job %q: step %q has no cache at %q (its caches: %s)", jobName, step, want, strings.Join(found.paths, ", "))
	}

	return []string{want}, found.tags, nil
}

// matchedSteps gathers, across every step a job runs under one name, what a clear of that name reaches.
type matchedSteps struct {
	cfg                *config.Config
	step               string
	matched            bool
	paths, tags, names []string
}

func (m *matchedSteps) visit(_ string, s *config.Step) error {
	caches, name, ok := cachesFor(m.cfg, *s)
	if !ok {
		return nil
	}

	m.names = append(m.names, name)
	if name != m.step {
		return nil
	}

	m.matched = true

	for _, p := range config.CachePaths(caches) {
		if !slices.Contains(m.paths, p) {
			m.paths = append(m.paths, p)
		}
	}

	if tag := placementTag(*s); tag != "" && !slices.Contains(m.tags, tag) {
		m.tags = append(m.tags, tag)
	}

	return nil
}

// cachesFor is a step's resolved caches and the name they are keyed by, for a task or agent step.
func cachesFor(cfg *config.Config, s config.Step) ([]config.TaskCache, string, bool) {
	switch kind, _ := s.Kind(); kind {
	case config.StepKindTask:
		resolved, err := cfg.ResolveTask(s)
		if err != nil {
			return nil, s.Task, true
		}

		return resolved.Caches, s.Task, true
	case config.StepKindAgent:
		resolved, err := cfg.ResolveAgentInvocation(s)
		if err != nil {
			return nil, s.Agent, true
		}

		return resolved.Caches, s.Agent, true
	case config.StepKindGet, config.StepKindPut, config.StepKindTry, config.StepKindInParallel, config.StepKindRace,
		config.StepKindEnsemble, config.StepKindLoadVar, config.StepKindApproval, config.StepKindDo:
	}

	return nil, "", false
}

func cachesOf(paths []string) []config.TaskCache {
	caches := make([]config.TaskCache, 0, len(paths))
	for _, p := range paths {
		caches = append(caches, config.TaskCache{Path: p})
	}

	return caches
}
