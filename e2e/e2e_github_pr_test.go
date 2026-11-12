package e2e

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

const prHeadSHA = "feedfeedfeedfeedfeedfeedfeedfeedfeedfeed"

// prPublishPipeline writes a title and a body, then puts them to a github-pr resource; params is the rest of the put's params, and files what the task writes ("" for neither).
func prPublishPipeline(fake *fakeGitHub, title, body, params string) string {
	var writes strings.Builder

	if title != "" {
		fmt.Fprintf(&writes, "printf '%%s\\n' '%s' > files/title\n      ", title)
	}

	if body != "" {
		fmt.Fprintf(&writes, "printf '%%s\\n' '%s' > files/body.md\n      ", body)
	}

	return `
resources:
- name: pr
  type: github-pr
  source:
    repo: ` + fakeRepo + `
    endpoint: ` + fake.URL() + `
jobs:
- name: publish
  plan:
  - task: write
    outputs: [files]
    run: |
      mkdir -p files
      ` + writes.String() + `true
  - put: pr
    inputs: [files]
    params:
      head: feature/widgets
` + params
}

const prTitleBody = "      title_file: files/title\n      body_file: files/body.md\n"

// TestEndToEndGitHubPROpensOne: a pushed branch with no open pull request gets one, from the files, onto the repository's default branch, and the put records its number and head commit.
func TestEndToEndGitHubPROpensOne(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.defaultBranch = "trunk"
	fake.pushBranch("feature/widgets", prHeadSHA)

	path := writePipeline(t, t.TempDir(), prPublishPipeline(fake, "Add widgets", "Widgets, plainly.", prTitleBody+"      draft: true\n      labels: [factory]\n      assignees: [alice]\n"))
	mustRun(t, "run", path, "--job", "publish")

	opened := fake.openedPRs()
	if len(opened) != 1 {
		t.Fatalf("opened %+v, want one pull request", opened)
	}

	got := opened[0]
	if got.Title != "Add widgets" || got.Body != "Widgets, plainly.\n" || got.Head != "feature/widgets" || got.Base != "trunk" || !got.Draft {
		t.Errorf("opened %+v, want the files' title and body, from feature/widgets onto the default branch trunk, as a draft", got)
	}

	if !slices.Equal(got.Labels, []string{"factory"}) || !slices.Equal(got.Assignees, []string{"alice"}) {
		t.Errorf("labels %v assignees %v, want [factory] [alice]", got.Labels, got.Assignees)
	}

	if shas := recordedPRVersions(t, path); !slices.Equal(shas, []string{strconv.Itoa(got.Number) + "@" + prHeadSHA}) {
		t.Errorf("recorded versions %v, want {number: %d, sha: %s}", shas, got.Number, prHeadSHA)
	}
}

// recordedPRVersions is each version the puts to pr recorded, as number@sha and sorted.
func recordedPRVersions(t *testing.T, path string) []string {
	t.Helper()

	versions := putVersions(t, path, "pr")
	recorded := make([]string, 0, len(versions))

	for _, version := range versions {
		recorded = append(recorded, fmt.Sprint(version["number"])+"@"+fmt.Sprint(version["sha"]))
	}

	slices.Sort(recorded)

	return recorded
}

// TestEndToEndGitHubPRUpdatesTheOpenOne: a second put for the same branch edits the pull request the first opened rather than opening another; its draft state, labels and assignees are not touched.
func TestEndToEndGitHubPRUpdatesTheOpenOne(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.pushBranch("feature/widgets", prHeadSHA)

	dir := t.TempDir()
	path := writePipeline(t, dir, prPublishPipeline(fake, "Add widgets", "First body.", prTitleBody+"      draft: true\n      labels: [factory]\n"))
	mustRun(t, "run", path, "--job", "publish")

	fake.movePush("feature/widgets", "beadbeadbeadbeadbeadbeadbeadbeadbeadbead")

	err := os.WriteFile(path, []byte(prPublishPipeline(fake, "Add widgets, v2", "Second body.", prTitleBody+"      draft: false\n      labels: [other]\n")), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, "run", path, "--job", "publish")

	opened := fake.openedPRs()
	if len(opened) != 1 {
		t.Fatalf("opened %+v, want the same single pull request", opened)
	}

	if got := opened[0]; got.Title != "Add widgets, v2" || got.Body != "Second body.\n" || !got.Draft || !slices.Equal(got.Labels, []string{"factory"}) {
		t.Errorf("after the second put %+v, want the new title and body on a still-draft pull request labelled only factory", got)
	}

	if got, want := recordedPRVersions(t, path), []string{"1@beadbeadbeadbeadbeadbeadbeadbeadbeadbead", "1@" + prHeadSHA}; !slices.Equal(got, want) {
		t.Errorf("recorded versions %v, want %v: the head as GitHub reported it at each put", got, want)
	}
}

// TestEndToEndGitHubPRWithNothingToSayChangesNothing: an existing pull request and no title_file or body_file is left alone, not sent an empty PATCH.
func TestEndToEndGitHubPRWithNothingToSayChangesNothing(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.addPR(fakePR{Number: 5, SHA: prHeadSHA, Title: "Hand written", Body: "mine", Author: "alice", Base: "main", Head: "feature/widgets"})

	path := writePipeline(t, t.TempDir(), prPublishPipeline(fake, "", "", ""))
	mustRun(t, "run", path, "--job", "publish")

	if got := fake.prNamed(5); got.Title != "Hand written" || got.Body != "mine" || fake.patchCount() != 0 {
		t.Errorf("pull request %+v after %d patches, want it untouched", got, fake.patchCount())
	}

	if versions := putVersions(t, path, "pr"); len(versions) != 1 || fmt.Sprint(versions[0]["number"]) != "5" {
		t.Errorf("recorded versions %v, want the existing #5", versions)
	}
}

// TestEndToEndGitHubPRReadyFlipsADraft: ready: true takes a draft out of draft. The pull request is opened as a draft by one put and flipped by the next, which is how a pipeline that reviews before it publishes uses it.
func TestEndToEndGitHubPRReadyFlipsADraft(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.pushBranch("feature/widgets", prHeadSHA)

	path := writePipeline(t, t.TempDir(), prPublishPipeline(fake, "Add widgets", "Body.", prTitleBody+"      draft: true\n"))
	mustRun(t, "run", path, "--job", "publish")

	if opened := fake.openedPRs(); len(opened) != 1 || !opened[0].Draft {
		t.Fatalf("opened %+v, want one draft", opened)
	}

	err := os.WriteFile(path, []byte(prPublishPipeline(fake, "", "", "      ready: true\n")), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, "run", path, "--job", "publish")

	if opened := fake.openedPRs(); len(opened) != 1 || opened[0].Draft {
		t.Errorf("after ready: true %+v, want the one pull request out of draft", opened)
	}

	if fake.patchCount() != 0 {
		t.Errorf("%d patches, want none: ready is a GraphQL mutation, not an edit", fake.patchCount())
	}
}

// TestEndToEndGitHubPRRefusesToOpenWithoutAField: opening needs both files, and the failure names the one missing, before anything is sent.
func TestEndToEndGitHubPRRefusesToOpenWithoutAField(t *testing.T) {
	for field, params := range map[string]string{
		"params.title_file": "      body_file: files/body.md\n",
		"params.body_file":  "      title_file: files/title\n",
	} {
		fake := newFakeGitHub(t)
		fake.pushBranch("feature/widgets", prHeadSHA)

		path := writePipeline(t, t.TempDir(), prPublishPipeline(fake, "Add widgets", "Body.", params))

		err := cli.Run([]string{"run", path, "--job", "publish"})
		if err == nil || !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "feature/widgets") {
			t.Errorf("%s: run = %v, want the put refused naming the field and the branch with no pull request", field, err)
		}

		if opened := fake.openedPRs(); len(opened) != 0 {
			t.Errorf("%s: opened %+v, want nothing", field, opened)
		}
	}
}

// TestEndToEndGitHubPRRefusesToRetarget: a base that is not the open pull request's fails the put and changes nothing, including a title it was also given.
func TestEndToEndGitHubPRRefusesToRetarget(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.addPR(fakePR{Number: 5, SHA: prHeadSHA, Title: "old", Author: "alice", Base: "develop", Head: "feature/widgets"})

	path := writePipeline(t, t.TempDir(), prPublishPipeline(fake, "New title", "Body.", prTitleBody+"      base: main\n"))

	err := cli.Run([]string{"run", path, "--job", "publish"})
	if err == nil || !strings.Contains(err.Error(), "params.base") || !strings.Contains(err.Error(), "develop") {
		t.Fatalf("run = %v, want the put refused naming params.base and the pull request's own base", err)
	}

	if got := fake.prNamed(5); got.Title != "old" || got.Base != "develop" || fake.patchCount() != 0 {
		t.Errorf("pull request %+v after %d patches, want it untouched", got, fake.patchCount())
	}
}

// TestEndToEndGitHubPRIgnoresAClosedOne: only an open pull request is the branch's; a closed one does not stop a new one opening.
func TestEndToEndGitHubPRIgnoresAClosedOne(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.pushBranch("feature/widgets", prHeadSHA)
	fake.addPR(fakePR{Number: 5, SHA: prHeadSHA, Title: "old", Author: "alice", Base: "main", Head: "feature/widgets", Closed: true})

	path := writePipeline(t, t.TempDir(), prPublishPipeline(fake, "Again", "Body.", prTitleBody))
	mustRun(t, "run", path, "--job", "publish")

	if opened := fake.openedPRs(); len(opened) != 1 || opened[0].Number != 6 {
		t.Errorf("opened %+v, want a new #6 beside the closed #5", opened)
	}
}
