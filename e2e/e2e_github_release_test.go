package e2e

import (
	"strings"
	"testing"
	"time"
)

// nodeReleases is a nodejs/node-shaped release list: several major lines at once, a prerelease, a draft, and an alpha GitHub does not flag as a prerelease, which only the constraint keeps out.
func nodeReleases() []fakeRelease {
	at := func(day int) time.Time { return time.Date(2026, 11, day, 16, 44, 0, 0, time.UTC) }

	return []fakeRelease{
		{ID: 1, Tag: "v24.20.0", Published: at(1)},
		{ID: 2, Tag: "v22.23.5", Published: at(11)},
		{ID: 3, Tag: "v24.22.3", Published: at(11), Annotated: true},
		{ID: 4, Tag: "v27.0.0-alpha.0.1.1", Published: at(17)},
		{ID: 5, Tag: "v24.23.0-rc.1", Published: at(18), Prerelease: true},
		{ID: 6, Tag: "v24.24.0", Draft: true},
	}
}

// rubyReleases is ruby/ruby's two spellings of a version: v4.0.10 since 4.0, v3_4_13 before it, and a preview.
func rubyReleases() []fakeRelease {
	at := func(day int) time.Time { return time.Date(2026, 11, day, 23, 0, 0, 0, time.UTC) }

	return []fakeRelease{
		{ID: 11, Tag: "v3_4_9", Published: at(1)},
		{ID: 12, Tag: "v4.1.0-preview1", Published: at(2), Prerelease: true},
		{ID: 13, Tag: "v3_4_13", Published: at(18)},
		{ID: 14, Tag: "v4.0.10", Published: at(18)},
	}
}

// releasePipeline follows one major line of a project, one build per release.
const releasePipeline = `
resources:
- name: node
  type: github-releases
  source:
    repo: ` + fakeRepo + `
    endpoint: ENDPOINT
    semver_constraint: "~24"
jobs:
- name: build
  plan:
  - get: node
    trigger: true
    version: every
  - task: record
    inputs: [node]
    run: echo "$(cat node/version)@$(cat node/commit_sha)" >> PROCESSED
`

// TestWatchGitHubReleaseFollowsAMajorLine: the first poll builds the newest release on the line and nothing older, as Concourse's first check reports only the latest; each later poll builds what is newer by version than the one it last saw. A patch to an older line, a prerelease, a draft and a release below the cursor never build; an annotated tag resolves to its commit.
func TestWatchGitHubReleaseFollowsAMajorLine(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.addReleases(nodeReleases()...)

	fixture := newWatchFixture(t, strings.ReplaceAll(releasePipeline, "ENDPOINT", fake.URL()))
	fixture.resources = []string{"node"}
	fixture.watch(t)
	fixture.assertDid(t, "24.22.3@"+releaseSHA(3))

	fake.addRelease(fakeRelease{ID: 7, Tag: "v24.22.4", Published: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)})
	fake.addRelease(fakeRelease{ID: 8, Tag: "v22.23.6", Published: time.Date(2026, 11, 20, 9, 1, 0, 0, time.UTC)})
	fake.addRelease(fakeRelease{ID: 9, Tag: "v24.21.1", Published: time.Date(2026, 11, 20, 9, 2, 0, 0, time.UTC)})
	fixture.watch(t)
	fixture.assertDid(t, "24.22.3@"+releaseSHA(3), "24.22.4@"+releaseSHA(7))
}

// TestEndToEndGitHubReleaseGetWritesConcourseFiles: a get writes the files Concourse's github-release writes, under the same names, so a task ported from a Concourse pipeline reads them unchanged; ordered by time, the newest published release is the one fetched whatever its version.
func TestEndToEndGitHubReleaseGetWritesConcourseFiles(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.addReleases(nodeReleases()...)
	fake.setRelease(2, func(release *fakeRelease) {
		release.Name, release.Body = "2026-11-11, Version 22.23.5 (LTS)", "Security release."
	})
	fake.setRelease(2, func(release *fakeRelease) { release.Published = time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC) })

	path := writePipeline(t, t.TempDir(), `
resources:
- name: node
  type: github-releases
  source:
    repo: `+fakeRepo+`
    endpoint: `+fake.URL()+`
    order_by: time
jobs:
- name: read
  plan:
  - get: node
  - task: show
    inputs: [node]
    run: |
      cd node
      echo "$(cat tag) $(cat version) $(cat timestamp) $(cat url) $(cat commit_sha) $(cat body)"
      test -s release.json
    assert:
      stdout: "v22.23.5 22.23.5 2026-11-19T00:00:00Z https://github.example/acme/app/releases/tag/v22.23.5 `+releaseSHA(2)+` Security release."
`)

	mustRun(t, "run", path, "--job", "read")
}
