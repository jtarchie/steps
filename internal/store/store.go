// Package store is the contract steps holds its state database to: the row
// types every driver returns, the errors every driver raises, and the Store
// interface every driver implements.
//
// It runs no SQL and opens no connection. A driver — internal/store/sqlite
// today — owns its schema, its version stamp and its queries end to end, so
// each can use what its database is good at rather than the intersection of
// two. What keeps them honest with each other is this file plus the tests
// written against it.
//
// Only internal/cli names a driver; every other package takes a Store it was
// handed.
package store

import (
	"context"
	"errors"
	"time"
)

// Store is one pipeline's view of the state database.
//
// The handle IS the scope, and that is the property the interface exists to
// preserve: a state file (or a Postgres database) may hold several pipelines,
// and no method here takes a pipeline name. A caller cannot forget to pass one
// and cannot pass the wrong one. It matters most for the cache — a merkle hash
// folds in kind, content and parent but NOT the pipeline, so two pipelines each
// with a job named `build` over an identical task hash the same, and an
// unscoped skip index would let the second one skip work it never did.
//
// `steps web` serving three pipelines from one database holds three of these.
// Reading ACROSS them is Reader, which is a different type for the same reason.
type Store interface {
	// Pipeline is the name this handle is scoped to.
	Pipeline() string

	// Description identifies the database behind this handle, in a form safe
	// to print: the file path for SQLite, a credential-free URL for a network
	// database. It is stable, so two handles reporting the same description
	// share a database — which is how a caller groups handles before asking
	// one Reader about several pipelines at once.
	Description() string

	// SetSourcePath records where this pipeline's YAML lives, so a reader of
	// the database can tell two checkouts apart. Only a command that LOADED
	// the pipeline calls it.
	SetSourcePath(ctx context.Context, source string) error

	// Reader reads across every pipeline in this database, unscoped by
	// construction.
	Reader() Reader

	// Close releases the connection, after whatever reclaim the driver owes
	// for what retention deleted. A handle obtained by RESOLVING a pipeline
	// rather than registering one must not write here: a command whose whole
	// contract is that it only asks changes no bytes.
	Close() error

	// runs: one row per invocation, what --resume continues and what the
	// history views list.

	// StartRun mints a run. An id some run already holds is ErrRunExists,
	// never an overwrite.
	StartRun(ctx context.Context, id, jobName, workspaceDir, configSHA string) error
	// ResumeRun continues a run this pipeline already has, ErrNoSuchRun
	// otherwise.
	ResumeRun(ctx context.Context, id, workspaceDir, configSHA string) error
	FinishRun(ctx context.Context, id, status string) error
	RecordRunStep(ctx context.Context, runID string, index int, name string) error
	RecordRunParent(ctx context.Context, runID, parentID string) error
	CompletedRunSteps(ctx context.Context, runID string) (map[int]string, error)
	// ListRuns returns a job's runs newest first. Zero means no limit, the
	// convention everywhere here.
	ListRuns(ctx context.Context, jobName string, limit int) ([]RunRow, error)
	LatestRunByJob(ctx context.Context) (map[string]RunRow, error)
	RunsUsingNode(ctx context.Context, hash string, limit int) ([]RunRow, error)
	FindRunRow(ctx context.Context, id string) (RunRow, bool, error)
	FirstRunSince(ctx context.Context, jobName string, since time.Time) (RunRow, bool, error)

	// job_runs and nodes: the merkle caches that decide what a rerun skips.

	RecordJobRun(ctx context.Context, jobName, rootHash, status string, runErr error) error
	HasSucceededBatch(ctx context.Context, jobName string, rootHashes []string) (map[string]bool, error)
	RecordNode(ctx context.Context, node NodeRecord, jobName, status string, result map[string]any, execErr error) error
	HasNodeSucceeded(ctx context.Context, jobName, hash string) (bool, error)
	ListNodes(ctx context.Context, jobName string, limit int) ([]NodeRow, error)
	NodesByHash(ctx context.Context, hashes []string) (map[string]NodeRow, error)
	// SaveNodeTranscript replaces the transcript of a node, truncated to
	// MaxTranscriptBytes along whole events so it stays valid JSON.
	SaveNodeTranscript(ctx context.Context, hash, transcript string) error
	NodeTranscript(ctx context.Context, hash string) (string, bool, error)

	// step blobs: what a cached step's outputs were, so a skip can restore
	// them.

	RecordStepBlobs(ctx context.Context, actionKey string, outputs map[string]string) error
	StepBlobs(ctx context.Context, actionKey string) (map[string]string, error)

	// resource versions: what a check saw, what a job consumed, what passed.

	// RecordVersions files what a check reported and prunes beyond limit,
	// returning how many were new to check-history. A version already filed
	// keeps its discovery order.
	RecordVersions(ctx context.Context, resourceName string, versions []map[string]any, limit int) (int, error)
	ResourceVersionsJSON(ctx context.Context, resourceName string) ([]string, error)
	VersionOrders(ctx context.Context, resourceName string) (map[string]int64, error)
	RecordVersionOrder(ctx context.Context, resourceName, versionJSON string) (int64, error)
	GreenVersions(ctx context.Context, resourceName string, upstreamJobs []string) ([]map[string]any, error)
	RecordCheckedVersion(ctx context.Context, resourceName, versionJSON string) error
	LastChecked(ctx context.Context, resourceName string) (CheckedResource, bool, error)
	CheckedResources(ctx context.Context) ([]CheckedResource, error)
	RecordPassedVersion(ctx context.Context, jobName, resourceName, versionJSON, buildID string) error
	PassedVersions(ctx context.Context, jobName string, limit int) ([]PassedVersion, error)
	HasPassedVersionSet(ctx context.Context, jobName string, want map[string]string) (bool, error)
	ConsumedMark(ctx context.Context, jobName, resourceName string) (int64, error)
	RecordConsumedMark(ctx context.Context, jobName, resourceName string, order int64) error
	RecordRunInput(ctx context.Context, runID, resourceName, versionJSON string) error
	RunInputs(ctx context.Context, runID string) (map[string]map[string]bool, error)

	// trigger queue: the work list one daemon drains, and the breaker that
	// pauses a job that keeps failing.

	EnqueueJob(ctx context.Context, jobName, reason string) error
	// ClaimNextJob takes the oldest pending row admitted by serial: and
	// max_in_flight, atomically, and reports false when nothing is ready.
	ClaimNextJob(ctx context.Context) (int64, string, bool, error)
	CompleteJob(ctx context.Context, id int64, status string, runErr error) error
	// ResetStaleRunning reclaims every running row as an abandoned leftover.
	// It is only true when nothing else is alive, which is why steps runs one
	// daemon per database.
	ResetStaleRunning(ctx context.Context) error
	ListTriggerQueue(ctx context.Context, limit int) ([]QueueRow, error)
	// SyncJobLimits replaces both admission mirrors from one configuration, in
	// one transaction: a job in neither map has no serial group and no limit.
	SyncJobLimits(ctx context.Context, groups map[string][]string, limits map[string]int) error
	SerialGroupHolder(ctx context.Context, jobName string) (string, error)
	RecordJobOutcome(ctx context.Context, jobName string, succeeded bool, maxFailures int) (paused bool, consecutive int, err error)
	ResetJobFailures(ctx context.Context, jobName string) error
	IsJobPaused(ctx context.Context, jobName string) (bool, error)
	PausedJobs(ctx context.Context) ([]PausedJob, error)

	// approvals and questions: the two things a build waits on a person for.

	RequestApproval(ctx context.Context, jobName, message string) (int64, error)
	DecideApproval(ctx context.Context, id int64, status, by, reason string) error
	ApprovalStatus(ctx context.Context, id int64) (Approval, error)
	// Approvals lists newest first; pendingOnly is never capped, because a
	// waiting build that scrolled off the list waits forever.
	Approvals(ctx context.Context, pendingOnly bool, limit int) ([]Approval, error)
	// AskQuestion records a pending question, or returns the one this run
	// already asked under the same memo key — reporting false when it did.
	// The memo is the row, so twelve across: cells asking the same thing all
	// reach one row rather than one person twelve times.
	AskQuestion(ctx context.Context, question Question) (Question, bool, error)
	// AnswerQuestion is a person's answer, ErrQuestionNotPending if somebody
	// or something got there first.
	AnswerQuestion(ctx context.Context, id int64, answer, by string) error
	// CloseQuestion resolves a question with a status the caller chooses, for
	// the endings nobody answered.
	CloseQuestion(ctx context.Context, id int64, status, answer, by string) error
	QuestionStatus(ctx context.Context, id int64) (Question, error)
	// Questions lists newest first; pendingOnly is never capped, for the same
	// reason Approvals is not.
	Questions(ctx context.Context, pendingOnly bool, limit int) ([]Question, error)

	// placements: which machine ran a step, and what it found there.

	RecordPlacement(ctx context.Context, placement Placement) error
	// RunPlacements returns a run's placements in plan order.
	RunPlacements(ctx context.Context, runID string) ([]Placement, error)

	// agent usage: the token and cost ledger.

	RecordAgentUsage(ctx context.Context, usage AgentUsage) error
	RunTokensSpent(ctx context.Context, runID string) (int, error)
	RunUsage(ctx context.Context, runID string) ([]AgentUsage, error)
	RunCostTotals(ctx context.Context, limit int) ([]RunTotals, error)

	// run events: what the web UI replays and streams.

	AppendRunEvent(ctx context.Context, row RunEventRow) error
	RunEvents(ctx context.Context, runID string, afterSeq int64, limit int) ([]RunEventRow, error)

	// revisions: the configuration a run was executed under, interned once.

	RecordRevision(ctx context.Context, sha, source string) error
	FindRevision(ctx context.Context, sha string) (Revision, bool, error)

	// Prune applies one retention policy at the end of a build: the job's runs
	// and its finished queue rows are capped by COUNT, newest kept, and a
	// reaped run takes its events, steps, usage, placements, questions and
	// transcripts with it. keepRunID is never reaped, however the counts fall.
	//
	// Newest is by insertion order, not by timestamp: two runs can land in the
	// same instant, and the one to keep is the one recorded last.
	//
	// The merkle caches are bounded here too, and by count alone. Age is the
	// wrong question for them: a fully-cached poll records no new node and
	// refreshes no timestamp, so an age floor sweeps past a working pipeline's
	// cache — the faster it polls, the sooner it loses it.
	Prune(ctx context.Context, policy Retention, keepRunID string) error
}

// ErrSchemaVersion is a database some other build of steps wrote.
//
// There is no migration path, deliberately: a driver refuses a schema it
// cannot write rather than upgrading it, and the answer is to delete the
// database. What this exists for is that somebody is TOLD — an unrecognised
// schema used to open fine, lack a column, fail every insert naming it, and
// leave a green build that had recorded nothing.
var ErrSchemaVersion = errors.New("the state database was written by a different version of steps")

// MaxStoredErrorBytes bounds the error text any column here keeps.
//
// A stored error is unbounded by default, and one of them is routinely enormous:
// a failing check or task reports "command %q failed", where the command is the
// whole generated shell script — about 1.3KB for the built-in git check, with
// its comments. That message is exactly right in a terminal and far too big to
// keep a copy of per failure, forever. A remote that is down at a one-minute
// poll wrote that same 1.3KB every minute.
//
// 2KB keeps the head of any real message — the part naming what failed — while
// making the worst case a rounding error rather than the largest thing a failing
// watch produces.
const MaxStoredErrorBytes = 2 * 1024
