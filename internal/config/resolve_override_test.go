package config

import "testing"

func TestResolveTaskInheritsAndOverridesLimitsAndTimeout(t *testing.T) {
	t.Parallel()

	taskLimits := &ContainerLimits{CPU: 512}
	stepLimits := &ContainerLimits{CPU: 1024}
	cfg := &Config{Tasks: []Task{{Name: "build", Run: "true", Limits: taskLimits, Timeout: "1m"}}}

	rt, err := cfg.ResolveTask(Step{Task: "build"})
	if err != nil {
		t.Fatalf("ResolveTask: %v", err)
	}

	if rt.Limits != taskLimits || rt.Timeout != "1m" {
		t.Errorf("Limits/Timeout = %v/%q, want the tasks: entry's", rt.Limits, rt.Timeout)
	}

	rt, err = cfg.ResolveTask(Step{Task: "build", Limits: stepLimits, Timeout: "5s"})
	if err != nil {
		t.Fatalf("ResolveTask: %v", err)
	}

	if rt.Limits != stepLimits || rt.Timeout != "5s" {
		t.Errorf("Limits/Timeout = %v/%q, want the step's overrides", rt.Limits, rt.Timeout)
	}
}

func TestResolveAgentRuntimeInheritsAndOverridesUserAndLimits(t *testing.T) {
	t.Parallel()

	agentLimits := &ContainerLimits{CPU: 512}
	stepLimits := &ContainerLimits{CPU: 1024}
	agent := &Agent{Name: "bot", User: "root", Limits: agentLimits}

	got := resolveAgentRuntime(agent, Step{Agent: "bot"})
	if got.User != "root" || got.Limits != agentLimits {
		t.Errorf("User/Limits = %q/%v, want the agents: entry's", got.User, got.Limits)
	}

	got = resolveAgentRuntime(agent, Step{Agent: "bot", User: "1000", Limits: stepLimits})
	if got.User != "1000" || got.Limits != stepLimits {
		t.Errorf("User/Limits = %q/%v, want the step's overrides", got.User, got.Limits)
	}
}
