package web

import (
	"context"
	"sync"

	"github.com/jtarchie/steps/internal/store"
	"github.com/labstack/echo/v5"
)

// pipelineReads is one request's view of a pipeline's store. The shell, the
// attention list, the mark and the page each ask the same few questions, and
// a board poll asked most of them twice or three times; here each is asked
// once per request, on first use, and never again. Request-scoped on purpose:
// a page reflects the store as of its own render, never a previous one's.
type pipelineReads struct {
	target *Pipeline

	paused      lazy[bool]
	held        lazy[[]store.HeldJob]
	checkErrors lazy[[]store.CheckError]
	approvals   lazy[[]store.Approval]
	questions   lazy[[]store.Question]
	latest      lazy[map[string]store.RunRow]
	finished    lazy[map[string]store.RunRow]
	queue       lazy[[]store.QueueRow]
}

type lazy[T any] struct {
	once  sync.Once
	value T
	err   error
}

func (l *lazy[T]) get(read func() (T, error)) (T, error) {
	l.once.Do(func() { l.value, l.err = read() })

	return l.value, l.err
}

// requestReads holds a pipelineReads per pipeline the request touches.
type requestReads struct {
	mu  sync.Mutex
	per map[*Pipeline]*pipelineReads
}

const readsKey = "reads"

// readsOf is the request's reads of target, made on first ask.
func readsOf(c *echo.Context, target *Pipeline) *pipelineReads {
	all, ok := c.Get(readsKey).(*requestReads)
	if !ok {
		all = &requestReads{per: map[*Pipeline]*pipelineReads{}}
		c.Set(readsKey, all)
	}

	all.mu.Lock()
	defer all.mu.Unlock()

	reads, ok := all.per[target]
	if !ok {
		reads = newReads(target)
		all.per[target] = reads
	}

	return reads
}

// newReads is a pipeline's reads outside any request's sharing.
func newReads(target *Pipeline) *pipelineReads {
	return &pipelineReads{target: target}
}

// paused is Paused for a caller with no request to share reads with.
func paused(ctx context.Context, target *Pipeline) bool {
	return newReads(target).Paused(ctx)
}

// Paused answers false when the store cannot say: a page that fails to draw
// is worse than a missing banner.
func (r *pipelineReads) Paused(ctx context.Context) bool {
	is, err := r.PausedErr(ctx)

	return err == nil && is
}

func (r *pipelineReads) PausedErr(ctx context.Context) (bool, error) {
	if r.target.Store == nil {
		return false, nil
	}

	return r.paused.get(func() (bool, error) { return r.target.Store.Paused(ctx) })
}

func (r *pipelineReads) HeldJobs(ctx context.Context) ([]store.HeldJob, error) {
	return r.held.get(func() ([]store.HeldJob, error) { return r.target.Store.HeldJobs(ctx) })
}

func (r *pipelineReads) CheckErrors(ctx context.Context) ([]store.CheckError, error) {
	return r.checkErrors.get(func() ([]store.CheckError, error) { return r.target.Store.CheckErrors(ctx) })
}

// PendingApprovals is every approval still waiting for a decision.
func (r *pipelineReads) PendingApprovals(ctx context.Context) ([]store.Approval, error) {
	return r.approvals.get(func() ([]store.Approval, error) { return r.target.Store.Approvals(ctx, true, 0) })
}

// PendingQuestions is every question still waiting for an answer.
func (r *pipelineReads) PendingQuestions(ctx context.Context) ([]store.Question, error) {
	return r.questions.get(func() ([]store.Question, error) { return r.target.Store.Questions(ctx, true, 0) })
}

func (r *pipelineReads) LatestRunByJob(ctx context.Context) (map[string]store.RunRow, error) {
	return r.latest.get(func() (map[string]store.RunRow, error) { return r.target.Store.LatestRunByJob(ctx) })
}

func (r *pipelineReads) LatestFinishedRunByJob(ctx context.Context) (map[string]store.RunRow, error) {
	return r.finished.get(func() (map[string]store.RunRow, error) { return r.target.Store.LatestFinishedRunByJob(ctx) })
}

// TriggerQueue is the newest limit rows of the trigger queue. Every caller
// asks for at most overviewLimit, and the queue is ordered by its unique id,
// so the first limit rows of one overviewLimit read are exactly what a read of
// limit would return.
func (r *pipelineReads) TriggerQueue(ctx context.Context, limit int) ([]store.QueueRow, error) {
	if limit <= 0 || limit > overviewLimit {
		return r.target.Store.ListTriggerQueue(ctx, limit) //nolint:wrapcheck // the caller wraps with its own context
	}

	rows, err := r.queue.get(func() ([]store.QueueRow, error) { return r.target.Store.ListTriggerQueue(ctx, overviewLimit) })
	if err != nil {
		return nil, err
	}

	return rows[:min(limit, len(rows)):min(limit, len(rows))], nil
}
