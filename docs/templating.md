# Template Rendering

Resource `check`/`in`/`out` commands and agent custom tools support `{{ .source.* }}` and `{{ .version.* }}` templating for dynamic command construction (e.g., `gh pr list --repo {{ .source.repo }}`). A custom tool's `run:` additionally sees the model's call arguments as `{{ .args.* }}` — the tool's parameter schema is derived from these references (`inferToolParams` in `internal/agent/tools.go`).

Templates have the full [slim-sprig](https://github.com/go-task/slim-sprig) function library available (string/list/default/date helpers, dependency-free) plus our own `shellquote`. These are the only two non-stdlib imports allowed into `internal/template`.

## Shell-quoting untrusted values

Since a rendered template runs via `sh -c`, any value interpolated into it that could contain shell metacharacters — backticks, `$(...)`, quotes, `; | &` — must be piped through `shellquote`, which renders it as one safely-quoted POSIX word and supplies its own quotes:

```
-b {{ .args.body | shellquote }}
```

Don't add surrounding `"..."` — `shellquote` already quotes only when needed. This matters most for LLM- or PR-authored values (e.g. a review body): without it, a body containing `` `replace` `` gets command-substituted by the shell and posted with those words silently missing. See `examples/agents.yml`'s `post_review` tool.

This is the safe idiom CLAUDE.md's trust-boundary notes assume when reasoning about the custom-tool arg-inference regex — see that file's "Trust Boundaries" section for why the regex has to keep matching the piped form.
