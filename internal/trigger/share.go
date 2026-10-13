package trigger

import (
	"context"
	"time"

	rsrc "github.com/jtarchie/steps/internal/resource"
)

// SharedChecks is one process's share of identical checks across the
// pipelines it polls (see resource.CheckShare). Only polls take part: a build
// checks for itself, because a person who triggers one right after a push
// wants what exists now, not what a poll saw.
type SharedChecks struct {
	share *rsrc.CheckShare
}

// NewSharedChecks reuses an answer for half an interval, so a pipeline's poll
// sees a version no later than a poll half an interval earlier would have.
func NewSharedChecks(interval time.Duration) *SharedChecks {
	return &SharedChecks{share: rsrc.NewCheckShare(interval / 2)}
}

// Context is ctx with the share installed, for one pipeline's poll loop.
func (s *SharedChecks) Context(ctx context.Context) context.Context {
	if s == nil {
		return ctx
	}

	return rsrc.WithCheckShare(ctx, s.share)
}
