package venue

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
)

func scopePins(t *testing.T, lifetime time.Duration) {
	t.Helper()

	old := pinLifetime
	pinLifetime = lifetime

	t.Cleanup(func() { pinLifetime = old })
}

// holdUnderRun runs a deferred step on worker as part of run, writing real bytes so the held entry weighs more than a shrunk bound, and returns the digest and holder.
func holdUnderRun(t *testing.T, worker, run string) (string, string) {
	t.Helper()

	runner := deferredRunner(t, worker)

	err := shell.Run(events.WithRunID(t.Context(), run), runner, "head -c 65536 /dev/urandom > out/blob")
	if err != nil {
		t.Fatal(err)
	}

	held, holder, _ := HeldOf(runner)

	err = runner.Close()
	if err != nil || held["out"] == "" {
		t.Fatalf("held %v, Close: %v", held, err)
	}

	return held["out"], holder
}

// pinsOf lists this process's pin volumes naming digest's alias.
func pinsOf(t *testing.T, digest string) []string {
	t.Helper()

	//nolint:gosec // filters this test built from its own pid
	out, err := exec.CommandContext(t.Context(), "docker", "volume", "ls", "--format", `{{.Name}} {{.Label "`+pinLabel+`"}}`,
		"--filter", "label="+cacheLabel+"=pin", "--filter", "label=steps.pid="+strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Fatalf("listing pins: %v", err)
	}

	var names []string

	for line := range strings.Lines(string(out)) {
		name, alias, _ := strings.Cut(strings.TrimSpace(line), " ")
		if strings.HasSuffix(alias, digest) {
			names = append(names, name)
		}
	}

	return names
}

// evictAll is another session on worker connecting with room for nothing, which trims every entry it may.
func evictAll(t *testing.T, worker string) {
	t.Helper()

	scopeEviction(t, 1, time.Hour)
	runAndClose(t, worker, payloadDir(t, 4096), "true")
}

// A held output no container mounts is neither layered on nor refused removal: only a pin on the worker keeps another session's eviction from taking what a later step of the run still reads.
func TestAHeldOutputSurvivesAnotherSessionsEviction(t *testing.T) {
	worker := localWorker(t, t.TempDir()).Worker

	scopeEviction(t, 8<<30, time.Hour)

	digest, holder := holdUnderRun(t, worker, "RUN-LIVE")

	evictAll(t, worker)

	_, err := Pull(t.Context(), holder, digest, t.TempDir())
	if err != nil {
		t.Fatalf("a run in flight lost its held output to another session's eviction: %v", err)
	}
}

// A pin outliving pinLifetime is a crashed run's, so it protects nothing and is itself reclaimed: a dead process cannot hold the cache over its bound forever.
func TestAPinPastItsLifetimeProtectsNothing(t *testing.T) {
	worker := localWorker(t, t.TempDir()).Worker

	scopeEviction(t, 8<<30, time.Hour)

	digest, holder := holdUnderRun(t, worker, "RUN-CRASHED")

	if len(pinsOf(t, digest)) == 0 {
		t.Fatal("holding an output pinned nothing, so this test proves nothing about expiry")
	}

	scopePins(t, 0)
	evictAll(t, worker)

	_, err := Pull(t.Context(), holder, digest, t.TempDir())
	if !errors.Is(err, errNotHeld) {
		t.Errorf("Pull = %v, want errNotHeld: an expired pin must not keep the entry past the bound", err)
	}

	if pins := pinsOf(t, digest); len(pins) != 0 {
		t.Errorf("expired pins %v were not reclaimed", pins)
	}
}

// Unpin is a finished run giving its outputs back to eviction; another run's pin on the same worker stays.
func TestUnpinReleasesOnlyThatRunsOutputs(t *testing.T) {
	worker := localWorker(t, t.TempDir()).Worker

	scopeEviction(t, 8<<30, time.Hour)

	done, holder := holdUnderRun(t, worker, "RUN-DONE")
	live, _ := holdUnderRun(t, worker, "RUN-STILL-GOING")

	err := Unpin(context.WithoutCancel(t.Context()), holder, "RUN-DONE")
	if err != nil {
		t.Fatal(err)
	}

	if pins := pinsOf(t, done); len(pins) != 0 {
		t.Fatalf("Unpin left %v", pins)
	}

	evictAll(t, worker)

	_, err = Pull(t.Context(), holder, done, t.TempDir())
	if !errors.Is(err, errNotHeld) {
		t.Errorf("Pull = %v, want errNotHeld: a released output is the cache's to evict again", err)
	}

	_, err = Pull(t.Context(), holder, live, t.TempDir())
	if err != nil {
		t.Errorf("another run's pin was released with this one's: %v", err)
	}
}
