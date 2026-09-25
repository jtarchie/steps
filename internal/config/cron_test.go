package config

import (
	"strings"
	"testing"
)

func cronPipeline(source string) string {
	return `
resources:
- name: tick
  type: cron
  source:
` + source + `
jobs:
- name: build
  plan:
  - get: tick
    trigger: true
`
}

// TestCronSourceIsDecodedStrictly: a misspelled key, a missing expression and an unknown zone are all load errors, not a schedule nobody reads.
func TestCronSourceIsDecodedStrictly(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ source, want string }{
		"misspelled key":     {"    expresion: '@hourly'", "expresion"},
		"missing expression": {"    location: UTC", "needs source.expression"},
		"unknown location":   {"    expression: '@hourly'\n    location: Mars/Olympus", `source.location "Mars/Olympus"`},
	} {
		_, err := LoadConfig(writeConfig(t, cronPipeline(tc.source)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestCronIsBuiltIn(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig(writeConfig(t, cronPipeline("    expression: '0 2 * * 1-5'\n    location: America/New_York\n    fire_immediately: true")))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	resourceType, err := cfg.FindResourceType(CronType)
	if err != nil {
		t.Fatal(err)
	}

	if resourceType.Config.Backend() != BackendCron {
		t.Errorf("backend = %s, want %s", resourceType.Config.Backend(), BackendCron)
	}

	source, err := cfg.Resources[0].CronSource()
	if err != nil {
		t.Fatal(err)
	}

	if source.Expression != "0 2 * * 1-5" || source.Location != "America/New_York" || !source.FireImmediately {
		t.Errorf("source = %+v, want every field carried", source)
	}
}

// TestCronCannotBeRedeclaredOrPutTo: the name is reserved, and a put has nothing to publish to.
func TestCronCannotBeRedeclaredOrPutTo(t *testing.T) {
	t.Parallel()

	_, err := LoadConfig(writeConfig(t, `
resource_types:
- name: cron
  config:
    check: printf '[]'
`+cronPipeline("    expression: '@hourly'")))
	if err == nil || !strings.Contains(err.Error(), "the name is built in") {
		t.Errorf("redeclaring cron: err = %v, want it refused", err)
	}

	_, err = LoadConfig(writeConfig(t, `
resources:
- name: tick
  type: cron
  source:
    expression: '@hourly'
jobs:
- name: build
  plan:
  - put: tick
`))
	if err == nil || !strings.Contains(err.Error(), "only tells time") {
		t.Errorf("put: err = %v, want it refused", err)
	}
}
