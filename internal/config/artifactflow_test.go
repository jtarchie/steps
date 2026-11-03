package config

import (
	"strings"
	"testing"
)

// TestValidateArtifactFlowRunsWithoutWorkspace confirms flow validation is
// always-on: even without a workspace: block, a step declaring an input
// nothing produced is caught — the "this job never fetched anything" mistake.
func TestValidateArtifactFlowRunsWithoutWorkspace(t *testing.T) {
	t.Parallel()

	cfg := &Config{} // no workspace: block

	job := &Job{
		Name: "j",
		Plan: []Step{
			{Task: "work", Run: "true", Inputs: Inputs("missing")},
		},
	}

	err := ValidateArtifactFlow(cfg, job)
	if err == nil || !strings.Contains(err.Error(), "not a resource fetched") {
		t.Fatalf("err = %v, want an undeclared-input error", err)
	}
}

func TestValidateArtifactFlowDeclaredProducer(t *testing.T) {
	t.Parallel()

	cfg := &Config{}

	job := &Job{
		Name: "j",
		Plan: []Step{
			{Get: "repo"},
			{Task: "work", Run: "true", Inputs: Inputs("repo")},
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

	cfg := &Config{
		Agents: []Agent{{Name: "r", Tools: []ToolSpec{{Builtin: "read_file"}}}},
	}

	t.Run("dir naming a declared available artifact passes", func(t *testing.T) {
		t.Parallel()

		job := &Job{Name: "j", Plan: []Step{
			{Get: "repo"},
			{Agent: "r", Dir: "repo/cmd", Inputs: Inputs("repo")},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Fatalf("err = %v, want nil (repo is available and declared; dir repo/cmd resolves to repo)", err)
		}
	})

	t.Run("dir naming an available but undeclared artifact errors", func(t *testing.T) {
		t.Parallel()

		job := &Job{Name: "j", Plan: []Step{
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

		job := &Job{Name: "j", Plan: []Step{
			{Agent: "r", Dir: "repo"},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil || !strings.Contains(err.Error(), "which is not a resource fetched") {
			t.Fatalf("err = %v, want a dir-not-available error", err)
		}
	})

	// As in Concourse's run.dir: an output exists, empty, before the step runs.
	t.Run("dir naming the step's own fresh output passes", func(t *testing.T) {
		t.Parallel()

		job := &Job{Name: "j", Plan: []Step{
			{Agent: "r", Dir: "report", Outputs: []string{"report"}},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Fatalf("err = %v, want nil (report is created empty for the step)", err)
		}
	})

	t.Run("dir naming neither an available artifact nor the step's own output errors", func(t *testing.T) {
		t.Parallel()

		job := &Job{Name: "j", Plan: []Step{
			{Agent: "r", Dir: "report", Outputs: []string{"other"}},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil || !strings.Contains(err.Error(), `dir "report"`) {
			t.Fatalf("err = %v, want the dir refused", err)
		}
	})
}

// TestAcrossFromFileArtifactMustBeAvailable checks an across: axis's
// from_file: by its first path component, exactly as dir: is — the runner
// reads it by materializing that artifact, so an axis pointing at something
// nothing produces would otherwise fail mid-run.
func TestAcrossFromFileArtifactMustBeAvailable(t *testing.T) {
	t.Parallel()

	cfg := &Config{}

	axis := func(from string) []AcrossVar {
		return []AcrossVar{{Var: "item", FromFile: from}}
	}

	t.Run("an earlier step's output passes", func(t *testing.T) {
		t.Parallel()

		job := &Job{Name: "j", Plan: []Step{
			{Task: "scan", Run: "true", Inputs: Inputs(), Outputs: []string{"findings"}},
			{Across: axis("findings/items.json"), Task: "work", Run: "true", Inputs: Inputs("findings")},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Fatalf("err = %v, want nil (findings is produced before the matrix)", err)
		}
	})

	t.Run("a fetched resource passes", func(t *testing.T) {
		t.Parallel()

		job := &Job{Name: "j", Plan: []Step{
			{Get: "repo"},
			{Across: axis("repo/matrix.json"), Task: "work", Run: "true", Inputs: Inputs("repo")},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Fatalf("err = %v, want nil (repo is fetched before the matrix)", err)
		}
	})

	t.Run("an artifact nothing produces errors", func(t *testing.T) {
		t.Parallel()

		job := &Job{Name: "j", Plan: []Step{
			{Across: axis("nowhere/items.json"), Task: "work", Run: "true", Inputs: Inputs()},
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
		job := &Job{Name: "j", Plan: []Step{
			{Across: axis("findings/items.json"), Task: "work", Run: "true", Inputs: Inputs()},
			{Task: "scan", Run: "true", Inputs: Inputs(), Outputs: []string{"findings"}},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil || !strings.Contains(err.Error(), "not a resource fetched or an output produced earlier") {
			t.Fatalf("err = %v, want a from_file-not-available error", err)
		}
	})
}

// TestValidateArtifactFlowPromptFileArtifact checks a run-time message_files:
// {artifact, path}'s artifact against both the plan (fetched/produced
// somewhere) and the step's own declared inputs: (materialized into its
// working directory) — see checkPromptFileArtifactAvailable.
func TestValidateArtifactFlowPromptFileArtifact(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Agents: []Agent{{Name: "reviewer"}},
	}

	t.Run("artifact fetched and declared as an input passes", func(t *testing.T) {
		t.Parallel()

		job := &Job{Name: "j", Plan: []Step{
			{Get: "repo"},
			{Agent: "reviewer", Inputs: Inputs("repo"), MessageFiles: []*FileRef{{Artifact: "repo", Path: "PROMPT.md"}}},
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
		// error), masking the message_files-specific check this case targets.
		job := &Job{Name: "j", Plan: []Step{
			{Agent: "reviewer", Inputs: Inputs(), MessageFiles: []*FileRef{{Artifact: "repo", Path: "PROMPT.md"}}},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil || !strings.Contains(err.Error(), "message_files artifact") {
			t.Fatalf("err = %v, want a message_files-artifact-not-available error", err)
		}
	})

	t.Run("artifact fetched but not declared as an input errors", func(t *testing.T) {
		t.Parallel()

		job := &Job{Name: "j", Plan: []Step{
			{Get: "repo"},
			{Agent: "reviewer", Inputs: Inputs(), MessageFiles: []*FileRef{{Artifact: "repo", Path: "PROMPT.md"}}},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil || !strings.Contains(err.Error(), "must also be declared in this step's inputs") {
			t.Fatalf("err = %v, want a must-be-declared error", err)
		}
	})

	t.Run("load-time scalar form names no artifact and is unaffected", func(t *testing.T) {
		t.Parallel()

		job := &Job{Name: "j", Plan: []Step{
			{Agent: "reviewer", Inputs: Inputs(), MessageFiles: []*FileRef{&FileRef{Path: "prompts/review.md"}}},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Fatalf("err = %v, want nil (a load-time message_files: names no artifact)", err)
		}
	})
}

// An agent hook reads a run-time message_files: artifact out of its own materialized directory, so it gets the same two checks a plan step does.
func TestValidateArtifactFlowHookMessageFiles(t *testing.T) {
	t.Parallel()

	cfg := &Config{Agents: []Agent{{Name: "reviewer"}}}

	hooked := func(hook Step) *Job {
		return &Job{Name: "j", Plan: []Step{
			{Get: "repo"},
			{Task: "work", Run: "true", Hooks: Hooks{OnFailure: &hook}},
		}}
	}

	prompt := func(artifact string) []*FileRef {
		return []*FileRef{{Artifact: artifact, Path: "PROMPT.md"}}
	}

	t.Run("an available artifact declared in the hook's inputs passes", func(t *testing.T) {
		t.Parallel()

		err := ValidateArtifactFlow(cfg, hooked(Step{Agent: "reviewer", Inputs: Inputs("repo"), MessageFiles: prompt("repo")}))
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
	})

	t.Run("an artifact the hook cannot see errors", func(t *testing.T) {
		t.Parallel()

		err := ValidateArtifactFlow(cfg, hooked(Step{Agent: "reviewer", Inputs: Inputs(), MessageFiles: prompt("missing")}))
		if err == nil || !strings.Contains(err.Error(), "is not available to this hook") {
			t.Fatalf("err = %v, want a message_files-not-available error", err)
		}
	})

	t.Run("an available artifact missing from the hook's inputs errors", func(t *testing.T) {
		t.Parallel()

		err := ValidateArtifactFlow(cfg, hooked(Step{Agent: "reviewer", Inputs: Inputs(), MessageFiles: prompt("repo")}))
		if err == nil || !strings.Contains(err.Error(), "must also be declared in this hook's inputs") {
			t.Fatalf("err = %v, want a must-be-declared error", err)
		}
	})
}

func TestValidateArtifactFlowHookDir(t *testing.T) {
	t.Parallel()

	cfg := &Config{Agents: []Agent{{Name: "r"}}}

	cases := map[string]struct {
		hook    Step
		wantErr string
	}{
		"an available artifact declared in the hook's inputs passes": {
			hook: Step{Agent: "r", Dir: "repo/cmd", Inputs: Inputs("repo")},
		},
		"the hook's own fresh output passes": {
			hook: Step{Agent: "r", Dir: "notes", Outputs: []string{"notes"}},
		},
		"a dir nothing fetched errors": {
			hook:    Step{Agent: "r", Dir: "nowhere"},
			wantErr: `(on_failure hook agent "r"): dir "nowhere" names "nowhere", which is not a resource fetched`,
		},
		"an available but undeclared artifact errors": {
			hook:    Step{Agent: "r", Dir: "repo"},
			wantErr: "which the step does not declare",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			job := &Job{Name: "j", Plan: []Step{
				{Get: "repo"},
				{Task: "work", Run: "true", Hooks: Hooks{OnFailure: &tc.hook}},
			}}

			err := ValidateArtifactFlow(cfg, job)

			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// The declared name is only what the task sees on disk; availability and publishing go by the MAPPED name.
func TestValidateArtifactFlowTaskMappings(t *testing.T) {
	t.Parallel()

	job := &Job{Name: "j", Plan: []Step{
		{Get: "repo"},
		{
			Task: "build", Run: "true",
			Inputs: Inputs("src"), InputMapping: map[string]string{"src": "repo"},
			Outputs: []string{"bin"}, OutputMapping: map[string]string{"bin": "release"},
		},
		{Task: "ship", Run: "true", Inputs: Inputs("release")},
	}}

	err := ValidateArtifactFlow(&Config{}, job)
	if err != nil {
		t.Fatalf("err = %v, want nil (src reads repo, bin publishes release)", err)
	}
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

	cfg := &Config{}

	t.Run("a wrapped task publishes its outputs", func(t *testing.T) {
		t.Parallel()

		job := &Job{
			Name: "j",
			Plan: []Step{
				{Try: &Step{Task: "build", Run: "true", Outputs: []string{"bin"}}},
				{Task: "deploy", Run: "true", Inputs: Inputs("bin")},
			},
		}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Fatalf("err = %v, want nil (the wrapped build produces bin)", err)
		}
	})

	t.Run("a wrapped task's undeclared input is still caught", func(t *testing.T) {
		t.Parallel()

		job := &Job{
			Name: "j",
			Plan: []Step{
				{Try: &Step{Task: "build", Run: "true", Inputs: Inputs("missing")}},
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

	cfg := &Config{}

	t.Run("undeclared input inside a try: hook is caught", func(t *testing.T) {
		t.Parallel()

		job := &Job{
			Name: "j",
			Plan: []Step{{
				Task: "work",
				Run:  "true",
				Hooks: Hooks{
					OnFailure: &Step{
						Try: &Step{Task: "notify", Run: "true", Inputs: Inputs("missing")},
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

		job := &Job{
			Name: "j",
			Plan: []Step{
				{Get: "repo"},
				{
					Task: "work",
					Run:  "true",
					Hooks: Hooks{
						OnFailure: &Step{
							Try: &Step{Task: "notify", Run: "true", Inputs: Inputs("repo")},
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

	cfg := &Config{}

	producer := Step{Task: "pick-tag", Run: "true", Outputs: []string{"meta"}}

	t.Run("file inside a declared input passes", func(t *testing.T) {
		t.Parallel()

		job := &Job{Name: "j", Plan: []Step{
			producer,
			{LoadVar: "tag", VarFile: "meta/version.txt", Inputs: Inputs("meta")},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Fatalf("err = %v, want nil (meta is produced and declared)", err)
		}
	})

	t.Run("a bare file name is refused", func(t *testing.T) {
		t.Parallel()

		job := &Job{Name: "j", Plan: []Step{
			producer,
			{LoadVar: "tag", VarFile: "version.txt"},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil || !strings.Contains(err.Error(), "is not inside a declared input") {
			t.Fatalf("err = %v, want the not-inside-a-declared-input error: nothing but declared artifacts is materialized at the root of a step's directory", err)
		}
	})

	t.Run("a file in an artifact other than the declared one is refused", func(t *testing.T) {
		t.Parallel()

		job := &Job{Name: "j", Plan: []Step{
			producer,
			{Get: "repo"},
			{LoadVar: "tag", VarFile: "meta/version.txt", Inputs: Inputs("repo")},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil || !strings.Contains(err.Error(), `names artifact "meta"`) {
			t.Fatalf("err = %v, want the error naming the undeclared artifact meta", err)
		}
	})

	t.Run("a file in an undeclared artifact is refused", func(t *testing.T) {
		t.Parallel()

		job := &Job{Name: "j", Plan: []Step{
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

		job := &Job{Name: "j", Plan: []Step{
			{LoadVar: "tag", VarFile: "meta/version.txt", Inputs: Inputs("meta")},
		}}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil {
			t.Fatal("want an error: no step produced meta")
		}
	})
}

// TestValidateArtifactFlowArtifactImage holds an image: naming an artifact to
// the rule an input is held to: an earlier get must fetch it.
func TestValidateArtifactFlowArtifactImage(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		plan []Step
		want string
	}{
		"before its get": {
			plan: []Step{{Task: "build", Run: "true", Image: "toolchain"}, {Get: "toolchain"}},
			want: `image "toolchain"`,
		},
		"after its get": {
			plan: []Step{{Get: "toolchain"}, {Task: "build", Run: "true", Image: "toolchain"}},
		},
		"an in_parallel sibling": {
			plan: []Step{{InParallel: &InParallel{Steps: []Step{
				{Get: "toolchain"},
				{Agent: "a", Image: "toolchain"},
			}}}},
			want: `image "toolchain"`,
		},
		"a step hook after the get": {
			plan: []Step{{Get: "toolchain", Hooks: Hooks{
				OnSuccess: &Step{Task: "note", Run: "true", Image: "toolchain"},
			}}},
		},
		"a step hook before the get": {
			plan: []Step{{Task: "t", Run: "true", Hooks: Hooks{
				OnFailure: &Step{Task: "note", Run: "true", Image: "toolchain"},
			}}},
			want: `image "toolchain"`,
		},
		"an earlier task's output of the same name": {
			plan: []Step{
				{Task: "make", Run: "true", Outputs: []string{"toolchain"}},
				{Task: "build", Run: "true", Image: "toolchain"},
			},
			want: `image "toolchain"`,
		},
		"an output shadowing a get": {
			plan: []Step{
				{Get: "toolchain"},
				{Task: "make", Run: "true", Outputs: []string{"toolchain"}},
				{Task: "build", Run: "true", Image: "toolchain"},
			},
		},
		"a resource never fetched": {
			plan: []Step{{Task: "build", Run: "true", Image: "toolchain"}},
			want: "rename the resource",
		},
		"an image that is not an artifact": {
			plan: []Step{{Task: "build", Run: "true", Image: "alpine:3"}},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			job := Job{Name: "j", Plan: tc.plan}
			cfg := &Config{
				Resources: []Resource{{Name: "toolchain", Type: "registry-image"}},
				Jobs:      []Job{job},
			}

			err := ValidateArtifactFlow(cfg, &job)

			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("err = %v, want nil", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateArtifactFlowUnknownInputErrors(t *testing.T) {
	t.Parallel()

	cfg := &Config{Workspace: &WorkspaceConfig{Strategy: "copy"}}
	job := &Job{
		Name: "build",
		Plan: []Step{
			{Task: "unit", Run: "true", Inputs: Inputs("repo")},
		},
	}

	err := ValidateArtifactFlow(cfg, job)
	if err == nil {
		t.Fatal("expected an error for a task input naming nothing fetched or produced earlier")
	}
}

func TestValidateArtifactFlowChainedOutputsResolve(t *testing.T) {
	t.Parallel()

	cfg := &Config{Workspace: &WorkspaceConfig{Strategy: "copy"}}
	job := &Job{
		Name: "build",
		Plan: []Step{
			{Get: "repo"},
			{Task: "build", Run: "true", Inputs: Inputs("repo"), Outputs: []string{"built"}},
			{Task: "publish", Run: "true", Inputs: Inputs("built")},
			{Put: "results", Inputs: Inputs("built")},
		},
	}

	err := ValidateArtifactFlow(cfg, job)
	if err != nil {
		t.Errorf("expected the plan's declared inputs/outputs to resolve cleanly, got %v", err)
	}
}

// TestValidateArtifactFlowGetsAccumulateInSharedWorkspace verifies that
// consecutive get steps accumulate their resources in the same build workspace,
// so an earlier get's resource (or output from a task step between them) is
// visible to later steps. This matches Concourse semantics: within a single
// build, every get fetches into the shared artifact store.
func TestValidateArtifactFlowGetsAccumulateInSharedWorkspace(t *testing.T) {
	t.Parallel()

	cfg := &Config{Workspace: &WorkspaceConfig{Strategy: "copy"}}

	t.Run("task output before a later get is visible", func(t *testing.T) {
		t.Parallel()

		job := &Job{
			Name: "build",
			Plan: []Step{
				{Get: "repo"},
				{Task: "build", Run: "true", Inputs: Inputs("repo"), Outputs: []string{"built"}},
				{Get: "other"},
				{Task: "test", Run: "true", Inputs: Inputs("built")},
			},
		}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Errorf("expected `built` (produced before `get other`) to be visible in the shared workspace, got %v", err)
		}
	})

	t.Run("an earlier get's resource is visible after a later get", func(t *testing.T) {
		t.Parallel()

		job := &Job{
			Name: "build",
			Plan: []Step{
				{Get: "repo"},
				{Get: "other"},
				{Task: "test", Run: "true", Inputs: Inputs("repo")},
			},
		}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Errorf("expected `repo` (fetched before `get other`) to be visible in the shared workspace, got %v", err)
		}
	})

	t.Run("every get's resource is available after consecutive gets", func(t *testing.T) {
		t.Parallel()

		job := &Job{
			Name: "build",
			Plan: []Step{
				{Get: "repo"},
				{Get: "other"},
				{Task: "test", Run: "true", Inputs: Inputs("repo", "other")},
			},
		}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Errorf("expected both `repo` and `other` to be visible in the shared workspace, got %v", err)
		}
	})
}

// TestValidateArtifactFlowStepHooks checks that a step hook's declared inputs
// are validated against the artifact view it will actually see: on_success
// sees the step's own outputs, failure-path hooks do not.
func TestValidateArtifactFlowStepHooks(t *testing.T) {
	t.Parallel()

	cfg := &Config{Workspace: &WorkspaceConfig{Strategy: "copy"}}

	t.Run("on_success hook may consume the step's own output", func(t *testing.T) {
		t.Parallel()

		job := &Job{
			Name: "build",
			Plan: []Step{
				{Get: "repo"},
				{
					Task: "build", Run: "true", Inputs: Inputs("repo"), Outputs: []string{"built"},
					Hooks: Hooks{OnSuccess: &Step{Put: "results", Inputs: Inputs("built")}},
				},
			},
		}

		err := ValidateArtifactFlow(cfg, job)
		if err != nil {
			t.Errorf("on_success hook consuming the step's output should validate, got %v", err)
		}
	})

	t.Run("ensure hook may not consume the step's own output", func(t *testing.T) {
		t.Parallel()

		job := &Job{
			Name: "build",
			Plan: []Step{
				{Get: "repo"},
				{
					Task: "build", Run: "true", Inputs: Inputs("repo"), Outputs: []string{"built"},
					Hooks: Hooks{Ensure: &Step{Put: "results", Inputs: Inputs("built")}},
				},
			},
		}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil {
			t.Fatal("expected an error: ensure runs on the failure path, before `built` is guaranteed to exist")
		}
	})

	t.Run("a hook output is not visible to a later plan step", func(t *testing.T) {
		t.Parallel()

		job := &Job{
			Name: "build",
			Plan: []Step{
				{Get: "repo"},
				{
					Task: "build", Run: "true", Inputs: Inputs("repo"),
					Hooks: Hooks{OnSuccess: &Step{Task: "gen", Run: "true", Outputs: []string{"extra"}}},
				},
				{Task: "consume", Run: "true", Inputs: Inputs("extra")},
			},
		}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil {
			t.Fatal("expected an error: a conditional hook's output must not satisfy a later plan step's input")
		}
	})

	t.Run("a hook's own nested hook is checked against the same view", func(t *testing.T) {
		t.Parallel()

		job := &Job{
			Name: "build",
			Plan: []Step{{
				Task: "build", Run: "true",
				Hooks: Hooks{OnFailure: &Step{
					Task: "notify", Run: "true",
					Hooks: Hooks{Ensure: &Step{Task: "cleanup", Run: "true", Inputs: Inputs("missing")}},
				}},
			}},
		}

		err := ValidateArtifactFlow(cfg, job)
		if err == nil || !strings.Contains(err.Error(), "on_failure.ensure hook") {
			t.Fatalf("err = %v, want the nested hook's undeclared input reported", err)
		}
	})
}

func TestValidateArtifactFlowNoOpWithoutWorkspace(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	job := &Job{
		Name: "build",
		Plan: []Step{
			{Task: "unit", Run: "true"},
		},
	}

	err := ValidateArtifactFlow(cfg, job)
	if err != nil {
		t.Errorf("validateArtifactFlow with no workspace: block should always be a no-op, got %v", err)
	}
}

// TestValidateArtifactFlowPutProducesNothing pins that a put is
// artifact-neutral: it publishes and fetches nothing (the implicit get was
// removed in the DSL audit — every artifact in the build is one a step
// declared). A later step naming the put as an input has to fail HERE —
// before anything runs — rather than at run time against a directory nobody
// created; the valid spelling is an explicit get: after the put.
func TestValidateArtifactFlowPutProducesNothing(t *testing.T) {
	t.Parallel()

	cfg := &Config{Workspace: &WorkspaceConfig{Strategy: "copy"}}
	job := &Job{
		Name: "build",
		Plan: []Step{
			{Put: "release"},
			{Task: "verify", Run: "true", Inputs: Inputs("release")},
		},
	}

	err := ValidateArtifactFlow(cfg, job)
	if err == nil {
		t.Fatal("expected an error: a put produces no artifact, so the task's input never exists")
	}
}

// TestValidateArtifactFlowExplicitGetAfterPut is the opt-in spelling: the get
// step after the put is what makes the artifact exist.
func TestValidateArtifactFlowExplicitGetAfterPut(t *testing.T) {
	t.Parallel()

	cfg := &Config{Workspace: &WorkspaceConfig{Strategy: "copy"}}
	job := &Job{
		Name: "build",
		Plan: []Step{
			{Put: "release"},
			{Get: "release"},
			{Task: "verify", Run: "true", Inputs: Inputs("release")},
		},
	}

	err := ValidateArtifactFlow(cfg, job)
	if err != nil {
		t.Errorf("an explicit get after the put must satisfy the task's input, got %v", err)
	}
}

// TestValidateArtifactFlowPutHooksSeeNoPutArtifact pins that a put's hooks
// cannot name the put as an input either — a put produces no artifact on any
// path, success or failure.
func TestValidateArtifactFlowPutHooksSeeNoPutArtifact(t *testing.T) {
	t.Parallel()

	cfg := &Config{Workspace: &WorkspaceConfig{Strategy: "copy"}}
	job := &Job{
		Name: "build",
		Plan: []Step{
			{
				Put: "release",
				Hooks: Hooks{
					OnFailure: &Step{Task: "cleanup", Run: "true", Inputs: Inputs("release")},
				},
			},
		},
	}

	err := ValidateArtifactFlow(cfg, job)
	if err == nil {
		t.Fatal("expected an error: a put produces no artifact, so its hook cannot declare it as an input")
	}
}
