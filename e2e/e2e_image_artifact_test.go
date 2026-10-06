package e2e

// End-to-end coverage for a step's image: naming an artifact an earlier get
// fetched (#151): the step runs in repository@digest, and that reference is
// what the cache keys on.

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
)

// imageArtifactCanary sits in the resource's source as a password. A
// registry-image source routinely holds one, and nothing about resolving an
// image may carry it into output or the event log.
const imageArtifactCanary = "s3cret-canary"

// alpineDigest is the digest the daemon already holds for alpine:3, so the
// test runs a real digest reference without a network pull.
func alpineDigest(t *testing.T) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	inspect := func() ([]byte, error) {
		return exec.CommandContext(ctx, "docker", "image", "inspect", dockerE2EImage, "--format", "{{index .RepoDigests 0}}").Output()
	}

	// Pulled only when the daemon has no digest for it: a pull per test was a registry round trip each, and could move the tag between two tests.
	out, err := inspect()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		_ = exec.CommandContext(ctx, "docker", "pull", "-q", dockerE2EImage).Run()

		out, err = inspect()
	}

	if err != nil {
		t.Skipf("no repo digest for %s: %v", dockerE2EImage, err)
	}

	_, digest, ok := strings.Cut(strings.TrimSpace(string(out)), "@")
	if !ok || digest == "" {
		t.Skipf("no repo digest for %s: %q", dockerE2EImage, out)
	}

	return digest
}

// imageArtifactPipeline is a get of a fake image resource whose version is
// {digest}, and a task that runs in it. root, when set, turns the step cache
// on.
func imageArtifactPipeline(t *testing.T, path, repository, digest, root, plan string) {
	t.Helper()

	workspace := ""
	if root != "" {
		workspace = fmt.Sprintf("workspace:\n  strategy: copy\n  root: %s\n", root)
	}

	writePipelineFile(t, path, fmt.Sprintf(`%[1]s
resource_types:
- name: fake-image
  config:
    check: echo '[{"digest":"%[3]s"}]'
    in: "true"

resources:
- name: toolchain
  type: fake-image
  source: {repository: %[2]s, password: %[4]s}

jobs:
- name: build
  plan:
  - get: toolchain
%[5]s`, workspace, repository, digest, imageArtifactCanary, plan))
}

const imageArtifactTask = `  - task: probe
    image: toolchain
    outputs: [out]
    run: |
      echo "release=$(cat /etc/alpine-release)"
      cat /etc/alpine-release > out/release
`

func TestImageArtifactRunsInTheFetchedDigest(t *testing.T) {
	requireDockerE2E(t)

	digest := alpineDigest(t)
	ref := "alpine@" + digest
	path := pipelinePath(t, t.TempDir())
	root := t.TempDir()

	imageArtifactPipeline(t, path, "alpine", digest, root, imageArtifactTask)

	out := captureStdout(t, func() { mustRun(t, path) })

	// The host has no /etc/alpine-release, so this line proves the container.
	if !strings.Contains(out, "release=3.") {
		t.Fatalf("output has no alpine release; the step did not run in the image:\n%s", out)
	}

	if !strings.Contains(out, "image: toolchain → "+ref) {
		t.Errorf("output does not name the resolved reference %q:\n%s", ref, out)
	}

	if !nodeContentContains(t, path, `"image":"`+ref+`"`) {
		t.Errorf("no recorded node carries image %q", ref)
	}

	assertNoCanary(t, path, out)

	// Unchanged: the planner and the executor hash the same resolved
	// reference, so the task is chain-skipped rather than run.
	// Not merely reused from the step cache, which would mask a disagreement.
	out = captureStdout(t, func() { mustRun(t, path) })
	if strings.Contains(out, "release=") || strings.Contains(out, "outputs reused") {
		t.Errorf("an unchanged rerun was not chain-skipped; planner and executor disagree about the task's hash:\n%s", out)
	}

	// Same digest, different repository: the get's source moves the chain,
	// and the task declares no inputs, so the image reference is the only
	// thing left to move its step-cache key. Hashing the literal name would
	// come back "outputs reused".
	imageArtifactPipeline(t, path, "docker.io/library/alpine", digest, root, imageArtifactTask)

	out = captureStdout(t, func() { mustRun(t, path) })
	if !strings.Contains(out, "release=3.") {
		t.Errorf("a new image reference was served from the step cache:\n%s", out)
	}
}

// TestImageArtifactAgentToolsRunInTheFetchedDigest: an agent step's image:
// override of its agents: entry may name the get too, and the agent's shell
// tool runs in that reference.
func TestImageArtifactAgentToolsRunInTheFetchedDigest(t *testing.T) {
	requireDockerE2E(t)

	digest := alpineDigest(t)
	ref := "alpine@" + digest
	path := pipelinePath(t, t.TempDir())

	fake := newFakeLLM(t,
		callsTool("run_shell", map[string]any{"command": "cat /etc/alpine-release"}),
		says("done"),
	)

	imageArtifactPipeline(t, path, "alpine", digest, "", fmt.Sprintf(`  - agent: inspector
    image: toolchain
    messages: [Say which release this is.]

agents:
- name: inspector
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell]

defaults:
  preflight:
    disabled: true
`, fake.URL))

	out := captureStdout(t, func() { mustRun(t, path) })

	// The host has no /etc/alpine-release, so a release proves the container.
	if result := lastToolResult(t, fake.request(2)); !strings.Contains(result, `"exit_code":0`) || !strings.Contains(result, "3.") {
		t.Errorf("run_shell result = %q, want alpine's release from the container", result)
	}

	if !strings.Contains(out, "image: toolchain → "+ref) {
		t.Errorf("output does not name the resolved reference %q:\n%s", ref, out)
	}

	if !nodeContentContains(t, path, `"image":"`+ref+`"`) {
		t.Errorf("no recorded node carries image %q", ref)
	}

	assertNoCanary(t, path, out)
}

func TestImageArtifactGuardRunsInTheFetchedDigest(t *testing.T) {
	requireDockerE2E(t)

	digest := alpineDigest(t)
	path := pipelinePath(t, t.TempDir())

	imageArtifactPipeline(t, path, "alpine", digest, "", `  - try:
      task: guarded
      image: toolchain
      run: echo guarded-ran
    when: cat /etc/alpine-release
`)

	out := captureStdout(t, func() { mustRun(t, path) })
	if !strings.Contains(out, "guarded-ran") {
		t.Errorf("the guard did not pass; it ran somewhere without /etc/alpine-release:\n%s", out)
	}
}

func TestImageArtifactRefusesAMalformedDigest(t *testing.T) {
	requireDockerE2E(t)

	path := pipelinePath(t, t.TempDir())
	imageArtifactPipeline(t, path, "alpine", "latest", "", imageArtifactTask)

	err := cli.Run([]string{path})
	if err == nil {
		t.Fatal("a digest of \"latest\" ran; want the step refused")
	}

	for _, want := range []string{"sha256", `"toolchain"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}

	if strings.Contains(err.Error(), imageArtifactCanary) {
		t.Errorf("error %q carries the source's password", err)
	}
}

func TestImageArtifactValidateRefusesAnUnfetchedImage(t *testing.T) {
	cases := map[string]struct {
		plan string
		want string
	}{
		"task before its get": {
			plan: `- name: early
  plan:
  - task: probe
    image: toolchain
    run: "true"
  - get: toolchain
`,
			want: `image "toolchain"`,
		},
		"a resource never fetched": {
			plan: `- name: never
  plan:
  - task: probe
    image: toolchain
    run: "true"
`,
			want: "rename the resource",
		},
		"an in_parallel sibling": {
			plan: `- name: sibling
  plan:
  - in_parallel:
      steps:
      - get: toolchain
      - task: probe
        image: toolchain
        run: "true"
`,
			want: `image "toolchain"`,
		},
		"a task output of the same name": {
			plan: `- name: output
  plan:
  - task: make
    outputs: [toolchain]
    run: "true"
  - task: probe
    image: toolchain
    run: "true"
`,
			want: `image "toolchain"`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := pipelinePath(t, t.TempDir())

			writePipelineFile(t, path, `
resource_types:
- name: fake-image
  config:
    check: echo '[]'
    in: "true"

resources:
- name: toolchain
  type: fake-image
  source: {repository: alpine}

jobs:
`+tc.plan)

			err := cli.Run([]string{"validate", path})
			if err == nil {
				t.Fatal("validate passed; want the artifact image refused")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// nodeContentContains reports whether any stored node's content holds want.
func nodeContentContains(t *testing.T, path, want string) bool {
	t.Helper()

	var found int

	err := openStateDB(t, path).QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM node_content WHERE instr(content, ?) > 0`, want).Scan(&found)
	if err != nil {
		t.Fatalf("query node_content: %v", err)
	}

	return found > 0
}

// assertNoCanary fails if the source's password reached the output or the
// persisted run events.
func assertNoCanary(t *testing.T, path, out string) {
	t.Helper()

	if strings.Contains(out, imageArtifactCanary) {
		t.Errorf("run output carries the source's password:\n%s", out)
	}

	var leaked int

	err := openStateDB(t, path).QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM run_events WHERE instr(text || name || detail || hash, ?) > 0`, imageArtifactCanary).Scan(&leaked)
	if err != nil {
		t.Fatalf("query run_events: %v", err)
	}

	if leaked > 0 {
		t.Errorf("%d run events carry the source's password", leaked)
	}
}

// TestImageArtifactResolvesAfterAResumeKeptTheGet is the seam between #190
// and #151: a resume keeps a get a later step already built on, and skipping
// the fetch must still record the get's source, or an image: naming it later
// in the plan has a digest and no repository to put it on.
func TestImageArtifactResolvesAfterAResumeKeptTheGet(t *testing.T) {
	requireDockerE2E(t)

	digest := alpineDigest(t)
	dir := t.TempDir()
	path := pipelinePath(t, dir)
	marker := filepath.Join(dir, "fixed")

	imageArtifactPipeline(t, path, "alpine", digest, "", fmt.Sprintf(`  - task: first
    run: echo first
  - task: fragile
    run: test -f %s
%s`, marker, imageArtifactTask))

	out := captureStdout(t, func() {
		err := cli.Run([]string{"run", path, "--job", "build"})
		if err == nil {
			t.Fatal("expected the fragile step to fail")
		}
	})

	writePipelineFile(t, marker, "")

	out = captureStdout(t, func() {
		err := cli.Run([]string{"run", path, "--resume", resumeID(t, out)})
		if err != nil {
			t.Fatalf("resume failed: %v", err)
		}
	})

	if !strings.Contains(out, "skip: toolchain (already fetched)") {
		t.Fatalf("the resume fetched the get again, so this test proves nothing about a kept one:\n%s", out)
	}

	if !strings.Contains(out, "image: toolchain → alpine@"+digest) || !strings.Contains(out, "release=3.") {
		t.Errorf("the image step did not run in the kept get's reference:\n%s", out)
	}
}
