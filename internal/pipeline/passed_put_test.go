package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/workspace"
)

type putFixture struct {
	cfg      *config.Config
	st       store.Store
	provider workspace.Provider
	dir      string
}

func newPutFixture(t *testing.T, yaml string) *putFixture {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	err := os.WriteFile(path, []byte(strings.ReplaceAll(yaml, "DIR", dir)), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	st, err := sqlite.OpenStore(filepath.Join(dir, "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	provider, err := workspace.NewProvider(nil, true)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	return &putFixture{cfg: cfg, st: st, provider: provider, dir: dir}
}

func (f *putFixture) run(ctx context.Context) error {
	j, err := f.cfg.FindJob("build")
	if err != nil {
		return fmt.Errorf("finding job: %w", err)
	}

	return RunJob(ctx, f.cfg, j, nil, f.provider, f.st, false)
}

func (f *putFixture) passed(t *testing.T, job string) map[string]bool {
	t.Helper()

	rows, err := f.st.PassedVersions(context.Background(), job, 0)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, row := range rows {
		got[row.Resource+"="+row.Version] = true
	}

	return got
}

const putTypes = `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: echo '[{"n":"old"}]'
    in: echo {{ .version.n | shellquote }} > n.txt
    out: OUT
resources:
- name: image
  type: feed
  source: {}
`

// TestPassedRecordsEveryPutAndTheLatestIsCurrent: several puts in one build all
// count, and the version put last is the newest green one because order is
// taken when the put runs, not in the map order of build-end recording.
func TestPassedRecordsEveryPutAndTheLatestIsCurrent(t *testing.T) {
	f := newPutFixture(t, strings.Replace(putTypes, "OUT", `echo "{\"n\":\"img-$(cat DIR/count)\"}"`, 1)+`
jobs:
- name: build
  plan:
  - get: image
  - task: one
    run: echo 1 > DIR/count
  - put: image
  - task: two
    run: echo 2 > DIR/count
  - put: image
  - task: three
    run: echo 3 > DIR/count
  - put: image
  - task: four
    run: echo 4 > DIR/count
  - put: image
  - task: five
    run: echo 5 > DIR/count
  - put: image
`)

	err := f.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	got := f.passed(t, "build")
	for _, want := range []string{"old", "img-1", "img-2", "img-3", "img-4", "img-5"} {
		if !got[`image={"n":"`+want+`"}`] {
			t.Errorf("version %q of image did not pass build; got %v", want, got)
		}
	}

	orders, err := f.st.VersionOrders(context.Background(), "image")
	if err != nil {
		t.Fatal(err)
	}

	for i := 1; i < 5; i++ {
		prev, next := fmt.Sprintf(`{"n":"img-%d"}`, i), fmt.Sprintf(`{"n":"img-%d"}`, i+1)
		if orders[prev] >= orders[next] {
			t.Errorf("puts are not ordered by when they ran: %v", orders)

			break
		}
	}
}

func TestPassedDropsAnOversizedPutVersion(t *testing.T) {
	f := newPutFixture(t, strings.Replace(putTypes, "OUT", `printf '{"n":"%s"}' "$(head -c 5000 /dev/zero | tr '\0' x)"`, 1)+`
jobs:
- name: build
  plan:
  - put: image
`)

	var err error

	out := captureStdout(t, func() { err = f.run(context.Background()) })
	if err != nil {
		t.Fatalf("an oversized version must not fail the build: %v", err)
	}

	if got := f.passed(t, "build"); len(got) != 0 {
		t.Errorf("an oversized version was recorded: %d entries", len(got))
	}

	if !strings.Contains(out, "is not recorded") {
		t.Errorf("no warning printed; got %q", out)
	}
}

// TestResumeWarnsThatASkippedPutIsNotRecorded pins the documented limit: an
// earlier attempt's put output is not recoverable, so the gate stays shut.
func TestResumeWarnsThatASkippedPutIsNotRecorded(t *testing.T) {
	f := newPutFixture(t, strings.Replace(putTypes, "OUT", `echo '{"n":"pushed"}'`, 1)+`
jobs:
- name: build
  plan:
  - put: image
  - task: gate
    run: test -f DIR/ok
`)

	ctx := context.Background()

	err := f.run(ctx)
	if err == nil {
		t.Fatal("the first attempt was supposed to fail")
	}

	err = os.WriteFile(filepath.Join(f.dir, "ok"), nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	runs, err := f.st.ListRuns(ctx, "build", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %v, %v", runs, err)
	}

	resumeCtx, dir, err := PrepareResume(ctx, f.st, runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}

	resumable, ok := f.provider.(workspace.Resumable)
	if !ok {
		t.Fatal("provider is not resumable")
	}

	resumable.Reuse(dir)

	out := captureStdout(t, func() { err = f.run(resumeCtx) })
	if err != nil {
		t.Fatalf("resume: %v", err)
	}

	if !strings.Contains(out, "resume: put image ran in an earlier attempt") {
		t.Errorf("no resume warning; got %q", out)
	}

	if got := f.passed(t, "build"); len(got) != 0 {
		t.Errorf("a skipped put's version was recorded: %v", got)
	}
}
