package pipeline

// The task half of the run context store: a `context: write` task records
// facts by writing files into config.ContextDir of its working space, and this
// collects them when the command finishes.
//
// A shell command cannot call a tool, so the filesystem is the interface it
// already has. The file NAME is the key, verbatim (see config.ContextDir).
//
// The recorded entries are ALSO stashed on the step's node result, because a
// task is skippable: on a cache hit the command never runs, so a run that
// skipped and a run that executed would otherwise disagree about which facts
// exist. See replayTaskContext.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// nodeResultContextKey is where a task's recorded facts live inside the node
// result map, so a skip can find them again.
const nodeResultContextKey = "context"

// collectTaskContext reads the files a task wrote into config.ContextDir under
// dir and returns them as key/value pairs, ordered by key.
//
// A refused file (bad key, oversized value) is SKIPPED with a warning rather
// than failing the step: the command already ran and succeeded, and failing it
// afterwards for the shape of a file name would discard real work over
// bookkeeping. The warning is the record that it happened.
func collectTaskContext(dir string) (map[string]string, error) {
	contextDir := filepath.Join(dir, config.ContextDir)

	entries, err := os.ReadDir(contextDir)
	if os.IsNotExist(err) {
		// A task may declare context: write and, on some paths, record
		// nothing. That is not a failure — a run where nothing went wrong
		// records no failure_cause.
		return map[string]string{}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", contextDir, err)
	}

	collected := map[string]string{}

	for _, entry := range entries {
		if entry.IsDir() {
			// Keys are flat, so a nested directory has no key to be. Skipped
			// rather than walked: inventing a separator here would make the
			// key layout depend on a rule nobody wrote down.
			slog.Warn("task.context.skipped", "name", entry.Name(), "reason", "directory")

			continue
		}

		key := entry.Name()

		err = config.ValidateContextKey(key)
		if err != nil {
			slog.Warn("task.context.skipped", "name", key, "reason", err.Error())

			continue
		}

		// The key came from a directory listing and passed ValidateContextKey,
		// which admits no path separator and rejects "." and ".." outright —
		// so this cannot reach outside contextDir.
		value, err := os.ReadFile(filepath.Join(contextDir, key)) //nolint:gosec // key is a validated single path element, see above
		if err != nil {
			return nil, fmt.Errorf("could not read context file %q: %w", key, err)
		}

		if len(value) > config.MaxContextValueLen {
			slog.Warn("task.context.skipped", "name", key, "reason", "value above the size limit",
				"bytes", len(value), "limit", config.MaxContextValueLen)

			continue
		}

		collected[key] = string(value)
	}

	return collected, nil
}

// recordTaskContext writes a task's collected facts into the run context.
func recordTaskContext(ctx context.Context, st *store.Store, runID, taskName string, collected map[string]string) error {
	if len(collected) == 0 || st == nil || runID == "" {
		return nil
	}

	// Sorted so two runs of the same task write in the same order — the rows
	// are timestamped, and an arbitrary order would make the record read
	// differently every time for no reason.
	for _, key := range sortedKeys(collected) {
		err := st.SetContext(ctx, runID, key, collected[key], taskName)
		if err != nil {
			return fmt.Errorf("task %q: %w", taskName, err)
		}
	}

	slog.Debug("task.context.recorded", "task", taskName, "keys", len(collected))

	return nil
}

// taskNodeResult is what a task records on its own node so a later SKIP of the
// same step can replay it. nil when the task recorded nothing, so an ordinary
// task's node result stays absent rather than holding an empty object.
func taskNodeResult(collected map[string]string) map[string]any {
	if len(collected) == 0 {
		return nil
	}

	return map[string]any{nodeResultContextKey: collected}
}

// replayTaskContext re-applies what a SKIPPED task recorded when it last ran.
//
// This is the correctness half of caching for this feature. Without it, a
// second run of an unchanged pipeline would reach the agent steps with none of
// the facts the first run's tasks recorded — the same pipeline, the same
// inputs, a different conversation. Best-effort on read: a missing or
// unreadable node result means there is nothing to replay, which is also what
// a task that recorded nothing looks like.
func replayTaskContext(ctx context.Context, st *store.Store, runID, taskName, hash string) error {
	if st == nil || runID == "" {
		return nil
	}

	result, err := st.NodeResult(ctx, hash)
	if err != nil || result == nil {
		return err //nolint:wrapcheck // the store's error already names the node
	}

	recorded, ok := result[nodeResultContextKey].(map[string]any)
	if !ok {
		return nil
	}

	return recordTaskContext(ctx, st, runID, taskName, decodeRecordedContext(recorded))
}

// decodeRecordedContext narrows a node result's stored facts back to strings.
// A non-string value cannot have come from this code, so it is dropped rather
// than coerced into a value no step ever recorded.
func decodeRecordedContext(recorded map[string]any) map[string]string {
	decoded := make(map[string]string, len(recorded))

	for key, value := range recorded {
		if text, ok := value.(string); ok {
			decoded[key] = text
		}
	}

	return decoded
}

// branchContextScope is the run-context write scope for one branch of a
// concurrent block: rows nobody else in the run touches, so two branches
// recording the same key cannot resolve to whichever finished last.
func branchContextScope(runID string, index int, name string) string {
	return fmt.Sprintf("%s#%d:%s", runID, index, name)
}

// mergeBranchContext folds what one branch recorded back into the run, under
// keys naming the branch that recorded them.
//
// Called at the JOIN, after every branch has finished, on one goroutine and in
// declaration order — which is what makes it deterministic where writing
// straight into the run would not have been. The branch name becomes a key
// prefix rather than being dropped: which branch established a fact is part of
// the fact, and a synthesizer needs to know that the security branch rated
// something high while the performance branch rated it low.
//
// Best-effort on read: a branch that recorded nothing has no rows, which is
// the common case and not an error.
func mergeBranchContext(ctx context.Context, st *store.Store, runID, scope, branch string) error {
	if st == nil || runID == "" {
		return nil
	}

	entries, err := st.RunContext(ctx, scope)
	if err != nil {
		return fmt.Errorf("branch %q context: %w", branch, err)
	}

	prefix := contextKeyPrefix(branch)

	for _, entry := range entries {
		err = st.SetContext(ctx, runID, prefix+entry.Key, entry.Value, entry.WrittenBy)
		if err != nil {
			return fmt.Errorf("branch %q context: %w", branch, err)
		}
	}

	if len(entries) > 0 {
		slog.Debug("branch.context.merged", "branch", branch, "keys", len(entries))
	}

	return nil
}

// contextKeyPrefix turns a branch name into a key prefix. A step name may hold
// characters a context key may not (a matrix cell's " [shard=a]", say), so it
// is reduced to the key charset rather than producing a key that renders one
// way and matches another.
func contextKeyPrefix(branch string) string {
	var b strings.Builder

	for _, r := range branch {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}

	return b.String() + "."
}

// sortedKeys returns m's keys in sorted order.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}
