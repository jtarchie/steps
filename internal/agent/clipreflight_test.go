package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// fakeCLIOnPath writes an executable named binary into a fresh directory and
// puts that directory first on PATH. It exists so a preflight test can answer
// "is the cli installed" without depending on whether the real one is.
func fakeCLIOnPath(t *testing.T, binary string) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("fake cli binaries are shell scripts")
	}

	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, binary), []byte("#!/bin/sh\nexit 0\n"), 0o700) //nolint:gosec // a test stub must be executable
	if err != nil {
		t.Fatalf("writing fake %s: %v", binary, err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// emptyPath removes every directory from PATH, so no real CLI can be found.
func emptyPath(t *testing.T) {
	t.Helper()

	t.Setenv("PATH", t.TempDir())
}

// fakeProbeEndpoint serves the smallest valid chat completion, so a hosted
// fallback's preflight probe succeeds without a real provider. One response
// body for the package: a second copy drifted from this one immediately, and
// the next change to what a probe requires would only have reached one.
func fakeProbeEndpoint(t *testing.T) string {
	t.Helper()

	url, _ := togglableProbeEndpoint(t)

	return url
}

func TestPreflightCLIBinaryMissing(t *testing.T) {
	ResetProbeCache()
	emptyPath(t)

	cfg := &config.Config{
		Agents: []config.Agent{{Name: "reviewer", Source: config.AgentSource{Model: "@claude/sonnet"}}},
	}

	problems := Preflight(t.Context(), cfg, []string{"reviewer"}, &config.Preflight{})

	if len(problems) != 1 {
		t.Fatalf("problems = %+v, want one", problems)
	}

	if !strings.Contains(problems[0].Detail, "not on PATH") {
		t.Errorf("detail = %q, want it to name the missing binary", problems[0].Detail)
	}
}

func TestPreflightCLIBinaryPresent(t *testing.T) {
	ResetProbeCache()
	fakeCLIOnPath(t, "claude")

	cfg := &config.Config{
		Agents: []config.Agent{{Name: "reviewer", Source: config.AgentSource{Model: "@claude/sonnet"}}},
	}

	// A present binary is all a CLI probe asks. It deliberately does NOT
	// spawn the process: that would put a launch in the path of every
	// `steps web` poll, and a CLI that is installed but broken produces a
	// better error at the step than a probe could synthesize.
	if problems := Preflight(t.Context(), cfg, []string{"reviewer"}, &config.Preflight{}); len(problems) != 0 {
		t.Errorf("problems = %+v, want none", problems)
	}
}

// TestPreflightCLIFailsOverToHostedProvider is the case failover exists for,
// crossing the boundary this feature introduced: the CLI is not installed on
// this machine, so the step runs against the hosted fallback instead of
// failing the job.
func TestPreflightCLIFailsOverToHostedProvider(t *testing.T) {
	ResetProbeCache()
	emptyPath(t)
	t.Setenv("OPENAI_API_KEY", "test-key")

	fake := fakeProbeEndpoint(t)

	cfg := &config.Config{
		Agents: []config.Agent{{
			Name:   "reviewer",
			Source: config.AgentSource{Model: "@claude/sonnet"},
			Fallback: []config.AgentFallback{
				//nolint:gosec // an env var NAME, not a credential
				{Source: config.AgentSource{Model: "openai/gpt-4o", Endpoint: fake, APIKeyEnv: "OPENAI_API_KEY"}},
			},
		}},
	}

	if problems := Preflight(t.Context(), cfg, []string{"reviewer"}, &config.Preflight{}); len(problems) != 0 {
		t.Fatalf("problems = %+v, want none — the fallback should have absorbed the missing cli", problems)
	}

	selection, selected := selectedSource(testPin("reviewer"))
	if !selected {
		t.Fatal("no fallback was selected for an agent whose cli is missing")
	}

	if selection.source.Model != "openai/gpt-4o" {
		t.Errorf("selected source = %q, want the hosted fallback", selection.source.Model)
	}

	// The invocation the step will actually run must have left the CLI path
	// entirely — not merely changed model.
	primary, err := cfg.ResolveAgentInvocation(config.Step{Agent: "reviewer"})
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	running, err := primary.WithSource(selection.source, nil)
	if err != nil {
		t.Fatalf("WithSource: %v", err)
	}

	if running.CLI != "" || running.BaseURL == "" {
		t.Errorf("running invocation = {CLI: %q, BaseURL: %q}, want the hosted path", running.CLI, running.BaseURL)
	}
}

// TestPreflightCLICacheKeySeparatesImages: an image-less CLI target is
// answered by a PATH lookup and an image-bearing one by starting that image,
// so a shared cache key would let one answer stand in for the other.
func TestPreflightCLICacheKeySeparatesImages(t *testing.T) {
	host := config.ResolvedInvocation{CLI: "claude", ModelName: "sonnet"}
	containerized := config.ResolvedInvocation{CLI: "claude", ModelName: "sonnet", Image: "my/claude:1"}

	if cliProbeKey(host) == cliProbeKey(containerized) {
		t.Errorf("a containerized and a host cli target share cache key %q", cliProbeKey(host))
	}
}
