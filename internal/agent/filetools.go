package agent

// list_dir, run_shell and write_file — the built-ins with no file of their own.

import (
	"context"
	"fmt"
)

// maxListDirEntries caps how many entries list_dir returns inline — a
// directory with tens of thousands of entries would otherwise flood the
// model's context the same way an uncapped file read would. Unlike text
// output, a directory listing is structured data with no natural byte
// preview, so it's bounded by entry count instead of being spilled to a file:
// past this many entries, execListDir returns the first maxListDirEntries
// plus the true total and a truncated flag, pointing the model at a narrower
// path or run_shell (e.g. `ls | grep`) instead. A judgment-call default, not
// derived from any hard constraint — tune freely.
const maxListDirEntries = 1_000

func execListDir(ctx context.Context, args map[string]any, env toolEnv) map[string]any {
	rel := stringArg(args, "path")
	if rel == "" {
		rel = "."
	}

	files := env.files()

	resolved, err := files.resolve(ctx, rel)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	entries, err := files.listDir(ctx, resolved)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	total := len(entries)
	truncated := total > maxListDirEntries

	if truncated {
		entries = entries[:maxListDirEntries]
	}

	items := make([]map[string]any, 0, len(entries))

	for _, e := range entries {
		items = append(items, map[string]any{"name": e.name, "is_dir": e.isDir, "size": e.size})
	}

	result := map[string]any{"entries": items, "total": total, "truncated": truncated}

	if truncated {
		result["message"] = fmt.Sprintf(
			"showing the first %d of %d entries; narrow path or use run_shell (e.g. `ls | grep ...`) to search a large directory",
			maxListDirEntries, total,
		)
	}

	return result
}

func execRunShell(ctx context.Context, args map[string]any, env toolEnv) map[string]any {
	command := stringArg(args, "command")
	if command == "" {
		return map[string]any{"error": `run_shell: missing required argument "command"`}
	}

	// run_shell is a builtin, so it carries no max_output_bytes: of its own
	// (validateMaxOutputBytesShape rejects one) — always the global cap.
	return shellToolResult(ctx, command, env, maxToolOutputBytes)
}

// execWriteFile writes (or appends to, if append: true) a UTF-8 text file at
// a path relative to env.dir. content is required but may legitimately be "";
// distinguishing "" from "not supplied" is why this checks args["content"]
// directly rather than going through stringArg.
func execWriteFile(ctx context.Context, args map[string]any, env toolEnv) map[string]any {
	rel := stringArg(args, "path")
	if rel == "" {
		return map[string]any{"error": `write_file: missing required argument "path"`}
	}

	content, ok := args["content"].(string)
	if !ok {
		return map[string]any{"error": `write_file: missing required argument "content"`}
	}

	resolved, err := env.files().resolveWrite(ctx, rel)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	appendArg, _ := args["append"].(bool)

	err = env.files().writeFile(ctx, resolved, []byte(content), appendArg)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	return map[string]any{"bytes_written": len(content), "path": rel}
}
