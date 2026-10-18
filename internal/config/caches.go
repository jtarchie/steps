package config

// caches: — directories a task or agent step keeps between runs, Concourse's task caches.

import (
	"fmt"
	"path"
	"strings"
)

// TaskCache is one directory a step keeps between runs, on the machine that ran it: Concourse's task cache. Scoped to pipeline + job + step name + path, empty on first use and on any other machine, saved whatever the step's exit code. Never hashed: a cache only makes the step faster, so a warm one must not make identical work look different. It is never an input or an output either: not passed to other steps, not captured by the step cache.
type TaskCache struct {
	// Path is relative to the step's working directory. It may sit inside an input, which it then shadows, as Concourse's own node_modules example does; it may not be or contain an input, nor touch an output, which would carry the cache into the step's results.
	Path string `yaml:"path"`
}

// CachePaths are caches' paths, cleaned, the form every consumer keys and mounts by.
func CachePaths(caches []TaskCache) []string {
	paths := make([]string, 0, len(caches))

	for _, c := range caches {
		paths = append(paths, path.Clean(c.Path))
	}

	return paths
}

// resolveCaches is declared-wins, like env:: a step's `caches: []` turns off its entry's caches.
func resolveCaches(entry []TaskCache, step Step) []TaskCache {
	if step.Caches != nil {
		return step.Caches
	}

	return entry
}

func (c *Config) validateCaches() error {
	for i := range c.Tasks {
		err := validateCachePaths(fmt.Sprintf("task %q", c.Tasks[i].Name), c.Tasks[i].Caches, nil, nil)
		if err != nil {
			return err
		}
	}

	for i := range c.Agents {
		err := validateCachePaths(fmt.Sprintf("agent %q", c.Agents[i].Name), c.Agents[i].Caches, nil, nil)
		if err != nil {
			return err
		}
	}

	for i := range c.Jobs {
		err := c.Jobs[i].visitSteps(c.checkStepCaches)
		if err != nil {
			return err
		}
	}

	return nil
}

// checkStepCaches judges the RESOLVED caches against the resolved inputs and outputs, since an entry's cache can collide with a step's own output.
func (c *Config) checkStepCaches(label string, step *Step) error {
	switch kind, _ := step.Kind(); kind {
	case StepKindTask:
		resolved, err := c.ResolveTask(*step)
		if err != nil {
			return nil //nolint:nilerr // an unresolvable task is validateStepReferences' error, not this one's
		}

		return validateCachePaths(label, resolved.Caches, resolved.Inputs, resolved.Outputs)
	case StepKindAgent:
		agent, err := c.FindAgent(step.Agent)
		if err != nil {
			return nil //nolint:nilerr // an unknown agent is validateStepReferences' error, not this one's
		}

		return validateCachePaths(label, resolveCaches(agent.Caches, *step), step.InputNames(), step.Outputs)
	case StepKindGet, StepKindPut, StepKindTry, StepKindInParallel, StepKindRace,
		StepKindEnsemble, StepKindLoadVar, StepKindApproval, StepKindDo:
	}

	if step.Caches != nil {
		return fmt.Errorf("%s: caches is only valid on task and agent steps", label)
	}

	return nil
}

func validateCachePaths(label string, caches []TaskCache, inputs, outputs []string) error {
	seen := make([]string, 0, len(caches))

	for _, cache := range caches {
		cleaned, ok := ConfinedPath(".", cache.Path)
		if !ok || cleaned == "." {
			return fmt.Errorf("%s: cache path %q must be a directory inside the working directory", label, cache.Path)
		}

		for _, other := range seen {
			if overlaps(cleaned, other) {
				return fmt.Errorf("%s: cache paths %q and %q overlap", label, other, cleaned)
			}
		}

		seen = append(seen, cleaned)

		err := checkCacheAgainstArtifacts(label, cleaned, inputs, outputs)
		if err != nil {
			return err
		}
	}

	return nil
}

func checkCacheAgainstArtifacts(label, cleaned string, inputs, outputs []string) error {
	for _, out := range outputs {
		if overlaps(cleaned, out) {
			return fmt.Errorf("%s: cache path %q overlaps output %q, which would carry the cache into the step's results", label, cleaned, out)
		}
	}

	for _, in := range inputs {
		if within(in, cleaned) {
			return fmt.Errorf("%s: cache path %q would replace input %q", label, cleaned, in)
		}
	}

	return nil
}

func overlaps(a, b string) bool {
	return within(a, b) || within(b, a)
}

// within reports whether p is dir or under it.
func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}
