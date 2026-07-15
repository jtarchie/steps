// Package pipeline orchestrates a job's plan: resolving/fetching get steps,
// running task/put/agent steps in order, and recording each step's outcome
// so later runs can skip unchanged work (see internal/merkle).
package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// nodeRecord converts a plan merkle.Node into the shape store.RecordNode
// persists, keeping the store package free of a dependency on merkle's Node
// type.
func nodeRecord(n merkle.Node) store.NodeRecord {
	return store.NodeRecord{
		Hash:       n.Hash,
		ParentHash: n.ParentHash,
		Kind:       string(n.Kind),
		StepIndex:  n.StepIndex,
		Resource:   n.Resource,
		Content:    n.Content,
	}
}

// RunJob executes job's plan steps in order. pinned applies to any `get`
// step's version selection. provider materializes every build/step
// workspace the run needs — see workspace.Provider; when cfg.Workspace is
// nil, provider is the shared, single-mutable-directory implementation.
//
// Before executing anything, it statically validates every task/agent/put
// step's declared inputs (see workspace.ValidateArtifactFlow — always runs, even
// under --force) and plans every chain the job's steps could resolve to
// (resolving get versions but running nothing), checking the store for chains
// that already succeeded with identical content so that already-run
// get/task work can be skipped entirely. put steps are never skipped — see
// runSteps. skipCache (--force) bypasses only the chain-skip planning and
// re-runs everything, though results are still recorded as usual.
func RunJob(ctx context.Context, cfg *config.Config, job *config.Job, pinned map[string]string, provider workspace.Provider, st *store.Store, skipCache bool) error {
	slog.Info("job.run", "job", job.Name, "steps", len(job.Plan))

	err := workspace.ValidateArtifactFlow(cfg, job)
	if err != nil {
		return fmt.Errorf("job %q: %w", job.Name, err)
	}

	if cfg.UsesImages() {
		err = shell.ValidateDocker(ctx)
		if err != nil {
			return fmt.Errorf("job %q: image: configured but docker is unavailable: %w", job.Name, err)
		}
	}

	bw, err := provider.NewBuild(ctx, job.Name)
	if err != nil {
		return fmt.Errorf("job %q: %w", job.Name, err)
	}
	defer workspace.CloseBuild(bw, job.Name)

	skippable := map[string]bool{}

	if !skipCache {
		chains, err := merkle.PlanChains(ctx, cfg, job.Name, job.Plan, pinned)
		if err != nil {
			return fmt.Errorf("job %q: planning: %w", job.Name, err)
		}

		skippable, err = buildSkippableIndex(ctx, st, job.Name, chains)
		if err != nil {
			return fmt.Errorf("job %q: %w", job.Name, err)
		}
	}

	err = runSteps(ctx, cfg, job.Name, job.Plan, pinned, provider, bw, st, skippable, "", false)
	if err != nil {
		return err
	}

	slog.Info("job.done", "job", job.Name)

	return nil
}

// buildSkippableIndex returns, for every node hash reachable across chains,
// whether every leaf merkle.Chain passing through it is already covered by a
// prior succeeded job_runs row. Any Unskippable chain (contains a put or
// agent step) is forced non-skippable everywhere along it — those steps
// (and everything feeding them) must always run. A node hash shared by
// multiple chains is skippable only if ALL chains through it are skippable
// (AND-rollup), which correctly forces get/task ancestors of an
// unskippable branch to execute even if a sibling branch is independently
// skippable.
func buildSkippableIndex(ctx context.Context, st *store.Store, jobName string, chains []merkle.Chain) (map[string]bool, error) {
	chainSkippable := make([]bool, len(chains))

	for i, chain := range chains {
		if chain.Unskippable {
			continue
		}

		ok, err := st.HasSucceeded(ctx, jobName, chain.RootHash)
		if err != nil {
			return nil, fmt.Errorf("job %q: %w", jobName, err)
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
func runSteps(ctx context.Context, cfg *config.Config, jobName string, steps []config.Step, pinned map[string]string, provider workspace.Provider, bw workspace.BuildWorkspace, st *store.Store, skippable map[string]bool, parentHash string, chainUnskippable bool) error {
	for i, step := range steps {
		if step.Get != "" {
			return runGetStep(ctx, cfg, jobName, i, step, steps[i+1:], pinned, provider, st, skippable, parentHash, chainUnskippable)
		}

		newParentHash, skipped, err := runNonGetStep(ctx, cfg, jobName, i, step, bw, st, skippable, parentHash)
		if err != nil {
			return err
		}

		if skipped {
			return nil
		}

		parentHash = newParentHash
		if step.Put != "" || step.Agent != "" || step.Fix != nil {
			chainUnskippable = true
		}
	}

	return recordChainSucceeded(ctx, st, jobName, parentHash, chainUnskippable)
}

// runNonGetStep dispatches a task/put/agent step — the three kinds that,
// unlike get, run in place and return a single new parentHash rather than
// fanning out or delegating the remainder of the plan. skipped is only ever
// true for a skipped task step; put/agent steps are never skippable.
func runNonGetStep(ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, bw workspace.BuildWorkspace, st *store.Store, skippable map[string]bool, parentHash string) (string, bool, error) {
	switch {
	case step.Task != "":
		return runTaskStep(ctx, cfg, jobName, i, step, bw, st, skippable, parentHash)
	case step.Put != "":
		hash, err := runPutStep(ctx, cfg, jobName, i, step, bw, st, parentHash)

		return hash, false, err
	case step.Agent != "":
		hash, err := agent.RunStep(ctx, cfg, jobName, i, step, bw, st, parentHash)
		if err != nil {
			return "", false, fmt.Errorf("agent step: %w", err)
		}

		return hash, false, nil
	default:
		return "", false, fmt.Errorf("step %d: unrecognized step (must be get, task, put, or agent)", i)
	}
}

// recordChainSucceeded records the leaf of a fully-executed chain as
// succeeded, unless it contains a put or agent step (those chains are
// never skippable, so recording job_runs for them would be unused).
func recordChainSucceeded(ctx context.Context, st *store.Store, jobName, rootHash string, chainUnskippable bool) error {
	if chainUnskippable {
		return nil
	}

	err := st.RecordJobRun(ctx, jobName, rootHash, "succeeded", nil)
	if err != nil {
		return fmt.Errorf("job %q: %w", jobName, err)
	}

	return nil
}

// runGetStep resolves and (unless skippable) fetches step's resource
// version(s), then runs the remainder of the plan for each — see
// runTriggeredBuild. It always terminates the calling runSteps loop, since
// a get step delegates the rest of the plan to its triggered build(s).
func runGetStep(ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, remainder []config.Step, pinned map[string]string, provider workspace.Provider, st *store.Store, skippable map[string]bool, parentHash string, chainUnskippable bool) error {
	resource, resourceType, versions, err := rsrc.ResolveVersions(ctx, cfg, step, pinned)
	if err != nil {
		return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	slog.Debug("job.step", "job", jobName, "index", i, "kind", "get", "resource", step.Get, "versions", len(versions))

	for _, version := range versions {
		content := merkle.GetNodeContent(*resourceType, resource.Source, version)

		hash, err := merkle.HashNode(merkle.NodeKindGet, content, parentHash)
		if err != nil {
			return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}

		if skippable[hash] {
			fmt.Printf("skip: %s (version: %v)\n", resource.Name, version)
			slog.Info("job.skip", "job", jobName, "index", i, "kind", "get", "resource", resource.Name, "hash", hash)

			continue
		}

		node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindGet, StepIndex: i, Resource: resource.Name, Content: content}

		err = runTriggeredBuild(ctx, cfg, jobName, *resource, *resourceType, version, remainder, pinned, provider, st, skippable, node, chainUnskippable)
		if err != nil {
			return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}
	}

	return nil
}

// runTaskStep hashes step against parentHash and, unless that hash is
// skippable, runs it. It returns the hash to use as parentHash for the
// next step (unchanged, along with skipped=true, when skipped).
func runTaskStep(ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, bw workspace.BuildWorkspace, st *store.Store, skippable map[string]bool, parentHash string) (string, bool, error) {
	rt, err := cfg.ResolveTask(step)
	if err != nil {
		return "", false, fmt.Errorf("step %d: %w", i, err)
	}

	content := merkle.TaskNodeContent(rt, cfg.Workspace)

	hash, err := merkle.HashNode(merkle.NodeKindTask, content, parentHash)
	if err != nil {
		return "", false, fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
	}

	if skippable[hash] {
		fmt.Printf("skip: %s\n", rt.Name)
		slog.Info("job.skip", "job", jobName, "index", i, "kind", "task", "task", rt.Name, "hash", hash)

		return parentHash, true, nil
	}

	slog.Debug("job.step", "job", jobName, "index", i, "kind", "task", "task", rt.Name, "run", rt.Run)

	fmt.Printf("task: %s\n", rt.Name)

	node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindTask, StepIndex: i, Resource: rt.Name, Content: content}

	space, err := bw.TaskSpace(ctx, rt.Name, rt.Inputs, rt.Outputs)
	if err != nil {
		wrapped := fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
		_ = st.RecordNode(ctx, nodeRecord(node), jobName, "failed", nil, wrapped)
		_ = st.RecordJobRun(ctx, jobName, hash, "failed", wrapped)

		return "", false, wrapped
	}
	defer workspace.CloseSpace(space, rt.Name)

	err = runTaskCommand(ctx, cfg, rt, space.Dir())
	if err != nil {
		_ = st.RecordNode(ctx, nodeRecord(node), jobName, "failed", nil, err)
		_ = st.RecordJobRun(ctx, jobName, hash, "failed", err)

		return "", false, fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
	}

	err = space.Capture(ctx)
	if err != nil {
		wrapped := fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
		_ = st.RecordNode(ctx, nodeRecord(node), jobName, "failed", nil, wrapped)
		_ = st.RecordJobRun(ctx, jobName, hash, "failed", wrapped)

		return "", false, wrapped
	}

	err = st.RecordNode(ctx, nodeRecord(node), jobName, "succeeded", nil, nil)
	if err != nil {
		return "", false, fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
	}

	return hash, false, nil
}

// runTaskCommand runs a task's run: command. Without a fix:, it streams
// output live and any nonzero exit is a hard failure (unchanged behavior).
// With a fix:, it captures output instead, and on a nonzero exit invokes the
// fix agent — seeded with that output and given the task itself as a rerun
// tool — then re-runs the command once; that re-run's exit code is the
// verdict. A green run never constructs the agent.
func runTaskCommand(ctx context.Context, cfg *config.Config, rt config.ResolvedTask, workspaceDir string) error {
	runner := shell.NewRunner(rt.Image)

	if rt.Fix == nil {
		err := runner.Run(ctx, rt.Run, workspaceDir)
		if err != nil {
			return fmt.Errorf("task %q: %w", rt.Name, err)
		}

		return nil
	}

	stdout, stderr, exitCode, err := runner.RunCaptureFull(ctx, rt.Run, workspaceDir)
	if err != nil {
		return fmt.Errorf("task %q: %w", rt.Name, err)
	}

	printTaskOutput(stdout, stderr)

	if exitCode == 0 {
		return nil
	}

	fmt.Printf("task %q failed (exit %d); invoking fix agent %q\n", rt.Name, exitCode, rt.Fix.Agent)

	err = agent.RunFix(ctx, cfg, rt, taskFailureOutput(stdout, stderr, exitCode), workspaceDir)
	if err != nil {
		return fmt.Errorf("fix agent %q: %w", rt.Fix.Agent, err)
	}

	// Verdict: re-run the command (its run:, not its fix:) and gate on it.
	stdout, stderr, exitCode, err = runner.RunCaptureFull(ctx, rt.Run, workspaceDir)
	if err != nil {
		return fmt.Errorf("task %q: %w", rt.Name, err)
	}

	printTaskOutput(stdout, stderr)

	if exitCode != 0 {
		return fmt.Errorf("still failing after fix agent %q (exit %d)", rt.Fix.Agent, exitCode)
	}

	return nil
}

// printTaskOutput echoes a captured task run's streams to the terminal, so a
// fix-enabled task's output is still visible (RunShellCaptureFull buffers
// rather than streaming live the way RunShell does).
func printTaskOutput(stdout, stderr string) {
	if stdout != "" {
		fmt.Print(stdout)
	}

	if stderr != "" {
		fmt.Fprint(os.Stderr, stderr)
	}
}

// taskFailureOutput formats a failed run's exit code and streams into the
// text seeded into the fix agent's prompt.
func taskFailureOutput(stdout, stderr string, exitCode int) string {
	var b strings.Builder

	fmt.Fprintf(&b, "exit code: %d\n", exitCode)

	if stdout != "" {
		b.WriteString("stdout:\n")
		b.WriteString(stdout)
		b.WriteString("\n")
	}

	if stderr != "" {
		b.WriteString("stderr:\n")
		b.WriteString(stderr)
		b.WriteString("\n")
	}

	return b.String()
}

// runPutStep hashes and always runs step (put steps are never skipped),
// returning the hash to use as parentHash for the next step.
func runPutStep(ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, bw workspace.BuildWorkspace, st *store.Store, parentHash string) (string, error) {
	resource, err := cfg.FindResource(step.Put)
	if err != nil {
		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	resourceType, err := cfg.FindResourceType(resource.Type)
	if err != nil {
		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	content := merkle.PutNodeContent(*resourceType, resource.Source, step.Params, step.Inputs, cfg.Workspace)

	hash, err := merkle.HashNode(merkle.NodeKindPut, content, parentHash)
	if err != nil {
		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	slog.Debug("job.step", "job", jobName, "index", i, "kind", "put", "resource", step.Put)

	fmt.Printf("put: %s\n", step.Put)

	node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindPut, StepIndex: i, Resource: resource.Name, Content: content}

	space, err := bw.PutSpace(ctx, step.Put, step.Inputs)
	if err != nil {
		wrapped := fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
		_ = st.RecordNode(ctx, nodeRecord(node), jobName, "failed", nil, wrapped)
		_ = st.RecordJobRun(ctx, jobName, hash, "failed", wrapped)

		return "", wrapped
	}
	defer workspace.CloseSpace(space, step.Put)

	result, err := rsrc.RunOut(ctx, *resourceType, resource.Source, step.Params, space.Dir())
	if err != nil {
		_ = st.RecordNode(ctx, nodeRecord(node), jobName, "failed", nil, err)
		_ = st.RecordJobRun(ctx, jobName, hash, "failed", err)

		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	err = st.RecordNode(ctx, nodeRecord(node), jobName, "succeeded", result, nil)
	if err != nil {
		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	return hash, nil
}

// runTriggeredBuild runs the build that a single resource version triggers:
// per Concourse's model, the version triggering a get is what starts a
// build, and every build gets its own isolated working directory. So this
// creates a fresh workspace for just this version, fetches the version
// into it, runs the remainder of the plan inside it, and tears the
// workspace down afterward — never sharing it with any other triggered
// build, including sibling versions fanned out by version:every.
func runTriggeredBuild(ctx context.Context, cfg *config.Config, jobName string, resource config.Resource, resourceType config.ResourceType, version map[string]any, remainder []config.Step, pinned map[string]string, provider workspace.Provider, st *store.Store, skippable map[string]bool, node merkle.Node, chainUnskippable bool) error {
	bw, err := provider.NewBuild(ctx, resource.Name)
	if err != nil {
		return fmt.Errorf("could not create workspace for %q: %w", resource.Name, err)
	}

	defer workspace.CloseBuild(bw, resource.Name)

	err = fetchGetStep(ctx, resource, resourceType, version, bw)
	if err != nil {
		_ = st.RecordNode(ctx, nodeRecord(node), jobName, "failed", nil, err)
		_ = st.RecordJobRun(ctx, jobName, node.Hash, "failed", err)

		return err
	}

	err = st.RecordNode(ctx, nodeRecord(node), jobName, "succeeded", nil, nil)
	if err != nil {
		return fmt.Errorf("could not record node %q: %w", node.Hash, err)
	}

	return runSteps(ctx, cfg, jobName, remainder, pinned, provider, bw, st, skippable, node.Hash, chainUnskippable)
}

// fetchGetStep places one version of a resource into bw's resource
// directory for resource.Name.
func fetchGetStep(ctx context.Context, resource config.Resource, resourceType config.ResourceType, version map[string]any, bw workspace.BuildWorkspace) error {
	fmt.Printf("get: %s (version: %v)\n", resource.Name, version)

	destDir, err := bw.ResourceDir(ctx, resource.Name)
	if err != nil {
		return fmt.Errorf("could not create resource dir for %q: %w", resource.Name, err)
	}

	err = rsrc.RunIn(ctx, resourceType, resource.Source, version, destDir)
	if err != nil {
		return fmt.Errorf("could not fetch resource %q: %w", resource.Name, err)
	}

	return nil
}
