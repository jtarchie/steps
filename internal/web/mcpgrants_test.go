package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

const mcpGrantsYAML = `
mcp_servers:
- name: fastmail
  endpoint: https://mcp.example.test/fastmail
  auth: { type: oauth }
- name: linear
  endpoint: https://mcp.example.test/linear
  auth: { type: oauth }

agents:
- name: bot
  source: { model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY }
  tools:
  - read_file
  - mcp: fastmail
    tools: [search_email, read_email]
  - mcp: linear
- name: guest
  source: { model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY }

jobs:
- name: answer
  plan:
  - agent: bot
  - agent: guest
- name: tidy
  plan:
  - agent: guest
`

func startMCPRun(t *testing.T, pipeline *Pipeline, runID, sha string) {
	t.Helper()

	err := pipeline.Store.StartRun(t.Context(), store.NewRun{ID: runID, JobName: "answer", Workspace: t.TempDir(), ConfigSHA: sha})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, runID, []store.RunEventRow{
		{Type: events.TypeStepSkipped, StepIndex: 0, StepName: "bot", StepKind: "agent", StepID: 1, Status: "skipped", Text: "when: guard was false"},
		{Type: events.TypeStepSkipped, StepIndex: 1, StepName: "guest", StepKind: "agent", StepID: 2, Status: "skipped", Text: "when: guard was false"},
	})
}

// stepRowOf cuts one step's row out of a run page: from its id to the next row.
func stepRowOf(t *testing.T, body, anchor string) string {
	t.Helper()

	_, row, found := strings.Cut(body, `id="`+anchor+`"`)
	if !found {
		t.Fatalf("no row %s:\n%s", anchor, body)
	}

	row, _, _ = strings.Cut(row, `<div id="step-`)

	return row
}

// Which servers an agent may reach decided what it could do, and the only page that said so was the mcp tab, read in the other direction: server first, then who uses it. A reader on a job, or on a run of it, asks agent first.
func TestAJobSaysWhichMCPServersItsAgentsReach(t *testing.T) {
	t.Parallel()

	server, _ := serverFromYAML(t, mcpGrantsYAML)

	code, job := get(t, server, "/p/demo/jobs/answer/detail")
	if code != http.StatusOK {
		t.Fatalf("job page = %d", code)
	}

	for _, said := range []string{`href="/p/demo/mcp#mcp-fastmail"`, `href="/p/demo/mcp#mcp-linear"`, "2 tools", "all tools"} {
		if !strings.Contains(job, said) {
			t.Errorf("the job page does not say %s:\n%s", said, job)
		}
	}

	_, board := get(t, server, "/p/demo")
	if !strings.Contains(board, `class="dagmcp"`) || !strings.Contains(board, "mcp fastmail, linear") {
		t.Errorf("the board does not name the servers answer's agents reach:\n%s", board)
	}

	if got := strings.Count(board, `class="dagmcp"`); got != 1 {
		t.Errorf("%d jobs carry an mcp line, want only answer: tidy's agent is granted none", got)
	}
}

func TestARunSaysWhichMCPServersItsAgentsReach(t *testing.T) {
	t.Parallel()

	server, pipeline := serverFromYAML(t, mcpGrantsYAML)

	startMCPRun(t, pipeline, "run-mcp", pipeline.Config().Revision.SHA)

	_, run := get(t, server, "/p/demo/runs/run-mcp")

	head, _, _ := strings.Cut(run, `id="run-strip"`)
	if !strings.Contains(head, `href="/p/demo/mcp#mcp-fastmail"`) || !strings.Contains(head, `href="/p/demo/mcp#mcp-linear"`) {
		t.Errorf("the run's head does not name the servers its job reaches:\n%s", head)
	}

	_, transcript, _ := strings.Cut(run, `id="transcript"`)

	bot := stepRowOf(t, transcript, "step-1-bot")
	for _, said := range []string{`href="/p/demo/mcp#mcp-fastmail"`, "search_email, read_email", `href="/p/demo/mcp#mcp-linear"`, "all tools"} {
		if !strings.Contains(bot, said) {
			t.Errorf("bot's row does not say %s:\n%s", said, bot)
		}
	}

	if guest := stepRowOf(t, transcript, "step-2-guest"); strings.Contains(guest, "/mcp#") {
		t.Errorf("guest is granted no server and its row names one:\n%s", guest)
	}
}

// The grants are today's configuration, so a run that opened against another one is told nothing rather than today's answer: the same gate the spend ceiling has.
func TestARunOnAnotherConfigurationNamesNoMCPServers(t *testing.T) {
	t.Parallel()

	server, pipeline := serverFromYAML(t, mcpGrantsYAML)

	startMCPRun(t, pipeline, "run-old", "")

	_, run := get(t, server, "/p/demo/runs/run-old")
	if strings.Contains(run, "/mcp#mcp-") {
		t.Errorf("a run whose configuration is not the loaded one names its servers:\n%s", run)
	}
}

func TestCompactCount(t *testing.T) {
	t.Parallel()

	for n, want := range map[int]string{
		0: "0", 9_999: "9,999", 10_000: "10.0k", 17_277: "17.3k", 99_949: "99.9k", 99_950: "100k", 355_856: "356k",
		999_499: "999k", 999_500: "1.0M", 1_422_326: "1.4M", 71_745_678: "71.7M", 999_500_000: "1.0B", 2_400_000_000: "2.4B",
	} {
		if got := compactCount(n); got != want {
			t.Errorf("compactCount(%d) = %q, want %q", n, got, want)
		}
	}
}
