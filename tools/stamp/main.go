// Command stamp ties a commit to the tree `task` last passed on: the sequence takes eight minutes, so a hook that RAN it would be bypassed by the second day, while one that only COMPARES tree hashes costs nothing and proves more — the tree tested is the tree committed. See tools/stamp/CLAUDE.md.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	pendingFile = "steps-task-pending"
	stampFile   = "steps-task-stamp"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: stamp begin|seal|check|guard")
		os.Exit(2)
	}

	err := run(context.Background(), os.Args[1], os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stamp: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, verb string, stdin io.Reader, stdout io.Writer) error {
	switch verb {
	case "begin":
		return begin(ctx)
	case "seal":
		return seal(ctx)
	case "check":
		return check(ctx)
	case "guard":
		return guard(stdin, stdout)
	default:
		return fmt.Errorf("unknown verb %q", verb)
	}
}

func begin(ctx context.Context) error {
	tree, err := worktreeTree(ctx)
	if err != nil {
		return err
	}

	return writeGitFile(ctx, pendingFile, tree)
}

func seal(ctx context.Context) error {
	began, err := readGitFile(ctx, pendingFile)
	if err != nil {
		return fmt.Errorf("no run to seal: %w", err)
	}

	tree, err := worktreeTree(ctx)
	if err != nil {
		return err
	}

	if tree != began {
		return fmt.Errorf("the tree changed while the checks ran, so they vouch for neither version — rerun `task`:\n%s", treeDiff(ctx, began, tree))
	}

	return writeGitFile(ctx, stampFile, tree)
}

func check(ctx context.Context) error {
	// git hands a hook the index it is committing through GIT_INDEX_FILE (a temporary one for `commit -a` and `commit <paths>`), so this is the tree of the commit and not merely of the staging area.
	staged, err := git(ctx, nil, "write-tree")
	if err != nil {
		return err
	}

	stamped, err := readGitFile(ctx, stampFile)
	if err != nil {
		return errors.New("`task` has not passed on this checkout — run it, then commit the tree it validated")
	}

	if staged != stamped {
		return fmt.Errorf("this commit is not the tree `task` last passed on. Stage everything that run saw (and nothing since), or rerun `task`. Validated → committing:\n%s", treeDiff(ctx, stamped, staged))
	}

	return nil
}

// worktreeTree is the tree a commit of everything here would have: tracked, modified and untracked-but-not-ignored alike.
func worktreeTree(ctx context.Context) (string, error) {
	index, err := git(ctx, nil, "rev-parse", "--git-path", "index")
	if err != nil {
		return "", err
	}

	dir, err := os.MkdirTemp("", "steps-stamp-*")
	if err != nil {
		return "", fmt.Errorf("scratch index: %w", err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	scratch := filepath.Join(dir, "index")

	// Seeded from the real index for its stat cache, which is what keeps `add -A` from rehashing the whole tree; a checkout with no index yet starts from nothing, and git creates the file.
	current, err := os.ReadFile(index) //nolint:gosec // a path git itself reported
	if err == nil {
		err = os.WriteFile(scratch, current, 0o600) //nolint:gosec // inside the directory MkdirTemp just made
	}

	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("seeding the scratch index: %w", err)
	}

	env := []string{"GIT_INDEX_FILE=" + scratch}

	_, err = git(ctx, env, "add", "-A")
	if err != nil {
		return "", err
	}

	return git(ctx, env, "write-tree")
}

func treeDiff(ctx context.Context, from, to string) string {
	out, err := git(ctx, nil, "diff", "--name-status", from, to)
	if err != nil {
		return "(could not list the difference: " + err.Error() + ")"
	}

	return out
}

func gitFile(ctx context.Context, name string) (string, error) {
	path, err := git(ctx, nil, "rev-parse", "--git-path", name)
	if err != nil {
		return "", err
	}

	return filepath.Clean(path), nil
}

func writeGitFile(ctx context.Context, name, content string) error {
	path, err := gitFile(ctx, name)
	if err != nil {
		return err
	}

	err = os.WriteFile(path, []byte(content+"\n"), 0o600)
	if err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}

	return nil
}

func readGitFile(ctx context.Context, name string) (string, error) {
	path, err := gitFile(ctx, name)
	if err != nil {
		return "", err
	}

	content, err := os.ReadFile(path) //nolint:gosec // a path git itself reported
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", name, err)
	}

	return strings.TrimSpace(string(content)), nil
}

func git(ctx context.Context, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // fixed subcommands; the only variable arguments are hashes this program read back from git
	cmd.Env = append(os.Environ(), env...)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(string(out)), nil
}

// guard refuses the Bash tool calls that would walk around the hook: the agent is the actor most likely to reach for them, and the one a prose rule in CLAUDE.md demonstrably does not stop.
//
// The refusal is a JSON decision on stdout with a ZERO exit, not the exit status 2 a hook may also block with: this runs under `go run`, which reports every nonzero child status as 1, and 1 is the status Claude Code shows and then ignores. It also means a stamp that does not compile fails open rather than refusing every Bash call, including the ones that would fix it.
func guard(stdin io.Reader, stdout io.Writer) error {
	var call struct {
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}

	err := json.NewDecoder(stdin).Decode(&call)
	if err != nil {
		// A call this cannot read is a call it cannot judge, and failing closed would break every Bash call on a harness format change.
		return nil //nolint:nilerr // deliberate: see above
	}

	reason := bypass(call.ToolInput.Command)
	if reason == "" {
		return nil
	}

	var decision struct {
		Output struct {
			Event    string `json:"hookEventName"`
			Decision string `json:"permissionDecision"`
			Reason   string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}

	decision.Output.Event = "PreToolUse"
	decision.Output.Decision = "deny"
	decision.Output.Reason = reason + ". The pre-commit hook proves the commit is the tree `task` passed on; if it refuses, rerun `task` rather than going around it — or ask the user, who can."

	err = json.NewEncoder(stdout).Encode(decision)
	if err != nil {
		return fmt.Errorf("writing the decision: %w", err)
	}

	return nil
}

// bypass names how a command would skip the commit hook, or nothing.
//
// ponytail: a quote-aware word splitter, not a shell parser — it does not expand variables or aliases, so `git $X` is not seen. Upgrade to mvdan.cc/sh if the agent is ever caught going around it that way.
func bypass(command string) string {
	var inGit, inCommit bool

	for _, word := range shellWords(withoutHeredocs(command)) {
		switch {
		case word == "git" || strings.HasSuffix(word, "/git"):
			inGit, inCommit = true, false
		case !inGit:
		case word == "commit":
			inCommit = true
		case strings.Contains(word, "core.hooksPath"):
			return "core.hooksPath is what points git at hack/hooks, so changing it disables the commit gate"
		case word == "--no-verify":
			return "--no-verify skips the commit gate"
		case inCommit && isShortFlagWith(word, 'n'):
			return "`git commit -n` is --no-verify, which skips the commit gate"
		}
	}

	return ""
}

var heredoc = regexp.MustCompile(`(?:^|[^<])<<-?\s*['"]?(\w+)`)

// withoutHeredocs drops heredoc bodies, which are data and not commands: a commit message written through one is the usual way this repo's messages are made, and the first commit through the gate was refused for DESCRIBING the flags it refuses.
func withoutHeredocs(command string) string {
	var (
		kept  []string
		until string
	)

	for _, line := range strings.Split(command, "\n") {
		if until != "" {
			if strings.TrimSpace(line) == until {
				until = ""
			}

			continue
		}

		kept = append(kept, line)

		if match := heredoc.FindStringSubmatch(line); match != nil {
			until = match[1]
		}
	}

	return strings.Join(kept, "\n")
}

// shellWords splits on unquoted whitespace and command separators, keeping a quoted span whole — which is what lets a commit MESSAGE say "--no-verify" without being refused for it.
func shellWords(command string) []string {
	var (
		words []string
		word  strings.Builder
		quote rune
	)

	flush := func() {
		if word.Len() > 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}

	for _, r := range command {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}

			word.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r

			word.WriteRune(r)
		case strings.ContainsRune(" \t\n;&|()", r):
			flush()
		default:
			word.WriteRune(r)
		}
	}

	flush()

	return words
}

// isShortFlagWith reports whether word is a bundle of short flags including flag, such as -an.
func isShortFlagWith(word string, flag rune) bool {
	if len(word) < 2 || word[0] != '-' || word[1] == '-' {
		return false
	}

	return strings.ContainsRune(word[1:], flag)
}
