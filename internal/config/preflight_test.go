package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cliPipeline is a minimal CLI-backed pipeline, containerized or not.
func cliPipeline(image string) string {
	imageLine := ""
	if image != "" {
		imageLine = "\n  image: " + image
	}

	return "agents:\n" +
		"- name: coder\n" +
		"  source: { model: \"@claude/sonnet\" }" + imageLine + "\n" +
		"jobs:\n" +
		"- name: j\n" +
		"  plan: [{ agent: coder, messages: [x], inputs: [] }]\n"
}

// TestCheckCLIBinariesMissingReportsTheOrchestrator pins issue #100's
// checklist item 16: since image: now places only a CLI agent's TOOLS (see
// cliexec.go), the cli binary is always needed on the orchestrator — the
// message must say so rather than pointing at the container.
func TestCheckCLIBinariesMissingReportsTheOrchestrator(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // guarantees "claude" resolves nowhere

	cfg, err := LoadConfig(writeConfig(t, cliPipeline("")))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	problems := cfg.CheckEnvironment()
	if len(problems) != 1 {
		t.Fatalf("problems = %+v, want exactly one", problems)
	}

	if problems[0].Target != `agent "coder"` {
		t.Errorf("Target = %q, want it to name the agent", problems[0].Target)
	}

	for _, want := range []string{"not found on PATH", "machine running steps"} {
		if !strings.Contains(problems[0].Detail, want) {
			t.Errorf("Detail = %q, want it to contain %q", problems[0].Detail, want)
		}
	}
}

// TestCheckCLIBinariesContainerizedStepStillNeedsIt pins the removed
// exemption: a containerized CLI agent step used to skip this check because
// the binary was assumed to live in the image. Since image: now places only
// the step's TOOLS, the cli process itself is always a host subprocess, so
// the binary is needed on the orchestrator regardless.
func TestCheckCLIBinariesContainerizedStepStillNeedsIt(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	cfg, err := LoadConfig(writeConfig(t, cliPipeline("alpine:3")))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	problems := cfg.CheckEnvironment()
	if len(problems) != 1 {
		t.Fatalf("problems = %+v, want the missing-binary problem even with image: set", problems)
	}

	if !strings.Contains(problems[0].Detail, "an image: does not supply it") {
		t.Errorf("Detail = %q, want it to say why image: does not help", problems[0].Detail)
	}
}

// TestCheckCLIBinariesPresentReportsNothing is the floor: a binary steps can
// find reports no problem at all.
func TestCheckCLIBinariesPresentReportsNothing(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")

	err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755) //nolint:gosec // a fake CLI standing in for PATH resolution
	if err != nil {
		t.Fatalf("writing fake claude: %v", err)
	}

	t.Setenv("PATH", filepath.Dir(bin))

	cfg, err := LoadConfig(writeConfig(t, cliPipeline("")))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if problems := cfg.CheckEnvironment(); len(problems) != 0 {
		t.Errorf("problems = %+v, want none once claude is on PATH", problems)
	}
}

// TestCheckResourceCredentials pins that an expr resource reading an unset
// variable by literal name is refused before a poll, and that a computed name
// or a merely-allowed one is not.
func TestCheckResourceCredentials(t *testing.T) {
	t.Setenv("PREFLIGHT_SET_KEY", "x")
	t.Setenv("PREFLIGHT_UNSET_KEY", "")

	cfg, err := LoadConfig(writeConfig(t, `
resource_types:
- name: api
  env: [PREFLIGHT_SET_KEY, PREFLIGHT_UNSET_KEY, PREFLIGHT_OPTIONAL]
  config:
    expr:
      check: |
        [{a: env("PREFLIGHT_SET_KEY"), b: env( "PREFLIGHT_UNSET_KEY" ), c: env("PREFLIGHT_UNSET_KEY")}]
      in: 'env(source.name ?? "PREFLIGHT_OPTIONAL")'
resources:
- name: used
  type: api
  source: {}
- name: unused
  type: api
  source: {}
jobs:
- name: j
  plan:
  - get: used
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	problems := cfg.CheckEnvironment()
	if len(problems) != 1 {
		t.Fatalf("problems = %+v, want exactly one (the unset literal, once, for the used resource)", problems)
	}

	if problems[0].Target != `resource "used"` || !strings.Contains(problems[0].Detail, "$PREFLIGHT_UNSET_KEY is not set") {
		t.Errorf("problem = %+v, want the used resource and the unset variable", problems[0])
	}
}

// TestCheckResourceCredentialsFollowsResourceAlias: a get or put renamed with
// resource: is checked against the resource it names, not its step name.
func TestCheckResourceCredentialsFollowsResourceAlias(t *testing.T) {
	t.Setenv("PREFLIGHT_ALIAS_KEY", "")

	for _, step := range []string{"get: src\n    resource: aliased", "put: publish\n    resource: aliased"} {
		cfg, err := LoadConfig(writeConfig(t, `
resource_types:
- name: api
  env: [PREFLIGHT_ALIAS_KEY]
  config:
    expr:
      check: '[{a: env("PREFLIGHT_ALIAS_KEY")}]'
      in: '{}'
      out: '{}'
resources:
- name: aliased
  type: api
  source: {}
jobs:
- name: j
  plan:
  - `+step+`
`))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}

		problems := cfg.CheckEnvironment()
		if len(problems) != 1 || problems[0].Target != `resource "aliased"` {
			t.Errorf("%s: problems = %+v, want the aliased resource's unset variable", step, problems)
		}
	}
}
