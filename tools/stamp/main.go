// Command stamp ties a commit to the tree `task` last passed on: the sequence takes eight minutes, so a hook that RAN it would be bypassed by the second day, while one that only COMPARES tree hashes costs nothing and proves more — the tree tested is the tree pushed. See tools/stamp/CLAUDE.md.
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
	"unicode"
)

const (
	pendingFile = "steps-task-pending"
	stampFile   = "steps-task-stamp"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: stamp begin|seal|check|pushed|guard")
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
	case "pushed":
		return pushed(ctx, stdin)
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

	if skip := os.Getenv("STEPS_TESTSHARD_SKIP"); skip != "" {
		return fmt.Errorf("STEPS_TESTSHARD_SKIP=%q left tests out of this run, so it vouches for less than a stamp says — unset it and rerun `task`", skip)
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

	if staged == stamped {
		return nil
	}

	// A SLICE of the validated tree is fine as long as nothing has been touched since: one run may be committed as a series, and the proof that the series ADDS UP to what was validated is pre-push's, where it lands on what actually leaves the machine.
	working, err := worktreeTree(ctx)
	if err != nil {
		return err
	}

	if working != stamped {
		return fmt.Errorf("the working tree is not the one `task` last passed on, so nothing here is vouched for — rerun `task`. Validated → now:\n%s", treeDiff(ctx, stamped, working))
	}

	return nil
}

// pushed is pre-push: every tip being sent must be EXACTLY the validated tree. git writes one line per ref on stdin — local ref, local sha, remote ref, remote sha.
func pushed(ctx context.Context, stdin io.Reader) error {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading the refs being pushed: %w", err)
	}

	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 || strings.Trim(fields[1], "0") == "" {
			// Nothing but zeros is a deletion: no tree leaves the machine.
			continue
		}

		err = pushedTip(ctx, fields[0], fields[1])
		if err != nil {
			return err
		}
	}

	return nil
}

func pushedTip(ctx context.Context, ref, sha string) error {
	tree, err := git(ctx, nil, "rev-parse", sha+"^{tree}")
	if err != nil {
		return err
	}

	stamped, err := readGitFile(ctx, stampFile)
	if err != nil {
		return errors.New("`task` has not passed on this checkout — run it, then push the tree it validated")
	}

	if tree != stamped {
		return fmt.Errorf("%s is not the tree `task` last passed on. A file listed as D was there when the checks ran and was never committed; anything else changed after them. Commit what is missing, or rerun `task`. Validated → pushing:\n%s", ref, treeDiff(ctx, stamped, tree))
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
	var state scan

	for _, raw := range shellWords(withoutHeredocs(command)) {
		if reason := state.next(raw); reason != "" {
			return reason
		}
	}

	return ""
}

// scan is where bypass is in the command line: inside which command, and whether the next word is data or a script to read.
type scan struct{ inGit, inCommit, inShell, takesValue, recurse bool }

func (s *scan) next(raw string) string {
	word := unquote(raw)

	switch {
	case raw == separator:
		*s = scan{}
	case s.recurse:
		s.recurse = false

		return bypass(word)
	case !s.inGit && s.shellStep(word):
	case word == "git" || strings.HasSuffix(word, "/git"):
		s.inGit, s.inCommit = true, false
	case s.inGit:
		return s.gitStep(raw, word)
	}

	return ""
}

// shellStep tracks `sh -c SCRIPT` and `eval SCRIPT`, whose next word is a command line of its own.
func (s *scan) shellStep(word string) bool {
	switch {
	case isShell(word):
		s.inShell = true
	case word == "eval" || (s.inShell && isShortFlagWith(word, 'c')):
		s.recurse = true
	default:
		return false
	}

	return true
}

func (s *scan) gitStep(raw, word string) string {
	switch {
	case strings.Contains(raw, "core.hooksPath"):
		return "core.hooksPath is what points git at hack/hooks, so changing it disables the commit gate"
	case s.takesValue:
		s.takesValue = false
	case word == "commit":
		s.inCommit = true
	case word == "--no-verify":
		return "--no-verify skips the commit gate"
	case s.inCommit && shortNoVerify(word):
		return "`git commit -n` is --no-verify, which skips the commit gate"
	case s.inCommit && commitValueFlags[word]:
		s.takesValue = true
	}

	return ""
}

// commitValueFlags are the git commit options whose next word is data, such as a message that may name the flags this guard refuses.
var commitValueFlags = map[string]bool{
	"-m": true, "-F": true, "-C": true, "-c": true, "-t": true, "--message": true, "--file": true, "--reuse-message": true,
	"--reedit-message": true, "--template": true, "--author": true, "--date": true, "--cleanup": true, "--fixup": true, "--squash": true,
}

func isShell(word string) bool {
	switch filepath.Base(word) {
	case "sh", "bash", "zsh", "dash":
		return true
	}

	return false
}

// shortNoVerify reports a short-flag bundle that reaches n before any letter that takes a value: in -am"fix n" the n belongs to the message.
func shortNoVerify(word string) bool {
	if len(word) < 2 || word[0] != '-' || word[1] == '-' {
		return false
	}

	for _, r := range word[1:] {
		switch {
		case r == 'n':
			return true
		case !unicode.IsLetter(r) || strings.ContainsRune("mFCctu", r):
			return false
		}
	}

	return false
}

// unquote drops the quote characters of a word, so "--no-verify" and --no-verify compare equal; the splitter already kept a quoted span whole.
func unquote(word string) string {
	if n := len(word); n >= 2 && (word[0] == '"' || word[0] == '\'') && word[n-1] == word[0] {
		return word[1 : n-1]
	}

	return strings.NewReplacer(`"`, "", `'`, "").Replace(word)
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

func closeQuote(open, r rune) rune {
	if r == open {
		return 0
	}

	return open
}

// separator is the token shellWords emits for an unquoted ; & | ( ) or newline, so a flag in one command is never read as belonging to the git command before it.
const separator = "\x00"

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
			quote = closeQuote(quote, r)

			word.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r

			word.WriteRune(r)
		case strings.ContainsRune(";&|()\n", r):
			flush()

			words = append(words, separator)
		case r == ' ' || r == '\t':
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
