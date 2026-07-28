// editfile.go is edit_file's matching engine: given the model's old_string,
// find the span of file content it most likely meant, then replace it.
//
// The chain shape is ported from opencode's edit tool (itself distilled
// from cline's and gemini-cli's diff-edit eval harnesses), trimmed to the
// three strategies that recover the failures a local model actually makes,
// in order of decreasing exactness:
//
//  1. exact — byte-for-byte, the only mode edit_file originally had.
//  2. line-trimmed — every line matches modulo leading/trailing whitespace.
//     Recovers the classic local-model miss: right content, wrong
//     indentation on some lines (a model re-deriving a block instead of
//     copying it).
//  3. block-anchor — first and last lines anchor a block of at least three
//     lines, with the middle judged by per-line similarity. Recovers small
//     interior drift (a comment line misquoted, one line slightly off)
//     while the block's shape is unmistakable.
//
// Each strategy yields candidate spans of the ORIGINAL content (preserving
// the file's own whitespace, so a forgiving match never rewrites untouched
// lines to the model's spelling). The first candidate that is unique in the
// file wins; a candidate that matches several places falls through to the
// next, and only if no candidate anywhere is unique does the call fail
// ambiguous. The disproportionate-match guard refuses a span far larger
// than the old_string that produced it — forgiveness should rescue a
// near-miss, never invent a match the model didn't roughly specify.
//
// Deliberately not ported from opencode: the whitespace-normalized,
// indentation-flexible, escape-normalized, trimmed-boundary, and
// context-aware replacers (diminishing returns past the three above, and
// each adds a way to match something the model didn't mean), and CRLF
// normalization (this codebase's workspaces are LF; revisit if a Windows
// checkout ever matters).

package agent

import (
	"errors"
	"fmt"
	"strings"
)

// blockAnchorSimilarityThreshold is the minimum average per-line similarity
// a block-anchor candidate's middle lines must reach. 0.65 is opencode's
// value for both the single- and multi-candidate paths; it is strict enough
// that a randomly anchoring block can't pass, loose enough to forgive one
// misquoted line in a five-line block.
const blockAnchorSimilarityThreshold = 0.65

// maxLevenshteinProduct caps the a*b length product the similarity
// calculation will build a matrix for. Source lines are short; a
// pathological minified line would turn forgiveness into an allocation
// bomb, so past the cap two lines score 0 unless identical.
const maxLevenshteinProduct = 1 << 20

// errEditNotFound / errEditDisproportionate mark replaceEditSpan's failure
// modes; editAmbiguousError carries the occurrence count for the message
// execEditFile's recovery instructions have always given the model.
var (
	errEditNotFound         = errors.New("edit span not found")
	errEditDisproportionate = errors.New("matched span disproportionately large")
)

type editAmbiguousError struct{ count int }

func (e editAmbiguousError) Error() string {
	return fmt.Sprintf("edit span matches %d places", e.count)
}

// editOutcome is a successful match-and-replace: the new file content, the
// byte offset of the first replaced span in the ORIGINAL content (for the
// first_line result field), how many replacements were made, and which
// matching strategy succeeded (surfaced to the model as match_mode, so an
// inexact edit is visible rather than silently magical).
type editOutcome struct {
	updated      string
	matchIndex   int
	replacements int
	mode         string
}

// editReplacer finds candidate spans of content that find may have meant,
// most-exact interpretation first. A returned span is always a substring of
// content.
type editReplacer struct {
	mode string
	find func(content, find string) []string
}

// editReplacers is the strategy chain, in the order documented at the top
// of this file.
//
//nolint:gochecknoglobals // static, read-only strategy table
var editReplacers = []editReplacer{
	{"exact", exactSpans},
	{"line-trimmed", lineTrimmedSpans},
	{"block-anchor", blockAnchorSpans},
}

// replaceEditSpan applies the replacer chain: the first candidate that is
// unique in content wins (or, with replaceAll, the first candidate at all —
// every occurrence of it is replaced). The returned error is one of
// errEditNotFound, editAmbiguousError, or errEditDisproportionate.
func replaceEditSpan(content, oldString, newString string, replaceAll bool) (editOutcome, error) {
	notFound := true
	ambiguousCount := 0

	for _, r := range editReplacers {
		for _, span := range r.find(content, oldString) {
			index := strings.Index(content, span)
			if index == -1 {
				continue
			}

			notFound = false

			if isDisproportionateMatch(span, oldString) {
				return editOutcome{}, errEditDisproportionate
			}

			if replaceAll {
				return editOutcome{
					updated:      strings.ReplaceAll(content, span, newString),
					matchIndex:   index,
					replacements: strings.Count(content, span),
					mode:         r.mode,
				}, nil
			}

			if index != strings.LastIndex(content, span) {
				ambiguousCount = strings.Count(content, span)

				continue
			}

			return editOutcome{
				updated:      content[:index] + newString + content[index+len(span):],
				matchIndex:   index,
				replacements: 1,
				mode:         r.mode,
			}, nil
		}
	}

	if notFound {
		return editOutcome{}, errEditNotFound
	}

	return editOutcome{}, editAmbiguousError{count: ambiguousCount}
}

// exactSpans is strategy 1: the old_string itself, unchanged.
func exactSpans(_, find string) []string {
	return []string{find}
}

// lineTrimmedSpans is strategy 2: blocks of len(findLines) consecutive
// content lines where every line matches its counterpart with both sides
// trimmed. The yielded span is the original block, indentation intact.
func lineTrimmedSpans(content, find string) []string {
	originalLines := strings.Split(content, "\n")
	searchLines := dropTrailingEmptyLine(strings.Split(find, "\n"))

	var spans []string

	for i := 0; i+len(searchLines) <= len(originalLines); i++ {
		matches := true

		for j, searchLine := range searchLines {
			if strings.TrimSpace(originalLines[i+j]) != strings.TrimSpace(searchLine) {
				matches = false

				break
			}
		}

		if matches {
			spans = append(spans, strings.Join(originalLines[i:i+len(searchLines)], "\n"))
		}
	}

	return spans
}

// blockAnchorSpans is strategy 3: blocks whose first and last lines match
// the search block's anchors (trimmed), whose length is within ±25% of the
// search block's, and whose middle lines average at least
// blockAnchorSimilarityThreshold per-line similarity. With several anchor
// candidates, the most similar one wins.
func blockAnchorSpans(content, find string) []string {
	searchLines := dropTrailingEmptyLine(strings.Split(find, "\n"))
	if len(searchLines) < 3 {
		return nil // fewer than 3 lines have no middle to judge; anchors alone could match anywhere
	}

	originalLines := strings.Split(content, "\n")

	candidates := blockAnchorCandidates(originalLines, searchLines)
	if len(candidates) == 0 {
		return nil
	}

	span := func(c anchorCandidate) string {
		return strings.Join(originalLines[c.start:c.end+1], "\n")
	}

	if len(candidates) == 1 {
		if anchorSimilarity(originalLines, searchLines, candidates[0]) >= blockAnchorSimilarityThreshold {
			return []string{span(candidates[0])}
		}

		return nil
	}

	best := -1.0

	var bestCandidate anchorCandidate

	for _, c := range candidates {
		if sim := anchorSimilarity(originalLines, searchLines, c); sim > best {
			best = sim
			bestCandidate = c
		}
	}

	if best >= blockAnchorSimilarityThreshold {
		return []string{span(bestCandidate)}
	}

	return nil
}

// anchorCandidate is one block in the file bounded by the search block's
// first and last anchor lines, inclusive and zero-indexed into the file's
// lines.
type anchorCandidate struct{ start, end int }

// blockAnchorCandidates finds every block whose first line matches the
// search block's first anchor and whose closing anchor (the FIRST later
// line matching the last anchor — an inner lone "}" can therefore shadow
// the block's real end, a known limitation inherited from opencode's
// original) lands within ±25% of the search block's length.
func blockAnchorCandidates(originalLines, searchLines []string) []anchorCandidate {
	firstAnchor := strings.TrimSpace(searchLines[0])
	lastAnchor := strings.TrimSpace(searchLines[len(searchLines)-1])
	searchSize := len(searchLines)
	maxLineDelta := max(1, searchSize/4)

	var candidates []anchorCandidate

	for i, line := range originalLines {
		if strings.TrimSpace(line) != firstAnchor {
			continue
		}

		for j := i + 2; j < len(originalLines); j++ {
			if strings.TrimSpace(originalLines[j]) != lastAnchor {
				continue
			}

			if actual := j - i + 1; actual-searchSize <= maxLineDelta && searchSize-actual <= maxLineDelta {
				candidates = append(candidates, anchorCandidate{i, j})
			}

			break // only the first closing anchor counts for this opening one
		}
	}

	return candidates
}

// anchorSimilarity averages per-line similarity across the candidate's
// middle lines (the anchors themselves are the match's given, not part of
// the score). A block with no middle lines scores 1: the anchors are the
// whole judgment.
func anchorSimilarity(originalLines, searchLines []string, c anchorCandidate) float64 {
	actualSize := c.end - c.start + 1
	linesToCheck := min(len(searchLines)-2, actualSize-2)

	if linesToCheck <= 0 {
		return 1
	}

	total := 0.0

	for j := 1; j <= linesToCheck; j++ {
		total += lineSimilarity(
			strings.TrimSpace(originalLines[c.start+j]),
			strings.TrimSpace(searchLines[j]),
		) / float64(linesToCheck)
	}

	return total
}

// dropTrailingEmptyLine removes the final empty element strings.Split
// leaves behind when find ends in a newline, so "foo\n" searches for one
// line, not "foo" plus a phantom blank.
func dropTrailingEmptyLine(lines []string) []string {
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		return lines[:len(lines)-1]
	}

	return lines
}

// lineSimilarity scores two (already trimmed) lines from 0 to 1: 1 when
// identical, otherwise 1 minus their normalized Levenshtein distance, with
// the maxLevenshteinProduct guard against quadratic blowup on
// pathologically long lines.
func lineSimilarity(a, b string) float64 {
	if a == b {
		return 1
	}

	longest := max(len(a), len(b))
	if longest == 0 {
		return 1
	}

	if len(a)*len(b) > maxLevenshteinProduct {
		return 0
	}

	return 1 - float64(levenshtein(a, b))/float64(longest)
}

// levenshtein is the textbook dynamic-programming edit distance.
func levenshtein(a, b string) int {
	previous := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}

	for i := 1; i <= len(a); i++ {
		current := make([]int, len(b)+1)
		current[0] = i

		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}

			current[j] = min(min(current[j-1]+1, previous[j]+1), previous[j-1]+cost)
		}

		previous = current
	}

	return previous[len(b)]
}

// isDisproportionateMatch refuses a span far larger than the old_string
// that produced it — ported from opencode's guard of the same name. A
// forgiving match that balloons (a short old_string matching a huge block
// via anchors) is more likely the engine inventing a match than the model
// meaning it, so it fails with a re-read instruction instead.
func isDisproportionateMatch(span, find string) bool {
	oldLines := strings.Count(find, "\n") + 1
	spanLines := strings.Count(span, "\n") + 1

	if spanLines >= max(oldLines+3, oldLines*2) {
		return true
	}

	if oldLines == 1 {
		return false
	}

	spanTrimmed := len(strings.TrimSpace(span))
	findTrimmed := len(strings.TrimSpace(find))

	return spanTrimmed > max(findTrimmed+500, findTrimmed*4)
}
