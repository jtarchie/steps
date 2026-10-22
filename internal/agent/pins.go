package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

// sourceSelection is which fallback: entry an agent is running on: the source
// itself, and its POSITION in the agent's fallback list.
//
// The position is stored rather than re-derived by searching the list for a
// matching source, because two entries may legitimately hold the same source
// (the same model reached through two entries that differ elsewhere, or an
// honest copy-paste). A value scan then reports the FIRST match, and a
// cascade continuing from that index re-tries the very source whose failure
// triggered it.
type sourceSelection struct {
	source config.AgentSource
	index  int
}

// pinRecord is a selection plus the two clocks that bound it. They are
// separate fields because they answer different questions and move at
// different times, and collapsing them into one was a defect in both
// directions.
//
// The selection is the IDENTITY, and it is what the compare-and-swap guards
// compare. Nothing else may join it: a timestamp inside the compared value
// makes a guard fail for a pin whose source and index never changed, which
// turned every concurrent renewal into a lost release.
type pinRecord struct {
	selection sourceSelection
	// since is when this agent LEFT its primary, and it never moves while the
	// pin stands. It is the bar a probe has to clear to be evidence about the
	// outage that caused this pin: an answer older than the pin was given
	// before the reason for it existed, whoever asked and in whichever pass.
	since time.Time
	// decided is when something last re-decided this pin from probes, which
	// is what pinLifetime counts down from. Separate from since precisely so
	// renewing the lifetime does not move the freshness bar — moving it meant
	// a renewal could make every existing answer stale, and a pin that can
	// never see a fresh answer can never be released at all.
	decided time.Time
}

// pinScope is WHOSE pin: an agent, in a pipeline.
//
// The pipeline half is the correction for a collision that was always latent
// and became live traffic once a pin gained an exit condition. One process
// serves several pipelines (one daemon holding several pipelines, `steps web` over
// a shared state file), two of them may declare an agent called `reviewer`
// with entirely different source: blocks, and keyed by name alone the first
// one's outage resolved the second onto an endpoint it never declared —
// probing it, and running steps against it.
//
// It is the same unscoped-global shape internal/store answers with a
// pipeline_id on every row, one package over, and it takes the same posture:
// the scope is part of the key, so a caller cannot forget it.
type pinScope struct {
	pipeline string
	agent    string
}

// agentPinScope names an agent's pin within the pipeline that declared it.
//
// config.Name is the identity the store and the web UI also use, so a pin log
// line saying pipeline=deploy joins to the run record written under deploy —
// and --name, which is an operator saying which pipeline this is, moves this
// scope along with the other two. It is stamped by the loader, so every real
// run has one; a Config built in a test rather than loaded shares the empty
// scope, as everything did before this existed.
func agentPinScope(cfg *config.Config, agentName string) pinScope {
	return pinScope{pipeline: cfg.Name, agent: agentName}
}

// pinLifetime is how long an agent stays on a fallback without anything
// re-affirming that its primary is still unwell.
//
// It exists because the pin's other exit condition — reconsiderPin, at the
// pre-run probe — lives inside a check three separate switches turn off
// (`preflight: false` on the agent, `defaults.preflight.disabled:`,
// `--no-preflight`), while the mid-run cascade that INSTALLS a pin is gated
// by none of them. A release that is a feature of a check you can disable is
// not a release: in exactly the configuration docs/agents.md recommends for a
// cold local model — the one that also names a paid hosted fallback — a
// ninety-second blip pinned a `steps web` for the life of the process.
//
// So the expiry rides on the pin itself, evaluated wherever one is read, and
// preflight stays what it always was: the cheaper, better-informed way to
// reach the same decision sooner, using a real probe instead of a clock.
//
// Fifteen minutes is the trade between the two costs. Expiring re-decides
// from the primary, which with no probe available costs one attempts: cycle
// against a source that may still be dead — slow, but bounded and paid at
// most once per lifetime per agent. NOT expiring costs every step of every
// run on a fallback that may be a paid provider, indefinitely. It is
// comfortably longer than defaults.preflight.cache: (5m), so a pin still
// outlives the probe answer that created it when preflight is on at all.
//
// The value lives in internal/config so validatePreflight can refuse a
// defaults.preflight.cache: longer than it — see config.PinLifetime.
const pinLifetime = config.PinLifetime

// selectedSources records which fallback an agent is running on, across runs.
// Process-scoped like probeCache and for the same reason: a `steps web`
// that failed over should stay failed over rather than re-failing-over on
// every poll.
//
// Keyed by pinScope, so a pipeline's pin is its own. It has two exit
// conditions and needs both: reconsiderPin, which is the informed one and is
// reachable only when preflight runs, and pinLifetime, which is the one that
// still works when it does not.
//
//nolint:gochecknoglobals // process-lifetime selection, deliberately shared across runs
var selectedSources = struct {
	mu sync.Mutex
	by map[pinScope]pinRecord
}{by: map[pinScope]pinRecord{}}

// selectSource pins an agent to the fallback entry at index for the rest of
// the process.
//
// Callers pin a source that has PROVED itself — answered a preflight probe,
// or served a conversation through to its own conclusion — never one merely
// chosen. Pinning on selection alone strands the process on a source that
// never answered until reconsiderPin's next probe notices, which is a whole
// run's worth of steps aimed at something known not to work.
func selectSource(scope pinScope, source config.AgentSource, index int) {
	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	now := time.Now()
	record := pinRecord{
		selection: sourceSelection{source: source, index: index},
		since:     now,
		decided:   now,
	}

	// since never moves while a pin stands: it dates the PRIMARY's outage,
	// and nothing that happens among the fallbacks speaks to that.
	//
	// decided is the lifetime's clock, and it turns on whether this is the
	// same pin or a different one. Re-pinning the SAME source says nothing
	// new — every step that runs on a pinned fallback and serves re-pins it
	// (llmclient.DecideCascade returns llmclient.PinThisSource for any source that carried a
	// conversation), so restarting the lifetime there would mean a busy
	// pipeline never re-decides at all. A cascade that MOVED is a fresh
	// decision backed by fresh evidence: the source it just installed carried
	// a conversation seconds ago. Handing it the old source's spent clock
	// dropped it on the very next read, and the run then paid a cycle at the
	// dead primary and another at the dead fallback to arrive back where it
	// already was — twice inside a window docs/agents.md promises costs one.
	if existing, ok := selectedSources.by[scope]; ok {
		record.since = existing.since

		if existing.selection == record.selection {
			record.decided = existing.decided
		}
	}

	selectedSources.by[scope] = record
}

// renewPinIf restarts a pin's LIFETIME, while it still holds the selection the
// caller evaluated, and reports whether it did.
//
// Compare-and-set for the same reason clearSourceIf is compare-and-delete, and
// the report matters for the same reason: a guard that quietly does nothing is
// a caller believing it wrote. reconsiderPin returns keepPin on the strength
// of this, and a pin another goroutine has since dropped is not one to keep.
//
// It touches `decided` and never `since`. Moving the freshness bar forward on
// a renewal would make every answer already in the probe cache stale, and a
// pin that cannot see a fresh answer cannot be released by one — the renewal
// would quietly become the thing that makes the pin permanent.
//
// One caller, deliberately: the pre-run probe, on the branch where it decides
// to KEEP a pin. It is what divides the pin's two exits — while preflight runs
// it re-decides every run from probes and the clock never runs out; where
// preflight is switched off nothing renews it and the clock is the only exit
// there is.
func renewPinIf(scope pinScope, expected sourceSelection) bool {
	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	current, ok := selectedSources.by[scope]
	if !ok || current.selection != expected {
		return false
	}

	current.decided = time.Now()
	selectedSources.by[scope] = current

	return true
}

// clearSource drops an agent's pin, so the next run resolves from its primary
// again. Called when a cascade tried every source and none served: continuing
// to prefer the last one tried would be preferring a source that just failed.
// reconsiderPin drops a pin for its own reasons, through clearSourceIf.
func clearSource(scope pinScope) {
	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	delete(selectedSources.by, scope)
}

// clearSourceIf drops an agent's pin only while it still holds the selection
// the caller evaluated, reporting whether it did.
//
// The unconditional clearSource is right for the mid-run cascade, whose read
// and write are the same step of the same conversation. It is wrong for a
// decision that spans network probes: `steps web --max-concurrent` and a
// multi-pipeline `steps web` both run jobs as concurrent goroutines against
// this one map, so another job's cascade can pin a source that has actually
// SERVED while this one is still probing. Losing that write to a stale
// reading is how an agent goes back to a primary that just died mid-run.
//
// It compares the SELECTION and not the whole record, which is the reason
// pinRecord keeps its clocks outside it: with a timestamp in the compared
// value, a concurrent pass that merely renewed the lifetime made this guard
// fail for a pin whose source and index had not moved at all — turning every
// such race into a silently lost release rather than a detected one.
func clearSourceIf(scope pinScope, expected sourceSelection) clearOutcome {
	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	current, ok := selectedSources.by[scope]
	if !ok {
		return pinAlreadyGone
	}

	if current.selection != expected {
		return pinReplaced
	}

	delete(selectedSources.by, scope)

	return clearedPin
}

// clearOutcome distinguishes the two ways a compare-and-delete can decline,
// because they call for opposite handling and one bool conflated them.
//
// pinAlreadyGone means there is no pin: the caller should fail over, since
// nothing is holding the agent anywhere. pinReplaced means a live pin exists
// that a concurrent writer installed on newer evidence than this pass read —
// failing over there walks the fallback list from index 0 and demotes the
// agent off the entry another job's cascade just proved, which is precisely
// what the guard exists to prevent.
type clearOutcome int

const (
	clearedPin clearOutcome = iota
	pinAlreadyGone
	pinReplaced
)

// selectedSource returns the fallback selection preflight (or a served
// mid-run swap) chose for an agent, if any. internal/pipeline has no need for
// it; the agent step itself applies it.
//
// Reading is where a pin expires, because reading is the one thing every path
// to a pin has in common — the pre-run probe, a step resolving its source,
// the cascade. Hanging the lifetime off any single writer would leave it
// exactly as skippable as reconsiderPin already is.
func selectedSource(scope pinScope) (sourceSelection, bool) {
	return selectedSourceAt(scope, time.Now())
}

// runBoundaryKey carries the instant a run began.
type runBoundaryKey struct{}

// WithRunBoundary stamps a context with the moment its run started, which is
// the instant every pin read inside that run is judged against.
//
// The lifetime still rides on the pin and is still applied wherever one is
// read — hanging it off a writer would leave it exactly as skippable as
// reconsiderPin. What this fixes is WHICH clock the read consults. Reading at
// wall-clock meant a job of six agent steps could cross the lifetime between
// steps 4 and 5: the pin vanished mid-job and the remaining steps resolved to
// a primary that was still down, each paying a full attempts: cycle before
// cascading and re-pinning. Nothing renews mid-job, because renewPinIf is
// reachable only from Preflight — once per job, and switched off entirely by
// --no-preflight, which is the configuration the lifetime exists to serve.
//
// Judging against the run's own start makes the release land at a boundary
// where nothing is half-done, which is what docs/agents.md has always
// promised: a run decides before it starts, and one job sees a settled source
// throughout. internal/pipeline stamps this once per job run, beside the
// OpenRouter session, so concurrent jobs each carry their own.
func WithRunBoundary(ctx context.Context) context.Context {
	return context.WithValue(ctx, runBoundaryKey{}, time.Now())
}

// runInstant is when the current run began, or now for a caller that never
// declared a run — every non-pipeline caller, and every test.
func runInstant(ctx context.Context) time.Time {
	at, ok := ctx.Value(runBoundaryKey{}).(time.Time)
	if !ok {
		return time.Now()
	}

	return at
}

// selectedSourceAt is selectedSource judged against a caller-chosen instant.
func selectedSourceAt(scope pinScope, now time.Time) (sourceSelection, bool) {
	selectedSources.mu.Lock()

	record, ok := selectedSources.by[scope]

	expired := ok && now.Sub(record.decided) >= pinLifetime
	if expired {
		delete(selectedSources.by, scope)
	}

	selectedSources.mu.Unlock()

	if expired {
		// As loud as arriving and as loud as returning: an operator watching
		// for a change in model quality needs every edge, and this one moves
		// an agent back to its primary with no probe behind it.
		slog.Warn("agent.failover.pin_expired",
			"pipeline", scope.pipeline,
			"agent", scope.agent,
			"model", record.selection.source.Model,
			"lifetime", pinLifetime,
			"reason", "the pin outlived its evidence; re-deciding from the primary")

		return sourceSelection{}, false
	}

	return record.selection, ok
}

// pinnedSource is selectedSource without the lifetime: whatever is recorded,
// however old, with the moment the agent left its primary. Only reconsiderPin
// wants this — see the note there.
func pinnedSource(scope pinScope) (pinRecord, bool) {
	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	record, ok := selectedSources.by[scope]

	return record, ok
}
