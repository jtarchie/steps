package e2e

import (
	"slices"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// labelPipeline moves issue 12 along a factory board. The issue comes from a custom type whose version carries only a number, because any input that names one addresses it, not just the github-* finders; params is the put's params: block.
func labelPipeline(fake *fakeGitHub, params string) string {
	return `
resource_types:
- name: queue
  config:
    check: echo '[{"number":"12"}]'
    in: "true"
resources:
- name: issue
  type: queue
  source: {}
- name: state
  type: github-label
  source:
    repo: ` + fakeRepo + `
    endpoint: ` + fake.URL() + `
jobs:
- name: claim
  plan:
  - get: issue
  - put: state
    inputs: [issue]
    params:` + params + `
`
}

const labelSwap = `
      add: [factory:building]
      remove: [factory:queued, needs/review, factory:failed]`

// TestEndToEndGitHubLabelSwapsState: one put moves the issue from queued to building, takes off a label whose name needs escaping in the route, finds factory:failed already gone and calls that success, and leaves the label nobody named where someone else put it. A second run, a replay, finds the board as asked.
func TestEndToEndGitHubLabelSwapsState(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.addLabeledIssue(12, "priority:high", "factory:queued", "needs/review")

	path := writePipeline(t, t.TempDir(), labelPipeline(fake, labelSwap))

	for range 2 {
		mustRun(t, "run", path, "--job", "claim")

		if got, want := fake.labelsOn(12), []string{"priority:high", "factory:building"}; !slices.Equal(got, want) {
			t.Errorf("labels on #12 = %v, want %v", got, want)
		}
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if fake.requests != 8 {
		t.Errorf("GitHub saw %d requests, want 8: one add and three removes per run, so the second run really replayed against a board already as asked", fake.requests)
	}
}

// TestEndToEndGitHubLabelLeavesBothOnARefusal: add goes before remove, so a put whose remove is refused leaves the issue with both states rather than neither, and fails naming GitHub's status, its words and the route.
func TestEndToEndGitHubLabelLeavesBothOnARefusal(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.addLabeledIssue(12, "factory:queued")
	fake.mu.Lock()
	fake.refuseLabelRemoval = true
	fake.mu.Unlock()

	err := cli.Run([]string{"run", writePipeline(t, t.TempDir(), labelPipeline(fake, labelSwap)), "--job", "claim"})
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "Resource not accessible") ||
		!strings.Contains(err.Error(), "DELETE /repos/acme/app/issues/12/labels/factory:queued") {
		t.Fatalf("run = %v, want the put failed naming the 403, GitHub's message and the route", err)
	}

	if got, want := fake.labelsOn(12), []string{"factory:queued", "factory:building"}; !slices.Equal(got, want) {
		t.Errorf("labels on #12 = %v, want %v: both states, never neither", got, want)
	}
}

// TestEndToEndGitHubLabelFailsOnAMissingIssue: GitHub answers 404 both for a label that is not on the issue and for an issue that is not there. Only the first is the world as asked; a remove on an issue that does not exist is a mistake in the pipeline, and fails.
func TestEndToEndGitHubLabelFailsOnAMissingIssue(t *testing.T) {
	fake := newFakeGitHub(t)

	err := cli.Run([]string{"run", writePipeline(t, t.TempDir(), labelPipeline(fake, "\n      remove: [factory:queued]")), "--job", "claim"})
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "DELETE /repos/acme/app/issues/12/labels/factory:queued") {
		t.Fatalf("run = %v, want the put failed naming the 404 and the route", err)
	}
}

// TestValidateGitHubLabelRefusals: what is wrong with a label put without asking GitHub is a load error.
func TestValidateGitHubLabelRefusals(t *testing.T) {
	fake := newFakeGitHub(t)

	for name, tc := range map[string]struct{ params, want string }{
		"nothing to do":      {"\n      from: issue", "params.add or params.remove is required"},
		"both ways":          {"\n      add: [Factory:Queued]\n      remove: [factory:queued]", `"factory:queued" is in both params.add and params.remove`},
		"from a non-input":   {"\n      add: [x]\n      from: elsewhere", `params.from: "elsewhere" is not a get among this put's inputs (issue)`},
		"from and number":    {"\n      add: [x]\n      from: issue\n      number: '12'", "params.from and params.number both say which"},
		"an empty name":      {"\n      remove: ['  ']", "params.remove: a label name cannot be empty"},
		"a number not a one": {"\n      add: [x]\n      number: '12/../13'", `params.number: "12/../13" is not an issue or pull request number`},
	} {
		t.Run(name, func(t *testing.T) {
			err := cli.Run([]string{"validate", writePipeline(t, t.TempDir(), labelPipeline(fake, tc.params))})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("validate = %v, want %q", err, tc.want)
			}
		})
	}

	get := strings.Replace(labelPipeline(fake, labelSwap), "  - put: state\n", "  - get: state\n  - put: state\n", 1)

	err := cli.Run([]string{"validate", writePipeline(t, t.TempDir(), get)})
	if err == nil || !strings.Contains(err.Error(), "only publishes") {
		t.Errorf("validate = %v, want a get of a github-label refused", err)
	}

	noInputs := strings.Replace(labelPipeline(fake, "\n      add: [x]"), "    inputs: [issue]\n", "", 1)

	err = cli.Run([]string{"validate", writePipeline(t, t.TempDir(), noInputs)})
	if err == nil || !strings.Contains(err.Error(), "no get among its inputs names the issue or pull request") {
		t.Errorf("validate = %v, want a put with nothing naming its issue refused", err)
	}

	byNumber := strings.Replace(labelPipeline(fake, "\n      add: [x]\n      number: '12'"), "    inputs: [issue]\n", "", 1)

	err = cli.Run([]string{"validate", writePipeline(t, t.TempDir(), byNumber)})
	if err != nil {
		t.Errorf("validate = %v, want a put naming its issue by number to load with no inputs", err)
	}
}

// TestEndToEndGitHubLabelAcrossNumbers: an across: put names each cell's issue with a rendered params.number, which load cannot read as digits yet; the put checks the number it rendered to, so a cell rendering a path is refused before it reaches the route.
func TestEndToEndGitHubLabelAcrossNumbers(t *testing.T) {
	pipeline := func(fake *fakeGitHub, values string) string {
		return `
resources:
- name: state
  type: github-label
  source:
    repo: ` + fakeRepo + `
    endpoint: ` + fake.URL() + `
jobs:
- name: triage
  plan:
  - across:
    - var: n
      values: ` + values + `
    put: state
    params:
      number: "{{ .vars.n }}"
      add: [triaged]
`
	}

	fake := newFakeGitHub(t)
	fake.addLabeledIssue(12)
	fake.addLabeledIssue(13)

	mustRun(t, "run", writePipeline(t, t.TempDir(), pipeline(fake, `["12", "13"]`)), "--job", "triage")

	for _, number := range []int{12, 13} {
		if got := fake.labelsOn(number); !slices.Equal(got, []string{"triaged"}) {
			t.Errorf("labels on #%d = %v, want [triaged]", number, got)
		}
	}

	err := cli.Run([]string{"run", writePipeline(t, t.TempDir(), pipeline(fake, `["12/../13"]`)), "--job", "triage"})
	if err == nil || !strings.Contains(err.Error(), `params.number: "12/../13" is not`) {
		t.Fatalf("run = %v, want the rendered number refused", err)
	}
}
