package e2e

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// issueFanOut is a task that writes one title and body per child it lists, and an across put that opens an issue for each; title is the shell that writes every title file.
func issueFanOut(fake *fakeGitHub, title string) string {
	return `
resources:
- name: issues
  type: github-issue
  source:
    repo: ` + fakeRepo + `
    endpoint: ` + fake.URL() + `
jobs:
- name: split
  plan:
  - task: plan
    outputs: [children]
    run: |
      printf '["login","search","docs"]' > children/items.json
      for c in login search docs; do
        ` + title + `
        printf 'Part of #7: %s\n' "$c" > children/$c.md
      done
  - across:
    - var: child
      from_file: children/items.json
    put: issues
    inputs: [children]
    params:
      title_file: children/{{ .vars.child }}.title
      body_file: children/{{ .vars.child }}.md
      labels: [factory, "{{ .vars.child }}"]
      assignees: [alice]
`
}

// TestEndToEndGitHubIssueOpensOnePerCell: a step decides the children, and each across cell opens its own issue, numbered after everything the repository already holds.
func TestEndToEndGitHubIssueOpensOnePerCell(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.addPR(reviewablePR())
	fake.addComment(fakeComment{ID: 501, Number: 12, Kind: "issue", Author: "alice", Body: "an old issue", Updated: time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)})

	path := writePipeline(t, t.TempDir(), issueFanOut(fake, `printf 'Fix %s\n' "$c" > children/$c.title`))
	mustRun(t, "run", path, "--job", "split")

	issues := fake.createdIssues()
	got := make([]string, 0, len(issues))

	for _, issue := range issues {
		got = append(got, fmt.Sprintf("#%d %q %q %v %v", issue.Number, issue.Title, issue.Body, issue.Labels, issue.Assignees))
	}

	want := []string{
		`#13 "Fix login" "Part of #7: login\n" [factory login] [alice]`,
		`#14 "Fix search" "Part of #7: search\n" [factory search] [alice]`,
		`#15 "Fix docs" "Part of #7: docs\n" [factory docs] [alice]`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("issues opened:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	versions := putVersions(t, path, "issues")
	numbers := make([]string, 0, len(versions))

	for _, version := range versions {
		if version["id"] == nil || version["id"] == "" {
			t.Errorf("version %v carries no id", version)
		}

		numbers = append(numbers, fmt.Sprint(version["number"]))
	}

	slices.Sort(numbers)

	if !slices.Equal(numbers, []string{"13", "14", "15"}) {
		t.Errorf("recorded versions number %v, want the three issues opened", numbers)
	}
}

// TestEndToEndGitHubIssueRefusesAMultiLineTitle: a title GitHub would take whole is cut by no one, so the put fails before anything is opened.
func TestEndToEndGitHubIssueRefusesAMultiLineTitle(t *testing.T) {
	fake := newFakeGitHub(t)

	path := writePipeline(t, t.TempDir(), issueFanOut(fake, `printf 'Fix %s\nand more\n' "$c" > children/$c.title`))

	err := cli.Run([]string{"run", path, "--job", "split"})
	if err == nil || !strings.Contains(err.Error(), "params.title_file") || !strings.Contains(err.Error(), "one line") {
		t.Fatalf("run = %v, want the put refused naming params.title_file and its lines", err)
	}

	if issues := fake.createdIssues(); len(issues) != 0 {
		t.Errorf("opened %+v, want nothing", issues)
	}
}

// putVersions is every version the puts to resource recorded in its history.
func putVersions(t *testing.T, path, resource string) []map[string]any {
	t.Helper()

	st, err := sqlite.OpenExisting(string(cli.StatePath(path, "")), cli.PipelineName(path))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	orders, err := st.VersionOrders(context.Background(), resource)
	if err != nil {
		t.Fatalf("reading versions: %v", err)
	}

	versions, err := store.DecodeVersions(slices.Collect(maps.Keys(orders)))
	if err != nil {
		t.Fatalf("decoding versions: %v", err)
	}

	return versions
}
