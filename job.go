package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// RunJob executes job's plan steps in order inside workspaceDir (a fresh
// temp directory). pinned applies to any `get` step's version selection.
//
// Before executing anything, it plans every chain the job's steps could
// resolve to (resolving get versions but running nothing) and checks store
// for chains that already succeeded with identical content, so that
// already-run get/task work can be skipped entirely. put steps are never
// skipped — see runSteps. skipCache (--force) bypasses this and re-runs
// everything, though results are still recorded as usual.
func RunJob(ctx context.Context, cfg *Config, job *Job, pinned map[string]string, workspaceDir string, store *Store, skipCache bool) error {
	slog.Info("job.run", "job", job.Name, "steps", len(job.Plan), "workspace_dir", workspaceDir)

	skippable := map[string]bool{}

	if !skipCache {
		chains, err := PlanChains(ctx, cfg, job.Name, job.Plan, pinned)
		if err != nil {
			return fmt.Errorf("job %q: planning: %w", job.Name, err)
		}

		skippable, err = buildSkippableIndex(ctx, store, job.Name, chains)
		if err != nil {
			return fmt.Errorf("job %q: %w", job.Name, err)
		}
	}

	err := runSteps(ctx, cfg, job.Name, job.Plan, pinned, workspaceDir, store, skippable, "", false)
	if err != nil {
		return err
	}

	slog.Info("job.done", "job", job.Name)

	return nil
}

// buildSkippableIndex returns, for every node hash reachable across chains,
// whether every leaf Chain passing through it is already covered by a
// prior succeeded job_runs row. Any Unskippable chain (contains a put or
// agent step) is forced non-skippable everywhere along it — those steps
// (and everything feeding them) must always run. A node hash shared by
// multiple chains is skippable only if ALL chains through it are skippable
// (AND-rollup), which correctly forces get/task ancestors of an
// unskippable branch to execute even if a sibling branch is independently
// skippable.
func buildSkippableIndex(ctx context.Context, store *Store, jobName string, chains []Chain) (map[string]bool, error) {
	chainSkippable := make([]bool, len(chains))

	for i, chain := range chains {
		if chain.Unskippable {
			continue
		}

		ok, err := store.HasSucceeded(ctx, jobName, chain.RootHash)
		if err != nil {
			return nil, err
		}

		chainSkippable[i] = ok
	}

	nodeChains := map[string][]int{}

	for i, chain := range chains {
		for _, node := range chain.Nodes {
			nodeChains[node.Hash] = append(nodeChains[node.Hash], i)
		}
	}

	skippable := make(map[string]bool, len(nodeChains))

	for hash, idxs := range nodeChains {
		all := true

		for _, idx := range idxs {
			if !chainSkippable[idx] {
				all = false

				break
			}
		}

		skippable[hash] = all
	}

	return skippable, nil
}

// runSteps executes steps in order. A `get` step fans out: for each version
// it selects (a single version normally, or every version returned by
// check when version:every is set), that version triggers its own build
// of the remainder of the plan — see runTriggeredBuild. It always
// terminates this loop, since it delegates the rest of the plan to its
// triggered build(s). A `task`/`put`/`agent` step is handled by
// runNonGetStep; `put`/`agent` steps are never looked up in skippable and
// always execute.
func runSteps(ctx context.Context, cfg *Config, jobName string, steps []Step, pinned map[string]string, workspaceDir string, store *Store, skippable map[string]bool, parentHash string, chainUnskippable bool) error {
	for i, step := range steps {
		if step.Get != "" {
			return runGetStep(ctx, cfg, jobName, i, step, steps[i+1:], pinned, store, skippable, parentHash, chainUnskippable)
		}

		newParentHash, skipped, err := runNonGetStep(ctx, cfg, jobName, i, step, workspaceDir, store, skippable, parentHash)
		if err != nil {
			return err
		}

		if skipped {
			return nil
		}

		parentHash = newParentHash
		if step.Put != "" || step.Agent != "" {
			chainUnskippable = true
		}
	}

	return recordChainSucceeded(ctx, store, jobName, parentHash, chainUnskippable)
}

// runNonGetStep dispatches a task/put/agent step — the three kinds that,
// unlike get, run in place and return a single new parentHash rather than
// fanning out or delegating the remainder of the plan. skipped is only ever
// true for a skipped task step; put/agent steps are never skippable.
func runNonGetStep(ctx context.Context, cfg *Config, jobName string, i int, step Step, workspaceDir string, store *Store, skippable map[string]bool, parentHash string) (string, bool, error) {
	switch {
	case step.Task != "":
		return runTaskStep(ctx, jobName, i, step, workspaceDir, store, skippable, parentHash)
	case step.Put != "":
		hash, err := runPutStep(ctx, cfg, jobName, i, step, workspaceDir, store, parentHash)

		return hash, false, err
	case step.Agent != "":
		hash, err := runAgentStep(ctx, cfg, jobName, i, step, workspaceDir, store, parentHash)

		return hash, false, err
	default:
		return "", false, fmt.Errorf("step %d: unrecognized step (must be get, task, put, or agent)", i)
	}
}

// recordChainSucceeded records the leaf of a fully-executed chain as
// succeeded, unless it contains a put or agent step (those chains are
// never skippable, so recording job_runs for them would be unused).
func recordChainSucceeded(ctx context.Context, store *Store, jobName, rootHash string, chainUnskippable bool) error {
	if chainUnskippable {
		return nil
	}

	err := store.RecordJobRun(ctx, jobName, rootHash, "succeeded", nil)
	if err != nil {
		return fmt.Errorf("job %q: %w", jobName, err)
	}

	return nil
}

// runGetStep resolves and (unless skippable) fetches step's resource
// version(s), then runs the remainder of the plan for each — see
// runTriggeredBuild. It always terminates the calling runSteps loop, since
// a get step delegates the rest of the plan to its triggered build(s).
func runGetStep(ctx context.Context, cfg *Config, jobName string, i int, step Step, remainder []Step, pinned map[string]string, store *Store, skippable map[string]bool, parentHash string, chainUnskippable bool) error {
	resource, resourceType, versions, err := resolveGetVersions(ctx, cfg, step, pinned)
	if err != nil {
		return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	slog.Debug("job.step", "job", jobName, "index", i, "kind", "get", "resource", step.Get, "versions", len(versions))

	for _, version := range versions {
		content := getNodeContent(*resourceType, resource.Source, version)

		hash, err := hashNode(NodeKindGet, content, parentHash)
		if err != nil {
			return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}

		if skippable[hash] {
			fmt.Printf("skip: %s (version: %v)\n", resource.Name, version)
			slog.Info("job.skip", "job", jobName, "index", i, "kind", "get", "resource", resource.Name, "hash", hash)

			continue
		}

		node := Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindGet, StepIndex: i, Resource: resource.Name, Content: content}

		err = runTriggeredBuild(ctx, cfg, jobName, *resource, *resourceType, version, remainder, pinned, store, skippable, node, chainUnskippable)
		if err != nil {
			return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}
	}

	return nil
}

// runTaskStep hashes step against parentHash and, unless that hash is
// skippable, runs it. It returns the hash to use as parentHash for the
// next step (unchanged, along with skipped=true, when skipped).
func runTaskStep(ctx context.Context, jobName string, i int, step Step, workspaceDir string, store *Store, skippable map[string]bool, parentHash string) (string, bool, error) {
	content := taskNodeContent(step.Run)

	hash, err := hashNode(NodeKindTask, content, parentHash)
	if err != nil {
		return "", false, fmt.Errorf("step %d (task %q): %w", i, step.Task, err)
	}

	if skippable[hash] {
		fmt.Printf("skip: %s\n", step.Task)
		slog.Info("job.skip", "job", jobName, "index", i, "kind", "task", "task", step.Task, "hash", hash)

		return parentHash, true, nil
	}

	slog.Debug("job.step", "job", jobName, "index", i, "kind", "task", "task", step.Task, "run", step.Run)

	fmt.Printf("task: %s\n", step.Task)

	node := Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindTask, StepIndex: i, Resource: step.Task, Content: content}

	err = RunShell(ctx, step.Run, workspaceDir)
	if err != nil {
		_ = store.RecordNode(ctx, node, jobName, "failed", nil, err)
		_ = store.RecordJobRun(ctx, jobName, hash, "failed", err)

		return "", false, fmt.Errorf("step %d (task %q): %w", i, step.Task, err)
	}

	err = store.RecordNode(ctx, node, jobName, "succeeded", nil, nil)
	if err != nil {
		return "", false, fmt.Errorf("step %d (task %q): %w", i, step.Task, err)
	}

	return hash, false, nil
}

// runPutStep hashes and always runs step (put steps are never skipped),
// returning the hash to use as parentHash for the next step.
func runPutStep(ctx context.Context, cfg *Config, jobName string, i int, step Step, workspaceDir string, store *Store, parentHash string) (string, error) {
	resource, err := cfg.FindResource(step.Put)
	if err != nil {
		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	resourceType, err := cfg.FindResourceType(resource.Type)
	if err != nil {
		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	content := putNodeContent(*resourceType, resource.Source, step.Params)

	hash, err := hashNode(NodeKindPut, content, parentHash)
	if err != nil {
		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	slog.Debug("job.step", "job", jobName, "index", i, "kind", "put", "resource", step.Put)

	fmt.Printf("put: %s\n", step.Put)

	node := Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindPut, StepIndex: i, Resource: resource.Name, Content: content}

	result, err := RunOut(ctx, *resourceType, resource.Source, step.Params, workspaceDir)
	if err != nil {
		_ = store.RecordNode(ctx, node, jobName, "failed", nil, err)
		_ = store.RecordJobRun(ctx, jobName, hash, "failed", err)

		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	err = store.RecordNode(ctx, node, jobName, "succeeded", result, nil)
	if err != nil {
		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	return hash, nil
}

// resolveGetVersions determines which version(s) of a get step's resource
// to fetch. An explicit CLI pin always wins (a single version); otherwise
// the step's own version: field decides between latest (default, a single
// version), every (all versions returned by check), or a YAML-pinned
// version.
func resolveGetVersions(ctx context.Context, cfg *Config, step Step, cliPinned map[string]string) (*Resource, *ResourceType, []map[string]any, error) {
	resource, err := cfg.FindResource(step.Get)
	if err != nil {
		return nil, nil, nil, err
	}

	resourceType, err := cfg.FindResourceType(resource.Type)
	if err != nil {
		return nil, nil, nil, err
	}

	versions, err := CheckVersions(ctx, *resourceType, resource.Source)
	if err != nil {
		return nil, nil, nil, err
	}

	// A CLI --version pin always wins; otherwise the step's own version:
	// field decides. version:every is the only path that fans out to more
	// than one version — every other path narrows to a single pin (an empty
	// pin meaning "latest").
	pin := cliPinned
	if len(pin) == 0 {
		mode, stepPinned := VersionMode(step)
		slog.Debug("resource.version_mode", "resource", resource.Name, "mode", mode)

		if mode == "every" {
			return resource, resourceType, versions, nil
		}

		pin = stepPinned
	}

	version, err := SelectVersion(versions, pin)
	if err != nil {
		return nil, nil, nil, err
	}

	return resource, resourceType, []map[string]any{version}, nil
}

// runTriggeredBuild runs the build that a single resource version triggers:
// per Concourse's model, the version triggering a get is what starts a
// build, and every build gets its own isolated working directory. So this
// creates a fresh workspace for just this version, fetches the version
// into it, runs the remainder of the plan inside it, and tears the
// workspace down afterward — never sharing it with any other triggered
// build, including sibling versions fanned out by version:every.
func runTriggeredBuild(ctx context.Context, cfg *Config, jobName string, resource Resource, resourceType ResourceType, version map[string]any, remainder []Step, pinned map[string]string, store *Store, skippable map[string]bool, node Node, chainUnskippable bool) error {
	buildWorkspace, err := os.MkdirTemp("", "steps-*")
	if err != nil {
		return fmt.Errorf("could not create workspace for %q: %w", resource.Name, err)
	}

	slog.Debug("workspace.create", "dir", buildWorkspace, "resource", resource.Name, "version", version)

	defer func() {
		removeErr := os.RemoveAll(buildWorkspace)
		if removeErr != nil {
			slog.Error("workspace.remove", "dir", buildWorkspace, "error", removeErr)

			return
		}

		slog.Debug("workspace.remove", "dir", buildWorkspace)
	}()

	err = fetchGetStep(ctx, resource, resourceType, version, buildWorkspace)
	if err != nil {
		_ = store.RecordNode(ctx, node, jobName, "failed", nil, err)
		_ = store.RecordJobRun(ctx, jobName, node.Hash, "failed", err)

		return err
	}

	err = store.RecordNode(ctx, node, jobName, "succeeded", nil, nil)
	if err != nil {
		return err
	}

	return runSteps(ctx, cfg, jobName, remainder, pinned, buildWorkspace, store, skippable, node.Hash, chainUnskippable)
}

// fetchGetStep places one version of a resource into workspaceDir/<name>.
func fetchGetStep(ctx context.Context, resource Resource, resourceType ResourceType, version map[string]any, workspaceDir string) error {
	fmt.Printf("get: %s (version: %v)\n", resource.Name, version)

	destDir := filepath.Join(workspaceDir, resource.Name)

	err := os.MkdirAll(destDir, 0o750)
	if err != nil {
		return fmt.Errorf("could not create resource dir %q: %w", destDir, err)
	}

	return RunIn(ctx, resourceType, resource.Source, version, destDir)
}
