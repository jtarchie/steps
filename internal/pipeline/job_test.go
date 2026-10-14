package pipeline

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/usage"
)

func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return &buf
}

func TestHumanCountSeparatesThousands(t *testing.T) {
	t.Parallel()

	for n, want := range map[int]string{
		0:       "0",
		999:     "999",
		1000:    "1,000",
		341204:  "341,204",
		3412040: "3,412,040",
	} {
		if got := humanCount(n); got != want {
			t.Errorf("humanCount(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestAResumedJobsUsageSplitsThisAttemptFromEarlier: the listed steps are this attempt's, so the line says what they add up to apart from what earlier attempts spent — and a fresh run says neither.
func TestAResumedJobsUsageSplitsThisAttemptFromEarlier(t *testing.T) {
	// Not t.Parallel(): mutates slog's default logger.
	logged := captureSlog(t)

	resumed := usage.NewResumedRun(700, 50)
	resumed.Add(usage.Step{Step: "review", Total: 10})

	ctx, printed := plainOutput(t)
	reportJobUsage(ctx, resumed)

	if want := "usage: 10 tokens across 1 agent step(s) this attempt, 60 total for the run (50 from earlier attempts)"; !strings.Contains(printed(), want) {
		t.Errorf("printed %q, want %q", printed(), want)
	}

	for _, want := range []string{"prior_tokens=50", "attempt_tokens=10", "budget_tokens=700"} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("job.usage line %q lacks %s", logged.String(), want)
		}
	}

	logged.Reset()

	fresh := usage.NewRun(0)
	fresh.Add(usage.Step{Step: "review", Total: 10})

	ctx, printed = plainOutput(t)
	reportJobUsage(ctx, fresh)

	if strings.Contains(printed(), "attempt") {
		t.Errorf("a fresh run printed %q, which speaks of attempts", printed())
	}

	for _, unwanted := range []string{"prior_tokens", "attempt_tokens", "budget_tokens"} {
		if strings.Contains(logged.String(), unwanted) {
			t.Errorf("a fresh, unbudgeted run logged %s: %q", unwanted, logged.String())
		}
	}
}

type spendStore struct {
	store.Store

	spent int
	err   error
}

func (s spendStore) RunTokensSpent(context.Context, string) (int, error) { return s.spent, s.err }

func (s spendStore) Prune(context.Context, store.Retention, string) error { return s.err }

// TestPriorSpendIsWhatEarlierAttemptsSpent: a resume starts its budget from what was already paid for, says so when it is anything, and fails open — to zero, loudly — when the store cannot answer.
func TestPriorSpendIsWhatEarlierAttemptsSpent(t *testing.T) {
	// Not t.Parallel(): mutates slog's default logger.
	resuming := &resumeState{id: "r-1", resuming: true}

	for name, c := range map[string]struct {
		st      store.Store
		resume  *resumeState
		want    int
		logLine string
	}{
		"resumed":          {spendStore{spent: 400}, resuming, 400, "run.resume.prior_spend"},
		"resumed, unspent": {spendStore{}, resuming, 0, ""},
		"store refuses":    {spendStore{spent: 400, err: os.ErrPermission}, resuming, 0, "run.resume.prior_spend_unavailable"},
		"fresh run":        {spendStore{spent: 400}, &resumeState{id: "r-1"}, 0, ""},
		"no resume state":  {spendStore{spent: 400}, nil, 0, ""},
		"no store":         {nil, resuming, 0, ""},
	} {
		logged := captureSlog(t)

		if got := priorSpend(context.Background(), c.st, c.resume); got != c.want {
			t.Errorf("%s: priorSpend = %d, want %d", name, got, c.want)
		}

		if c.logLine == "" && logged.Len() != 0 {
			t.Errorf("%s: logged %q, want nothing", name, logged.String())
		}

		if c.logLine != "" && !strings.Contains(logged.String(), "msg="+c.logLine+" ") {
			t.Errorf("%s: logged %q, want %s", name, logged.String(), c.logLine)
		}
	}
}

// TestAFailedPruneIsLogged: retention failing does not fail the build, so the warning is the only sign the state file has stopped being bounded.
func TestAFailedPruneIsLogged(t *testing.T) {
	logged := captureSlog(t)

	pruneHistory(context.Background(), spendStore{err: os.ErrPermission}, &config.Config{}, "build", "r-1")

	if !strings.Contains(logged.String(), "msg=store.prune ") || !strings.Contains(logged.String(), "job=build") {
		t.Errorf("logged %q, want a store.prune warning naming the job", logged.String())
	}

	logged.Reset()

	pruneHistory(context.Background(), spendStore{}, &config.Config{}, "build", "r-1")

	if logged.Len() != 0 {
		t.Errorf("a prune that worked logged %q", logged.String())
	}
}
