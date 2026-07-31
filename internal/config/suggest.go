package config

// "did you mean" suggestions, shared by the unknown-key errors and the Find*
// lookups, so a misspelled name points at the right one instead of only
// reporting that it was not found.

import (
	"fmt"
	"strings"
)

// notFound is the shared shape of every "you named something that isn't
// there" error: the kind of thing looked up, what was written, the nearest
// match when there is one, and the full list to choose from. FindJob has
// reported the available names for a while; the other lookups now do too,
// since a misspelled name is the most common way to misconfigure a pipeline
// that is otherwise well-formed.
func notFound(kind, name string, available []string) error {
	return fmt.Errorf("no %s named %q%s (available: %v)", kind, name, suggestion(name, available), available)
}

// names collects the Name field of every entry, in declaration order, via get.
func names[T any](entries []T, get func(T) string) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, get(entry))
	}

	return out
}

// suggestion renders the parenthetical for an error message — ` (did you mean
// "prompt"?)` — or "" when nothing is close enough to be worth guessing.
func suggestion(got string, candidates []string) string {
	best := closest(got, candidates)
	if best == "" {
		return ""
	}

	return fmt.Sprintf(" (did you mean %q?)", best)
}

// closest returns the candidate nearest to got by edit distance, or "" when
// none is near enough. The cutoff scales with the length of what was written
// so short names don't match everything: a candidate has to be within roughly
// a third of the word's length to be called a typo of it.
func closest(got string, candidates []string) string {
	limit := len(got)/3 + 1

	best := ""
	bestDistance := 0

	for _, candidate := range candidates {
		distance := editDistance(strings.ToLower(got), strings.ToLower(candidate))
		if distance > limit {
			continue
		}

		if best == "" || distance < bestDistance {
			best, bestDistance = candidate, distance
		}
	}

	return best
}

// editDistance is the Levenshtein distance between a and b, over runes.
func editDistance(a, b string) int {
	first, second := []rune(a), []rune(b)

	previous := make([]int, len(second)+1)
	current := make([]int, len(second)+1)

	for j := range previous {
		previous[j] = j
	}

	for i := 1; i <= len(first); i++ {
		current[0] = i

		for j := 1; j <= len(second); j++ {
			cost := 1
			if first[i-1] == second[j-1] {
				cost = 0
			}

			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}

		previous, current = current, previous
	}

	return previous[len(second)]
}
