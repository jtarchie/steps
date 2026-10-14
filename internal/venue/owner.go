package venue

// Ownership that survives the daemon (#120): every machine a daemon starts or launches carries a steps-owner tag (EC2) or label (GCE) naming it, so a restarted daemon can tell its own machines from everybody else's by asking the cloud. Untagged stays somebody else's, which was the only rule before.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/events"
)

// labelOwner is one spelling valid as both an EC2 tag key and a GCE label key.
const labelOwner = "steps-owner"

// stampWindow is how far an instance's own last start may sit from the stamp written just before asking for it. Past it, somebody started the machine after the stamp — after a crash between tagging and starting, say — so it is not this daemon's start.
const stampWindow = 15 * time.Minute

type ownerKey struct{}

func withOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, ownerKey{}, owner)
}

func ownerOf(ctx context.Context) string {
	owner, _ := ctx.Value(ownerKey{}).(string)

	return owner
}

// stamp is the tag's value: whose, which rung, and when steps asked. Dashes and lowercase only, because it is a GCE label value too.
func stamp(owner string, rung Rung, at time.Time) string {
	return owner + "-" + string(rung) + "-" + strconv.FormatInt(at.Unix(), 10)
}

// ownedBy reads a stamp as this owner's, and the instance's own last start as the one the stamp asked for; anything else is somebody else's machine.
func ownedBy(owner, value string, started time.Time) (Rung, bool) {
	parts := strings.Split(value, "-")
	if owner == "" || len(parts) != 3 || parts[0] != owner {
		return "", false
	}

	rung := Rung(parts[1])
	if rung != RungStopped && rung != RungLaunch {
		return "", false
	}

	seconds, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", false
	}

	gap := started.Sub(time.Unix(seconds, 0))
	if gap < -stampWindow || gap > stampWindow {
		return "", false
	}

	return rung, true
}

// ownedInstance is a running instance carrying a steps-owner stamp, and its own last start.
type ownedInstance struct {
	name    string
	stamp   string
	started time.Time
	// template is the launch or instance template the cloud itself recorded, which a principal that can only write tags cannot forge.
	template string
}

// ownerTagDenied is per cloud, so the operator is told once rather than on every acquisition.
var ownerTagDenied sync.Map //nolint:gochecknoglobals // one warning per process, by design

// ownerTagFailed falls back to an untagged machine, which is how steps behaved before tags: a job never fails over one.
func ownerTagFailed(ctx context.Context, worker Worker, instance, verb string, err error, denied bool) {
	if !denied {
		slog.WarnContext(ctx, "worker.owner_tag_failed", "worker", worker.URL, "instance", instance, "action", verb, "error", err)

		return
	}

	if _, told := ownerTagDenied.LoadOrStore(worker.Scheme, struct{}{}); told {
		return
	}

	slog.ErrorContext(ctx, "worker.owner_tag_denied", "worker", worker.URL, "instance", instance, "action", verb, "error", err,
		"consequence", "machines this daemon starts are not marked as its own, so after a crash a restarted daemon cannot find and give them back",
		"fix", ownerTagPermission(worker.Scheme))
	events.Note(ctx, events.NoteWarn, fmt.Sprintf("worker %s: %s %s was refused (%s); machines are not marked as this daemon's, so one a crash leaves running is not given back after a restart", worker.URL, verb, instance, ownerTagPermission(worker.Scheme)))
}

func ownerTagPermission(scheme Scheme) string {
	if scheme == SchemeGCP {
		return "grant compute.instances.setLabels and compute.instances.list"
	}

	return "grant ec2:CreateTags and ec2:DeleteTags"
}

// untagThen removes the stamp before give runs, so a crash between the two leaves a machine reading "not ours" — the safe side. A failed untag never stops the give-back: a stale stamp on a stopped machine reads as somebody else's, by its start time, the next time anyone starts it.
func untagThen(untag func(context.Context), give func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		untag(ctx)

		return give(ctx)
	}
}

// sweepTarget is one location a daemon's workers name, and how to give back what it owns there.
type sweepTarget struct {
	worker Worker
	// parked and launched are what this location's mappings name: a stamp is readable by anyone who can list instances, so it is never the only proof, or whoever can write one tag could have the daemon terminate any machine.
	parked   map[string]bool
	launched func(one ownedInstance) bool
	list     func(ctx context.Context) ([]ownedInstance, error)
	park     func(name string) func(context.Context) error
	remove   func(name string) func(context.Context) error
}

// sweep gives back every running machine this owner stamped at one location, before anything in the process could be using it.
func sweep(ctx context.Context, owner string, target sweepTarget) error {
	found, err := target.list(ctx)
	if err != nil {
		return fmt.Errorf("listing %s's machines for %q: %w", owner, target.worker.URL, err)
	}

	var failures []error

	for _, one := range found {
		rung, ours := ownedBy(owner, one.stamp, one.started)
		if !ours || (rung == RungStopped && !target.parked[one.name]) || (rung == RungLaunch && !target.launched(one)) {
			continue
		}

		give, action := target.park(one.name), "parked"
		if rung == RungLaunch {
			give, action = target.remove(one.name), "removed"
		}

		err := giveBack(ctx, give)
		if err != nil {
			failures = append(failures, fmt.Errorf("giving back %s at %q: %w", one.name, target.worker.URL, err))

			continue
		}

		slog.InfoContext(ctx, "worker.swept", "instance", one.name, "worker", target.worker.URL, "action", action, "why", "this daemon started it and nothing in this process is using it")
	}

	return errors.Join(failures...)
}

// sweepScope is what the mappings at one location name.
func sweepScope(workers []Worker) (map[string]bool, map[string]bool) {
	parked, templates := map[string]bool{}, map[string]bool{}

	for _, worker := range workers {
		switch worker.Rung {
		case RungStopped:
			parked[worker.Instance] = true
		case RungLaunch:
			templates[worker.Template] = true
		case RungStatic:
		}
	}

	return parked, templates
}
