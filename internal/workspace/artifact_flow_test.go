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

	t.Run("dir naming a declared available artifact passes", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{Name: "j", Plan: []config.Step{
			{Get: "repo"},
			{Agent: "r", Dir: "repo/cmd", Inputs: config.Inputs("repo")},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Fatalf("err = %v, want nil (repo is available and declared; dir repo/cmd resolves to repo)", err)
		}
	})

	t.Run("dir naming an available but undeclared artifact errors", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{Name: "j", Plan: []config.Step{
			{Get: "repo"},
			{Agent: "r", Dir: "repo/cmd"},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil {
			t.Fatal("want an error: only declared artifacts are materialized, so an undeclared dir: is a missing directory at run time")
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

// TestAcrossFromFileArtifactMustBeAvailable checks an across: axis's
// from_file: by its first path component, exactly as dir: is — the runner
// reads it by materializing that artifact, so an axis pointing at something
// nothing produces would otherwise fail mid-run.
func TestAcrossFromFileArtifactMustBeAvailable(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}

	axis := func(from string) []config.AcrossVar {
		return []config.AcrossVar{{Var: "item", FromFile: from}}
	}

	t.Run("an earlier step's output passes", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{Name: "j", Plan: []config.Step{
			{Task: "scan", Run: "true", Inputs: config.Inputs(), Outputs: []string{"findings"}},
			{Across: axis("findings/items.json"), Task: "work", Run: "true", Inputs: config.Inputs("findings")},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Fatalf("err = %v, want nil (findings is produced before the matrix)", err)
		}
	})

	t.Run("a fetched resource passes", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{Name: "j", Plan: []config.Step{
			{Get: "repo"},
			{Across: axis("repo/matrix.json"), Task: "work", Run: "true", Inputs: config.Inputs("repo")},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Fatalf("err = %v, want nil (repo is fetched before the matrix)", err)
		}
	})

	t.Run("an artifact nothing produces errors", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{Name: "j", Plan: []config.Step{
			{Across: axis("nowhere/items.json"), Task: "work", Run: "true", Inputs: config.Inputs()},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil || !strings.Contains(err.Error(), "not a resource fetched or an output produced earlier") {
			t.Fatalf("err = %v, want a from_file-not-available error", err)
		}
	})

	t.Run("a LATER step's output errors", func(t *testing.T) {
		t.Parallel()

		// The file has to exist by the time the matrix expands, so producing it
		// afterwards is the same mistake as consuming any other artifact early.
		job := &config.Job{Name: "j", Plan: []config.Step{
			{Across: axis("findings/items.json"), Task: "work", Run: "true", Inputs: config.Inputs()},
			{Task: "scan", Run: "true", Inputs: config.Inputs(), Outputs: []string{"findings"}},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil || !strings.Contains(err.Error(), "not a resource fetched or an output produced earlier") {
			t.Fatalf("err = %v, want a from_file-not-available error", err)
		}
	})
}

// TestValidateArtifactFlowPromptFileArtifact checks a run-time prompt_file:
// {artifact, path}'s artifact against both the plan (fetched/produced
// somewhere) and the step's own declared inputs: (materialized into its
// working directory) — see checkPromptFileArtifactAvailable.
func TestValidateArtifactFlowPromptFileArtifact(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Agents: []config.Agent{{Name: "reviewer"}},
	}

	t.Run("artifact fetched and declared as an input passes", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{Name: "j", Plan: []config.Step{
			{Get: "repo"},
			{Agent: "reviewer", Inputs: config.Inputs("repo"), PromptFile: &config.FileRef{Artifact: "repo", Path: "PROMPT.md"}},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Fatalf("err = %v, want nil (repo is fetched and declared as an input)", err)
		}
	})

	t.Run("artifact never fetched errors", func(t *testing.T) {
		t.Parallel()

		// Inputs deliberately doesn't declare "repo": if it did,
		// checkInputsAvailable would reject it first (a plain undeclared-input
		// error), masking the prompt_file-specific check this case targets.
		job := &config.Job{Name: "j", Plan: []config.Step{
			{Agent: "reviewer", Inputs: config.Inputs(), PromptFile: &config.FileRef{Artifact: "repo", Path: "PROMPT.md"}},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil || !strings.Contains(err.Error(), "prompt_file artifact") {
			t.Fatalf("err = %v, want a prompt_file-artifact-not-available error", err)
		}
	})

	t.Run("artifact fetched but not declared as an input errors", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{Name: "j", Plan: []config.Step{
			{Get: "repo"},
			{Agent: "reviewer", Inputs: config.Inputs(), PromptFile: &config.FileRef{Artifact: "repo", Path: "PROMPT.md"}},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil || !strings.Contains(err.Error(), "must also be declared in this step's inputs") {
			t.Fatalf("err = %v, want a must-be-declared error", err)
		}
	})

	t.Run("load-time scalar form names no artifact and is unaffected", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{Name: "j", Plan: []config.Step{
			{Agent: "reviewer", Inputs: config.Inputs(), PromptFile: &config.FileRef{Path: "prompts/review.md"}},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Fatalf("err = %v, want nil (a load-time prompt_file: names no artifact)", err)
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

// TestValidateArtifactFlowThroughTry checks that a try: wrapper is transparent
// to artifact flow in both directions. It used to fall into the switch's
// default and return nil, which made a wrapped producer invisible: the very
// next step naming its output failed static validation — before anything ran —
// with the misleading "nothing produces bin", and a wrapped step's own bogus
// inputs: went unchecked.
func TestValidateArtifactFlowThroughTry(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}

	t.Run("a wrapped task publishes its outputs", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{
			Name: "j",
			Plan: []config.Step{
				{Try: &config.Step{Task: "build", Run: "true", Outputs: []string{"bin"}}},
				{Task: "deploy", Run: "true", Inputs: config.Inputs("bin")},
			},
		}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Fatalf("err = %v, want nil (the wrapped build produces bin)", err)
		}
	})

	t.Run("a wrapped task's undeclared input is still caught", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{
			Name: "j",
			Plan: []config.Step{
				{Try: &config.Step{Task: "build", Run: "true", Inputs: config.Inputs("missing")}},
			},
		}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil || !strings.Contains(err.Error(), "not a resource fetched") {
			t.Fatalf("err = %v, want an undeclared-input error", err)
		}
	})
}

// TestValidateArtifactFlowTryWrappedHook covers a gap the kindswitch analyzer
// found: validateHookArtifactFlow dispatched on task/put/agent only, so a
// try:-wrapped hook — which is a legal hook body — matched no case, was left
// with an empty input list, and had its wrapped step's inputs: checked against
// nothing at all.
func TestValidateArtifactFlowTryWrappedHook(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}

	t.Run("undeclared input inside a try: hook is caught", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{
			Name: "j",
			Plan: []config.Step{{
				Task: "work",
				Run:  "true",
				Hooks: config.Hooks{
					OnFailure: &config.Step{
						Try: &config.Step{Task: "notify", Run: "true", Inputs: config.Inputs("missing")},
					},
				},
			}},
		}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil || !strings.Contains(err.Error(), "not available to this hook") {
			t.Fatalf("err = %v, want the wrapped hook's undeclared input reported", err)
		}
	})

	t.Run("an available input inside a try: hook still passes", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{
			Name: "j",
			Plan: []config.Step{
				{Get: "repo"},
				{
					Task: "work",
					Run:  "true",
					Hooks: config.Hooks{
						OnFailure: &config.Step{
							Try: &config.Step{Task: "notify", Run: "true", Inputs: config.Inputs("repo")},
						},
					},
				},
			},
		}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Fatalf("err = %v, want nil (repo is fetched before the step the hook hangs off)", err)
		}
	})
}

// TestValidateArtifactFlowLoadVar pins that a load_var: step is checked like
// every other consuming kind. It reads a file out of a directory materialized
// from its OWN inputs, so a bare `file: version.txt` — the spelling every
// pipeline used when a single shared directory held everything — names
// nothing that can exist, and must fail at plan time rather than mid-run.
func TestValidateArtifactFlowLoadVar(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}

	producer := config.Step{Task: "pick-tag", Run: "true", Outputs: []string{"meta"}}

	t.Run("file inside a declared input passes", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{Name: "j", Plan: []config.Step{
			producer,
			{LoadVar: "tag", VarFile: "meta/version.txt", Inputs: config.Inputs("meta")},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Fatalf("err = %v, want nil (meta is produced and declared)", err)
		}
	})

	t.Run("a bare file name is refused", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{Name: "j", Plan: []config.Step{
			producer,
			{LoadVar: "tag", VarFile: "version.txt"},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil {
			t.Fatal("want an error: nothing but declared artifacts is materialized at the root of a step's directory")
		}
	})

	t.Run("a file in an undeclared artifact is refused", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{Name: "j", Plan: []config.Step{
			producer,
			{LoadVar: "tag", VarFile: "meta/version.txt"},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil {
			t.Fatal("want an error: meta exists in the plan but this step does not declare it")
		}
	})

	t.Run("an input nothing produced is refused", func(t *testing.T) {
		t.Parallel()

		job := &config.Job{Name: "j", Plan: []config.Step{
			{LoadVar: "tag", VarFile: "meta/version.txt", Inputs: config.Inputs("meta")},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil {
			t.Fatal("want an error: no step produced meta")
		}
	})
}
