package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo is a scratch git checkout the test process is moved into, because the tool under test finds its repository the way git does: from the working directory.
func repo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	t.Chdir(dir)

	// A hook run inherits these from git; a test must not inherit them from whoever ran the tests.
	for _, name := range []string{"GIT_DIR", "GIT_INDEX_FILE", "GIT_WORK_TREE"} {
		t.Setenv(name, "")

		_ = os.Unsetenv(name)
	}

	sh(t, "init", "-q")
	write(t, "kept.txt", "one")

	return dir
}

func sh(t *testing.T, args ...string) {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput() //nolint:gosec // fixed git subcommands written in this file
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, name, content string) {
	t.Helper()

	err := os.WriteFile(name, []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func validate(t *testing.T) {
	t.Helper()

	ctx := context.Background()

	err := begin(ctx)
	if err != nil {
		t.Fatal(err)
	}

	err = seal(ctx)
	if err != nil {
		t.Fatal(err)
	}
}

func TestACommitOfTheValidatedTreeIsAllowed(t *testing.T) {
	repo(t)
	validate(t)
	sh(t, "add", "-A")

	err := check(context.Background())
	if err != nil {
		t.Fatalf("the validated tree was refused: %v", err)
	}
}

func TestACommitIsRefusedUntilTaskHasPassed(t *testing.T) {
	repo(t)
	sh(t, "add", "-A")

	err := check(context.Background())
	if err == nil {
		t.Fatal("a commit was allowed on a checkout task never passed on")
	}
}

func TestAnEditAfterTheRunIsRefusedByName(t *testing.T) {
	repo(t)
	validate(t)
	write(t, "kept.txt", "two")
	sh(t, "add", "-A")

	err := check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "kept.txt") {
		t.Fatalf("error = %v, want a refusal naming kept.txt", err)
	}
}

func TestAFileTheRunSawButTheCommitLeavesOutIsRefused(t *testing.T) {
	repo(t)
	write(t, "stray.txt", "left by a sabotage")
	validate(t)
	sh(t, "add", "kept.txt")

	err := check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stray.txt") {
		t.Fatalf("error = %v, want a refusal naming stray.txt", err)
	}
}

func TestAnIgnoredFileIsNotPartOfTheTree(t *testing.T) {
	repo(t)
	write(t, ".gitignore", "state.db\n")
	validate(t)
	write(t, "state.db", "written by the tests themselves")
	sh(t, "add", "-A")

	err := check(context.Background())
	if err != nil {
		t.Fatalf("an ignored file changed the verdict: %v", err)
	}
}

func TestATreeEditedDuringTheRunIsNotSealed(t *testing.T) {
	dir := repo(t)
	ctx := context.Background()

	err := begin(ctx)
	if err != nil {
		t.Fatal(err)
	}

	write(t, "kept.txt", "edited by another session mid-run")

	err = seal(ctx)
	if err == nil || !strings.Contains(err.Error(), "kept.txt") {
		t.Fatalf("error = %v, want a refusal naming kept.txt", err)
	}

	_, statErr := os.Stat(filepath.Join(dir, ".git", stampFile))
	if statErr == nil {
		t.Fatal("a stamp was written for a tree the checks did not see")
	}
}

func TestBypass(t *testing.T) {
	refused := []string{
		"git commit --no-verify -m x",
		"git commit -n -m x",
		"git commit -anm x",
		"cd repo && git push --no-verify",
		"git -c core.hooksPath=/dev/null commit -m x",
		"git config core.hooksPath /dev/null",
		"/usr/bin/git commit --no-verify",
		"cat > msg.txt <<EOF\nan innocent message\nEOF\ngit commit --no-verify -F msg.txt",
		"git commit -F - <<<'message' --no-verify",
	}

	for _, command := range refused {
		if bypass(command) == "" {
			t.Errorf("allowed: %s", command)
		}
	}

	allowed := []string{
		"git commit -m 'guard refuses --no-verify and -n'",
		"git commit -m \"$(cat <<'EOF'\nstamp: refuse git commit -n\nEOF\n)\"",
		"cat > msg.txt <<'EOF'\nsealing points core.hooksPath at hack/hooks; git commit -n is refused\nEOF\ngit add x && git commit -q -F msg.txt",
		"git push -n",
		"git log -n 5",
		"grep -rn -- --no-verify CLAUDE.md",
		"echo -n hi && git status",
	}

	for _, command := range allowed {
		if reason := bypass(command); reason != "" {
			t.Errorf("refused %q: %s", command, reason)
		}
	}
}

func TestGuardDeniesThroughStdoutBecauseGoRunFlattensExitCodes(t *testing.T) {
	var out strings.Builder

	err := guard(strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"git commit --no-verify"}}`), &out)
	if err != nil {
		t.Fatalf("a refusal must not be an error, or `go run` turns it into an ignorable exit 1: %v", err)
	}

	for _, want := range []string{`"permissionDecision":"deny"`, `"hookEventName":"PreToolUse"`, "--no-verify"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("decision = %s, want it to contain %s", out.String(), want)
		}
	}
}

func TestGuardSaysNothingAboutAnOrdinaryOrUnreadableCall(t *testing.T) {
	for _, input := range []string{`{"tool_name":"Bash","tool_input":{"command":"git status"}}`, `not json`} {
		var out strings.Builder

		err := guard(strings.NewReader(input), &out)
		if err != nil || out.Len() != 0 {
			t.Errorf("guard(%s) = %q, %v; want silence, which is how a hook allows", input, out.String(), err)
		}
	}
}
