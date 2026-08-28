package shell

// Reclaiming containers a previous run could not clean up after itself.

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jtarchie/steps/internal/dockerapi"
)

// Labels stamped on every container this tool starts, so a container that
// outlives the process that made it can still be identified as ours and
// attributed to the run that made it.
const (
	dockerOwnerLabel = "steps.owner"
	dockerPIDLabel   = "steps.pid"
	dockerHostLabel  = "steps.host"
)

// dockerSweepTimeout bounds the whole orphan sweep. It runs before the first
// step, so it must not be able to hang a run: past this the sweep gives up and
// the (worst case) orphan waits for the next one.
const dockerSweepTimeout = 30 * time.Second

// ownerHostname identifies the machine whose process ids the steps.pid label
// refers to. A pid means nothing without it: with a remote daemon the
// containers on it belong to processes on other machines entirely, and
// checking their pids against this host's would be reading a different
// machine's process table — which is how a sweep ends up killing a container
// that is very much still in use.
func ownerHostname() string {
	name, err := os.Hostname()
	if err != nil {
		return "unknown"
	}

	return name
}

// SweepOrphanedContainers removes containers left behind by a steps process
// that never got to clean up after itself.
//
// Close removes a step's container on every path a program can control,
// including failures and Ctrl-C. SIGKILL is the one it cannot: the process
// stops between instructions, and the container keeps running. Before this,
// the only thing that eventually reclaimed it was the keepalive expiring a day
// later — so a box where runs get killed (an OOM, a CI executor reaped
// mid-build, a laptop closed) accumulated live containers holding memory for
// 24 hours each.
//
// An orphan is identified by its owning process no longer existing. That is
// precise where an age heuristic is not: a legitimately long step can run for
// hours, and killing its container because it looked old would break a working
// build to tidy up after a broken one. Pid reuse only ever makes this
// conservative — a reused pid means the container is skipped and reclaimed by
// a later run instead.
//
// Only containers labeled with THIS hostname are considered, since a pid is
// meaningful only on the machine that issued it.
//
// dockerHost names the daemon to sweep; empty is this machine's own. A WORKER
// daemon is swept on exactly the same terms and needs no special case, because
// the labels already answer the question that matters: a container a placed
// step started carries the ORCHESTRATOR's hostname and the ORCHESTRATOR's pid,
// so the host filter keeps it to containers this machine is responsible for
// and the pid is checkable against this machine's process table — which is
// precisely what ownerHostname was written for. Without pointing it anywhere
// but here, a placed containerized step whose teardown could not reach the
// daemon left a container running on a machine nothing ever swept.
//
// Entirely best-effort: every failure is logged and none is returned. Running
// the pipeline matters more than tidying up before it.
func SweepOrphanedContainers(ctx context.Context, dockerHost string) {
	ctx, cancel := context.WithTimeout(ctx, dockerSweepTimeout)
	defer cancel()

	client, err := dockerapi.New(dockerHost)
	if err != nil {
		slog.Debug("shell.docker.sweep_unavailable", "error", err)

		return
	}

	defer func() { _ = client.Close() }()

	orphans := listOrphanedContainers(ctx, client)
	if len(orphans) == 0 {
		return
	}

	for _, id := range orphans {
		slog.Info("shell.docker.sweep_orphan", "container", id)

		err := client.RemoveContainer(ctx, id)
		if err != nil {
			slog.Warn("shell.docker.sweep_failed", "container", id, "error", err)
		}
	}
}

// listOrphanedContainers returns the ids of our containers whose owning
// process is gone.
func listOrphanedContainers(ctx context.Context, client *dockerapi.Client) []string {
	containers, err := client.ListContainers(ctx, map[string]string{
		dockerOwnerLabel: "steps",
		dockerHostLabel:  ownerHostname(),
	})
	if err != nil {
		slog.Debug("shell.docker.sweep_list_failed", "error", err)

		return nil
	}

	var orphans []string

	for _, container := range containers {
		pid, ok := ownerPID(container.Labels[dockerPIDLabel])
		if !ok {
			continue
		}

		if processAlive(pid) {
			continue
		}

		orphans = append(orphans, container.ID)
	}

	return orphans
}

// ownerPID reads the pid a container was labelled with. A container without a
// usable one is skipped rather than swept: an unattributable container is
// exactly the one not to remove on a guess.
func ownerPID(label string) (int, bool) {
	pid, err := strconv.Atoi(strings.TrimSpace(label))
	if err != nil || pid <= 0 {
		return 0, false
	}

	return pid, true
}

// processAlive reports whether pid names a live process on this machine.
//
// Signal 0 performs the existence and permission checks without delivering
// anything — the standard way to ask. os.FindProcess never fails on unix, so
// the signal is the whole test. EPERM counts as alive: the process exists,
// it just is not ours, and a container belonging to another user's live run is
// emphatically not an orphan.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}

	return errorIsPermission(err)
}

func errorIsPermission(err error) bool {
	return strings.Contains(err.Error(), "operation not permitted") || os.IsPermission(err)
}
