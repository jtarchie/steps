package main

import (
	"context"
	"io"
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

	// A hook run inherits the GIT_ ones from git, and the release gate exports the skip; a test must inherit neither from whoever ran it.
	for _, name := range []string{"GIT_DIR", "GIT_INDEX_FILE", "GIT_WORK_TREE", "GOFLAGS"} {
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

	err = seal(ctx, io.Discard)
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

// One task run may be committed as a SERIES: the run costs eight minutes, and demanding the whole validated tree in one commit taught exactly one lesson, which was to bundle unrelated changes.
func TestASliceOfTheValidatedTreeMayBeCommitted(t *testing.T) {
	repo(t)
	write(t, "second.txt", "validated in the same run, committed later")
	validate(t)
	sh(t, "add", "kept.txt")

	err := check(context.Background())
	if err != nil {
		t.Fatalf("a slice of an untouched validated tree was refused: %v", err)
	}
}

func TestASliceIsRefusedOnceAnythingWasEditedAfterTheRun(t *testing.T) {
	repo(t)
	write(t, "second.txt", "validated")
	validate(t)
	write(t, "second.txt", "edited after the run, and NOT part of this commit")
	sh(t, "add", "kept.txt")

	err := check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "second.txt") {
		t.Fatalf("error = %v, want a refusal naming second.txt: the working tree is no longer what the checks saw", err)
	}
}

// commit makes a real commit with the hooks out of the way: what is under test is pushed(), asked directly.
func commit(t *testing.T, message string) string {
	t.Helper()

	sh(t, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", message)

	out, err := exec.CommandContext(t.Context(), "git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}

	return strings.TrimSpace(string(out))
}

const zeroSHA = "0000000000000000000000000000000000000000"

func dockerMarkerPath(t *testing.T) string {
	t.Helper()

	marker, err := gitFile(context.Background(), dockerMarker)
	if err != nil {
		t.Fatal(err)
	}

	return marker
}

func TestBeginClearsAMarkerAnUnsealedRunLeft(t *testing.T) {
	repo(t)

	marker := dockerMarkerPath(t)
	write(t, marker, "left by a run that never sealed")

	err := begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	_, err = os.Stat(marker)
	if err == nil {
		t.Error("begin kept a marker an earlier run left, so this run would be reported for tests it has not run")
	}
}

// D13: a run whose docker tier skipped wholesale is sealed all the same, but says so at seal and again at push rather than vouching in silence.
func TestASealOverASkippedDockerTierSaysSoWithoutRefusing(t *testing.T) {
	repo(t)
	ctx := context.Background()

	err := begin(ctx)
	if err != nil {
		t.Fatal(err)
	}

	write(t, dockerMarkerPath(t), "no docker daemon\n")

	var sealed strings.Builder

	err = seal(ctx, &sealed)
	if err != nil {
		t.Fatalf("a run with the docker tier skipped was refused; D13 says note it, never fail it: %v", err)
	}

	if strings.Count(sealed.String(), "\n") != 1 || !strings.Contains(sealed.String(), "no docker daemon") {
		t.Errorf("seal said %q, want one line carrying the reason", sealed.String())
	}

	sh(t, "add", "-A")
	tip := commit(t, "validated without docker")
	refs := "refs/heads/main " + tip + " refs/heads/main " + zeroSHA + "\n"

	var pushing strings.Builder

	err = pushed(ctx, strings.NewReader(refs), &pushing)
	if err != nil || !strings.Contains(pushing.String(), "no docker daemon") {
		t.Errorf("pushed = %q, %v; want the push allowed with the skipped tier named", pushing.String(), err)
	}

	validate(t)
	pushing.Reset()

	err = pushed(ctx, strings.NewReader(refs), &pushing)
	if err != nil || pushing.Len() != 0 {
		t.Errorf("after a run that reached docker, pushed = %q, %v; want silence", pushing.String(), err)
	}
}

func TestAPushOfTheWholeValidatedTreeIsAllowed(t *testing.T) {
	repo(t)
	write(t, "second.txt", "validated")
	validate(t)
	sh(t, "add", "kept.txt")
	commit(t, "first of the series")
	sh(t, "add", "second.txt")
	tip := commit(t, "second of the series")

	err := pushed(context.Background(), strings.NewReader("refs/heads/main "+tip+" refs/heads/main "+zeroSHA+"\n"), io.Discard)
	if err != nil {
		t.Fatalf("the tip of a complete series was refused: %v", err)
	}
}

// This is the refusal the series allowance moved from commit time to push time: a file the tests ran with and nobody added is "works on my machine", and it must not leave the machine.
func TestAPushThatLeavesAValidatedFileBehindIsRefusedByName(t *testing.T) {
	repo(t)
	write(t, "forgotten.txt", "the tests needed this")
	validate(t)
	sh(t, "add", "kept.txt")
	tip := commit(t, "the series, abandoned halfway")

	err := pushed(context.Background(), strings.NewReader("refs/heads/main "+tip+" refs/heads/main "+zeroSHA+"\n"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "forgotten.txt") {
		t.Fatalf("error = %v, want a refusal naming forgotten.txt", err)
	}
}

func TestDeletingARemoteBranchPushesNoTreeToJudge(t *testing.T) {
	repo(t)

	err := pushed(context.Background(), strings.NewReader("(delete) "+zeroSHA+" refs/heads/gone "+zeroSHA+"\n"), io.Discard)
	if err != nil {
		t.Fatalf("a branch deletion was refused: %v", err)
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

	err = seal(ctx, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "kept.txt") {
		t.Fatalf("error = %v, want a refusal naming kept.txt", err)
	}

	_, statErr := os.Stat(filepath.Join(dir, ".git", stampFile))
	if statErr == nil {
		t.Fatal("a stamp was written for a tree the checks did not see")
	}
}

// A run with -skip in GOFLAGS left tests out, so it vouches for less than the stamp says.
func TestARunThatSkippedTestsIsNotSealed(t *testing.T) {
	dir := repo(t)
	ctx := context.Background()

	err := begin(ctx)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("GOFLAGS", "-skip=TestSlow")

	err = seal(ctx, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "GOFLAGS") {
		t.Fatalf("error = %v, want a refusal naming GOFLAGS", err)
	}

	_, statErr := os.Stat(filepath.Join(dir, ".git", stampFile))
	if statErr == nil {
		t.Fatal("a stamp was written for a run that skipped tests")
	}
}

func TestBypass(t *testing.T) {
	// Split, so running this file's own commands never names the key the guard refuses.
	key := "core." + "hooksPath"

	refused := []string{
		"git commit --no-verify -m x",
		"git commit -n -m x",
		"git commit -anm x",
		"cd repo && git push --no-verify",
		"git -c " + key + "=/dev/null commit -m x",
		"git config " + key + " /dev/null",
		"git -c " + strings.ToLower(key) + "=/dev/null commit -m x",
		"git config " + strings.ToUpper(key) + " /dev/null",
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=" + key + " GIT_CONFIG_VALUE_0=/dev/null git commit -m x",
		`env GIT_CONFIG_PARAMETERS="'` + strings.ToLower(key) + `=/dev/null'" git commit -m x`,
		"export GIT_CONFIG_KEY_0=" + key + "; git commit -m x",
		"GIT_CONFIG_KEY_0=" + key + " bash -c 'git commit -m x'",
		"git commit --no-verif -m x",
		"git push --no-veri",
		"/usr/bin/git commit --no-verify",
		"cat > msg.txt <<EOF\nan innocent message\nEOF\ngit commit --no-verify -F msg.txt",
		"git commit -F - <<<'message' --no-verify",
		"bash -c 'git commit --no-verify'",
		`sh -c "git commit -n"`,
		`git commit "--no-verify"`,
		`git commit "-n" -m x`,
		`zsh -lc "cd x && git commit -nm y"`,
		`eval "git commit --no-verify"`,
		"git status; git commit -n",
	}

	for _, command := range refused {
		if bypass(command) == "" {
			t.Errorf("allowed: %s", command)
		}
	}

	allowed := []string{
		"git commit -m 'guard refuses --no-verify and -n'",
		"git commit -m \"$(cat <<'EOF'\nstamp: refuse git commit -n\nEOF\n)\"",
		"cat > msg.txt <<'EOF'\nsealing points " + key + " at hack/hooks; git commit -n is refused\nEOF\ngit add x && git commit -q -F msg.txt",
		"grep -rn " + key + " tools/stamp",
		"git status && grep -rn " + key + " tools/stamp",
		"git commit --no-verbose -m x",
		"git push -n",
		"git log -n 5",
		"grep -rn -- --no-verify CLAUDE.md",
		"echo -n hi && git status",
		"git commit -m x && tail -n 5 f",
		"git commit -m x; head -n 3 f",
		"git add . | grep -n x; git commit -q -m x",
		`git commit -m "fix n handling" -a`,
		`git commit -am "drop -n and --no-verify"`,
		`bash -c 'git commit -m "a -n b"'`,
		`sh -c "git status && wc -n"`,
		"git commit --amend --no-edit",
		"git commit -m x && git push origin main",
		`git commit -m"fix n"`,
		"git commit -m eval -a",
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
