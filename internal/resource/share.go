package resource

// One check, many pipelines.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/shell"
)

// CheckShare runs a check once for every caller asking the identical question
// inside a window, and hands each the same answer.
//
// A daemon polling many pipelines that watch the same repository ran the same
// `git ls-remote` once per pipeline per interval. Identical means the same
// rendered command — which folds in source: and the cursor — run the same way:
// image, env names, user, network, privilege, limits. Two pipelines whose
// cursors differ render different commands and share nothing. Only the
// execution is shared: each caller records its own history and triggers its
// own jobs, which is narrower than Concourse's opt-in global resources, where
// pipelines share one version history.
//
// An answer is reused for ttl, which costs a caller at most that much
// staleness — the same as its poll having happened that much earlier. A failed
// check is never reused, only coalesced with callers already waiting on it.
type CheckShare struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]*sharedCheck

	// joined, when set, is called as a caller joins a run already in flight:
	// a test's way to know waiters are waiting without sleeping on it.
	joined func()
}

type sharedCheck struct {
	done     chan struct{}
	at       time.Time
	versions []map[string]any
	err      error

	leaderCancelled bool
}

// NewCheckShare shares answers for ttl.
func NewCheckShare(ttl time.Duration) *CheckShare {
	return &CheckShare{ttl: ttl, entries: map[string]*sharedCheck{}}
}

type checkShareKey struct{}

// WithCheckShare makes the shell checks under ctx that run on this machine
// share their answers through share. A check placed on a worker never does:
// where it runs is a function on the context, which no key can compare.
func WithCheckShare(ctx context.Context, share *CheckShare) context.Context {
	return context.WithValue(ctx, checkShareKey{}, share)
}

// sharedCheckFor answers the share a check under ctx may use and the key it
// would use, or nil.
func sharedCheckFor(ctx context.Context, command string, spec shell.RunnerSpec) (*CheckShare, string) {
	share, _ := ctx.Value(checkShareKey{}).(*CheckShare)
	if share == nil {
		return nil, ""
	}

	if placer, _ := ctx.Value(placerKey{}).(Placer); placer != nil {
		return nil, ""
	}

	identity, err := json.Marshal(struct {
		Command     string   `json:"command"`
		Image       string   `json:"image"`
		User        string   `json:"user"`
		Network     string   `json:"network"`
		Env         []string `json:"env"`
		Privileged  bool     `json:"privileged"`
		CPUShares   int      `json:"cpu_shares"`
		MemoryBytes int64    `json:"memory_bytes"`
	}{command, spec.Image, spec.User, spec.Network, spec.Env, spec.Privileged, spec.CPUShares, spec.MemoryBytes})
	if err != nil {
		return nil, ""
	}

	sum := sha256.Sum256(identity)

	return share, hex.EncodeToString(sum[:])
}

// do answers key from a fresh enough earlier run, from a run already in
// flight, or by running check itself.
func (s *CheckShare) do(ctx context.Context, key string, check func() ([]map[string]any, error)) ([]map[string]any, error) {
	for {
		entry, leader := s.join(key)
		if leader {
			return s.lead(ctx, key, entry, check)
		}

		select {
		case <-entry.done:
		case <-ctx.Done():
			return nil, ctx.Err() //nolint:wrapcheck // the caller's own cancellation, as it was given
		}

		// The check ran under the leader's context, so a leader cancelled
		// mid-check (its pipeline replaced or removed) failed for a reason
		// that says nothing about the resource. A live waiter asks again.
		if entry.leaderCancelled && ctx.Err() == nil {
			continue
		}

		return cloneVersions(entry.versions), entry.err
	}
}

// join answers the entry for key and whether the caller must run it.
func (s *CheckShare) join(key string) (*sharedCheck, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	for other, entry := range s.entries {
		if entry.finished() && now.Sub(entry.at) >= s.ttl {
			delete(s.entries, other)
		}
	}

	if entry, ok := s.entries[key]; ok && entry != nil {
		if s.joined != nil {
			s.joined()
		}

		return entry, false
	}

	entry := &sharedCheck{done: make(chan struct{})}
	s.entries[key] = entry

	return entry, true
}

func (s *CheckShare) lead(ctx context.Context, key string, entry *sharedCheck, check func() ([]map[string]any, error)) ([]map[string]any, error) {
	versions, err := check()

	s.mu.Lock()
	entry.at, entry.versions, entry.err = time.Now(), versions, err
	entry.leaderCancelled = err != nil && ctx.Err() != nil

	if err != nil {
		delete(s.entries, key)
	}

	s.mu.Unlock()
	close(entry.done)

	return cloneVersions(versions), err
}

func (e *sharedCheck) finished() bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// cloneVersions hands each caller maps of its own: a version flows on into a
// template, a store write and a selection, and one caller's edit must not
// become another pipeline's version.
func cloneVersions(versions []map[string]any) []map[string]any {
	if versions == nil {
		return nil
	}

	out := make([]map[string]any, len(versions))
	for i, version := range versions {
		out[i] = maps.Clone(version)
	}

	return out
}
