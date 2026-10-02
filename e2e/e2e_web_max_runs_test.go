package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestWebMaxRunsHoldsOtherPipelinesPending.
//
// --max-concurrent is per pipeline, so the cap that spans them has to be
// proven ACROSS two: one pipeline's build fills the only slot, and the other
// pipeline's trigger must wait as a pending queue row, say why on its follow
// page, and survive the daemon going down — it never started, so a restart
// owes it a run rather than an abort.
func TestWebMaxRunsHoldsOtherPipelinesPending(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "hold.started")
	gate := filepath.Join(dir, "hold.gate")
	ran := filepath.Join(dir, "next.ran")

	holdPath := filepath.Join(dir, "hold.yml")
	writePipelineFile(t, holdPath, `
jobs:
- name: hold
  interruptible: true
  plan:
  - task: wait
    inputs: []
    run: |
      touch `+started+`
      until [ -f `+gate+` ]; do sleep 0.05; done
`)

	nextPath := filepath.Join(dir, "next.yml")
	writePipelineFile(t, nextPath, `
jobs:
- name: next
  plan:
  - task: mark
    inputs: []
    run: echo ran >> `+ran+`
`)

	// --max-concurrent 2 so nothing per-pipeline is what holds `next` back.
	served := startWebFor(t, holdPath, "--max-runs", "1", "--max-concurrent", "2", "--interval", "1h")
	defer served.stopIfRunning(t)

	served.set(t, "next", nextPath)

	served.trigger(t, "hold", "hold")
	waitForFile(t, started)

	served.trigger(t, "next", "next")

	waitForBlockingCheck(t, served, "next", "next", "--max-runs (1) is full")

	// Again after several drain cycles: a worker that claimed first and then
	// waited for a slot would have turned the row running by now.
	time.Sleep(time.Second)

	follow := waitForBlockingCheck(t, served, "next", "next", "--max-runs (1) is full")
	if follow.State != "pending" {
		t.Errorf("next's follow state = %q, want pending", follow.State)
	}

	_, err := os.Stat(ran)
	if err == nil {
		t.Fatal("next ran while hold filled the only --max-runs slot")
	}

	served.stop(t)

	err = os.WriteFile(gate, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	restarted := startWeb(t, "--db", served.state, "--max-runs", "1", "--interval", "1h")
	defer restarted.stopIfRunning(t)

	waitForQueueSuccess(t, served.state, "next", 1)

	body, err := os.ReadFile(filepath.Clean(ran))
	if err != nil {
		t.Fatalf("next never ran after the restart: %v", err)
	}

	if got := strings.Count(string(body), "ran"); got != 1 {
		t.Errorf("next ran %d times, want 1", got)
	}
}

type followAnswer struct {
	State  string `json:"state"`
	Checks []struct {
		Text  string `json:"text"`
		Clear bool   `json:"clear"`
	} `json:"checks"`
}

// waitForBlockingCheck polls the follow page's JSON until a blocking check contains want.
func waitForBlockingCheck(t *testing.T, served *webProcess, name, job, want string) followAnswer {
	t.Helper()

	since := time.Now().Add(-time.Minute).UnixMilli()
	deadline := time.Now().Add(5 * time.Second)

	var last string

	for time.Now().Before(deadline) {
		_, body := served.get(t, "/p/"+name+"/jobs/"+job+"/latest-run?since="+strconv.FormatInt(since, 10))
		last = body

		var answer followAnswer

		err := json.Unmarshal([]byte(body), &answer)
		if err != nil {
			t.Fatalf("follow answer %q: %v", body, err)
		}

		for _, check := range answer.Checks {
			if !check.Clear && strings.Contains(check.Text, want) {
				return answer
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("follow page never blamed %q; last answer: %s", want, last)

	return followAnswer{}
}
