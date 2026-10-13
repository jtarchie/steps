package resource

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/shell"
)

func TestCheckShareReusesAnAnswerForItsWindow(t *testing.T) {
	t.Parallel()

	share := NewCheckShare(time.Hour)

	var runs atomic.Int32

	check := func() ([]map[string]any, error) {
		runs.Add(1)

		return []map[string]any{{"ref": "a"}}, nil
	}

	_, _ = share.do(t.Context(), "k", check)
	second, _ := share.do(t.Context(), "k", check)
	_, _ = share.do(t.Context(), "other", check)

	if runs.Load() != 2 {
		t.Fatalf("ran %d checks for two keys, want 2", runs.Load())
	}

	// One caller's edit must not become another pipeline's version.
	second[0]["ref"] = "mutated"

	third, _ := share.do(t.Context(), "k", check)
	if third[0]["ref"] != "a" {
		t.Errorf("a caller's edit reached a later caller's answer: %v", third)
	}
}

func TestCheckShareForgetsAnExpiredAnswer(t *testing.T) {
	t.Parallel()

	share := NewCheckShare(time.Nanosecond)

	var runs atomic.Int32

	check := func() ([]map[string]any, error) {
		runs.Add(1)

		return nil, nil
	}

	_, _ = share.do(t.Context(), "k", check)

	time.Sleep(time.Millisecond)

	_, _ = share.do(t.Context(), "k", check)

	if runs.Load() != 2 {
		t.Errorf("ran %d checks across an expired window, want 2", runs.Load())
	}
}

func TestCheckShareNeverReusesAFailure(t *testing.T) {
	t.Parallel()

	share := NewCheckShare(time.Hour)

	var runs atomic.Int32

	check := func() ([]map[string]any, error) {
		runs.Add(1)

		return nil, errors.New("rate limited")
	}

	_, first := share.do(t.Context(), "k", check)
	_, second := share.do(t.Context(), "k", check)

	if first == nil || second == nil || runs.Load() != 2 {
		t.Errorf("errors %v, %v after %d runs: a failure was reused", first, second, runs.Load())
	}
}

func TestCheckShareCoalescesCallersInFlight(t *testing.T) {
	t.Parallel()

	share := NewCheckShare(time.Hour)

	var runs atomic.Int32

	release := make(chan struct{})
	started := make(chan struct{})

	versions := []map[string]any{{"ref": "a"}}

	check := func() ([]map[string]any, error) {
		if runs.Add(1) == 1 {
			close(started)
		}

		<-release

		if versions == nil {
			return nil, errors.New("no versions")
		}

		return versions, nil
	}

	var wg sync.WaitGroup

	wg.Go(func() { _, _ = share.do(t.Context(), "k", check) })

	<-started

	for range 5 {
		wg.Go(func() {
			got, err := share.do(t.Context(), "k", check)
			if err != nil || len(got) != 1 {
				t.Errorf("a waiting caller got %v, %v", got, err)
			}
		})
	}

	// Let the waiters reach the in-flight entry before it finishes.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if runs.Load() != 1 {
		t.Errorf("ran %d checks for six callers at once, want 1", runs.Load())
	}
}

// Where a placed check runs is a function on the context, which no key can
// compare, so it never shares.
func TestAPlacedCheckDoesNotShare(t *testing.T) {
	t.Parallel()

	ctx := WithCheckShare(context.Background(), NewCheckShare(time.Hour))

	if share, _ := sharedCheckFor(ctx, "true", shell.RunnerSpec{}); share == nil {
		t.Fatal("a local check under a share did not share")
	}

	placed := context.WithValue(ctx, placerKey{}, Placer(func(shell.RunnerSpec) (shell.Runner, error) { return nil, errors.New("never called") }))

	if share, _ := sharedCheckFor(placed, "true", shell.RunnerSpec{}); share != nil {
		t.Error("a placed check shared its answer")
	}

	_, one := sharedCheckFor(ctx, "true", shell.RunnerSpec{Image: "a"})
	_, two := sharedCheckFor(ctx, "true", shell.RunnerSpec{Image: "b"})

	if one == two {
		t.Error("checks in different images shared a key")
	}
}
