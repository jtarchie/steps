package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/jtarchie/steps/internal/config"
)

// contextBlock is one resolved context_paths: file — the path as declared
// (shown to the model, so it can cite or re-read the file) and its full
// contents.
type contextBlock struct {
	path    string
	content string
	// image is the file itself when it is one the model can be shown; content
	// then only describes it.
	image *toolImage
}

// loadContextBlocks reads an agent's declared context_paths out of the
// step's working directory at preparation time. Every path is confined to
// the workspace by resolveAgentPath — the same guard the file tools use —
// and capped at limit bytes apiece (max_context_bytes:, default
// config.DefaultMaxContextBytes). A missing or escaping file is a hard
// preparation error: context_paths is operator-authored config, so a bad one
// should fail the step loudly before a token is spent, rather than surface as
// a surprise mid-conversation. A file that is merely too BIG is not that —
// see loadContextBlock.
func loadContextBlocks(ctx context.Context, dir string, paths []string, limit int, sight imageSight) ([]contextBlock, error) {
	if len(paths) == 0 {
		return nil, nil
	}

	// A negative limit reaches here only from a direct caller — a validated
	// config rejects one — and would otherwise make io.LimitReader hand over
	// nothing at all. 0 is NOT that case: it is max_context_bytes: 0, the
	// explicit "no ceiling", and passes through.
	if limit < 0 {
		limit = config.DefaultMaxContextBytes
	}

	blocks := make([]contextBlock, 0, len(paths))

	for _, p := range paths {
		block, err := loadContextBlock(ctx, dir, p, limit, sight)
		if err != nil {
			return nil, err
		}

		blocks = append(blocks, block)
	}

	return blocks, nil
}

// loadContextBlock reads one context file, capped at limit bytes.
//
// An oversized file is TRUNCATED, not refused.
//
// It used to fail the step, on the reasoning that context_paths: is
// operator-authored config and a bad one should be loud. But the operator
// authors a PATH, not a size: `pr/pr.diff` is a perfectly correct path that
// fails the moment the pull request it names grows past the limit — which is
// nothing they did, and is likeliest exactly when the review matters most. A
// matrix cell's path is model-authored now besides.
//
// So it degrades the way read_file does for the same reason: hand over what
// fits, and say plainly that there is more and how to reach it. Losing the
// tail of a diff costs a reviewer some context; failing the step costs the
// entire review.
//
// Only limit bytes are ever READ. Slurping the whole file to keep a prefix of
// it made a multi-megabyte diff cost its full size in memory to deliver 100KB
// — and then paid for a second copy of it, since the truncating branch
// converted the entire buffer to a string before slicing. The size in the
// notice comes from a stat instead, which is where it was always available.
//
// An image, or any binary file, is not truncated text at all: it gets
// read_file's own answer for it, so a screenshot handed over as context
// arrives as the image a model that can see one would have read itself — and
// is bounded by the image limit rather than by max_context_bytes:, which
// budgets text. That answer is never a preparation error either, an image too
// big to send included: like a long diff, it is a fact about this run's
// input, not a mistake in the pipeline.
func loadContextBlock(ctx context.Context, dir, path string, limit int, sight imageSight) (contextBlock, error) {
	resolved, err := resolveAgentPath(dir, path)
	if err != nil {
		return contextBlock{}, fmt.Errorf("context path %q: %w", path, err)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return contextBlock{}, fmt.Errorf("context path %q: %w", path, err)
	}

	file, err := os.Open(resolved) //nolint:gosec // resolveAgentPath confines to dir
	if err != nil {
		return contextBlock{}, fmt.Errorf("context path %q: %w", path, err)
	}
	defer func() { _ = file.Close() }() // read-only; nothing to report on close

	// limit 0 is max_context_bytes: 0 — hand the file over whole. The reader
	// is unwrapped rather than given a huge bound, so the "is there more?"
	// test below can stay a single comparison.
	var reader io.Reader = file
	if limit > 0 {
		reader = io.LimitReader(file, int64(limit))
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		return contextBlock{}, fmt.Errorf("context path %q: %w", path, err)
	}

	if isNonText(data) {
		answer := nonTextResult(ctx, hostTree{dir: dir}, resolved, path, data, info.Size(), sight)

		return nonTextContextBlock(path, answer), nil
	}

	content := string(data)

	if limit > 0 && info.Size() > int64(limit) {
		content += fmt.Sprintf(
			"\n\n[truncated: %s is %d bytes, and the first %d are shown. "+
				"Use read_file with start_line/end_line to page through the rest.]",
			path, info.Size(), limit)

		slog.WarnContext(ctx, "agent.context_path_truncated", "path", path,
			"bytes", info.Size(), "limit", limit)
	}

	return contextBlock{path: path, content: content}, nil
}

// nonTextContextBlock turns read_file's answer for a non-text file into a
// context block, whichever of its three shapes the answer took.
func nonTextContextBlock(path string, answer map[string]any) contextBlock {
	block := contextBlock{path: path}

	block.content, _ = answer["content"].(string)
	if refusal, ok := answer["error"].(string); ok {
		block.content = refusal
	}

	if img, ok := answer[imageKey].(toolImage); ok {
		block.image = &img
	}

	return block
}
