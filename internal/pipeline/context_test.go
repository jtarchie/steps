package pipeline

// The join's own bookkeeping: the prefix a branch's facts are qualified by, and
// the key they end up under.
//
// Both are keys the ENGINE synthesizes rather than a model writing them, so
// neither passes the set_context tool boundary that validates every other key.
// That is what these cover (#40.1, #40.4).

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// openTestStore opens a throwaway store for one test.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.OpenStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	return st
}

// TestBranchPrefixesDisambiguateCollisions covers the sanitation collision:
// every character outside the key charset maps to `_`, so two differently
// named branches can reduce to the same prefix and overwrite each other's facts
// at the join — the lost update the branch scopes exist to prevent,
// reintroduced by the naming.
func TestBranchPrefixesDisambiguateCollisions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		results []branchResult
		want    []string
	}{
		{
			name:    "distinct names keep their plain prefix",
			results: []branchResult{{index: 0, name: "security"}, {index: 1, name: "performance"}},
			want:    []string{"security.", "performance."},
		},
		{
			name: "two names sanitizing to one are both qualified",
			// `lint.go` and `lint_go` both reduce to `lint_go`. BOTH are
			// suffixed, so which name got the plain form does not depend on
			// declaration order.
			results: []branchResult{{index: 0, name: "lint.go"}, {index: 1, name: "lint_go"}},
			want:    []string{"lint_go-0.", "lint_go-1."},
		},
		{
			name: "a literal collision with a disambiguated form still resolves",
			results: []branchResult{
				{index: 0, name: "lint.go"}, {index: 1, name: "lint_go"}, {index: 2, name: "lint_go-1"},
			},
			want: []string{"lint_go-0.", "lint_go-1.", "lint_go-1_."},
		},
		{
			name:    "an unnamed branch is a block, named by position",
			results: []branchResult{{index: 0, name: ""}, {index: 1, name: ""}},
			want:    []string{"branch0.", "branch1."},
		},
		{
			name:    "a matrix cell keeps its coordinates, sanitized",
			results: []branchResult{{index: 0, name: "check [shard=a]"}, {index: 1, name: "check [shard=b]"}},
			want:    []string{"check__shard_a_.", "check__shard_b_."},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := branchPrefixes(tc.results)
			if len(got) != len(tc.want) {
				t.Fatalf("branchPrefixes = %v, want %v", got, tc.want)
			}

			seen := map[string]bool{}

			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("prefix %d = %q, want %q", i, got[i], tc.want[i])
				}

				if seen[got[i]] {
					t.Errorf("prefix %d (%q) collides with an earlier one", i, got[i])
				}

				seen[got[i]] = true
			}
		})
	}
}

// TestMergedContextKeyStaysValid covers the other half: the merge writes
// straight through SetContext, so a key it synthesizes never meets the
// validation every key written by a step passes.
func TestMergedContextKeyStaysValid(t *testing.T) {
	t.Parallel()

	t.Run("an ordinary key is left exactly as it was", func(t *testing.T) {
		t.Parallel()

		if got := mergedContextKey("security", "security.", "finding"); got != "security.finding" {
			t.Errorf("mergedContextKey = %q, want security.finding", got)
		}
	})

	t.Run("an over-long key is cut to the ceiling and marked", func(t *testing.T) {
		t.Parallel()

		// A key at the limit under a long branch name: legal when written,
		// illegal once qualified. Prefixes also compound with nesting, so this
		// is not only reachable by a pathological name.
		key := strings.Repeat("k", config.MaxContextKeyLen)
		prefix := strings.Repeat("b", 40) + "."

		got := mergedContextKey("branch", prefix, key)

		err := config.ValidateContextKey(got)
		if err != nil {
			t.Fatalf("merged key %q does not validate: %v", got, err)
		}

		if !strings.HasPrefix(got, prefix) {
			t.Errorf("merged key %q lost the branch prefix", got)
		}
	})

	t.Run("two long keys sharing a prefix do not collapse onto one", func(t *testing.T) {
		t.Parallel()

		// The whole reason the cut carries a digest: truncating alone would
		// merge these two rows, which is exactly the lost update the branch
		// scopes exist to prevent.
		prefix := strings.Repeat("b", 40) + "."
		shared := strings.Repeat("k", config.MaxContextKeyLen-1)

		first := mergedContextKey("branch", prefix, shared+"1")
		second := mergedContextKey("branch", prefix, shared+"2")

		if first == second {
			t.Errorf("two distinct long keys both merged to %q", first)
		}
	})

	t.Run("a branch named internal cannot land in the reserved namespace", func(t *testing.T) {
		t.Parallel()

		got := mergedContextKey("internal", "internal.", "finding")

		err := config.ValidateContextKey(got)
		if err != nil {
			t.Fatalf("merged key %q does not validate: %v", got, err)
		}

		if strings.HasPrefix(got, config.ReservedContextPrefix) {
			t.Errorf("merged key %q is inside the reserved namespace", got)
		}
	})
}

// TestMergeBranchContextKeepsBothCollidingBranches is the end of the same
// story, through the store: two branches whose names sanitize alike each keep
// their fact.
func TestMergeBranchContextKeepsBothCollidingBranches(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := t.Context()

	results := []branchResult{{index: 0, name: "lint.go"}, {index: 1, name: "lint_go"}}

	// The witness for what this is testing: unqualified, these two names ARE
	// the same prefix, which is how the second branch used to overwrite the
	// first at the join.
	if contextKeySegment(results[0].name) != contextKeySegment(results[1].name) {
		t.Fatalf("the fixture no longer collides; pick two names that sanitize alike")
	}

	prefixes := branchPrefixes(results)

	for i, result := range results {
		scope := branchContextScope("run", result.index, result.name)

		err := st.SetContext(ctx, scope, "verdict", result.name+" says no", result.name)
		if err != nil {
			t.Fatalf("SetContext: %v", err)
		}

		err = mergeBranchContext(ctx, st, "run", scope, result.name, prefixes[i])
		if err != nil {
			t.Fatalf("mergeBranchContext: %v", err)
		}
	}

	entries, err := st.RunContext(ctx, "run")
	if err != nil {
		t.Fatalf("RunContext: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("run holds %d facts, want 2 (one per branch): %+v", len(entries), entries)
	}

	for _, entry := range entries {
		err = config.ValidateContextKey(entry.Key)
		if err != nil {
			t.Errorf("merged key %q does not validate: %v", entry.Key, err)
		}
	}
}
