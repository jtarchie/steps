package e2e

// examples/factory.yml (steps#139), run as the file a reader would run: every agent pointed at a scripted provider, every github-* resource at the fake GitHub, and the repository at a local one. What only a real run can show — whether the decomposition is any good — is not asserted; the seams are: an epic is decomposed once, a proposed issue builds nothing until a person promotes it, and an answer to a review lands on the branch it was asked on.

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
)

// factoryAgainstFakes writes examples/factory.yml into dir aimed at the fakes. The CLI-only settings: line goes, because the scripted provider is a hosted one and the field means nothing there.
func factoryAgainstFakes(t *testing.T, dir, llm, github, repo string) string {
	t.Helper()

	body, err := os.ReadFile(repoFile("examples", "factory.yml"))
	if err != nil {
		t.Fatal(err)
	}

	const named = "    repo: ((repo))\n"
	if !strings.Contains(string(body), named) {
		t.Fatalf("examples/factory.yml names no %q to point at the fake GitHub", strings.TrimSpace(named))
	}

	pointed := strings.ReplaceAll(string(body), named, named+"    endpoint: "+github+"\n")
	pointed = strings.ReplaceAll(pointed, "https://github.com/((repo)).git", repo)
	pointed = strings.ReplaceAll(pointed, "  settings: project\n", "")
	pointed = strings.ReplaceAll(pointed, "    token_env: GH_TOKEN\n", "")

	return writePipeline(t, dir, injectFakeProvider(t, pointed, llm, "", false))
}

func runFactory(t *testing.T, path, job string) error {
	t.Helper()

	_, err := runAndCapture(t, "run", path, "--job", job, "--var", "repo="+fakeRepo)

	return err
}

// decomposerScript cuts any epic into two pieces, writing them on the second message as the example asks.
func decomposerScript(req capturedRequest) turn {
	if modelHasCalled(req, "write_file") {
		return says("written")
	}

	if !requestMentions(req, "check the cut") {
		return says("two pieces: the schema, then the api")
	}

	return callsTools(
		call("write_file", map[string]any{"path": "steps-work/issues/index.json", "content": `["01-schema","02-api"]`}),
		call("write_file", map[string]any{"path": "steps-work/issues/01-schema.title", "content": "Add the cache table\n"}),
		call("write_file", map[string]any{"path": "steps-work/issues/01-schema.md", "content": "## What\nA table.\n## Why\nTo cache.\n## Notes\nNone.\n"}),
		call("write_file", map[string]any{"path": "steps-work/issues/02-api.title", "content": "Serve the cache\n"}),
		call("write_file", map[string]any{"path": "steps-work/issues/02-api.md", "content": "## What\nAn endpoint.\n## Why\nTo read it.\n## Notes\nNone.\n"}),
	)
}

func TestFactoryDecomposesAnEpicIntoProposedIssues(t *testing.T) {
	t.Setenv("GH_TOKEN", fakeGitHubToken)
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	github := newFakeGitHub(t)
	github.addIssue(fakeIssue{Number: 7, Title: "Cache everything", Body: "Cache the widgets, then serve them.", Author: "jt", Labels: []string{"epic"}})

	fake := newRoutedFakeLLM(t, decomposerScript)
	path := factoryAgainstFakes(t, t.TempDir(), fake.URL, github.URL(), fixtureRepo(t, "app\n"))

	err := runFactory(t, path, "decompose")
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}

	created := github.createdIssues()
	if len(created) != 2 {
		t.Fatalf("opened %d issues, want the two the decomposer wrote: %+v", len(created), created)
	}

	for _, issue := range created {
		if !strings.HasSuffix(strings.TrimSpace(issue.Body), "Part of #7") {
			t.Errorf("issue %q does not end naming its epic:\n%s", issue.Title, issue.Body)
		}

		if labels := github.labelsOn(issue.Number); len(labels) != 1 || labels[0] != "self-build/proposed" {
			t.Errorf("issue %q is labeled %v, want only self-build/proposed: anything else builds before a person promotes it", issue.Title, labels)
		}
	}

	if posted := github.postedComments(); len(posted) == 0 || posted[0].Number != 7 || !strings.HasPrefix(posted[0].Body, "factory: decomposing") {
		t.Errorf("the epic was not marked before its issues were opened: %+v", posted)
	}
}

// An epic the factory already marked is refused before the model is asked anything, so a relabel or a retry cannot open a second set of children.
func TestFactoryDecomposesAnEpicOnce(t *testing.T) {
	t.Setenv("GH_TOKEN", fakeGitHubToken)
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	github := newFakeGitHub(t)
	github.addIssue(fakeIssue{Number: 7, Title: "Cache everything", Body: "Cache the widgets.", Author: "jt", Labels: []string{"epic"}})
	github.addComment(fakeComment{ID: 901, Number: 7, Kind: "issue", Author: "steps-bot", Body: "factory: decomposing this epic into proposed issues (run `X`).", Updated: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)})

	fake := newRoutedFakeLLM(t, decomposerScript)
	path := factoryAgainstFakes(t, t.TempDir(), fake.URL, github.URL(), fixtureRepo(t, "app\n"))

	printed, err := runAndCapture(t, "run", path, "--job", "decompose", "--var", "repo="+fakeRepo)
	if err == nil || !strings.Contains(printed, "was already decomposed") {
		t.Fatalf("decompose over an epic already decomposed: err=%v, want the once guard's refusal:\n%s", err, printed)
	}

	if n := fake.requestCount(); n != 0 {
		t.Errorf("the model was asked %d times about an epic already decomposed", n)
	}

	if created := github.createdIssues(); len(created) != 0 {
		t.Errorf("opened %d issues for an epic already decomposed", len(created))
	}

	err = cli.Run([]string{"validate", path, "--var", "repo=" + fakeRepo})
	if err != nil {
		t.Errorf("the pointed example no longer validates: %v", err)
	}
}

// builderScript plays planner, implementer and reviewer, told apart by their standing instructions; each acts once and then answers.
func builderScript(req capturedRequest) turn {
	system := req.systemMessage()

	switch {
	case modelHasCalled(req, "verdict"):
		return says("decided")
	case strings.Contains(system, "You review"):
		if !modelHasCalled(req, "write_file") {
			return callsTool("write_file", map[string]any{"path": "steps-work/pr-body.md", "content": "## What\nA cache.\n## Why\nSpeed.\n## Notes\nRan the checks.\n"})
		}

		return callsTool("verdict", map[string]any{"choice": "approve", "note": "closes the issue"})
	case modelHasCalled(req, "write_file"):
		return says("done")
	case strings.Contains(system, "You plan"):
		if !requestMentions(req, "write the corrected plan") {
			return says("a cache keyed on content")
		}

		return callsTool("write_file", map[string]any{"path": "steps-work/plan.md", "content": "Add cache.txt.\n"})
	case strings.Contains(system, "You change code"):
		return callsTool("write_file", map[string]any{"path": "cache.txt", "content": requestedChange(req) + "\n"})
	default:
		return says("nothing asked of me")
	}
}

// requestedChange is what the implementer writes: the review command when it was asked one, so the commit shows which request it answers.
func requestedChange(req capturedRequest) string {
	// context_paths: arrive as a read_file exchange ahead of the first message, not as user text.
	if requestMentions(req, "/factory") || req.toolResultContains("/factory") {
		return "changed as asked"
	}

	return "cached"
}

// TestFactoryBuildsAPromotedIssueAndNothingProposed: a proposed issue is older and would be built first if self-build/proposed matched the filter, so the pull request naming the promoted one is the proof it did not; it ends one branch and one pull request, marked ready, carrying the plan comment on the issue.
func TestFactoryBuildsAPromotedIssueAndNothingProposed(t *testing.T) {
	t.Setenv("GH_TOKEN", fakeGitHubToken)
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	github := newFakeGitHub(t)
	github.addIssue(fakeIssue{Number: 9, Title: "Proposed, not promoted", Body: "Part of #7", Author: "steps-bot", Labels: []string{"self-build/proposed"}})
	github.addIssue(fakeIssue{Number: 12, Title: "Add the cache", Body: "Cache the widgets.\n\nPart of #7", Author: "jt", Labels: []string{"self-build"}})
	// The fake opens a pull request only for a branch it was told was pushed; the push itself lands in the local repository below.
	github.pushBranch("issue-12", "pushed-by-the-run")

	repo := fixtureRepo(t, "app\n")
	fake := newRoutedFakeLLM(t, builderScript)
	path := factoryAgainstFakes(t, t.TempDir(), fake.URL, github.URL(), repo)

	printed, err := runAndCapture(t, "run", path, "--job", "implement", "--var", "repo="+fakeRepo)
	if err != nil {
		t.Fatalf("implement: %v\n%s", err, printed)
	}

	opened := github.openedPRs()
	if len(opened) != 1 || opened[0].Head != "issue-12" {
		t.Fatalf("opened %+v, want one pull request from issue-12: a proposed issue must build nothing", opened)
	}

	pr := github.prNamed(opened[0].Number)
	if pr.Draft || !strings.Contains(pr.Body, "Closes #12") || !strings.Contains(pr.Body, "## What") {
		t.Errorf("pull request = %+v, want it ready, described by the reviewer, closing #12", pr)
	}

	if got := gitOut(t, repo, "show", "issue-12:cache.txt"); got != "cached" {
		t.Errorf("issue-12 holds cache.txt = %q, want the implementer's file pushed", got)
	}

	assertPlanPostedOnlyOn(t, github, 12, 9)
}

// assertPlanPostedOnlyOn: the plan reached the issue being built, and nothing at all reached the proposed one.
func assertPlanPostedOnlyOn(t *testing.T, github *fakeGitHub, built, proposed int) {
	t.Helper()

	var plan bool

	for _, posted := range github.postedComments() {
		plan = plan || (posted.Number == built && strings.Contains(posted.Body, "Add cache.txt."))
		if posted.Number == proposed {
			t.Errorf("the proposed issue was commented on: %q", posted.Body)
		}
	}

	if !plan {
		t.Errorf("the plan was not posted on #%d: %+v", built, github.postedComments())
	}
}

// TestFactoryAnswersAReviewOnTheSameBranch: a /factory command on a factory pull request becomes a commit on that pull request's own branch, answered where it was asked and marked done — and the answer is not itself a command, or the next check would build it.
func TestFactoryAnswersAReviewOnTheSameBranch(t *testing.T) {
	t.Setenv("GH_TOKEN", fakeGitHubToken)
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	repo, before := repoWithBranch(t, "issue-12")

	github := newFakeGitHub(t)
	github.addPR(fakePR{Number: 13, SHA: before, Title: "Add the cache", Author: "octocat", Base: "main", Head: "issue-12", Labels: []string{"factory"}})
	github.addComment(fakeComment{ID: 1301, Number: 13, Kind: "conversation", Author: "jt", Body: "/factory say what was asked", Updated: time.Now().UTC().Add(-time.Minute)})

	fake := newRoutedFakeLLM(t, builderScript)
	path := factoryAgainstFakes(t, t.TempDir(), fake.URL, github.URL(), repo)

	printed, err := runAndCapture(t, "run", path, "--job", "respond", "--var", "repo="+fakeRepo)
	if err != nil {
		t.Fatalf("respond: %v\n%s", err, printed)
	}

	if opened := github.openedPRs(); len(opened) != 0 {
		t.Errorf("respond opened %+v, want no new pull request", opened)
	}

	after := gitOut(t, repo, "rev-parse", "issue-12")
	if after == before || gitOut(t, repo, "show", "issue-12:cache.txt") != "changed as asked" {
		t.Fatalf("issue-12 is %s (was %s), want the requested change committed on it", after, before)
	}

	if gitOut(t, repo, "rev-parse", "issue-12~1") != before {
		t.Errorf("the change was not committed on top of the branch's own work")
	}

	assertOneReplyNaming(t, github, 13, after)

	if reactions := github.reactionsOn(1301); len(reactions) != 1 || reactions[0].Content != "rocket" {
		t.Errorf("reactions on the command = %+v, want the rocket that marks it answered", reactions)
	}
}

// repoWithBranch is a local repository whose branch holds one commit of its own on top of main, as a factory pull request's branch does; it answers the repository and that commit.
func repoWithBranch(t *testing.T, branch string) (string, string) {
	t.Helper()

	repo := fixtureRepo(t, "app\n")
	gitOut(t, repo, "checkout", "-q", "-b", branch)

	err := os.WriteFile(repo+"/cache.txt", []byte("cached\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	gitOut(t, repo, "add", "cache.txt")
	gitOut(t, repo, "commit", "-q", "-m", "Add the cache")
	head := gitOut(t, repo, "rev-parse", branch)
	gitOut(t, repo, "checkout", "-q", "main")

	return repo, head
}

// assertOneReplyNaming: the command got one answer, naming the commit, and the answer is not a command itself.
func assertOneReplyNaming(t *testing.T, github *fakeGitHub, number int, sha string) {
	t.Helper()

	var replies []string

	for _, posted := range github.postedComments() {
		if posted.Number == number {
			replies = append(replies, posted.Body)
		}
	}

	if len(replies) != 1 || !strings.Contains(replies[0], "Addressed in "+sha) {
		t.Fatalf("replies on #%d = %q, want one naming %s", number, replies, sha)
	}

	if strings.HasPrefix(replies[0], "/factory") {
		t.Errorf("the factory's reply is itself a command, so it would trigger respond again: %q", replies[0])
	}
}
