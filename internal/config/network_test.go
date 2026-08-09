package config

import (
	"strings"
	"testing"
)

func TestValidateNetworkValuesRejectsFlagLikeValues(t *testing.T) {
	t.Parallel()

	cfg := &Config{Tasks: []Task{{Name: "build", Image: "alpine", Network: "--privileged"}}}

	err := cfg.validateNetworkValues()
	if err == nil {
		t.Fatal("expected a flag-like network: value to be rejected")
	}

	if !strings.Contains(err.Error(), "must not start with '-'") {
		t.Errorf("error = %v, want it to name the flag-parsing hazard", err)
	}
}

// TestValidateNetworkValuesAcceptsNamedNetworks pins that the value is passed
// through rather than checked against a fixed set: "none" and "host" are the
// ones that matter, but a pipeline reaching a service on a compose network
// has a real use for a named one, and docker reports a typo itself.
func TestValidateNetworkValuesAcceptsNamedNetworks(t *testing.T) {
	t.Parallel()

	for _, network := range []string{"none", "host", "bridge", "my-compose-net"} {
		cfg := &Config{Tasks: []Task{{Name: "build", Image: "alpine", Network: network}}}

		err := cfg.validateNetworkValues()
		if err != nil {
			t.Errorf("network %q: %v", network, err)
		}
	}
}

// TestValidateNetworkNeedsImage covers the rule that keeps network: from
// promising something it cannot deliver: a host command uses the host's
// network, so `network: none` there would be isolation in name only.
func TestValidateNetworkNeedsImage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cfg     *Config
		wantErr bool
	}{
		{"task without image", &Config{Tasks: []Task{{Name: "b", Network: "none"}}}, true},
		{"task with image", &Config{Tasks: []Task{{Name: "b", Image: "alpine", Network: "none"}}}, false},
		{"agent without image", &Config{Agents: []Agent{{Name: "a", Network: "none"}}}, true},
		{"resource_type without image", &Config{ResourceTypes: []ResourceType{{Name: "rt", Network: "none"}}}, true},
		{
			"step whose own image supplies it",
			&Config{Jobs: []Job{{Name: "j", Plan: []Step{{Task: "b", Run: "true", Image: "alpine", Network: "none"}}}}},
			false,
		},
		{
			"step with no image anywhere",
			&Config{Jobs: []Job{{Name: "j", Plan: []Step{{Task: "b", Run: "true", Network: "none"}}}}},
			true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.cfg.validateNetworkNeedsImage()
			if tc.wantErr && err == nil {
				t.Error("expected network: without image: to be rejected")
			}

			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestValidateNetworkNeedsImageResolvesThroughTheTask is the case a naive
// check gets wrong: the step sets network:, the tasks: entry it references
// supplies the image, and comparing the step's own (empty) image: would
// reject a perfectly valid pipeline.
func TestValidateNetworkNeedsImageResolvesThroughTheTask(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Tasks: []Task{{Name: "build", Run: "true", Image: "alpine"}},
		Jobs:  []Job{{Name: "j", Plan: []Step{{Task: "build", Network: "none"}}}},
	}

	err := cfg.validateNetworkNeedsImage()
	if err != nil {
		t.Errorf("network: on a step whose task supplies the image should be valid: %v", err)
	}
}

func TestValidateNetworkNeedsImageResolvesThroughTheAgent(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Agents: []Agent{{Name: "reviewer", Source: AgentSource{Model: "openai/gpt-4o"}, Image: "python:3.12"}},
		Jobs:   []Job{{Name: "j", Plan: []Step{{Agent: "reviewer", Prompt: "x", Network: "none"}}}},
	}

	err := cfg.validateNetworkNeedsImage()
	if err != nil {
		t.Errorf("network: on a step whose agent supplies the image should be valid: %v", err)
	}
}

func TestResolveTaskNetworkOverride(t *testing.T) {
	t.Parallel()

	cfg := &Config{Tasks: []Task{{Name: "build", Run: "true", Image: "alpine", Network: "none"}}}

	rt, err := cfg.ResolveTask(Step{Task: "build"})
	if err != nil {
		t.Fatalf("ResolveTask: %v", err)
	}

	if rt.Network != "none" {
		t.Errorf("Network = %q, want none inherited from the tasks: entry", rt.Network)
	}

	rt, err = cfg.ResolveTask(Step{Task: "build", Network: "host"})
	if err != nil {
		t.Fatalf("ResolveTask: %v", err)
	}

	if rt.Network != "host" {
		t.Errorf("Network = %q, want the step's override", rt.Network)
	}
}
