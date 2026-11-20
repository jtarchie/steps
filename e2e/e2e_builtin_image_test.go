package e2e

// A resource_types: entry naming a built-in type with only image: is that built-in run in the image (#263), as Concourse runs every resource type in one. alpine:3 has no git, so a command that reached it says so, and the host, which has git, would not.

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinGitTypeRunsInTheImageItIsGiven(t *testing.T) {
	t.Parallel()
	requireDockerE2E(t)
	dockerSlot(t)

	dir := t.TempDir()
	path := writePipeline(t, dir, `
resource_types:
- name: git
  image: alpine:3

resources:
- name: repo
  type: git
  source: {uri: https://example.invalid/acme/app.git}

jobs:
- name: build
  plan:
  - get: repo
`)

	err := stepsRun(t, []string{"run", path, "--job", "build"})
	if err == nil || !strings.Contains(err.Error(), `ls-remote`) || !strings.Contains(err.Error(), `failed in image "alpine:3"`) || !strings.Contains(err.Error(), "status 127") {
		t.Fatalf("run = %v, want the built-in's check to fail inside alpine:3, which has no git", err)
	}
}

func TestGitHubCheckoutGitRunsInTheImageItIsGiven(t *testing.T) {
	t.Parallel()
	requireDockerE2E(t)
	dockerSlot(t)

	fake := newFakeGitHub(t)
	repo := newFakeGitRepo(t)
	fake.serveGit(repo)
	fake.addPR(gitPR(repo))

	dir := t.TempDir()
	marker := filepath.Join(dir, "ok")
	pipeline := "resource_types:\n- name: github-prs\n  image: alpine:3\n" + gitPRPipeline(fake, repo, marker)

	err := stepsRun(t, []string{"run", writePipeline(t, dir, pipeline), "--job", "look"})
	if err == nil || !strings.Contains(err.Error(), `git binary in image "alpine:3"`) {
		t.Fatalf("run = %v, want the clone to fail inside alpine:3, which has no git", err)
	}
}

// An image cannot see this machine's files or run what steps runs in-process, so a built-in given one where it cannot apply is refused at load, naming why: git over a local path (fetch: needs one), and a type that runs no command.
func TestABuiltinIsRefusedAnImageItCannotUse(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()

	for _, tc := range []struct{ name, pipeline, want string }{
		{"local uri", "resource_types:\n- {name: git, image: alpine/git}\nresources:\n- {name: repo, type: git, source: {uri: " + repo + "}}\n", "a path on this machine"},
		{"file uri", "resource_types:\n- {name: git, image: alpine/git}\nresources:\n- {name: repo, type: git, source: {uri: \"file://" + repo + "\"}}\n", "a path on this machine"},
		{"fetch", "resource_types:\n- {name: git, image: alpine/git}\nresources:\n- {name: repo, type: git, source: {uri: " + repo + ", branch: main, fetch: true}}\n", "a path on this machine"},
		{"no command", "resource_types:\n- {name: cron, image: alpine:3}\n", "image: cannot apply to cron"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writePipeline(t, t.TempDir(), tc.pipeline+"jobs:\n- name: build\n  plan:\n  - task: noop\n    run: \"true\"\n")

			err := stepsRun(t, []string{"validate", "--syntax-only", path})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}

	path := writePipeline(t, t.TempDir(), "resource_types:\n- {name: git, image: alpine/git}\nresources:\n- {name: repo, type: git, source: {uri: \"https://example.invalid/acme/app.git\"}}\njobs:\n- name: build\n  plan:\n  - get: repo\n")

	err := stepsRun(t, []string{"validate", "--syntax-only", path})
	if err != nil {
		t.Fatalf("a remote uri in an image was refused: %v", err)
	}
}
