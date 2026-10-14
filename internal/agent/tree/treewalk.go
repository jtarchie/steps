package tree

// The tree-side half of search_files: walking a directory under resolved options and accumulating what the model is shown.

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// MaxSearchLineBytes bounds a single returned matching line. A minified
	// bundle or a generated table can carry lines of tens of KB, any one of
	// which would blow the whole result budget on its own. 500 bytes keeps
	// nearly every real source line whole — the previous 200 cut most
	// matching lines mid-statement, which mattered for edit_file old_strings
	// copied from results.
	MaxSearchLineBytes = 500

	// MaxSearchContentBudgetBytes bounds the TOTAL bytes of kept content
	// matches (line text plus SearchMatchOverheadBytes each). This is what
	// lets both the line bound and the result ceiling be generous: short
	// typical lines can fill all maxSearchContentResults slots, while
	// maximum-length lines saturate the byte budget after ~45 matches —
	// either way the result fits inline, by construction.
	MaxSearchContentBudgetBytes = 28_000

	// SearchMatchOverheadBytes is the per-match allowance for the path, line
	// number, and JSON scaffolding around the line text.
	SearchMatchOverheadBytes = 120

	// MaxSearchFileBytes skips files too large to be worth scanning line by
	// line. A match inside a 2MB file is nearly always a generated artifact.
	MaxSearchFileBytes = 2 << 20

	// MaxSearchFilesScanned bounds the walk itself, so a search rooted at a
	// huge tree terminates in bounded time rather than stat-ing every inode.
	MaxSearchFilesScanned = 20_000

	// searchBinarySniffBytes is how much of a file's head is checked for a
	// NUL byte before deciding it is binary and skipping it.
	searchBinarySniffBytes = 8000
)

// MaxScanBytes bounds the largest single line readFileRange will pull
// into memory before deciding what to keep. It's well above maxReadFileBytes
// (the return budget) so a spilled file's long line — base64/minified/`jq -c`
// output, which frequently has no newlines at all — is still readable
// (byte-truncated to the budget) rather than failing the scan with a cryptic
// "token too long"; a line even larger than this degrades to truncated=true,
// still not a hard error. Matches internal/shell's SpillMaxBytes.
const MaxScanBytes = 10 << 20 // 10 MiB

// SpillDirName is the fixed name of the subdirectory a
// run_shell/custom tool's oversized output is spilled into (see
// newToolOutputSpillDir). config.artifactNamePattern requires a declared
// input/output name to start with a letter or digit, so this leading '.'
// can never collide with a real one.
const SpillDirName = ".steps-agent-out"

// searchSkipDirs are pruned from every walk. These are the directories whose
// contents are either not the user's code (dependencies, vendored trees) or
// actively hostile to a text search (a git object store is compressed
// binary), and scanning them is the single biggest source of both latency
// and false matches.
//
//nolint:gochecknoglobals // static, read-only lookup table
var searchSkipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	SpillDirName:   true,
}

// SearchOpts is one resolved search_files call.
type SearchOpts struct {
	Re        *regexp.Regexp // nil for a filename-only search
	Glob      string         // "" matches every file
	Mode      string
	HeadLimit int
	// pattern and caseInsensitive are the model's own arguments, kept because a container tree cannot hand a compiled *regexp.Regexp to grep — it renders the pattern to POSIX ERE instead, and folding is a grep flag rather than part of the expression.
	Pattern         string
	CaseInsensitive bool
}

// Match is one matching line in content mode.
type Match struct {
	Path string
	Line int
	Text string
}

// SearchResult accumulates a walk. total counts every match found, not just
// the ones kept, so the model is told the true scale and can narrow instead
// of assuming it saw everything — the same total/truncated contract
// execListDir already exposes.
type SearchResult struct {
	Files        []string
	Matches      []Match
	Counts       []map[string]any
	Total        int
	FilesScanned int
	BudgetHit    bool
	// contentBytes accumulates the kept matches' line text plus per-match
	// overhead, enforcing MaxSearchContentBudgetBytes (see addMatches).
	contentBytes int
}

// searchWalk walks base, applying the glob and (when set) the content
// pattern. It keeps walking after headLimit is reached so total reflects
// every match, stopping early only when the file-scan budget is exhausted.
func searchWalk(base string, opts SearchOpts) (SearchResult, error) {
	var result SearchResult

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
func searchWalkFile(result *SearchResult, base, path string, opts SearchOpts) error {
	if result.FilesScanned >= MaxSearchFilesScanned {
		result.BudgetHit = true

		return filepath.SkipAll
	}

	rel, err := filepath.Rel(base, path)
	if err != nil {
		// A path we can't relativize is one we can't report; skip it.
		return nil //nolint:nilerr // deliberate: an unreportable entry is not a fatal search error
	}

	if opts.Glob != "" && !matchGlob(rel, opts.Glob) {
		return nil
	}

	result.FilesScanned++
	searchOneFile(result, path, rel, opts)

	return nil
}

// searchOneFile applies opts to a single already-glob-matched file,
// appending whatever it contributes to result.
func searchOneFile(result *SearchResult, path, rel string, opts SearchOpts) {
	// Filename-only search: matching the glob is the whole test.
	if opts.Re == nil {
		result.addFile(rel, opts.HeadLimit)

		return
	}

	info, err := os.Stat(path)
	if err != nil || info.Size() > MaxSearchFileBytes {
		return
	}

	matches, count := scanFileMatches(path, rel, opts)
	if count == 0 {
		return
	}

	switch opts.Mode {
	case "content":
		result.addMatches(matches, count, opts.HeadLimit)
	case "count":
		result.addCount(rel, count, opts.HeadLimit)
	default: // files_with_matches
		result.addFile(rel, opts.HeadLimit)
	}
}

// addFile records one matching file, keeping it only while under the head
// limit — total still counts it, so the caller learns the true scale.
func (r *SearchResult) addFile(rel string, headLimit int) {
	r.Total++

	if len(r.Files) < headLimit {
		r.Files = append(r.Files, rel)
	}
}

// addCount records one file's match count.
func (r *SearchResult) addCount(rel string, count, headLimit int) {
	r.Total++

	if len(r.Counts) < headLimit {
		r.Counts = append(r.Counts, map[string]any{"path": rel, "count": count})
	}
}

// addMatches records a file's matching lines. total advances by the file's
// full count even when only some lines are kept. A match is kept only while
// both the head limit and the content byte budget hold — the byte budget is
// what keeps a saturated result inline regardless of line lengths.
func (r *SearchResult) addMatches(matches []Match, count, headLimit int) {
	r.Total += count

	for _, m := range matches {
		// The path is part of what a match costs on the wire, and an
		// unbounded one (a deep tree, a long generated filename) would
		// otherwise be free — 200 cheap matches under long paths could then
		// exceed the inline cap the budget exists to guarantee.
		cost := len(m.Text) + len(m.Path) + SearchMatchOverheadBytes
		if len(r.Matches) >= headLimit || r.contentBytes+cost > MaxSearchContentBudgetBytes {
			return
		}

		r.Matches = append(r.Matches, m)
		r.contentBytes += cost
	}
}

// scanFileMatches returns the matching lines of one file (capped at the
// mode's head limit, since a single pathological file must not consume the
// whole budget) plus the true number of matching lines in it. A file with a
// NUL byte in its head is treated as binary and skipped.
func scanFileMatches(path, rel string, opts SearchOpts) ([]Match, int) {
	f, err := os.Open(path) //nolint:gosec // path comes from a WalkDir under a ResolvePath-confined base
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
		matches []Match
		count   int
	)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), MaxScanBytes)

	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if !opts.Re.MatchString(text) {
			continue
		}

		count++

		if opts.Mode == "content" && len(matches) < opts.HeadLimit {
			matches = append(matches, Match{Path: rel, Line: line, Text: truncateSearchLine(text)})
		}
	}

	return matches, count
}

// truncateSearchLine bounds one returned line, marking it when it cuts so a
// model doesn't copy a half-line believing it is complete (which matters
// especially for edit_file's old_string).
func truncateSearchLine(s string) string {
	if len(s) <= MaxSearchLineBytes {
		return s
	}

	cut := MaxSearchLineBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut] + " …[line truncated]"
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
