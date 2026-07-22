package workspace

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestValidateArtifactFlowRunsWithoutWorkspace confirms flow validation is
// always-on: even without a workspace: block, a step declaring an input
// nothing produced is caught — the "this job never fetched anything" mistake.
func TestValidateArtifactFlowRunsWithoutWorkspace(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{} // no workspace: block

	job := &config.Job{
		Name: "j",
		Plan: []config.Step{
			{Task: "work", Run: "true", Inputs: config.Inputs("missing")},
		},
	}

	err := ValidateArtifactFlow(cfg, job)
	if err == nil || !strings.Contains(err.Error(), "not a resource fetched") {
		t.Fatalf("err = %v, want an undeclared-input error", err)
	}
}

func TestValidateArtifactFlowDeclaredProducer(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}

	job := &config.Job{
		Name: "j",
		Plan: []config.Step{
			{Get: "repo"},
			{Task: "work", Run: "true", Inputs: config.Inputs("repo")},
		},
	}

	err := ValidateArtifactFlow(cfg, job)
	if err != nil {
		t.Fatalf("err = %v, want nil (repo is fetched before the task)", err)
	}
}

// TestValidateArtifactFlowDir checks an agent step's dir: is validated by its
// first path component.
func TestValidateArtifactFlowDir(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Agents: []config.Agent{{Name: "r", Tools: []config.ToolSpec{{Builtin: "read_file"}}}},
	}

	t.Run("dir naming an available artifact passes", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{Name: "j", Plan: []config.Step{
			{Get: "repo"},
			{Agent: "r", Dir: "repo/cmd"},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Fatalf("err = %v, want nil (repo is available; dir repo/cmd resolves to repo)", err)
		}
	})

	t.Run("dir naming nothing fetched errors", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{Name: "j", Plan: []config.Step{
			{Agent: "r", Dir: "repo"},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil || !strings.Contains(err.Error(), "dir") {
			t.Fatalf("err = %v, want a dir-not-available error", err)
		}
	})
}

func TestFirstPathComponent(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"repo":       "repo",
		"repo/cmd":   "repo",
		"repo/a/b/c": "repo",
		".":          ".",
		"./repo":     "repo",
	}

	for in, want := range cases {
		if got := firstPathComponent(in); got != want {
			t.Errorf("firstPathComponent(%q) = %q, want %q", in, got, want)
		}
	}
}
