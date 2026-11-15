package config

import "testing"

// type: registry-image is rewritten into the image and the three scripts, and its defaults: lie under each resource's own source:, which win.
func TestARegistryImageTypeBecomesItsImageAndScripts(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig(writeConfig(t, `
resource_types:
- name: git
  type: registry-image
  source: {repository: concourse/git-resource, tag: "1.18"}
  defaults: {branch: main, depth: 1}
resources:
- name: repo
  type: git
  source: {uri: https://example.com/a.git, depth: 5}
jobs:
- name: build
  plan:
  - get: repo
  - put: repo
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	rt, _ := cfg.FindResourceType("git")
	if rt.Image != "concourse/git-resource:1.18" || rt.Config.Backend() != BackendConcourse || rt.Config.In == "" || rt.Config.Out == "" {
		t.Errorf("type = %+v, want the image with its tag and the Concourse backend's scripts", rt)
	}

	repo, _ := cfg.FindResource("repo")
	if repo.Source["branch"] != "main" || repo.Source["depth"] != 5 || repo.Source["uri"] == nil {
		t.Errorf("source = %v, want defaults under the resource's own keys", repo.Source)
	}
}

func TestARegistryImageTypeRefusesWhatItCannotMean(t *testing.T) {
	t.Parallel()

	for want, rt := range map[string]string{
		"only type: registry-image is supported":     "type: docker-image\n  source: {repository: a/b}",
		"needs source.repository":                    "type: registry-image\n  source: {tag: x}",
		"source.username is not supported":           "type: registry-image\n  source: {repository: a/b, username: me}",
		"type: and image: both name the image":       "type: registry-image\n  image: a/b\n  source: {repository: a/b}",
		"runs the image's own /opt/resource scripts": "type: registry-image\n  source: {repository: a/b}\n  config: {check: echo}",
		"source.tag is empty":                        "type: registry-image\n  source: {repository: a/b, tag: \"\"}",
		"source.tag is empty; omit it":               "type: registry-image\n  source: {repository: a/b, tag: null}",
		"YAML read this one as 1.1; quote it":        "type: registry-image\n  source: {repository: a/b, tag: 1.10}",
		"source.tag must be a string":                "type: registry-image\n  source: {repository: a/b, tag: true}",
	} {
		wantLoadError(t, writeConfig(t, "resource_types:\n- name: t\n  "+rt+"\njobs:\n- name: j\n  plan:\n  - task: x\n    run: \"true\"\n"), want)
	}
}

// A Concourse type's scripts are commands in a container, as a shell type's are, so a worker can run them whole.
func TestARegistryImageTypeCanBePlaced(t *testing.T) {
	t.Parallel()

	_, err := LoadConfig(writeConfig(t, `
resource_types:
- name: time
  type: registry-image
  source: {repository: concourse/time-resource}
resources:
- name: clock
  type: time
  tags: [vpc]
  source: {}
jobs:
- name: build
  plan:
  - get: clock
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
}
