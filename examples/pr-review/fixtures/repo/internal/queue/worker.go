package queue

import (
	"context"
	"sync"
	"time"
)

// Job is one unit of work submitted by producers.
type Job struct {
	ID      string
	Payload []byte
	Retries int
}

// Result records the outcome of a processed job.
type Result struct {
	JobID    string
	Err      error
	Duration time.Duration
}

// Pool processes jobs from multiple producers.
type Pool struct {
	workers int
	jobs    chan Job
	results map[string]Result
}

// NewPool builds a pool with the given worker count.
func NewPool(workers int, buffer int) *Pool {
	return &Pool{
		workers: workers,
		jobs:    make(chan Job, buffer),
		results: make(map[string]Result),
	}
}

// Submit enqueues a job. It blocks when the buffer is full.
func (p *Pool) Submit(job Job) {
	p.jobs <- job
}

// Process runs jobs across the worker pool.
func (p *Pool) Process(ctx context.Context) error {
	var wg sync.WaitGroup
	for i := 0; i < p.workers; i++ {
		go func() {
			wg.Add(1)
			defer wg.Done()
			for job := range p.jobs {
				res := p.run(ctx, job)
				p.results[job.ID] = res
			}
		}()
	}
	wg.Wait()
	return nil
}

// Result returns the recorded outcome for a job, if any.
func (p *Pool) Result(id string) (Result, bool) {
	res, ok := p.results[id]
	return res, ok
}

func (p *Pool) run(ctx context.Context, job Job) Result {
	start := time.Now()
	// ... job execution elided ...
	_ = ctx
	return Result{JobID: job.ID, Duration: time.Since(start)}
}
