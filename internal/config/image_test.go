package config

import (
	"strings"
	"testing"
)

// TestImagesCollectsEveryDistinctImage covers what the pre-pull walks: every
// place image: can be set, deduped, so a pipeline naming one image in four
// places pulls it once.
func TestImagesCollectsEveryDistinctImage(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		ResourceTypes: []ResourceType{{Name: "rt", Image: "alpine:3"}},
		Agents:        []Agent{{Name: "a", Image: "python:3.12"}},
		Tasks:         []Task{{Name: "t", Image: "alpine:3"}},
		Jobs: []Job{{Name: "j", Plan: []Step{
			{Task: "t", Run: "true", Image: "golang:1.26"},
			{Task: "t", Run: "true"},
		}}},
	}

	got := cfg.Images()

	want := []string{"alpine:3", "golang:1.26", "python:3.12"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Images() = %v, want %v (sorted and deduped)", got, want)
	}
}

// TestImagesIsEmptyForAHostOnlyPipeline is what keeps a pipeline that never
// containerizes from touching docker at all.
func TestImagesIsEmptyForAHostOnlyPipeline(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Tasks: []Task{{Name: "t", Run: "true"}},
		Jobs:  []Job{{Name: "j", Plan: []Step{{Task: "t"}}}},
	}

	if got := cfg.Images(); len(got) != 0 {
		t.Errorf("Images() = %v, want none", got)
	}

	if cfg.UsesImages() {
		t.Error("UsesImages() = true for a pipeline that sets no image:")
	}
}

// TestImagesSkipsAPlacedStepsImage keeps a worker's image off this machine's
// daemon.
//
// A placed step's container runs on the WORKER's daemon, which does not exist
// yet when this is asked — a machine acquired for the job has not been
// acquired. Collecting it here made prepareImages demand a local daemon for a
// pipeline whose every container lives elsewhere, and pull the image to a
// machine that will never run it — worse, `docker image inspect` finding a
// LOCALLY built tag skipped the pull the worker was the one that needed.
func TestImagesSkipsAPlacedStepsImage(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Jobs: []Job{{Name: "j", Plan: []Step{
			{Task: "remote", Image: "alpine:3", Tags: []string{"box"}, Run: "true"},
		}}},
	}

	if got := cfg.Images(); len(got) != 0 {
		t.Errorf("Images() = %v, want none — that image runs on the worker's daemon", got)
	}

	if cfg.UsesImages() {
		t.Error("UsesImages() = true, so the job demands a local daemon it never uses")
	}

	// And the same image on a step that is NOT placed is still pulled here,
	// which is the behaviour this must not cost.
	cfg.Jobs[0].Plan = append(cfg.Jobs[0].Plan, Step{Task: "local", Image: "alpine:3", Run: "true"})

	if got := cfg.Images(); len(got) != 1 || got[0] != "alpine:3" {
		t.Errorf("Images() = %v, want the un-placed step's image", got)
	}
}

// TestImagesSkipsATaskEntryOnlyPlacedStepsUse pins the other half of the
// placed-step rule.
//
// Images() is what the orchestrator pre-pulls and what makes it demand a
// local daemon. A step's own image: is already skipped when it carries tags:,
// because that container runs on the worker — but a tasks: entry is visited
// on its own, knowing nothing about who references it, so an image reached
// only through a tagged step was still pulled here. On a machine with no
// daemon that refused the job outright; with one, it pulled an image that
// will never run — and for a locally BUILT tag, found it, skipped the pull,
// and left the worker to fail with "Unable to find image".
func TestImagesSkipsATaskEntryOnlyPlacedStepsUse(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Tasks: []Task{{Name: "remote", Image: "alpine:3", Run: "true"}},
		Jobs: []Job{{Name: "j", Plan: []Step{
			{Task: "remote", Tags: []string{"box"}},
		}}},
	}

	if got := cfg.Images(); len(got) != 0 {
		t.Errorf("Images() = %v, want none — that image runs on the worker", got)
	}
}

// TestImagesKeepsATaskEntryAnyLocalStepUses is the guard on the guard: one
// untagged reference is enough to need the image here.
func TestImagesKeepsATaskEntryAnyLocalStepUses(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Tasks: []Task{{Name: "both", Image: "alpine:3", Run: "true"}},
		Jobs: []Job{{Name: "j", Plan: []Step{
			{Task: "both", Tags: []string{"box"}},
			{Task: "both"},
		}}},
	}

	got := cfg.Images()
	if len(got) != 1 || got[0] != "alpine:3" {
		t.Errorf("Images() = %v, want alpine:3 — a local step still runs it here", got)
	}
}

// TestImagesKeepsAResourceTypeSharingAPlacedTasksName is the namespace half.
//
// A resource_type's check, in and out always run on THIS machine, so its
// image can never be one a worker needs — but all three collections put their
// names into one label format, and keying on the bare name made a
// resource_type called "build" inherit a placed task's answer and lose its
// pre-pull. Losing it is not cosmetic: UsesImages() then reports false, which
// skips the daemon preflight and the orphan sweep as well.
func TestImagesKeepsAResourceTypeSharingAPlacedTasksName(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		ResourceTypes: []ResourceType{{Name: "build", Image: "registry/rt:1"}},
		Tasks:         []Task{{Name: "build", Image: "registry/task:1", Run: "true"}},
		Jobs: []Job{{Name: "j", Plan: []Step{
			{Task: "build", Tags: []string{"box"}},
		}}},
	}

	got := cfg.Images()
	if len(got) != 1 || got[0] != "registry/rt:1" {
		t.Errorf("Images() = %v, want registry/rt:1 — a resource type never runs on a worker", got)
	}
}

// TestImagesKeepsAnAgentSharingAPlacedTasksName is the same collision between
// the two collections a step CAN reference, where the name alone is likewise
// not the identity.
func TestImagesKeepsAnAgentSharingAPlacedTasksName(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Agents: []Agent{{Name: "shared", Image: "registry/agent:1"}},
		Tasks:  []Task{{Name: "shared", Image: "registry/task:1", Run: "true"}},
		Jobs: []Job{{Name: "j", Plan: []Step{
			{Task: "shared", Tags: []string{"box"}},
			{Agent: "shared"},
		}}},
	}

	got := cfg.Images()
	if len(got) != 1 || got[0] != "registry/agent:1" {
		t.Errorf("Images() = %v, want registry/agent:1 — the agent entry is used locally", got)
	}
}

// TestImagesSkipsAResourceTypeOnlyPlacedResourcesUse: a resource_type's
// commands run wherever its resources say, so its image is pulled here only
// while some resource of the type is checked from here.
func TestImagesSkipsAResourceTypeOnlyPlacedResourcesUse(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		ResourceTypes: []ResourceType{{Name: "probe", Image: "alpine:3"}},
		Resources:     []Resource{{Name: "repo", Type: "probe", Tags: []string{"vpc"}}},
	}

	if got := cfg.Images(); len(got) != 0 {
		t.Errorf("Images() = %v, want none — every check, in and out of the type runs on the worker", got)
	}

	// One untagged resource of the type keeps the image local: the poller
	// checks it from this machine.
	cfg.Resources = append(cfg.Resources, Resource{Name: "other", Type: "probe"})

	if got := cfg.Images(); len(got) != 1 || got[0] != "alpine:3" {
		t.Errorf("Images() = %v, want the type's image, pulled for the un-placed resource", got)
	}
}

// TestImagesSkipsATaskEntryAJobTagPlaces: a job's tags: place its steps as
// surely as their own, so a tasks: entry only such steps use is never pulled
// here — and one also used by an untagged job still is.
func TestImagesSkipsATaskEntryAJobTagPlaces(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		other string
		want  string
	}{
		{"placed only", "", ""},
		{"also local", `
- name: here
  plan:
  - task: build
`, "alpine:3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := LoadConfig(writeConfig(t, `
tasks:
- name: build
  image: alpine:3
  run: "true"
jobs:
- name: there
  tags: [box]
  plan:
  - task: build
`+tc.other))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}

			if got := strings.Join(cfg.Images(), ","); got != tc.want {
				t.Errorf("Images() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestImageReference(t *testing.T) {
	t.Parallel()

	sha256 := "sha256:" + strings.Repeat("a", 64)
	sha512 := "sha512:" + strings.Repeat("b", 128)
	source := map[string]any{"repository": "ghcr.io/me/toolchain", "password": "s3cret"}

	cases := map[string]struct {
		source, version map[string]any
		want, err       string
	}{
		"sha256":               {source, map[string]any{"digest": sha256}, "ghcr.io/me/toolchain@" + sha256, ""},
		"sha512":               {source, map[string]any{"digest": sha512}, "ghcr.io/me/toolchain@" + sha512, ""},
		"no repository":        {map[string]any{"password": "s3cret"}, map[string]any{"digest": sha256}, "", "no repository"},
		"repository not text":  {map[string]any{"repository": 7}, map[string]any{"digest": sha256}, "", "no repository"},
		"no digest":            {source, map[string]any{"ref": "v1"}, "", "no digest"},
		"digest not text":      {source, map[string]any{"digest": 42}, "", "not a string"},
		"a tag":                {source, map[string]any{"digest": "latest"}, "", "sha256:<64 hex>"},
		"uppercase hex":        {source, map[string]any{"digest": "sha256:" + strings.Repeat("A", 64)}, "", "sha256:<64 hex>"},
		"short hex":            {source, map[string]any{"digest": "sha256:" + strings.Repeat("a", 63)}, "", "sha256:<64 hex>"},
		"trailing text":        {source, map[string]any{"digest": sha256 + " --privileged"}, "", "sha256:<64 hex>"},
		"flag repository":      {map[string]any{"repository": "--privileged"}, map[string]any{"digest": sha256}, "", "must not start with '-'"},
		"spaced repository":    {map[string]any{"repository": "alpine x"}, map[string]any{"digest": sha256}, "", "whitespace"},
		"control repository":   {map[string]any{"repository": "alpine\x00"}, map[string]any{"digest": sha256}, "", "control"},
		"repository with at":   {map[string]any{"repository": "alpine@sha256:x"}, map[string]any{"digest": sha256}, "", "'@'"},
		"repository from ver.": {map[string]any{}, map[string]any{"digest": sha256, "repository": "evil.io/x"}, "", "no repository"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := ImageReference(tc.source, tc.version)
			if tc.err == "" {
				if err != nil || got != tc.want {
					t.Fatalf("ImageReference = %q, %v; want %q", got, err, tc.want)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("ImageReference err = %v, want one containing %q", err, tc.err)
			}

			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("error %q quotes the source's password", err)
			}
		})
	}
}

// resolveFixture is one fetched artifact, "toolchain", and the reference it
// resolves to.
func resolveFixture() (map[string]bool, map[string]Fetched, string) {
	digest := "sha256:" + strings.Repeat("c", 64)
	fetched := map[string]Fetched{"toolchain": {
		Source:  map[string]any{"repository": "ghcr.io/me/toolchain"},
		Version: map[string]any{"digest": digest},
	}}

	return map[string]bool{"toolchain": true}, fetched, "ghcr.io/me/toolchain@" + digest
}

func TestResolveArtifactImagePassesALiteralThrough(t *testing.T) {
	t.Parallel()

	artifacts, fetched, _ := resolveFixture()

	got, ok, err := ResolveArtifactImage(Step{Task: "t", Image: "alpine:3"}, artifacts, fetched)
	if err != nil || ok || got.Image != "alpine:3" {
		t.Fatalf("got %q, %v, %v; want the literal untouched", got.Image, ok, err)
	}
}

// TestResolveArtifactImageIsIdempotent matters because across: cells and a
// try:'s body pass through resolution again.
func TestResolveArtifactImageIsIdempotent(t *testing.T) {
	t.Parallel()

	artifacts, fetched, ref := resolveFixture()

	got, ok, err := ResolveArtifactImage(Step{Task: "t", Image: "toolchain"}, artifacts, fetched)
	if err != nil || !ok || got.Image != ref {
		t.Fatalf("got %q, %v, %v; want %q", got.Image, ok, err, ref)
	}

	again, ok, err := ResolveArtifactImage(got, artifacts, fetched)
	if err != nil || ok || again.Image != ref {
		t.Fatalf("second pass got %q, %v, %v; want %q unchanged", again.Image, ok, err, ref)
	}
}

func TestResolveArtifactImageRefusesAnUnfetchedArtifact(t *testing.T) {
	t.Parallel()

	artifacts, _, _ := resolveFixture()

	_, _, err := ResolveArtifactImage(Step{Task: "t", Image: "toolchain"}, artifacts, nil)
	if err == nil || !strings.Contains(err.Error(), "rename the resource") {
		t.Fatalf("err = %v, want the unfetched-artifact error", err)
	}
}

func TestResolveArtifactImageResolvesATryBodyOnACopy(t *testing.T) {
	t.Parallel()

	artifacts, fetched, ref := resolveFixture()
	inner := &Step{Task: "t", Image: "toolchain"}
	step := Step{Try: &Step{Try: inner}}

	got, ok, err := ResolveArtifactImage(step, artifacts, fetched)
	if err != nil || !ok || got.Unwrap().Image != ref {
		t.Fatalf("got %q, %v, %v; want %q", got.Unwrap().Image, ok, err, ref)
	}

	if inner.Image != "toolchain" || step.Try.Try != inner {
		t.Errorf("the original step was written through: inner image %q", inner.Image)
	}
}

func TestImagesSkipsAnArtifactImage(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Resources: []Resource{{Name: "toolchain"}},
		Jobs: []Job{{Name: "j", Plan: []Step{
			{Get: "toolchain"},
			{Task: "t", Run: "true", Image: "toolchain"},
		}}},
	}

	if got := cfg.Images(); len(got) != 0 {
		t.Errorf("Images() = %v; an artifact name is not an image to pull", got)
	}

	if !cfg.UsesImages() {
		t.Error("UsesImages() = false for a local step running in an artifact image")
	}

	cfg.Jobs[0].Plan[1].Tags = []string{"gpu"}

	if cfg.UsesImages() {
		t.Error("UsesImages() = true when the only artifact image is placed")
	}
}

func TestArtifactImageRefusedWhereNoGetReaches(t *testing.T) {
	t.Parallel()

	const resources = `
resource_types:
- name: fake-image
  config:
    check: echo '[]'
    in: "true"
resources:
- name: toolchain
  type: fake-image
  source: {repository: alpine}
`

	cases := map[string]struct{ pipeline, want string }{
		"a tasks: entry": {`
tasks:
- name: build
  image: toolchain
  run: "true"
jobs:
- name: j
  plan:
  - get: toolchain
  - task: build
`, `task "build": image "toolchain" names a resource or get`},
		"an agents: entry": {`
agents:
- name: a
  image: toolchain
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - get: toolchain
`, `agent "a": image "toolchain" names a resource or get`},
		"a tasks: entry named after a get alias": {`
tasks:
- name: build
  image: tools
  run: "true"
jobs:
- name: j
  plan:
  - get: tools
    resource: toolchain
  - task: build
`, `task "build": image "tools" names a resource or get`},
		"a job hook": {`
jobs:
- name: j
  plan:
  - get: toolchain
  ensure:
    task: note
    image: toolchain
    run: "true"
`, "job-level hook"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			wantLoadError(t, writeConfig(t, resources+tc.pipeline), tc.want)
		})
	}
}

func TestResolveArtifactImagePassesATryWrappedLiteralThrough(t *testing.T) {
	t.Parallel()

	artifacts, fetched, _ := resolveFixture()
	step := Step{Try: &Step{Task: "t", Image: "alpine:3"}}

	got, ok, err := ResolveArtifactImage(step, artifacts, fetched)
	if err != nil || ok || got.Try != step.Try {
		t.Fatalf("got %+v, %v, %v; want the wrapper untouched", got.Try, ok, err)
	}
}

// TestImageArtifactsNamesResourcesAndTheJobsGets: a get alias is an artifact
// only in the job that declares it; a resource is one everywhere.
func TestImageArtifactsNamesResourcesAndTheJobsGets(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Resources: []Resource{{Name: "toolchain"}},
		Jobs: []Job{
			{Name: "a", Plan: []Step{{Get: "tools", Resource: "toolchain"}}},
			{Name: "b"},
		},
	}

	if got := cfg.ImageArtifacts("a"); !got["toolchain"] || !got["tools"] {
		t.Errorf("ImageArtifacts(a) = %v, want the resource and the alias", got)
	}

	if got := cfg.ImageArtifacts("b"); !got["toolchain"] || got["tools"] {
		t.Errorf("ImageArtifacts(b) = %v, want the resource and not job a's alias", got)
	}
}
