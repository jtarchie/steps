package web

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// The staleness probe.
//
// Every page here answers a question whose answer changes while somebody is
// looking at it: a job starts, an agent parks a question, a check records a
// version. The page is refreshed by swapping the elements marked `data-live`,
// so anything that changes and is NOT inside one of them is a line the reader
// can only get by reloading — which is exactly how the job page sat there
// saying "No runs recorded yet" through a run it had itself triggered.
//
// The test renders each page, changes the state behind it, renders again, and
// insists every line that appeared lies inside a live region. It is a probe
// for a whole class of bug rather than one instance of it: a new page, or a
// new stateful section on an old one, fails here until it is marked.

// liveRegions returns the byte spans of every element marked data-live.
//
// A tag-depth scan rather than a parser: internal/web renders its own
// templates, the markup is balanced, and adding an HTML parser to this
// package's dependency list to check a nesting question is a poor trade.
func liveRegions(t *testing.T, body string) [][2]int {
	t.Helper()

	var spans [][2]int

	for at := 0; ; {
		mark := strings.Index(body[at:], " data-live=")
		if mark < 0 {
			break
		}

		mark += at

		open := strings.LastIndex(body[:mark], "<")
		if open < 0 {
			t.Fatalf("data-live at %d is not inside a tag", mark)
		}

		name := body[open+1:]
		if end := strings.IndexAny(name, " \t\r\n>"); end >= 0 {
			name = name[:end]
		}

		spans = append(spans, [2]int{open, closeOf(t, body, open, name)})
		at = mark + 1
	}

	return spans
}

// closeOf finds the index just past the element's closing tag, counting
// nested opens of the same name.
func closeOf(t *testing.T, body string, open int, name string) int {
	t.Helper()

	depth := 0

	for at := open; at < len(body); {
		next := strings.Index(body[at:], "<"+name)
		shut := strings.Index(body[at:], "</"+name+">")

		if shut < 0 {
			t.Fatalf("element <%s> opened at %d is never closed", name, open)
		}

		if next >= 0 && next < shut {
			depth++
			at += next + 1

			continue
		}

		depth--
		at += shut + len(name) + 3

		if depth == 0 {
			return at
		}
	}

	t.Fatalf("element <%s> opened at %d is never closed", name, open)

	return 0
}

// timeText blanks what the browser's own ticker rewrites: two renders seconds
// apart legitimately disagree about "4s ago", and that is not staleness.
var timeText = regexp.MustCompile(`<time[^>]*>[^<]*</time>`)

func TestNothingThatChangesLivesOutsideALiveRegion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		path   string
		setup  func(t *testing.T) (*Server, *Pipeline)
		change func(t *testing.T, pipeline *Pipeline)
	}{
		{
			// The reported bug: a run triggered while this page is open has to
			// show up in the history without a reload.
			name:  "job page",
			path:  "/p/demo/jobs/build",
			setup: testPipeline,
			change: func(t *testing.T, pipeline *Pipeline) {
				t.Helper()
				startRunningBuild(t, pipeline)
			},
		},
		{
			name:  "jobs board",
			path:  "/p/demo",
			setup: testPipeline,
			change: func(t *testing.T, pipeline *Pipeline) {
				t.Helper()
				startRunningBuild(t, pipeline)
			},
		},
		{
			name:  "runs page",
			path:  "/p/demo/runs",
			setup: testPipeline,
			change: func(t *testing.T, pipeline *Pipeline) {
				t.Helper()
				startRunningBuild(t, pipeline)
			},
		},
		{
			name:  "approvals page",
			path:  "/p/demo/approvals",
			setup: testPipeline,
			change: func(t *testing.T, pipeline *Pipeline) {
				t.Helper()

				_, err := pipeline.Store.RequestApproval(context.Background(), "build", "ship it?")
				if err != nil {
					t.Fatalf("RequestApproval: %v", err)
				}
			},
		},
		{
			name:  "questions page",
			path:  "/p/demo/questions",
			setup: testPipeline,
			change: func(t *testing.T, pipeline *Pipeline) {
				t.Helper()
				startRunningBuild(t, pipeline)

				_, _, err := pipeline.Store.AskQuestion(context.Background(), store.Question{
					RunID: "run-1", JobName: "build", AgentName: "review",
					Question: "which branch?",
				})
				if err != nil {
					t.Fatalf("AskQuestion: %v", err)
				}
			},
		},
		{
			name:  "resources page",
			path:  "/p/demo/resources",
			setup: testPipeline,
			change: func(t *testing.T, pipeline *Pipeline) {
				t.Helper()

				err := pipeline.Store.RecordCheckedVersion(context.Background(), "repo", `{"ref":"abc123"}`)
				if err != nil {
					t.Fatalf("RecordCheckedVersion: %v", err)
				}
			},
		},
		{
			name: "overview",
			path: "/",
			setup: func(t *testing.T) (*Server, *Pipeline) {
				t.Helper()

				server, pipelines := testPipelines(t, "app", "infra")

				return server, pipelines[0]
			},
			change: func(t *testing.T, pipeline *Pipeline) {
				t.Helper()
				startRunningBuild(t, pipeline)
			},
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server, pipeline := test.setup(t)

			_, before := get(t, server, test.path)
			test.change(t, pipeline)
			_, after := get(t, server, test.path)

			assertChangesAreLive(t, test.path, before, after)
		})
	}
}

// assertChangesAreLive fails for every line the second render added that the
// refresh would not reach.
func assertChangesAreLive(t *testing.T, path, before, after string) {
	t.Helper()

	was := map[string]bool{}
	for _, line := range strings.Split(timeText.ReplaceAllString(before, "<time/>"), "\n") {
		was[strings.TrimSpace(line)] = true
	}

	regions := liveRegions(t, after)
	fresh := 0
	at := 0

	for _, line := range strings.Split(after, "\n") {
		start := at
		at += len(line) + 1

		if was[strings.TrimSpace(timeText.ReplaceAllString(line, "<time/>"))] {
			continue
		}

		fresh++

		if !within(regions, start) {
			t.Errorf("this line changed but sits outside every data-live region,\n"+
				"so a reader only sees it by reloading %s:\n\t%s", path, strings.TrimSpace(line))
		}
	}

	if fresh == 0 {
		t.Fatal("the state change did not alter this page at all — the probe proves nothing")
	}

	// The poller is gated on a region inside <main>: the nav's badges are
	// marked live too, and a page whose only marked region were the shared
	// chrome would never ask the server anything.
	main := strings.Index(after, "<main>")
	if main < 0 || !within(regions, strings.Index(after[main:], " data-live=")+main) {
		t.Error("no live region inside <main>, so this page never polls at all")
	}
}

func within(spans [][2]int, at int) bool {
	for _, span := range spans {
		if at >= span[0] && at < span[1] {
			return true
		}
	}

	return false
}

// startRunningBuild leaves the run RUNNING, which is the state a page open
// across a trigger has to show.
func startRunningBuild(t *testing.T, pipeline *Pipeline) {
	t.Helper()

	err := pipeline.Store.StartRun(context.Background(), "run-1", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
}
