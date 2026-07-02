// Package toolreg is the registry of named Go functions used two ways:
// as action-state handlers (the engine authors the args from the rendered
// input block) and as agent tools (the model authors the args, optionally
// guarded). Ships a small builtin library so YAML-only machines can do
// real work.
package toolreg

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ActionFunc is the uniform tool signature: JSON-ish maps in and out.
type ActionFunc func(ctx context.Context, args map[string]any) (map[string]any, error)

// Tool is a registered function.
type Tool struct {
	Name        string
	Description string
	Fn          ActionFunc
}

// Registry holds registered tools by name.
type Registry struct {
	tools map[string]Tool
}

// New returns a registry pre-loaded with the builtin library.
func New() *Registry {
	r := &Registry{tools: map[string]Tool{}}
	registerBuiltins(r)
	return r
}

// Register adds a tool. Names are namespaced with dots (file.write).
func (r *Registry) Register(name, description string, fn ActionFunc) {
	r.tools[name] = Tool{Name: name, Description: description, Fn: fn}
}

// Has reports whether name is registered (for the machine validator).
func (r *Registry) Has(name string) bool { _, ok := r.tools[name]; return ok }

// Get returns a tool.
func (r *Registry) Get(name string) (Tool, bool) { t, ok := r.tools[name]; return t, ok }

// Names lists registered tools, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.tools))
	for k := range r.tools {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Call invokes a tool by name.
func (r *Registry) Call(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	t, ok := r.tools[name]
	if !ok {
		return nil, fmt.Errorf("tool %q is not registered (have: %v)", name, r.Names())
	}
	return t.Fn(ctx, args)
}

// confine joins path under root and refuses escapes — tool args may be
// model-authored, and the model does not get to read outside the sandbox.
func confine(root, path string) (string, error) {
	joined := filepath.Join(root, path)
	rel, err := filepath.Rel(root, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes root %q", path, root)
	}
	return joined, nil
}

func str(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing required argument %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string, got %T", key, v)
	}
	return s, nil
}

// splitHunks explodes per-file entries into per-hunk entries, each carrying
// the file header for context — huge files become several small scout
// contexts instead of one big one.
func splitHunks(files []any) []any {
	var out []any
	for _, f := range files {
		file, ok := f.(map[string]any)
		if !ok {
			continue
		}
		patch, _ := file["patch"].(string)
		header, hunks := carveHunks(patch)
		if len(hunks) <= 1 {
			out = append(out, file)
			continue
		}
		for i, hunk := range hunks {
			counts := countChanges(hunk)
			out = append(out, map[string]any{
				"path":      file["path"],
				"hunk":      i + 1,
				"hunks":     len(hunks),
				"patch":     header + "\n" + hunk,
				"additions": counts[0],
				"deletions": counts[1],
			})
		}
	}
	return out
}

func carveHunks(patch string) (header string, hunks []string) {
	lines := strings.Split(patch, "\n")
	var head, current []string
	for _, line := range lines {
		if strings.HasPrefix(line, "@@") {
			if len(current) > 0 {
				hunks = append(hunks, strings.Join(current, "\n"))
			}
			current = []string{line}
			continue
		}
		if current == nil {
			head = append(head, line)
		} else {
			current = append(current, line)
		}
	}
	if len(current) > 0 {
		hunks = append(hunks, strings.Join(current, "\n"))
	}
	return strings.Join(head, "\n"), hunks
}

func countChanges(hunk string) [2]int {
	var c [2]int
	for _, line := range strings.Split(hunk, "\n") {
		switch {
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			c[0]++
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			c[1]++
		}
	}
	return c
}

func registerBuiltins(r *Registry) {
	r.Register("file.write", "Write content to a file, creating parent directories",
		func(ctx context.Context, args map[string]any) (map[string]any, error) {
			path, err := str(args, "path")
			if err != nil {
				return nil, err
			}
			content, err := str(args, "content")
			if err != nil {
				return nil, err
			}
			if dir := filepath.Dir(path); dir != "." {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return nil, err
				}
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return nil, err
			}
			return map[string]any{"path": path, "bytes": len(content)}, nil
		})

	r.Register("file.read", "Read a file as text; an optional root confines and anchors relative paths",
		func(ctx context.Context, args map[string]any) (map[string]any, error) {
			path, err := str(args, "path")
			if err != nil {
				return nil, err
			}
			if root, _ := args["root"].(string); root != "" {
				joined, err := confine(root, path)
				if err != nil {
					return nil, err
				}
				path = joined
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			return map[string]any{"content": string(raw), "bytes": len(raw)}, nil
		})

	r.Register("diff.split", "Split a unified diff into per-file (or per-hunk, by: hunk) entries; a root attaches current file contents (capped by context_bytes)",
		func(ctx context.Context, args map[string]any) (map[string]any, error) {
			diff, err := str(args, "diff")
			if err != nil {
				return nil, err
			}
			by, _ := args["by"].(string)
			if by != "" && by != "file" && by != "hunk" {
				return nil, fmt.Errorf("by must be file or hunk, got %q", by)
			}
			root, _ := args["root"].(string)
			contextBytes := 4096
			if v, _ := args["context_bytes"].(string); v != "" {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil || n < 0 {
					return nil, fmt.Errorf("context_bytes must be a non-negative integer, got %q", v)
				}
				contextBytes = n
			}
			var files []any
			var current map[string]any
			var patch []string
			flush := func() {
				if current != nil {
					current["patch"] = strings.Join(patch, "\n")
					files = append(files, current)
				}
				patch = nil
			}
			for _, line := range strings.Split(diff, "\n") {
				switch {
				case strings.HasPrefix(line, "diff --git "):
					flush()
					// "diff --git a/path b/path" — the b/ side is the new path.
					path := line[len("diff --git "):]
					if i := strings.LastIndex(path, " b/"); i >= 0 {
						path = path[i+3:]
					}
					current = map[string]any{"path": path, "additions": 0, "deletions": 0}
				case current == nil:
					continue
				case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
					current["additions"] = current["additions"].(int) + 1
				case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
					current["deletions"] = current["deletions"].(int) + 1
				}
				if current != nil {
					patch = append(patch, line)
				}
			}
			flush()
			if by == "hunk" {
				files = splitHunks(files)
			}
			// Deterministic enrichment: the machine attaches the current
			// file so scouts see code around the patch, capped so scout
			// prompts stay cheap. The senior pulls FULL files on demand
			// via a guarded file.read tool instead.
			if root != "" && contextBytes > 0 {
				for _, f := range files {
					file, ok := f.(map[string]any)
					if !ok {
						continue
					}
					path, _ := file["path"].(string)
					joined, err := confine(root, path)
					if err != nil {
						continue
					}
					raw, err := os.ReadFile(joined)
					if err != nil {
						continue // deleted/renamed files simply carry no content
					}
					content := string(raw)
					if len(content) > contextBytes {
						content = content[:contextBytes] + "\n… (truncated; full file available via file.read)"
					}
					file["content"] = content
				}
			}
			return map[string]any{"files": files, "count": len(files)}, nil
		})

	registerGH(r)

	r.Register("http.get", "HTTP GET a URL and return the body (up to 256KB)",
		func(ctx context.Context, args map[string]any) (map[string]any, error) {
			url, err := str(args, "url")
			if err != nil {
				return nil, err
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return nil, err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
			if err != nil {
				return nil, err
			}
			return map[string]any{"status": resp.StatusCode, "body": string(body)}, nil
		})
}
