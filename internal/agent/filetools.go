package agent

// list_dir, run_shell and write_file — the built-ins with no file of their own.

import (
	"context"
	"fmt"
	"os"
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

func execListDir(_ context.Context, args map[string]any, env toolEnv) map[string]any {
	rel := stringArg(args, "path")
	if rel == "" {
		rel = "."
	}

	resolved, err := resolveAgentPath(env.dir, rel)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	entries, err := os.ReadDir(resolved)
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
		size := int64(0)

		info, infoErr := e.Info()
		if infoErr == nil {
			size = info.Size()
		}

		items = append(items, map[string]any{"name": e.Name(), "is_dir": e.IsDir(), "size": size})
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
func execWriteFile(_ context.Context, args map[string]any, env toolEnv) map[string]any {
	rel := stringArg(args, "path")
	if rel == "" {
		return map[string]any{"error": `write_file: missing required argument "path"`}
	}

	content, ok := args["content"].(string)
	if !ok {
		return map[string]any{"error": `write_file: missing required argument "content"`}
	}

	resolved, err := resolveWritePath(env.dir, rel)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC

	appendArg, _ := args["append"].(bool)
	if appendArg {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}

	f, err := os.OpenFile(resolved, flags, 0o644) //nolint:gosec,mnd // resolveWritePath rejects paths escaping dir; 0644 is an ordinary file, not a secret
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer func() { _ = f.Close() }()

	n, err := f.WriteString(content)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	return map[string]any{"bytes_written": n, "path": rel}
}
