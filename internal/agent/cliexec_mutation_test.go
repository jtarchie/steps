package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

func TestCLIEnvForwardsTheNamedAPIKey(t *testing.T) {
	t.Setenv("STEPS_TEST_CLIEXEC_KEY", "sekret")

	env := cliEnv(context.Background(), config.ResolvedInvocation{APIKeyEnv: "STEPS_TEST_CLIEXEC_KEY"}) //nolint:gosec // the name of a variable, not a credential
	if !slices.Contains(env, cliAPIKeyEnv+"=sekret") {
		t.Errorf("api_key_env named a set variable, but the child's environment has no %s", cliAPIKeyEnv)
	}
}

// The child is still writing when the parse gives up on an over-long line; without a drain it blocks on a full pipe and the step waits out its whole timeout.
func TestCLIExecDrainsAChildThatOutranTheParser(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake cli binaries are shell scripts")
	}

	dir := t.TempDir()
	script := "#!/bin/sh\ncat > /dev/null\n" +
		"head -c " + strconv.Itoa(cliStreamMaxLine+1024) + " /dev/zero | tr '\\0' a\nprintf '\\n'\n" +
		"head -c 1048576 /dev/zero | tr '\\0' b\nprintf '\\n'\n"

	err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700) //nolint:gosec // a test stub must be executable
	if err != nil {
		t.Fatalf("writing the fake cli: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Generous on purpose: pumping ~2MB through tr took 18s under task's -race shards, and the assertion only has to tell "ended with the child" from "waited out the timeout".
	const timeout = 3 * time.Minute

	start := time.Now()

	_, err = runCLIConversation(t.Context(), cliPrepared(t, nil), timeout)
	if err == nil || !strings.Contains(err.Error(), "reading claude output") {
		t.Fatalf("err = %v, want the over-long line reported as a read failure", err)
	}

	if elapsed := time.Since(start); elapsed > timeout/2 {
		t.Errorf("the step took %s to fail, want it to end when the child does rather than at its %s timeout", elapsed, timeout)
	}
}
