# tools/stamp: the commit gate

`task` is the whole validation sequence and takes about eight minutes. There is no CI, deliberately, so nothing used to connect "task passed" to "git commit" except a sentence in CLAUDE.md — which an agent can fail to follow, and a hurried human can forget.

**The hook compares; it never runs anything.** A pre-commit hook that reran eight minutes of checks would be bypassed within a day. Instead `task` ends by recording the git tree hash of what it validated (`.git/steps-task-stamp`), and `hack/hooks/pre-commit` refuses a commit whose tree is any other. That costs milliseconds and proves MORE than "task ran recently": the tree tested is the tree committed. A file edited after the run, a fix left unstaged, and a stray directory a sabotage left under `e2e/` are all the same refusal, and the refusal names the files.

- **`begin` / `seal` bracket the run**, after `go fmt` and `go mod tidy` have made their edits. Eight minutes is long enough for another session to edit the tree, and a stamp taken only at the end would vouch for files no test saw. `seal` refuses if the tree moved, and says which files did.
- **The tree hash covers tracked, modified and untracked-but-not-ignored files** (`git add -A` into a scratch index — the real index is never touched). So the commit must stage everything the run saw. A partial commit of a tested tree is refused on purpose: the remainder was part of what passed.
- **The stamp is per checkout** (`git rev-parse --git-path`), so a worktree has its own and cannot borrow the main checkout's.
- **`task` points `core.hooksPath` at `hack/hooks`** when it seals. A stamp nobody reads is decoration, so the two are installed together; a checkout that has never run `task` has no hook, and has nothing worth committing either.
- **`guard` is the Claude Code half** (`.claude/settings.json`, PreToolUse on Bash). It refuses `--no-verify`, `git commit -n` and anything naming `core.hooksPath`, because the agent is the actor most likely to reach for them when a hook says no. It splits words with quote awareness so a commit MESSAGE may say `--no-verify`. It fails OPEN on input it cannot parse: a harness format change must not break every Bash call. **It denies with a JSON decision on stdout and exit 0, never exit 2**: it runs under `go run`, which flattens every nonzero child status to 1 — the status Claude Code shows and ignores. The first version exited 2 and would have blocked nothing; only piping a real tool call through the real command found it.

## When the hook refuses

Rerun `task`, stage what it validated, commit. That is the whole procedure. A human can `git commit --no-verify` and owns that decision; the agent cannot, and should ask rather than look for a third way.

## What this is not

Not a security boundary. The stamp is a plain file in `.git`, and anyone who wants to forge it can. It exists to make the honest path the easy one and the skipped step loud, which is the failure this repo has actually had.
