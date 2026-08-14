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
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jtarchie/steps/internal/shell"
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

// maxEditFileBytes bounds the file edit_file will pull into memory to do an
// exact-string replacement. It matches maxReadFileScanBytes rather than
// maxReadFileBytes deliberately: a model can only produce a verbatim
// old_string from text it has read, and read_file pages arbitrarily far into
// a large file, so the edit bound has to sit above the read bound, not at it.
const maxEditFileBytes = 10 << 20

// execEditFile replaces a string in an existing file, so a model can change
// part of a large file without re-emitting all of it (write_file's only
// mode). Matching is the forgiving chain above — exact first, then
// line-trimmed, then block-anchor — so a near-miss old_string from a local
// model can still land instead of burning a turn.
//
// Every error it returns is phrased as a next-turn instruction rather than a
// bare diagnosis, because the two common failures (a near-miss old_string, an
// ambiguous one) are both recoverable without leaving the conversation.
func execEditFile(_ context.Context, args map[string]any, env toolEnv) map[string]any {
	rel := stringArg(args, "path")
	if rel == "" {
		return map[string]any{"error": `edit_file: missing required argument "path"`}
	}

	oldString := stringArg(args, "old_string")
	if oldString == "" {
		return map[string]any{"error": `edit_file: "old_string" must not be empty — use write_file to create or replace a whole file`}
	}

	// Not stringArg: "" is a legal new_string (it deletes old_string), so
	// absent must be distinguished from empty.
	newString, ok := args["new_string"].(string)
	if !ok {
		return map[string]any{"error": `edit_file: missing required argument "new_string"`}
	}

	if oldString == newString {
		return map[string]any{"error": "edit_file: old_string and new_string are identical; nothing to do"}
	}

	resolved, err := resolveWritePath(env.dir, rel)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	replaceAll, _ := args["replace_all"].(bool)

	return applyEdit(resolved, rel, oldString, newString, replaceAll)
}

// readEditTarget stats and reads the file edit_file is about to modify,
// returning its contents and mode. A non-nil third return is the caller's
// ready-made error result.
func readEditTarget(resolved, rel string) (string, os.FileMode, map[string]any) {
	stat, err := os.Stat(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", 0, map[string]any{"error": fmt.Sprintf("edit_file: %q does not exist — use write_file to create it", rel)}
		}

		return "", 0, map[string]any{"error": err.Error()}
	}

	if stat.IsDir() {
		return "", 0, map[string]any{"error": fmt.Sprintf("edit_file: %q is a directory, not a file", rel)}
	}

	if stat.Size() > maxEditFileBytes {
		return "", 0, map[string]any{"error": fmt.Sprintf(
			"edit_file: %q is %s, over the %s edit limit",
			rel, shell.FormatBytes(int(stat.Size())), shell.FormatBytes(maxEditFileBytes),
		)}
	}

	data, err := os.ReadFile(resolved) //nolint:gosec // resolveWritePath rejects paths escaping dir
	if err != nil {
		return "", 0, map[string]any{"error": err.Error()}
	}

	return string(data), stat.Mode().Perm(), nil
}

// applyEdit performs execEditFile's replacement against an already-resolved
// path. It preserves the file's existing mode, so editing a checked-in shell
// script doesn't silently strip its executable bit. The strategy that matched
// is reported back to the model as match_mode, so an inexact edit is visible
// rather than silent.
func applyEdit(resolved, rel, oldString, newString string, replaceAll bool) map[string]any {
	content, mode, errResult := readEditTarget(resolved, rel)
	if errResult != nil {
		return errResult
	}

	outcome, err := replaceEditSpan(content, oldString, newString, replaceAll)
	if err != nil {
		return map[string]any{"error": editFailureAdvice(err, rel)}
	}

	err = os.WriteFile(resolved, []byte(outcome.updated), mode)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	return map[string]any{
		"path":         rel,
		"replacements": outcome.replacements,
		"first_line":   1 + strings.Count(content[:outcome.matchIndex], "\n"),
		"match_mode":   outcome.mode,
	}
}

// editFailureAdvice turns a match failure into the next thing the model
// should try, rather than a bare diagnosis.
func editFailureAdvice(err error, rel string) string {
	var ambiguous editAmbiguousError

	switch {
	case errors.Is(err, errEditNotFound):
		return fmt.Sprintf(
			"edit_file: old_string was not found in %q. Read the file with read_file and copy the text exactly, including leading whitespace.",
			rel)
	case errors.As(err, &ambiguous):
		return fmt.Sprintf(
			"edit_file: old_string appears %d times in %q. Include more surrounding lines to make it unique, or pass replace_all: true.",
			ambiguous.count, rel)
	case errors.Is(err, errEditDisproportionate):
		return fmt.Sprintf(
			"edit_file: the span matching old_string in %q is much larger than old_string itself, so the edit was refused. Re-read the file and provide the full exact text of the block you intend to replace.",
			rel)
	default:
		return err.Error()
	}
}
