package e2e

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// The run page says what a job spent once in its head and per step on each row, so the terminal's end-of-job usage report is a third copy there; what the rows could not say is a matrix's total, which its own row now carries.
func TestRunPageTotalsAContainersSpendOnItsRow(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fake := newRepeatingFakeLLM(t, says("reviewed").spending(400))

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: reviewer
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }

jobs:
- name: fan
  plan:
  - across:
    - var: item
      values: [a, b, c]
    agent: reviewer
    inputs: []
    messages:
      - "\"Review {{ .vars.item }}\""
  - agent: reviewer
    inputs: []
    messages:
      - "\"Sum up\""
`, fake.URL+"/v1/"))

	mustRun(t, path)

	server, target := webServerFor(t, path)

	runs, err := target.Store.ListRuns(t.Context(), "fan", 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns = %d runs, %v", len(runs), err)
	}

	code, page := webGet(t, server, "/p/"+target.Slug+"/runs/"+runs[0].ID)
	if code != http.StatusOK {
		t.Fatalf("run page = %d", code)
	}

	if strings.Contains(page, "tokens across") {
		t.Errorf("the run page repeats the terminal's usage report:\n%s", page)
	}

	head := regexp.MustCompile(`(?s)<span class="kind">across</span>.*?</div>\s*</div>`).FindString(page)
	if head == "" {
		t.Fatalf("no across row:\n%s", page)
	}

	if !strings.Contains(head, "1,200 tokens") {
		t.Errorf("the across row does not total its cells' spend:\n%s", head)
	}
}
