package agent

// search_files inside the container: find enumerates, Go decides, grep matches.

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

// search runs the walk as two commands and keeps every judgement on this side. find only lists paths — a format no two implementations can disagree about — so the prune list, the glob, the size limit, the head limit, the per-line cap, the byte budget and files_scanned are all still decided by the same Go that decides them for a host agent. grep only reports matching lines. Nothing about which results the model sees is delegated to the image.
func (c containerTree) search(ctx context.Context, base string, opts searchOpts) (searchResult, error) {
	var result searchResult

	paths, err := c.candidates(ctx, base)
	if err != nil {
		return result, err
	}

	kept := c.filterCandidates(&result, base, paths, opts)

	if opts.re == nil {
		for _, rel := range kept {
			result.addFile(rel, opts.headLimit)
		}

		return result, nil
	}

	// AFTER filterCandidates, exactly where searchOneFile consults it: a file too large to open still counts against files_scanned on this machine, and only then contributes nothing.
	kept, err = c.dropOversize(ctx, base, kept)
	if err != nil {
		return result, err
	}

	ere, err := patternToERE(opts.pattern)
	if err != nil {
		return result, err
	}

	hits, err := c.grep(ctx, base, kept, ere, opts)
	if err != nil {
		return result, err
	}

	binary, err := c.binaryFiles(ctx, base, matchedPaths(hits))
	if err != nil {
		return result, err
	}

	collectSearchHits(&result, kept, hits, binary, opts)

	return result, nil
}

// matchedPaths is the distinct files grep reported something in, which is the only set worth asking about: a binary file that matched nothing is already absent from the answer.
func matchedPaths(hits []grepHit) []string {
	seen := make(map[string]bool, len(hits))

	var out []string

	for _, hit := range hits {
		if seen[hit.rel] {
			continue
		}

		seen[hit.rel] = true

		out = append(out, hit.rel)
	}

	return out
}

// binaryFiles applies the host's own rule — a NUL in the first searchBinarySniffBytes — to the files that matched.
//
// Asking cannot be avoided by reading grep's output, and that was measured rather than assumed: busybox grep treats a NUL as a line separator, so the byte that makes a file binary never reaches this side at all, and a fixture whose binary file the host skipped came back reported on alpine and skipped on debian. Only the matched files are sniffed, so the cost is a handful of processes on a search that found something rather than one per candidate.
func (c containerTree) binaryFiles(ctx context.Context, base string, rels []string) (map[string]bool, error) {
	binary := make(map[string]bool, len(rels))

	for _, batch := range batchPaths(rels, maxGrepArgBytes) {
		args := make([]string, 0, len(batch))
		for _, rel := range batch {
			args = append(args, shQuote(path.Join(base, rel)))
		}

		script := fmt.Sprintf(
			`for f in %s; do a=$(head -c %d -- "$f" | wc -c); b=$(head -c %d -- "$f" | tr -d '\000' | wc -c); [ "$a" = "$b" ] || printf '%%s\n' "$f"; done`,
			strings.Join(args, " "), searchBinarySniffBytes, searchBinarySniffBytes)

		out, _, err := c.read(ctx, script)
		if err != nil {
			return nil, err
		}

		for _, row := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
			if row == "" {
				continue
			}

			binary[strings.TrimPrefix(row, base+"/")] = true
		}
	}

	return binary, nil
}

// candidates asks find for every file under base that a host walk would have scanned: the prune list applied to DIRECTORIES below the base, and regular files only. No size rule — that one is the caller's, and it is applied where the host applies it.
//
// -mindepth 1 and -type d are both about matching the host exactly. searchWalk prunes a skip name only when it is BELOW the base (`path != base`) and only when it is a directory, so without them a search of `vendor` itself returns nothing and a plain file named `node_modules` disappears.
func (c containerTree) candidates(ctx context.Context, base string) ([]string, error) {
	prunes := make([]string, 0, len(searchSkipDirs))
	for name := range searchSkipDirs {
		prunes = append(prunes, `-name `+shQuote(name))
	}

	sort.Strings(prunes)

	script := fmt.Sprintf(`find %s -mindepth 1 \( -type d -a \( %s \) \) -prune -o -type f -print`,
		shQuote(base), strings.Join(prunes, " -o "))

	return c.findPaths(ctx, script)
}

// dropOversize removes the files a host content scan would have refused to open. `-size +Nc` counts BYTES and is strictly greater than N, which is the same comparison searchOneFile makes — find's default 512-byte blocks would round the threshold into a different one.
func (c containerTree) dropOversize(ctx context.Context, base string, rels []string) ([]string, error) {
	big, err := c.findPaths(ctx, fmt.Sprintf(`find %s -mindepth 1 -type f -size +%dc -print`, shQuote(base), maxSearchFileBytes))
	if err != nil {
		return nil, err
	}

	if len(big) == 0 {
		return rels, nil
	}

	skip := make(map[string]bool, len(big))
	for _, p := range big {
		skip[strings.TrimPrefix(p, base+"/")] = true
	}

	out := make([]string, 0, len(rels))

	for _, rel := range rels {
		if !skip[rel] {
			out = append(out, rel)
		}
	}

	return out, nil
}

// findPaths runs a find and answers the paths it printed, in the order a host walk would have visited them.
func (c containerTree) findPaths(ctx context.Context, script string) ([]string, error) {
	out, code, err := c.read(ctx, script)
	if err != nil {
		return nil, err
	}

	if code != 0 && out == "" {
		return nil, nil
	}

	paths := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	sortLikeWalkDir(paths)

	return paths, nil
}

// sortLikeWalkDir puts find's output in the order filepath.WalkDir would have produced, which is what head_limit makes load-bearing: both sides cap the result, so an unordered walk shows the model a DIFFERENT subset of the same matches depending on where its agent ran.
//
// WalkDir sorts each directory's entries and descends, so the order is lexical on the path's COMPONENTS. Comparing whole paths is not the same — "sub.txt" sorts before "sub/deep.go" because '.' is below '/' — so the separator is compared as a byte below every byte a filename may hold.
func sortLikeWalkDir(paths []string) {
	sort.Slice(paths, func(i, j int) bool {
		return walkOrderKey(paths[i]) < walkOrderKey(paths[j])
	})
}

func walkOrderKey(p string) string {
	return strings.ReplaceAll(p, "/", "\x00")
}

// filterCandidates applies the glob and the scan budget, in the order and with the accounting a host walk uses, and answers the paths grep should be asked about — relative, because that is how the model is shown them.
func (c containerTree) filterCandidates(result *searchResult, base string, paths []string, opts searchOpts) []string {
	var kept []string

	for _, p := range paths {
		if p == "" {
			continue
		}

		if result.filesScanned >= maxSearchFilesScanned {
			result.budgetHit = true

			break
		}

		rel := strings.TrimPrefix(p, base+"/")
		if rel == p || rel == "" {
			continue
		}

		if opts.glob != "" && !matchGlob(rel, opts.glob) {
			continue
		}

		result.filesScanned++

		kept = append(kept, rel)
	}

	return kept
}

// maxGrepArgBytes bounds one grep command's file list, so a large tree becomes several commands rather than one the kernel refuses.
const maxGrepArgBytes = 96 << 10

// grepHit is one matching line as the container reported it.
type grepHit struct {
	rel  string
	line int
	text string
}

// grep runs the rendered ERE over the kept files in batches. -a is passed because busybox accepts -I and then ignores it — measured — so asking grep to skip binary files would work on debian and silently not on alpine; instead every file is read as text and collectSearchHits drops the ones whose own matched lines prove they are not.
func (c containerTree) grep(ctx context.Context, base string, rels []string, ere string, opts searchOpts) ([]grepHit, error) {
	var hits []grepHit

	flags := "-n -H -a -E"
	if opts.caseInsensitive {
		flags += " -i"
	}

	for _, batch := range batchPaths(rels, maxGrepArgBytes) {
		args := make([]string, 0, len(batch))
		for _, rel := range batch {
			args = append(args, shQuote(path.Join(base, rel)))
		}

		// grep's own status is reported on stderr because the pipe to head is what the script exits with. Without it a grep that could not compile the pattern — musl and GNU do not accept quite the same ERE, which is the whole reason patternToERE exists — comes back as an empty result and the model is told the tree holds no matches.
		script := fmt.Sprintf(`{ grep %s -e %s -- %s; printf '%s%%s\n' "$?" >&2; } | head -c %d`,
			flags, shQuote(ere), strings.Join(args, " "), grepStatusPrefix, maxScriptOutputBytes)

		out, diag, _, err := c.readDiag(ctx, script)
		if err != nil {
			return nil, err
		}

		err = grepRefusal(diag, ere)
		if err != nil {
			return nil, err
		}

		hits = append(hits, parseGrepHits(out, base, batch)...)
	}

	return hits, nil
}

const (
	// grepStatusPrefix tags the line the grep script writes to stderr so its own exit status can be read back past the pipe.
	grepStatusPrefix = "steps-grep-status="
	// grepMatchedNothing is grep's ordinary "no lines selected", which is an answer rather than a failure.
	grepMatchedNothing = 1
	// grepSIGPIPE is what grep exits with when head has taken all the output it was going to take — the cap doing its job, not a refusal.
	grepSIGPIPE = 141
)

// errGrepRefused is the container's grep declining to run at all. Returned as tool data, like errUnportablePattern, because the model wrote the pattern and is the one who can rewrite it.
var errGrepRefused = errors.New("search_files: the container's grep could not run the pattern")

// grepRefusal reads the status the grep script reported on stderr and answers non-nil when grep declined the work, with grep's own diagnostic in the message.
func grepRefusal(diag, ere string) error {
	var (
		status  = 0
		message []string
	)

	for _, line := range strings.Split(diag, "\n") {
		if rest, found := strings.CutPrefix(strings.TrimSpace(line), grepStatusPrefix); found {
			status, _ = strconv.Atoi(rest)

			continue
		}

		if strings.TrimSpace(line) != "" {
			message = append(message, strings.TrimSpace(line))
		}
	}

	if status <= grepMatchedNothing || status == grepSIGPIPE {
		return nil
	}

	return fmt.Errorf("%w %q (exit %d): %s", errGrepRefused, ere, status, strings.Join(message, "; "))
}

// batchPaths splits rels into groups whose rendered argument lists stay under limit.
func batchPaths(rels []string, limit int) [][]string {
	var (
		out  [][]string
		size int
	)

	batch := make([]string, 0, len(rels))

	for _, rel := range rels {
		cost := len(rel) + 4
		if size+cost > limit && len(batch) > 0 {
			out = append(out, batch)
			batch, size = make([]string, 0, len(rels)), 0
		}

		batch = append(batch, rel)
		size += cost
	}

	if len(batch) > 0 {
		out = append(out, batch)
	}

	return out
}

// parseGrepHits reads grep's `path:line:text` rows, resolving the path by longest prefix against the batch that was asked for rather than by cutting at the first colon — a filename may legitimately contain one, and cutting would attribute its matches to a file that does not exist.
func parseGrepHits(out, base string, batch []string) []grepHit {
	byPath := make(map[string]string, len(batch))
	for _, rel := range batch {
		byPath[path.Join(base, rel)] = rel
	}

	var hits []grepHit

	for _, row := range strings.Split(out, "\n") {
		if row == "" {
			continue
		}

		rel, rest, ok := splitGrepPath(row, byPath)
		if !ok {
			continue
		}

		number, text, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}

		line, err := strconv.Atoi(number)
		if err != nil {
			continue
		}

		hits = append(hits, grepHit{rel: rel, line: line, text: text})
	}

	return hits
}

// splitGrepPath finds which of the asked-for files a row belongs to, preferring the longest match so `a.txt` never claims a row that belongs to `a.txt:1.txt`.
func splitGrepPath(row string, byPath map[string]string) (rel, rest string, ok bool) {
	best := ""

	for full := range byPath {
		if len(full) > len(best) && strings.HasPrefix(row, full+":") {
			best = full
		}
	}

	if best == "" {
		return "", "", false
	}

	return byPath[best], row[len(best)+1:], true
}

// collectSearchHits folds the container's rows into the result the model sees, in the order a host walk would have produced them so the two answer identically.
func collectSearchHits(result *searchResult, kept []string, hits []grepHit, binary map[string]bool, opts searchOpts) {
	grouped := make(map[string][]grepHit, len(kept))

	for _, hit := range hits {
		grouped[hit.rel] = append(grouped[hit.rel], hit)
	}

	for _, rel := range kept {
		if binary[rel] {
			continue
		}

		rows := grouped[rel]
		if len(rows) == 0 {
			continue
		}

		switch opts.mode {
		case "content":
			result.addMatches(matchesFrom(rows, opts.headLimit), len(rows), opts.headLimit)
		case "count":
			result.addCount(rel, len(rows), opts.headLimit)
		default:
			result.addFile(rel, opts.headLimit)
		}
	}
}

// matchesFrom keeps at most headLimit of one file's lines, mirroring the host scan's rule that a single pathological file must not spend the whole budget.
func matchesFrom(rows []grepHit, headLimit int) []searchMatch {
	out := make([]searchMatch, 0, min(len(rows), headLimit))

	for _, row := range rows {
		if len(out) >= headLimit {
			break
		}

		out = append(out, searchMatch{path: row.rel, line: row.line, text: truncateSearchLine(row.text)})
	}

	return out
}
