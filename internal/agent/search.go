package agent

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jtarchie/steps/internal/agent/tree"
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
// (tree.MaxSearchLineBytes + a path) is ~25KB, comfortably under
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
	// additionally bounded by tree.MaxSearchContentBudgetBytes below — the count
	// ceiling alone no longer guarantees the inline fit, the byte budget
	// does. TestSearchWorstCaseFitsInlineBudget pins that, since the
	// arithmetic is the entire guarantee this tool offers.
	maxSearchPathResults    = 200
	maxSearchContentResults = 200

	// Defaults when head_limit is omitted. Lower than the ceilings, since
	// the common case is orienting ("where does this live?"), not surveying.
	defaultSearchPathResults    = 50
	defaultSearchContentResults = 50
)

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
	tree.MaxSearchLineBytes,
	defaultSearchPathResults, defaultSearchContentResults,
	maxSearchPathResults, maxSearchContentResults,
	".git/node_modules/vendor",
	"2 MB",
)

// execSearchFiles validates a search_files call and runs it. Argument errors
// are returned as data with a corrective hint, the same posture the other
// tools take, since a bad regexp or a missing pattern is recoverable on the
// model's next turn.
func execSearchFiles(ctx context.Context, args map[string]any, env toolEnv) map[string]any {
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

	files := env.files()

	base, err := files.Resolve(ctx, rel)
	if err != nil {
		return errorResult(err.Error())
	}

	result, err := files.Search(ctx, base, opts)
	if err != nil {
		return errorResult(err.Error())
	}

	return searchResultMap(base, opts, result)
}

// searchMode validates the requested output mode against what the supplied
// arguments can actually produce. A non-nil second return is a ready-made
// error result for the caller.
func searchMode(pattern, glob, requested string) (string, map[string]any) {
	if pattern == "" && glob == "" {
		return "", errorResult(`search_files: supply at least one of "pattern" (a regexp matched against file contents) or "glob" (a shell pattern matched against file paths)`)
	}

	mode := requested
	if mode == "" {
		mode = "files_with_matches"
	}

	if mode != "files_with_matches" && mode != "content" && mode != "count" {
		return "", errorResult(fmt.Sprintf(
			"search_files: unknown output_mode %q (expected files_with_matches, content, or count)", mode,
		))
	}

	// content and count both report per-line matches, which a filename-only
	// search has none of.
	if pattern == "" && mode != "files_with_matches" {
		return "", errorResult(fmt.Sprintf(
			"search_files: output_mode %q needs a pattern; a glob-only search can only report which files matched", mode,
		))
	}

	return mode, nil
}

// buildSearchOpts compiles the pattern and resolves head_limit against the
// mode's default and ceiling. A non-nil second return is a ready-made error
// result for the caller.
func buildSearchOpts(args map[string]any, pattern, glob, mode string) (tree.SearchOpts, map[string]any) {
	opts := tree.SearchOpts{Glob: glob, Mode: mode, Pattern: pattern}

	if strings.Contains(glob, "**") && !strings.HasPrefix(glob, "**/") {
		return opts, errorResult(fmt.Sprintf(
			"search_files: glob %q uses ** somewhere other than the start; ** is only supported as a leading segment (e.g. \"**/*.go\"). Match on a file name (\"*_test.go\") or a full relative path instead.",
			glob,
		))
	}

	if pattern != "" {
		expr := pattern

		opts.CaseInsensitive, _ = args["case_insensitive"].(bool)
		if opts.CaseInsensitive {
			expr = "(?i)" + expr
		}

		re, err := regexp.Compile(expr)
		if err != nil {
			return opts, errorResult("search_files: invalid pattern: " + err.Error())
		}

		opts.Re = re
	}

	limit, ceiling := defaultSearchPathResults, maxSearchPathResults
	if mode == "content" {
		limit, ceiling = defaultSearchContentResults, maxSearchContentResults
	}

	if supplied, ok := intArg(args, "head_limit"); ok && supplied > 0 {
		limit = min(supplied, ceiling)
	}

	opts.HeadLimit = limit

	return opts, nil
}

// searchResultMap shapes a completed walk into the tool result, mirroring
// execListDir's entries/total/truncated/message contract so a model that has
// learned one already knows how to read the other.
func searchResultMap(base string, opts tree.SearchOpts, r tree.SearchResult) map[string]any {
	result := map[string]any{
		"mode":          opts.Mode,
		"base":          base,
		"total":         r.Total,
		"files_scanned": r.FilesScanned,
	}

	var kept int

	switch opts.Mode {
	case "content":
		items := make([]map[string]any, 0, len(r.Matches))
		for _, m := range r.Matches {
			items = append(items, map[string]any{"path": m.Path, "line": m.Line, "text": m.Text})
		}

		result["matches"], kept = items, len(items)
	case "count":
		if r.Counts == nil {
			r.Counts = []map[string]any{}
		}

		result["counts"], kept = r.Counts, len(r.Counts)
	default:
		if r.Files == nil {
			r.Files = []string{}
		}

		result["files"], kept = r.Files, len(r.Files)
	}

	truncated := kept < r.Total
	result["truncated"] = truncated

	switch {
	case r.BudgetHit:
		result["message"] = fmt.Sprintf(
			"stopped after scanning %d files; narrow path or glob to search a smaller part of the tree", r.FilesScanned,
		)
	case truncated:
		result["message"] = fmt.Sprintf(
			"showing %d of %d matches; narrow the pattern or glob rather than paging — there is no page 2", kept, r.Total,
		)
	}

	return result
}
