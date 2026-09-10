package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/web"
)

// A daemon whose loops did not share machines would let one job's end stop the instance another is on — the registry off in the one mode it exists for, with nothing else able to notice.
func TestTheDaemonSharesAcquiredMachinesAcrossItsLoops(t *testing.T) {
	t.Parallel()

	held := newDaemon(t.Context(), nil, web.NewLocalRunner(nil, nil, 1, false),
		filepath.Join(t.TempDir(), "steps.db"), ExecFlags{}, HistoryFlags{}, time.Hour)
	defer held.Close()

	if !pipeline.SharesWorkers(held.base) {
		t.Fatal("the daemon's loops run on a context with no shared registry")
	}
}

// The stale-build sweep removes every b-* directory under a workspace.root: with no ownership check, so a neighbour set, or renamed, onto the same root deleted the build in flight there.
func TestANeighbourOnTheSameRootLeavesABuildInFlightAlone(t *testing.T) {
	t.Parallel()

	held := servingDaemon(t)
	root := t.TempDir()
	gate := filepath.Join(t.TempDir(), "gate")

	setPipeline(t, held, "a", onRoot(root, "", "while [ ! -e "+gate+" ]; do sleep 0.02; done; echo built > out/x"))
	enqueue(t, held, "a")

	build := buildInFlight(t, root)

	setPipeline(t, held, "b", onRoot(root, "", "true"))

	err := held.Rename(t.Context(), "b", "c")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}

	_, err = os.Stat(build)
	if err != nil {
		t.Errorf("a neighbour set and renamed onto the same root removed the build in flight there: %v", err)
	}

	writePipelineFile(t, gate, "")

	if status := finishedRun(t, held, "a"); status != "succeeded" {
		t.Errorf("the run whose build was in flight %s, want succeeded", status)
	}
}

// A set that changed workspace: over a durable root used to install a provider keeping every build, to dodge the sweep — so each run from then on left its tree behind until a restart.
func TestAChangedWorkspaceBlockStillRemovesItsBuilds(t *testing.T) {
	t.Parallel()

	for _, change := range []struct {
		name, cache string
		move        bool
	}{
		{name: "the root moved", move: true},
		{name: "only the cache changed", cache: "  cache:\n    resources: true\n"},
	} {
		t.Run(change.name, func(t *testing.T) {
			t.Parallel()

			held := servingDaemon(t)
			root := t.TempDir()

			setPipeline(t, held, "app", onRoot(root, "", "echo built > out/x"))

			if change.move {
				root = t.TempDir()
			}

			setPipeline(t, held, "app", onRoot(root, change.cache, "echo built > out/x"))
			enqueue(t, held, "app")

			if status := finishedRun(t, held, "app"); status != "succeeded" {
				t.Fatalf("the run %s, want succeeded", status)
			}

			left, err := filepath.Glob(filepath.Join(root, "b-*"))
			if err != nil {
				t.Fatal(err)
			}

			if len(left) > 0 {
				t.Errorf("a successful run under the replaced workspace left %v behind", left)
			}
		})
	}
}

// A rename is one UPDATE and what serves the new name is built after it, so a refusal there used to leave the pipeline served under neither name while the database — and so the next restart — named the new one.
func TestARefusedRenameLeavesThePipelineWhereItWas(t *testing.T) {
	t.Parallel()

	held := servingDaemon(t)
	root := t.TempDir()

	setPipeline(t, held, "app", onRoot(root, "", "echo built > out/x"))

	err := os.Chmod(root, 0o500) //nolint:gosec // read-only is the point: the workspace probe writes, and this root refuses it
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(root, 0o700) }) //nolint:gosec // a directory needs its execute bit to be removable

	err = held.Rename(t.Context(), "app", "renamed")
	if !errors.Is(err, web.ErrRefused) {
		t.Fatalf("a rename whose new name this machine cannot serve = %v, want refused", err)
	}

	if held.server.Lookup("renamed") != nil {
		t.Error("a refused rename serves the new name")
	}

	if names := pipelineNames(t, held.state); !slices.Contains(names, "app") || slices.Contains(names, "renamed") {
		t.Errorf("after a refused rename the database holds %v, want app and not renamed", names)
	}

	err = os.Chmod(root, 0o700) //nolint:gosec // the root is the test's own
	if err != nil {
		t.Fatal(err)
	}

	enqueue(t, held, "app")

	if status := finishedRun(t, held, "app"); status != "succeeded" {
		t.Errorf("after a refused rename the pipeline's run %s, want it still served and running", status)
	}
}

// Every mid-life close lets go of one pipeline's handle while its neighbours keep writing to the same file, and Close's reclaim holds the file's write lock as long as it takes — 31s measured after a destroy freed a gigabyte, the neighbours' queue writes failing SQLITE_BUSY meanwhile.
func TestOnlyTheDaemonsExitCompactsTheFile(t *testing.T) {
	t.Parallel()

	held := servingDaemon(t)

	setPipeline(t, held, "doomed", idlePipeline)
	setPipeline(t, held, "kept", idlePipeline)

	doomed := served(t, held, "doomed").Store

	for i := range 16 {
		err := doomed.RecordRevision(t.Context(), fmt.Sprintf("big-%d", i), strconv.Itoa(i)+strings.Repeat("x", 64<<10), nil)
		if err != nil {
			t.Fatalf("RecordRevision: %v", err)
		}
	}

	err := held.Destroy(t.Context(), "doomed")
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}

	assertUncompacted(t, held.state, "a destroy")

	_, err = held.Set(t.Context(), "refused", web.SetRequest{Source: idlePipeline, ExpectSHA: "stale"})
	if !errors.Is(err, web.ErrRevisionMoved) {
		t.Fatalf("a set against a sha nothing serves = %v, want refused", err)
	}

	assertUncompacted(t, held.state, "a refused set")

	err = held.Rename(t.Context(), "kept", "renamed")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}

	assertUncompacted(t, held.state, "a rename")

	held.Close()

	if free := freePages(t, held.state); free != 0 {
		t.Errorf("the daemon's exit left %d freed pages unreclaimed", free)
	}
}

// A sender that went away between the revision and the switch to it, or a file too busy for a write after the switch, used to leave the database naming a configuration the daemon never started serving, which the next restart then quietly switched to.
func TestASetTheSenderHangsUpOnStillServesWhatTheDatabaseNames(t *testing.T) {
	t.Parallel()

	for _, after := range []string{"RecordRevision", "SetCurrentRevision"} {
		t.Run(after, func(t *testing.T) {
			t.Parallel()

			held := servingDaemon(t)

			raw, err := sqlite.OpenStore(held.state, "app")
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}

			sender := &hangsUp{Store: raw}
			serveOn(t, held, "app", sender, idlePipeline)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			sender.after, sender.cancel = after, cancel

			edited := strings.Replace(idlePipeline, `"true"`, `"echo edited"`, 1)

			_, err = held.Set(ctx, "app", web.SetRequest{Source: edited, From: "/src/app.yml"})
			if err != nil {
				t.Fatalf("a set whose sender hung up after %s = %v, want it applied: all it can refuse for was asked before the first write", after, err)
			}

			current, _, err := raw.CurrentRevision(t.Context())
			if err != nil {
				t.Fatalf("CurrentRevision: %v", err)
			}

			if serving := served(t, held, "app").Config().Revision.SHA; current.Source != edited || serving != current.SHA {
				t.Errorf("the database names %s and the daemon serves %s, want both the configuration that was set", shortConfig(current.SHA), shortConfig(serving))
			}
		})
	}
}

// hangsUp is a sender gone mid-set: once the write it names returns, the request's context is cancelled, and after the switch the file refuses this handle's writes as a busy one does.
type hangsUp struct {
	store.Store
	after  string
	cancel context.CancelFunc
	busy   atomic.Bool
}

var errBusy = errors.New("database is locked")

func (h *hangsUp) RecordRevision(ctx context.Context, sha, source string, includes map[string]string) error {
	if h.busy.Load() {
		return errBusy
	}

	err := h.Store.RecordRevision(ctx, sha, source, includes)
	h.hangUp("RecordRevision")

	return err //nolint:wrapcheck // the wrapped store's error, untouched, is what the daemon has to handle
}

func (h *hangsUp) SetCurrentRevision(ctx context.Context, sha, from string) error {
	if h.busy.Load() {
		return errBusy
	}

	err := h.Store.SetCurrentRevision(ctx, sha, from)
	h.hangUp("SetCurrentRevision")

	return err //nolint:wrapcheck // as above
}

func (h *hangsUp) SetSourcePath(ctx context.Context, source string) error {
	if h.busy.Load() {
		return errBusy
	}

	return h.Store.SetSourcePath(ctx, source) //nolint:wrapcheck // as above
}

func (h *hangsUp) hangUp(write string) {
	if h.cancel == nil || write != h.after {
		return
	}

	h.cancel()
	h.busy.Store(write == "SetCurrentRevision")
}

// serveOn serves a pipeline as a first set does, over a handle the test holds, so a wrapper can stand between the daemon and the file.
func serveOn(t *testing.T, held *daemon, name string, st store.Store, source string) {
	t.Helper()

	held.mu.Lock()
	defer held.mu.Unlock()

	cfg, err := held.accept(name, source, nil)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	req := web.SetRequest{Source: source, From: "/src/" + name + ".yml"}

	err = held.record(t.Context(), st, cfg, req)
	if err != nil {
		t.Fatalf("record: %v", err)
	}

	provider, err := held.provider(cfg, st)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}

	held.start(name, cfg, st, provider, req.From)
}

const idlePipeline = "jobs:\n- name: build\n  plan:\n  - task: work\n    inputs: []\n    run: \"true\"\n"

// servingDaemon is the daemon `steps web` builds, over a state file of the test's own.
func servingDaemon(t *testing.T) *daemon {
	t.Helper()

	local := web.NewLocalRunner(nil, nil, 1, false)

	server, err := web.New(nil, local)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	held := newDaemon(t.Context(), server, local, filepath.Join(t.TempDir(), "steps.db"), ExecFlags{}, HistoryFlags{}, time.Hour)
	t.Cleanup(held.Close)

	return held
}

func setPipeline(t *testing.T, held *daemon, name, source string) {
	t.Helper()

	_, err := held.Set(t.Context(), name, web.SetRequest{Source: source, From: "/src/" + name + ".yml"})
	if err != nil {
		t.Fatalf("set %s: %v", name, err)
	}
}

// onRoot is one job building under a durable workspace.root:, with extra spliced into the workspace block.
func onRoot(root, extra, run string) string {
	return "workspace:\n  root: " + root + "\n" + extra +
		"jobs:\n- name: build\n  plan:\n  - task: work\n    outputs: [out]\n    run: " + strconv.Quote(run) + "\n"
}

func served(t *testing.T, held *daemon, name string) *web.Pipeline {
	t.Helper()

	target := held.server.Lookup(name)
	if target == nil {
		t.Fatalf("%s is not served", name)
	}

	return target
}

func enqueue(t *testing.T, held *daemon, name string) {
	t.Helper()

	err := served(t, held, name).Store.EnqueueJob(t.Context(), "build", "test")
	if err != nil {
		t.Fatalf("enqueue %s: %v", name, err)
	}
}

// finishedRun waits out the newest run of the pipeline's build job and says how it ended.
func finishedRun(t *testing.T, held *daemon, name string) string {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)

	for time.Now().Before(deadline) {
		runs, err := served(t, held, name).Store.ListRuns(t.Context(), "build", 1)
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}

		if len(runs) == 1 && !runs[0].FinishedAt.IsZero() {
			return runs[0].Status
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("%s never finished a build", name)

	return ""
}

// buildInFlight waits for a build under root to reach its step, and names the build's directory.
func buildInFlight(t *testing.T, root string) string {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)

	for time.Now().Before(deadline) {
		steps, err := filepath.Glob(filepath.Join(root, "b-*", "steps", "*"))
		if err != nil {
			t.Fatal(err)
		}

		if len(steps) > 0 {
			return filepath.Dir(filepath.Dir(steps[0]))
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("no build under the root reached its step")

	return ""
}

func pipelineNames(t *testing.T, state string) []string {
	t.Helper()

	reader, err := sqlite.OpenReader(state)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}

	defer func() { _ = reader.Close() }()

	rows, err := reader.Pipelines(t.Context())
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}

	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}

	return names
}

func assertUncompacted(t *testing.T, state, after string) {
	t.Helper()

	if freePages(t, state) == 0 {
		t.Errorf("%s compacted the shared file, holding its write lock while the other pipelines in it were writing", after)
	}
}

// freePages is what deletes freed and no reclaim has handed back yet.
func freePages(t *testing.T, state string) int {
	t.Helper()

	db, err := sql.Open("sqlite", state)
	if err != nil {
		t.Fatalf("open %s: %v", state, err)
	}

	defer func() { _ = db.Close() }()

	var free int

	err = db.QueryRowContext(t.Context(), "PRAGMA freelist_count").Scan(&free)
	if err != nil {
		t.Fatalf("freelist_count: %v", err)
	}

	return free
}
