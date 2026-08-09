package config

import (
	"strings"
	"testing"
)

func TestValidateUserValuesRejectsFlagLikeValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  *Config
	}{
		{"resource_type", &Config{ResourceTypes: []ResourceType{{Name: "git", User: "--privileged"}}}},
		{"agent", &Config{Agents: []Agent{{Name: "reviewer", User: "-v"}}}},
		{"task", &Config{Tasks: []Task{{Name: "build", User: "--network=host"}}}},
		{"step", &Config{Jobs: []Job{{Name: "j", Plan: []Step{{Task: "b", Run: "true", User: "--privileged"}}}}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.cfg.validateUserValues()
			if err == nil {
				t.Fatal("expected a flag-like user: value to be rejected")
			}

			if !strings.Contains(err.Error(), "must not start with '-'") {
				t.Errorf("error = %v, want it to name the flag-parsing hazard", err)
			}
		})
	}
}

func TestValidateUserValuesAcceptsRealUsers(t *testing.T) {
	t.Parallel()

	cfg := &Config{Tasks: []Task{
		{Name: "a", User: "root"},
		{Name: "b", User: "1000:1000"},
		{Name: "c", User: "nobody"},
		{Name: "d"},
	}}

	err := cfg.validateUserValues()
	if err != nil {
		t.Errorf("validateUserValues: %v", err)
	}
}

func TestValidateUserPlacementRejectsGetAndPut(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		step Step
		want string
	}{
		{"get", Step{Get: "repo", User: "root"}, "not valid on get steps"},
		{"put", Step{Put: "results", User: "root"}, "not valid on put steps"},
		{"try", Step{Try: &Step{Task: "b", Run: "true"}, User: "root"}, "not valid on a try: step"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := &Config{Jobs: []Job{{Name: "j", Plan: []Step{tc.step}}}}

			err := cfg.validateUserPlacement()
			if err == nil {
				t.Fatalf("expected user: on a %s step to be rejected", tc.name)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestResolveTaskUserOverride(t *testing.T) {
	t.Parallel()

	cfg := &Config{Tasks: []Task{{Name: "build", Run: "true", User: "root"}}}

	rt, err := cfg.ResolveTask(Step{Task: "build"})
	if err != nil {
		t.Fatalf("ResolveTask: %v", err)
	}

	if rt.User != "root" {
		t.Errorf("User = %q, want root inherited from the tasks: entry", rt.User)
	}

	rt, err = cfg.ResolveTask(Step{Task: "build", User: "1000:1000"})
	if err != nil {
		t.Fatalf("ResolveTask: %v", err)
	}

	if rt.User != "1000:1000" {
		t.Errorf("User = %q, want the step's override", rt.User)
	}
}
