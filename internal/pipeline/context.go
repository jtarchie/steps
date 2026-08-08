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
	"crypto/sha256"
	"encoding/hex"
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
func branchContextScope(enclosing string, index int, name string) string {
	return fmt.Sprintf("%s#%d:%s", enclosing, index, name)
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
//
// prefix is resolved by the caller over the whole sibling set, not derived from
// branch here — two differently-named branches can reduce to one prefix, and
// only something holding all of them can see that (see branchPrefixes).
func mergeBranchContext(ctx context.Context, st *store.Store, into, scope, branch, prefix string) error {
	if st == nil || into == "" {
		return nil
	}

	entries, err := st.RunContext(ctx, scope)
	if err != nil {
		return fmt.Errorf("branch %q context: %w", branch, err)
	}

	for _, entry := range entries {
		key := mergedContextKey(branch, prefix, entry.Key)

		err = st.SetContext(ctx, into, key, entry.Value, entry.WrittenBy)
		if err != nil {
			return fmt.Errorf("branch %q context: %w", branch, err)
		}
	}

	if len(entries) > 0 {
		slog.Debug("branch.context.merged", "branch", branch, "keys", len(entries))
	}

	return nil
}

// mergedContextKey is the key one branch's fact lands under in the enclosing
// scope, repaired if the prefixing pushed it outside what a key may be.
//
// The merge synthesizes keys and writes them straight through SetContext, so it
// is the one writer that bypasses the tool boundary's validation. Two rules can
// break: the length ceiling, since a prefix is added to a key that may already
// be at the limit and prefixes COMPOUND with nesting
// (`branch0.reviewer.finding`); and the reserved namespace, if a branch is
// literally named `internal`.
//
// Repaired rather than dropped. The branch already did the work, and a fact
// discarded at the join is indistinguishable from one nobody recorded.
func mergedContextKey(branch, prefix, key string) string {
	merged := prefix + key

	if config.ValidateContextKey(merged) == nil {
		return merged
	}

	repaired := repairContextKey(prefix, key)

	slog.Warn("branch.context.key_repaired", "branch", branch, "key", merged, "stored_as", repaired)

	return repaired
}

// repairContextKey returns a valid key carrying as much of prefix+key as fits.
//
// Total by construction: a key that survives neither the cut nor the escape
// falls back to the digest alone, which is still unique and still valid.
// Without that floor this could hand SetContext a key the tool boundary would
// have refused, while logging that it had repaired one.
func repairContextKey(prefix, key string) string {
	digest := contextKeyDigest(prefix + key)

	repaired := prefix + key
	if len(repaired) > config.MaxContextKeyLen {
		repaired = truncateContextKey(prefix, key, digest)
	}

	// Length-preserving on purpose ("internal." and "internal_" are both nine
	// characters), so escaping cannot push a just-cut key back over the ceiling.
	if strings.HasPrefix(repaired, config.ReservedContextPrefix) {
		repaired = "internal_" + repaired[len(config.ReservedContextPrefix):]
	}

	if config.ValidateContextKey(repaired) != nil {
		return "merged." + digest
	}

	return repaired
}

// contextKeyHashLen is how much of a digest a repaired key carries.
const contextKeyHashLen = 8

// contextKeyDigest is the marker a repaired key ends with: short, hex, and
// derived from the WHOLE untruncated key.
//
// The digest is what keeps a repair from becoming a collision. Cutting alone
// would collapse two long keys that share a prefix onto one row — the lost
// update the branch scopes exist to prevent, reintroduced by the repair.
func contextKeyDigest(key string) string {
	sum := sha256.Sum256([]byte(key))

	return hex.EncodeToString(sum[:])[:contextKeyHashLen]
}

// truncateContextKey cuts an over-long merged key down to the ceiling, keeping
// the KEY whole and spending what is left on the prefix.
//
// That split is the point. The key is the fact's name — what a later step reads
// it back by and what a synthesizer sees in a recap — while the prefix is
// provenance, and provenance is what compounds: one matrix cell named from a
// model-authored label can fill the budget on its own. Cutting the tail instead
// would leave `<118 characters of branch name>.<digest>` and destroy the name of
// every fact that branch recorded.
//
// The prefix keeps its TAIL, since the nearest enclosing block is its most
// informative segment. Keys are ASCII by charset, so no cut lands mid-rune.
func truncateContextKey(prefix, key, digest string) string {
	budget := config.MaxContextKeyLen - len(digest) - 1

	// A key that overruns on its own leaves nothing to spend on provenance.
	if len(key) >= budget {
		return key[:budget] + "." + digest
	}

	return prefix[len(prefix)-(budget-len(key)):] + key + "." + digest
}

// branchPrefixes resolves the key prefix each branch's facts are qualified by,
// over the whole sibling set at once.
//
// Together, not one at a time, because sanitation is lossy: contextKeySegment
// maps every character outside the key charset to `_`, so branches named
// `lint.go` and `lint_go` both reduce to `lint_go` and their merged facts
// overwrite each other at the join — the same lost update the branch scopes
// prevent, reintroduced by the naming rather than by the scope.
//
// A collision disambiguates by branch INDEX rather than by making the
// sanitation cleverer: the index is already unique within the set, and any
// cleverer mapping is one more rule a reader has to know to predict a key.
func branchPrefixes(results []branchResult) []string {
	segments := make([]string, len(results))
	counts := map[string]int{}

	for i, result := range results {
		segments[i] = contextKeySegment(branchPrefixName(result))
		counts[segments[i]]++
	}

	var (
		taken    = map[string]bool{}
		prefixes = make([]string, len(results))
	)

	for i, segment := range segments {
		// EVERY member of a colliding group is suffixed, not just the later
		// ones: qualifying one branch and not its twin would make which name
		// got the plain form depend on declaration order.
		if counts[segment] > 1 {
			segment = fmt.Sprintf("%s-%d", segment, results[i].index)
		}

		// A third sibling may be literally named like a disambiguated one, so
		// keep widening until nothing else has claimed it.
		for taken[segment] {
			segment += "_"
		}

		taken[segment] = true
		prefixes[i] = segment + "."
	}

	return prefixes
}

// contextKeySegment turns a branch name into one segment of a key prefix. A
// step name may hold characters a context key may not (a matrix cell's
// " [shard=a]", say), so it is reduced to the key charset rather than producing
// a key that renders one way and matches another.
func contextKeySegment(branch string) string {
	var b strings.Builder

	for _, r := range branch {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}

	return b.String()
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
