package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// capacityProbe is N held jobs sharing one gate: each marks itself started, records how many are live at that instant, and holds until the gate opens.
type capacityProbe struct {
	dir     string
	started string
	peaks   string
	gate    string
}

func newCapacityProbe(t *testing.T) *capacityProbe {
	t.Helper()

	dir := t.TempDir()
	probe := &capacityProbe{
		dir:     dir,
		started: filepath.Join(dir, "started"),
		peaks:   filepath.Join(dir, "peaks"),
		gate:    filepath.Join(dir, "gate"),
	}

	for _, sub := range []string{"started", "live"} {
		err := os.MkdirAll(filepath.Join(dir, sub), 0o750)
		if err != nil {
			t.Fatal(err)
		}
	}

	return probe
}

// job is the plan of one held job. A job removes its live marker BEFORE it returns, so the slot it frees is never counted beside the job that takes it: a peak above the cap is a leaked cap, not a race in the probe.
func (p *capacityProbe) job(name string) string {
	return `- name: ` + name + `
  interruptible: true
  plan:
  - task: hold
    inputs: []
    run: |
      touch ` + filepath.Join(p.dir, "live", name) + `
      ls ` + filepath.Join(p.dir, "live") + ` | wc -l >> ` + p.peaks + `
      touch ` + filepath.Join(p.started, name) + `
      until [ -f ` + p.gate + ` ]; do sleep 0.05; done
      rm ` + filepath.Join(p.dir, "live", name) + `
`
}

func (p *capacityProbe) open(t *testing.T) {
	t.Helper()

	err := os.WriteFile(p.gate, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func (p *capacityProbe) startedCount(t *testing.T) int {
	t.Helper()

	entries, err := os.ReadDir(p.started)
	if err != nil {
		t.Fatal(err)
	}

	return len(entries)
}

// reach waits for exactly want jobs to be running, then waits long enough for several drain cycles to admit a surplus one, if the cap would let it.
func (p *capacityProbe) reach(t *testing.T, want int) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)

	for p.startedCount(t) < want {
		if time.Now().After(deadline) {
			t.Fatalf("only %d jobs started, want %d: the cap is lower than configured", p.startedCount(t), want)
		}

		time.Sleep(25 * time.Millisecond)
	}

	time.Sleep(time.Second)

	got := p.startedCount(t)
	if got != want {
		t.Fatalf("%d jobs running at once, want exactly %d: the cap leaked", got, want)
	}
}

// peak is the highest live count any job saw when it started.
func (p *capacityProbe) peak(t *testing.T, jobs int) int {
	t.Helper()

	body, err := os.ReadFile(p.peaks)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Fields(string(body))
	if len(lines) != jobs {
		t.Fatalf("%d jobs recorded a peak, want %d", len(lines), jobs)
	}

	highest := 0

	for _, line := range lines {
		n, err := strconv.Atoi(line)
		if err != nil {
			t.Fatalf("peak %q: %v", line, err)
		}

		highest = max(highest, n)
	}

	return highest
}

// TestWebMaxRunsIsTheDaemonCeilingAcrossPipelines proves the cap is reached and never exceeded: five pipelines, each with room to run its job, and only --max-runs holds any of them back.
func TestWebMaxRunsIsTheDaemonCeilingAcrossPipelines(t *testing.T) {
	const pipelines, limit = 5, 3

	probe := newCapacityProbe(t)

	var served *webProcess

	names := make([]string, pipelines)

	for i := range names {
		names[i] = fmt.Sprintf("p%d", i)
		path := filepath.Join(probe.dir, names[i]+".yml")
		writePipelineFile(t, path, "jobs:\n"+probe.job(names[i]))

		if served == nil {
			served = startWebFor(t, path, "--max-runs", strconv.Itoa(limit), "--max-concurrent", strconv.Itoa(pipelines), "--interval", "1h")
			defer served.stopIfRunning(t)
			defer probe.open(t)

			continue
		}

		served.set(t, names[i], path)
	}

	for _, name := range names {
		served.trigger(t, name, name)
	}

	probe.reach(t, limit)
	probe.open(t)

	for _, name := range names {
		waitForQueueSuccess(t, served.state, name, 1)
	}

	if got := probe.peak(t, pipelines); got != limit {
		t.Errorf("peak concurrency %d, want %d", got, limit)
	}
}

// TestWebMaxConcurrentIsThePipelineCeiling is the same proof for one pipeline holding many ready jobs, where no daemon-wide cap is set.
func TestWebMaxConcurrentIsThePipelineCeiling(t *testing.T) {
	const jobs, limit = 5, 3

	probe := newCapacityProbe(t)

	var plan strings.Builder

	plan.WriteString("jobs:\n")

	names := make([]string, jobs)

	for i := range names {
		names[i] = fmt.Sprintf("j%d", i)
		plan.WriteString(probe.job(names[i]))
	}

	path := filepath.Join(probe.dir, "one.yml")
	writePipelineFile(t, path, plan.String())

	served := startWebFor(t, path, "--max-concurrent", strconv.Itoa(limit), "--interval", "1h")
	defer served.stopIfRunning(t)
	defer probe.open(t)

	for _, name := range names {
		served.trigger(t, "one", name)
	}

	probe.reach(t, limit)
	probe.open(t)

	waitForQueueSuccess(t, served.state, "one", jobs)

	if got := probe.peak(t, jobs); got != limit {
		t.Errorf("peak concurrency %d, want %d", got, limit)
	}
}
