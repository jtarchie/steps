package config

import (
	"slices"
	"testing"
)

// A step with its own run: is inline and ResolveTask never reads the same-named tasks: entry, so no rule may read that entry's image, fix or sandbox for it either.
func TestAnInlineRunIgnoresItsSameNamedTaskEntry(t *testing.T) {
	t.Parallel()

	cfg := namesakeConfig()
	inline := Step{Task: "build", Run: "curl https://example.com", Tags: []string{"gpu"}}

	if got := cfg.PlacedImage(inline); got != "" {
		t.Errorf("PlacedImage = %q, want \"\": the inline command runs bare", got)
	}

	if got := cfg.resolvedStepFix(inline); got != nil {
		t.Errorf("resolvedStepFix = %+v, want nil: the inline command has no fix:", got)
	}

	if got := cfg.effectiveFix(&inline); got != nil {
		t.Errorf("effectiveFix = %+v, want nil: the inline command has no fix:", got)
	}

	if reached, _ := cfg.jobReachesMCPServer(Job{Name: "j", Plan: []Step{inline}}, "docs"); reached {
		t.Error("an inline step reached the MCP server its namesake entry's fix: grants")
	}

	if cfg.resolvedStepSandbox(inline) {
		t.Error("an inline step inherited its namesake entry's sandbox: true")
	}

	if env, _ := cfg.effectiveEnvAndSecrets(&inline); env != nil {
		t.Errorf("effectiveEnvAndSecrets env = %v, want nil: the inline command declared none", env)
	}
}

// The other side: a step that names the entry without run: still gets everything the entry declares.
func TestAReferencingStepKeepsItsTaskEntry(t *testing.T) {
	t.Parallel()

	cfg := namesakeConfig()
	referenced := Step{Task: "build", Tags: []string{"gpu"}}
	env, _ := cfg.effectiveEnvAndSecrets(&referenced)

	if cfg.PlacedImage(referenced) != "golang:1.26" || cfg.resolvedStepFix(referenced) == nil || cfg.effectiveFix(&referenced) == nil ||
		!cfg.resolvedStepSandbox(referenced) || len(env) != 1 {
		t.Error("a step referencing the entry lost the entry's image, fix, sandbox or env")
	}
}

func namesakeConfig() *Config {
	return &Config{
		Tasks: []Task{{Name: "build", Image: "golang:1.26", Run: "go build", Sandbox: true, Env: []string{"BUILD_TOKEN"}, Fix: &FixSpec{Agent: "mender", Tools: []ToolSpec{{MCP: "docs"}}}}},
	}
}

// The entry's only real user is placed, so its image belongs to the worker; an untagged inline namesake runs nothing from it and must not pull it here.
func TestImagesSkipsAnEntryOnlyAnInlineNamesakeRunsLocally(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Tasks: []Task{{Name: "build", Image: "golang:1.26", Run: "go build"}},
		Jobs: []Job{{Name: "j", Plan: []Step{
			{Task: "build", Run: "echo local"},
			{Task: "build", Tags: []string{"gpu"}},
		}}},
	}

	if got := cfg.Images(); slices.Contains(got, "golang:1.26") {
		t.Errorf("Images() = %v, want the placed-only entry's image left to the worker", got)
	}
}
