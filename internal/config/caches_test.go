package config

import (
	"path/filepath"
	"testing"
)

func TestCachesLoadErrors(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, step, want string }{
		{"absolute", "caches: [{path: /tmp/c}]", `cache path "/tmp/c" must be a directory inside the working directory`},
		{"escapes", "caches: [{path: ../c}]", `cache path "../c" must be a directory inside the working directory`},
		{"the whole working directory", "caches: [{path: ./}]", "must be a directory inside the working directory"},
		{"empty", "caches: [{path: ''}]", "must be a directory inside the working directory"},
		{"nested caches", "caches: [{path: c}, {path: c/d}]", `cache paths "c" and "c/d" overlap`},
		{"duplicate after cleaning", "caches: [{path: c}, {path: ./c/}]", `cache paths "c" and "c" overlap`},
		{"is an output", "outputs: [bin]\n    caches: [{path: bin}]", `cache path "bin" overlaps output "bin"`},
		{"inside an output", "outputs: [bin]\n    caches: [{path: bin/obj}]", `cache path "bin/obj" overlaps output "bin"`},
		{"is an input", "inputs: [src]\n    caches: [{path: src}]", `cache path "src" would replace input "src"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "pipeline.yml")
			writeFile(t, path, `
jobs:
- name: build
  plan:
  - task: src
    outputs: [src]
    run: "true"
  - task: compile
    run: "true"
    `+tc.step+`
`)

			wantLoadError(t, path, tc.want)
		})
	}
}

func TestCachesRejectedOnAPutStep(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "pipeline.yml")
	writeFile(t, path, `
resource_types:
- name: dummy
  config:
    check: "true"
    in: "true"
    out: "true"

resources:
- name: results
  type: dummy
  source: {}

jobs:
- name: publish
  plan:
  - put: results
    caches: [{path: c}]
`)

	wantLoadError(t, path, "caches is only valid on task and agent steps")
}

// TestCachesInsideAnInputLoad is Concourse's own documented case: a node_modules cache inside the checkout it belongs to.
func TestCachesInsideAnInputLoad(t *testing.T) {
	t.Parallel()

	loadOK(t, `
jobs:
- name: build
  plan:
  - task: src
    outputs: [src]
    run: "true"
  - task: compile
    inputs: [src]
    caches: [{path: src/node_modules}]
    run: "true"
`)
}

// TestCachesOfANamedTaskJudgedAgainstTheStep: the collision can only be seen once the entry's cache meets the step's own outputs.
func TestCachesOfANamedTaskJudgedAgainstTheStep(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "pipeline.yml")
	writeFile(t, path, `
tasks:
- name: compile
  run: "true"
  caches: [{path: bin}]

jobs:
- name: build
  plan:
  - task: compile
    outputs: [bin]
`)

	wantLoadError(t, path, `cache path "bin" overlaps output "bin"`)
}

func TestCachesStepOverridesEntry(t *testing.T) {
	t.Parallel()

	cfg := loadOK(t, `
tasks:
- name: compile
  run: "true"
  caches: [{path: a}]

jobs:
- name: build
  plan:
  - task: compile
  - task: compile
    caches: []
`)

	steps := cfg.Jobs[0].Plan

	first, err := cfg.ResolveTask(steps[0])
	if err != nil {
		t.Fatal(err)
	}

	second, err := cfg.ResolveTask(steps[1])
	if err != nil {
		t.Fatal(err)
	}

	if len(first.Caches) != 1 || len(second.Caches) != 0 {
		t.Errorf("caches = %v and %v, want the entry's then none (an explicit caches: [] overrides)", first.Caches, second.Caches)
	}
}
