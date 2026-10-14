package pipeline

import (
	"context"

	"github.com/jtarchie/steps/internal/workspace"
)

// Run is what a caller decides about one job run before it starts. These
// were context values once, and a value read off a context is one nothing
// checks a caller for: a rerun key could override a positional --force, and
// shims existed only to forward to internal/agent, which RunJob now feeds
// from here. What stays on the context is what other packages read (the
// events bus, run and step ids, agent's own values) and what a poll reads as
// well as a run — workers, the shared registry, leases, --keep-workspace and
// --no-preflight — because a poll has no Run.
type Run struct {
	// Provider materializes every build/step workspace the run needs.
	Provider workspace.Provider
	// Pinned applies to every get step's version selection.
	Pinned map[string]string
	// Force (--force) bypasses chain-skip planning only; results are still
	// recorded.
	Force bool
	// ID fixes the id of the run RunJob starts, so a caller can address that
	// run — to abort it — before it exists. Empty mints one; a resume keeps
	// the id of the run it continues.
	ID string
	// RerunOf names the recorded run this one re-runs, from the top and
	// against the versions it was created with; see PrepareRerun. Implies
	// Force.
	RerunOf string
	// ReopenTaken makes the `version: every` cursor stop filtering, so
	// versions an earlier run already took are fanned out again. Only `steps
	// test` asks: its execution assertions must hold on every rerun against
	// one state file. Force does not imply it (#145) — replaying a resource's
	// history repeats effects the cache never skips.
	ReopenTaken bool
	// Backlog is told, as the run takes its version, how many input sets are
	// still waiting behind it. The daemon queues the job again; `steps run`
	// says so, building one version per invocation as `fly trigger-job`
	// does. Told at the take rather than at the finish, so the next build can
	// start beside this one when max_in_flight allows it.
	Backlog func(ctx context.Context, sets int)
	// Answers are --answer seeds for ask_user; see ParseAnswers.
	Answers []AnswerSeed
	// AnswerDB is the --db a parked step's printed answer command needs, ""
	// when the read commands' default already opens this run's state.
	AnswerDB string
	// ExternalURL is the daemon's address, published to every step as
	// STEPS_URL.
	ExternalURL string

	// setIndex builds the index-th waiting set instead of the oldest, and
	// builds hands the job assert to the BuildEveryVersion judging every run
	// of it; only BuildEveryVersion sets either.
	setIndex int
	builds   *jobBuilds
}
