package config

import (
	"strings"
	"testing"
)

func TestValidateSandbox(t *testing.T) {
	t.Parallel()

	fenced := Task{Name: "build", Run: "true", Sandbox: true}
	fencedType := ResourceType{Name: "git", Sandbox: true}

	cases := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"host task", &Config{Tasks: []Task{fenced}, Jobs: []Job{{Name: "j", Plan: []Step{{Task: "build"}}}}}, ""},
		{"task beside its own image", &Config{Tasks: []Task{{Name: "build", Sandbox: true, Image: "alpine"}}}, "image"},
		{"agent beside its own image", &Config{Agents: []Agent{{Name: "a", Sandbox: true, Image: "alpine"}}}, "image"},
		{"resource_type beside its own image", &Config{ResourceTypes: []ResourceType{{Name: "rt", Sandbox: true, Image: "alpine"}}}, "image"},
		{
			"step image overriding a sandboxed task",
			&Config{Tasks: []Task{fenced}, Jobs: []Job{{Name: "j", Plan: []Step{{Task: "build", Image: "alpine"}}}}},
			"image",
		},
		{
			"step placed on a worker",
			&Config{Tasks: []Task{fenced}, Jobs: []Job{{Name: "j", Plan: []Step{{Task: "build", Tags: []string{"gpu"}}}}}},
			"tags",
		},
		{
			"whole job placed on a worker",
			&Config{Tasks: []Task{fenced}, Jobs: []Job{{Name: "j", Tags: []string{"gpu"}, Plan: []Step{{Task: "build"}}}}},
			"tags",
		},
		{
			"get through a tagged resource of a sandboxed type",
			&Config{
				ResourceTypes: []ResourceType{fencedType},
				Resources:     []Resource{{Name: "repo", Type: "git", Tags: []string{"lan"}}},
				Jobs:          []Job{{Name: "j", Plan: []Step{{Get: "repo"}}}},
			},
			"tags",
		},
		{
			"inline run under a tagged job is not sandboxed, so placing it is fine",
			&Config{Tasks: []Task{fenced}, Jobs: []Job{{Name: "j", Tags: []string{"gpu"}, Plan: []Step{{Task: "build", Run: "true"}}}}},
			"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.cfg.validateSandbox()

			switch {
			case tc.want == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestAFixAgentCannotAskForItsOwnSandbox(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Agents: []Agent{{Name: "fixer", Source: AgentSource{Model: "openai/gpt-4o"}, Sandbox: true}},
		Tasks:  []Task{{Name: "build", Run: "false", Fix: &FixSpec{Agent: "fixer"}}},
	}

	err := cfg.validateFixAgentImages()
	if err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Errorf("err = %v: a fix agent runs under the task's fence, so its own sandbox: true would be a promise nothing keeps", err)
	}
}

// network: none is the one network a sandbox can give a host command, and it must be legal whether the entry or the step referencing it spells it.
func TestNetworkNoneIsLegalOnASandboxedHostCommand(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cfg     *Config
		wantErr bool
	}{
		{"entry spells none", &Config{Tasks: []Task{{Name: "b", Sandbox: true, Network: "none"}}}, false},
		{"entry spells bridge", &Config{Tasks: []Task{{Name: "b", Sandbox: true, Network: "bridge"}}}, true},
		{"agent spells none", &Config{Agents: []Agent{{Name: "a", Sandbox: true, Network: "none"}}}, false},
		{"resource_type spells none", &Config{ResourceTypes: []ResourceType{{Name: "rt", Sandbox: true, Network: "none"}}}, false},
		{
			"step spells none over a sandboxed task",
			&Config{Tasks: []Task{{Name: "b", Run: "true", Sandbox: true}}, Jobs: []Job{{Name: "j", Plan: []Step{{Task: "b", Network: "none"}}}}},
			false,
		},
		{
			"step spells none over an unsandboxed task",
			&Config{Tasks: []Task{{Name: "b", Run: "true"}}, Jobs: []Job{{Name: "j", Plan: []Step{{Task: "b", Network: "none"}}}}},
			true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.cfg.validateNetworkNeedsImage()
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestResolvedSandboxReachesTheRunner(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Tasks:  []Task{{Name: "build", Run: "true", Sandbox: true}},
		Agents: []Agent{{Name: "a", Source: AgentSource{Model: "openai/gpt-4o"}, Sandbox: true}},
	}

	rt, err := cfg.ResolveTask(Step{Task: "build"})
	if err != nil || !rt.Sandbox {
		t.Errorf("ResolveTask = %+v, %v: want Sandbox from the tasks: entry", rt.Sandbox, err)
	}

	ri, err := cfg.ResolveAgentInvocation(Step{Agent: "a", Messages: []string{"x"}})
	if err != nil || !ri.Sandbox {
		t.Errorf("ResolveAgentInvocation = %+v, %v: want Sandbox from the agents: entry", ri.Sandbox, err)
	}
}
