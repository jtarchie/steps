package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// search_files exists because the alternative — letting a model reach for a
// shell grep, or for a fuzzy symbol search over an MCP server — is how an
// agent step floods its own context. A real run of experiments/self-build
// made eight calls to one such search tool and took ~100KB of results, of
// which the useful part was the top few lines of each; per-turn latency grew
// from 48s to 81s as that accumulated, and the step hit its timeout without
// producing anything.
//
// So every bound here is chosen so the WORST case is small enough to not
// matter, rather than large enough to be safe: maxSearchContentResults *
// (maxSearchLineBytes + a path) is ~25KB, comfortably under
// maxToolOutputBytes. That is the whole design — the cap is arithmetic, not
// a truncation applied after the fact, and this tool can never spill.
//
// It is implemented in Go rather than shelling out to grep/find for four
// reasons: BSD and GNU grep differ in flags and output; a hard cap has to be
// enforced deterministically rather than by piping through head (which pays
// for the output before discarding it); under a step's image: a shell-out
// would run inside a container whose filesystem may not match the host-side
// file tools'; and regexp + filepath.WalkDir are stdlib, so the package's
// depguard import allow-list is untouched.
const (
	// Hard ceilings a model cannot raise with head_limit. Content mode is
	// additionally bounded by maxSearchContentBudgetBytes below — the count
	// ceiling alone no longer guarantees the inline fit, the byte budget
	// does. TestSearchWorstCaseFitsInlineBudget pins that, since the
	// arithmetic is the entire guarantee this tool offers.
	maxSearchPathResults    = 200
	maxSearchContentResults = 200

	// Defaults when head_limit is omitted. Lower than the ceilings, since
	// the common case is orienting ("where does this live?"), not surveying.
	defaultSearchPathResults    = 50
	defaultSearchContentResults = 50

	// maxSearchLineBytes bounds a single returned matching line. A minified
	// bundle or a generated table can carry lines of tens of KB, any one of
	// which would blow the whole result budget on its own. 500 bytes keeps
	// nearly every real source line whole — the previous 200 cut most
	// matching lines mid-statement, which mattered for edit_file old_strings
	// copied from results.
	maxSearchLineBytes = 500

	// maxSearchContentBudgetBytes bounds the TOTAL bytes of kept content
	// matches (line text plus searchMatchOverheadBytes each). This is what
	// lets both the line bound and the result ceiling be generous: short
	// typical lines can fill all maxSearchContentResults slots, while
	// maximum-length lines saturate the byte budget after ~45 matches —
	// either way the result fits inline, by construction.
	maxSearchContentBudgetBytes = 28_000

	// searchMatchOverheadBytes is the per-match allowance for the path, line
	// number, and JSON scaffolding around the line text.
	searchMatchOverheadBytes = 120

	// maxSearchFileBytes skips files too large to be worth scanning line by
	// line. A match inside a 2MB file is nearly always a generated artifact.
	maxSearchFileBytes = 2 << 20

	// maxSearchFilesScanned bounds the walk itself, so a search rooted at a
	// huge tree terminates in bounded time rather than stat-ing every inode.
	maxSearchFilesScanned = 20_000

	// searchBinarySniffBytes is how much of a file's head is checked for a
	// NUL byte before deciding it is binary and skipping it.
	searchBinarySniffBytes = 8000
)

// searchSkipDirs are pruned from every walk. These are the directories whose
// contents are either not the user's code (dependencies, vendored trees) or
// actively hostile to a text search (a git object store is compressed
// binary), and scanning them is the single biggest source of both latency
// and false matches.
//
//nolint:gochecknoglobals // static, read-only lookup table
var searchSkipDirs = map[string]bool{
	".git":                 true,
	"node_modules":         true,
	"vendor":               true,
	toolOutputSpillDirName: true,
}

// searchFilesDescription is built with Sprintf from the constants above so
// the numbers the model is told can never drift from the ones enforced —
// the same discipline listDirDescription uses.
//
//nolint:gochecknoglobals // computed once from consts; not a mutable global
var searchFilesDescription = fmt.Sprintf(
	"Search files under a directory and return a HARD-CAPPED result set — use this instead of a shell grep or find,"+
		" which can flood the conversation with thousands of matches. Supply pattern (a regular expression matched"+
		" against each line), glob (a shell pattern matched against a file's path, e.g. \"**/*.go\"), or both; glob"+
		" alone makes this a filename search. path is the directory to search, relative to the working directory or an"+
		" absolute path inside it, defaulting to \".\". output_mode is \"files_with_matches\" (default — just the paths,"+
		" cheapest; then read the ones you want with read_file), \"content\" (matching lines WITH their line numbers,"+
		" each capped at %d bytes — this is where you get file:line for a citation), or \"count\" (matches per file)."+
		" head_limit caps results: default %d paths / %d lines, hard ceiling %d / %d. Skips %s, binary files, and files"+
		" over %s. The result's total and truncated fields tell you whether more matched — narrow the pattern or glob"+
		" rather than paging, since there is no page-2.",
	maxSearchLineBytes,
	defaultSearchPathResults, defaultSearchContentResults,
	maxSearchPathResults, maxSearchContentResults,
	".git/node_modules/vendor",
	"2 MB",
)

// searchOpts is one resolved search_files call.
type searchOpts struct {
	re        *regexp.Regexp // nil for a filename-only search
	glob      string         // "" matches every file
	mode      string
	headLimit int
}

// searchMatch is one matching line in content mode.
type searchMatch struct {
	path string
	line int
	text string
}

// searchResult accumulates a walk. total counts every match found, not just
// the ones kept, so the model is told the true scale and can narrow instead
// of assuming it saw everything — the same total/truncated contract
// execListDir already exposes.
type searchResult struct {
	files        []string
	matches      []searchMatch
	counts       []map[string]any
	total        int
	filesScanned int
	budgetHit    bool
	// contentBytes accumulates the kept matches' line text plus per-match
	// overhead, enforcing maxSearchContentBudgetBytes (see addMatches).
	contentBytes int
}

// execSearchFiles validates a search_files call and runs it. Argument errors
// are returned as data with a corrective hint, the same posture the other
// tools take, since a bad regexp or a missing pattern is recoverable on the
// model's next turn.
func execSearchFiles(_ context.Context, args map[string]any, env toolEnv) map[string]any {
	pattern, glob := stringArg(args, "pattern"), stringArg(args, "glob")

	mode, errResult := searchMode(pattern, glob, stringArg(args, "output_mode"))
	if errResult != nil {
		return errResult
	}

	opts, errResult := buildSearchOpts(args, pattern, glob, mode)
	if errResult != nil {
		return errResult
	}

	rel := stringArg(args, "path")
	if rel == "" {
		rel = "."
	}

	base, err := resolveAgentPath(env.dir, rel)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	result, err := searchWalk(base, opts)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	return searchResultMap(base, opts, result)
}

// searchMode validates the requested output mode against what the supplied
// arguments can actually produce. A non-nil second return is a ready-made
// error result for the caller.
func searchMode(pattern, glob, requested string) (string, map[string]any) {
	if pattern == "" && glob == "" {
		return "", map[string]any{"error": `search_files: supply at least one of "pattern" (a regexp matched against file contents) or "glob" (a shell pattern matched against file paths)`}
	}

	mode := requested
	if mode == "" {
		mode = "files_with_matches"
	}

	if mode != "files_with_matches" && mode != "content" && mode != "count" {
		return "", map[string]any{"error": fmt.Sprintf(
			"search_files: unknown output_mode %q (expected files_with_matches, content, or count)", mode,
		)}
	}

	// content and count both report per-line matches, which a filename-only
	// search has none of.
	if pattern == "" && mode != "files_with_matches" {
		return "", map[string]any{"error": fmt.Sprintf(
			"search_files: output_mode %q needs a pattern; a glob-only search can only report which files matched", mode,
		)}
	}

	return mode, nil
}

// buildSearchOpts compiles the pattern and resolves head_limit against the
// mode's default and ceiling. A non-nil second return is a ready-made error
// result for the caller.
func buildSearchOpts(args map[string]any, pattern, glob, mode string) (searchOpts, map[string]any) {
	opts := searchOpts{glob: glob, mode: mode}

	if strings.Contains(glob, "**") && !strings.HasPrefix(glob, "**/") {
		return opts, map[string]any{"error": fmt.Sprintf(
			"search_files: glob %q uses ** somewhere other than the start; ** is only supported as a leading segment (e.g. \"**/*.go\"). Match on a file name (\"*_test.go\") or a full relative path instead.",
			glob,
		)}
	}

	if pattern != "" {
		expr := pattern
		if caseInsensitive, _ := args["case_insensitive"].(bool); caseInsensitive {
			expr = "(?i)" + expr
		}

		re, err := regexp.Compile(expr)
		if err != nil {
			return opts, map[string]any{"error": "search_files: invalid pattern: " + err.Error()}
		}

		opts.re = re
	}

	limit, ceiling := defaultSearchPathResults, maxSearchPathResults
	if mode == "content" {
		limit, ceiling = defaultSearchContentResults, maxSearchContentResults
	}

	if supplied, ok := intArg(args, "head_limit"); ok && supplied > 0 {
		limit = min(supplied, ceiling)
	}

	opts.headLimit = limit

	return opts, nil
}

// searchWalk walks base, applying the glob and (when set) the content
// pattern. It keeps walking after headLimit is reached so total reflects
// every match, stopping early only when the file-scan budget is exhausted.
func searchWalk(base string, opts searchOpts) (searchResult, error) {
	var result searchResult

	walk := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory mid-tree shouldn't fail the whole
			// search; skip it and keep going.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}

			return nil
		}

		if d.IsDir() {
			if path != base && searchSkipDirs[d.Name()] {
				return fs.SkipDir
			}

			return nil
		}

		return searchWalkFile(&result, base, path, opts)
	}

	err := filepath.WalkDir(base, walk)
	if err != nil {
		return result, fmt.Errorf("search_files: %w", err)
	}

	return result, nil
}

// searchWalkFile is searchWalk's per-file leg: budget check, glob filter,
// then the content scan. Split out to keep the walk callback itself small.
func searchWalkFile(result *searchResult, base, path string, opts searchOpts) error {
	if result.filesScanned >= maxSearchFilesScanned {
		result.budgetHit = true

		return filepath.SkipAll
	}

	rel, err := filepath.Rel(base, path)
	if err != nil {
		// A path we can't relativize is one we can't report; skip it.
		return nil //nolint:nilerr // deliberate: an unreportable entry is not a fatal search error
	}

	if opts.glob != "" && !matchGlob(rel, opts.glob) {
		return nil
	}

	result.filesScanned++
	searchOneFile(result, path, rel, opts)

	return nil
}

// searchOneFile applies opts to a single already-glob-matched file,
// appending whatever it contributes to result.
func searchOneFile(result *searchResult, path, rel string, opts searchOpts) {
	// Filename-only search: matching the glob is the whole test.
	if opts.re == nil {
		result.addFile(rel, opts.headLimit)

		return
	}

	info, err := os.Stat(path)
	if err != nil || info.Size() > maxSearchFileBytes {
		return
	}

	matches, count := scanFileMatches(path, rel, opts)
	if count == 0 {
		return
	}

	switch opts.mode {
	case "content":
		result.addMatches(matches, count, opts.headLimit)
	case "count":
		result.addCount(rel, count, opts.headLimit)
	default: // files_with_matches
		result.addFile(rel, opts.headLimit)
	}
}

// addFile records one matching file, keeping it only while under the head
// limit — total still counts it, so the caller learns the true scale.
func (r *searchResult) addFile(rel string, headLimit int) {
	r.total++

	if len(r.files) < headLimit {
		r.files = append(r.files, rel)
	}
}

// addCount records one file's match count.
func (r *searchResult) addCount(rel string, count, headLimit int) {
	r.total++

	if len(r.counts) < headLimit {
		r.counts = append(r.counts, map[string]any{"path": rel, "count": count})
	}
}

// addMatches records a file's matching lines. total advances by the file's
// full count even when only some lines are kept. A match is kept only while
// both the head limit and the content byte budget hold — the byte budget is
// what keeps a saturated result inline regardless of line lengths.
func (r *searchResult) addMatches(matches []searchMatch, count, headLimit int) {
	r.total += count

	for _, m := range matches {
		// The path is part of what a match costs on the wire, and an
		// unbounded one (a deep tree, a long generated filename) would
		// otherwise be free — 200 cheap matches under long paths could then
		// exceed the inline cap the budget exists to guarantee.
		cost := len(m.text) + len(m.path) + searchMatchOverheadBytes
		if len(r.matches) >= headLimit || r.contentBytes+cost > maxSearchContentBudgetBytes {
			return
		}

		r.matches = append(r.matches, m)
		r.contentBytes += cost
	}
}

// scanFileMatches returns the matching lines of one file (capped at the
// mode's head limit, since a single pathological file must not consume the
// whole budget) plus the true number of matching lines in it. A file with a
// NUL byte in its head is treated as binary and skipped.
func scanFileMatches(path, rel string, opts searchOpts) ([]searchMatch, int) {
	f, err := os.Open(path) //nolint:gosec // path comes from a WalkDir under a resolveAgentPath-confined base
	if err != nil {
		return nil, 0
	}
	defer func() { _ = f.Close() }()

	head := make([]byte, searchBinarySniffBytes)

	n, _ := f.Read(head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return nil, 0
	}

	_, err = f.Seek(0, 0)
	if err != nil {
		return nil, 0
	}

	var (
		matches []searchMatch
		count   int
	)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxReadFileScanBytes)

	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if !opts.re.MatchString(text) {
			continue
		}

		count++

		if opts.mode == "content" && len(matches) < opts.headLimit {
			matches = append(matches, searchMatch{path: rel, line: line, text: truncateSearchLine(text)})
		}
	}

	return matches, count
}

// truncateSearchLine bounds one returned line, marking it when it cuts so a
// model doesn't copy a half-line believing it is complete (which matters
// especially for edit_file's old_string).
func truncateSearchLine(s string) string {
	if len(s) <= maxSearchLineBytes {
		return s
	}

	return s[:maxSearchLineBytes] + " …[line truncated]"
}

// matchGlob reports whether a file's path relative to the search base
// matches glob. A glob with no separator matches against the base name
// ("*_test.go"), so the common case needs no path juggling; otherwise it
// matches the whole relative path. A leading "**/" means "at any depth" and
// is handled explicitly, because filepath.Match's * never crosses a
// separator — buildSearchOpts rejects ** anywhere else rather than letting
// it silently match nothing.
func matchGlob(rel, glob string) bool {
	if rest, found := strings.CutPrefix(glob, "**/"); found {
		if ok, _ := filepath.Match(rest, filepath.Base(rel)); ok {
			return true
		}

		ok, _ := filepath.Match(rest, rel)

		return ok
	}

	if !strings.ContainsRune(glob, filepath.Separator) {
		ok, _ := filepath.Match(glob, filepath.Base(rel))

		return ok
	}

	ok, _ := filepath.Match(glob, rel)

	return ok
}

// searchResultMap shapes a completed walk into the tool result, mirroring
// execListDir's entries/total/truncated/message contract so a model that has
// learned one already knows how to read the other.
func searchResultMap(base string, opts searchOpts, r searchResult) map[string]any {
	result := map[string]any{
		"mode":          opts.mode,
		"base":          base,
		"total":         r.total,
		"files_scanned": r.filesScanned,
	}

	var kept int

	switch opts.mode {
	case "content":
		items := make([]map[string]any, 0, len(r.matches))
		for _, m := range r.matches {
			items = append(items, map[string]any{"path": m.path, "line": m.line, "text": m.text})
		}

		result["matches"], kept = items, len(items)
	case "count":
		if r.counts == nil {
			r.counts = []map[string]any{}
		}

		result["counts"], kept = r.counts, len(r.counts)
	default:
		if r.files == nil {
			r.files = []string{}
		}

		result["files"], kept = r.files, len(r.files)
	}

	truncated := kept < r.total
	result["truncated"] = truncated

	switch {
	case r.budgetHit:
		result["message"] = fmt.Sprintf(
			"stopped after scanning %d files; narrow path or glob to search a smaller part of the tree", r.filesScanned,
		)
	case truncated:
		result["message"] = fmt.Sprintf(
			"showing %d of %d matches; narrow the pattern or glob rather than paging — there is no page 2", kept, r.total,
		)
	}

	return result
}
