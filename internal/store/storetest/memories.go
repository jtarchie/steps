package storetest

// memories: what an agent step keeps about a scope, and the walls around it —
// another scope, another pipeline, and the cap.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

func remember(ctx context.Context, t *testing.T, st store.Store, scope, text string, limit int) store.Memory {
	t.Helper()

	memory, _, err := st.Remember(ctx, store.Memory{Scope: scope, Text: text}, limit)
	if err != nil {
		t.Fatalf("Remember(%q, %q): %v", scope, text, err)
	}

	return memory
}

func memoryTexts(ctx context.Context, t *testing.T, st store.Store, scope string) []string {
	t.Helper()

	memories, err := st.ListMemories(ctx, scope, 0)
	if err != nil {
		t.Fatalf("ListMemories(%q): %v", scope, err)
	}

	texts := make([]string, 0, len(memories))
	for _, memory := range memories {
		texts = append(texts, memory.Text)
	}

	return texts
}

// TestMemoriesListNewestFirst: the preload reads from the top and stops at a
// byte cap, so the order is what decides which facts a long memory keeps
// showing the model.
func (s suite) TestMemoriesListNewestFirst(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	remember(ctx, t, st, "U1", "likes go", 0)
	remember(ctx, t, st, "U1", "works nights", 0)
	remember(ctx, t, st, "U1", "prefers code samples", 0)

	got := strings.Join(memoryTexts(ctx, t, st, "U1"), "|")
	if got != "prefers code samples|works nights|likes go" {
		t.Errorf("memories = %s, want newest first", got)
	}

	two, err := st.ListMemories(ctx, "U1", 2)
	if err != nil {
		t.Fatal(err)
	}

	if len(two) != 2 || two[0].Text != "prefers code samples" {
		t.Errorf("ListMemories(2) = %+v, want the newest two", two)
	}
}

// TestRememberingTheSameTextFilesItOnce: a model that re-saves what it was
// handed every run would otherwise fill its own preload with copies.
func (s suite) TestRememberingTheSameTextFilesItOnce(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	first, added, err := st.Remember(ctx, store.Memory{Scope: "U1", Text: "likes go"}, 0)
	if err != nil || !added {
		t.Fatalf("first Remember: added=%v err=%v", added, err)
	}

	again, added, err := st.Remember(ctx, store.Memory{Scope: "U1", Text: "likes go"}, 0)
	if err != nil {
		t.Fatal(err)
	}

	if added || again.ID != first.ID {
		t.Errorf("second Remember = %+v added=%v, want the first entry (%d) back, not a copy", again, added, first.ID)
	}

	if got := memoryTexts(ctx, t, st, "U1"); len(got) != 1 {
		t.Errorf("memories = %v, want one entry", got)
	}

	// The same text under another scope is that scope's own fact.
	_, added, err = st.Remember(ctx, store.Memory{Scope: "U2", Text: "likes go"}, 0)
	if err != nil || !added {
		t.Errorf("the same text under another scope: added=%v err=%v, want it filed", added, err)
	}
}

// TestAScopeSeesOnlyItsOwnMemories is the isolation the feature exists for:
// what one user told the bot is not what another user is shown.
func (s suite) TestAScopeSeesOnlyItsOwnMemories(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	remember(ctx, t, st, "U1", "dana's birthday is in may", 0)
	remember(ctx, t, st, "U2", "prefers short answers", 0)

	if got := memoryTexts(ctx, t, st, "U2"); len(got) != 1 || got[0] != "prefers short answers" {
		t.Errorf("U2 sees %v, want only its own entry", got)
	}

	if got := memoryTexts(ctx, t, st, "U3"); len(got) != 0 {
		t.Errorf("a scope with nothing remembered sees %v", got)
	}
}

// TestForgetDeletesOnlyFromTheScopeNamed: the id comes from a model, and a
// model can be told any id. One that is another scope's must miss.
func (s suite) TestForgetDeletesOnlyFromTheScopeNamed(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	theirs := remember(ctx, t, st, "U1", "dana's birthday is in may", 0)
	mine := remember(ctx, t, st, "U2", "prefers short answers", 0)

	forgot, err := st.Forget(ctx, "U2", theirs.ID)
	if err != nil {
		t.Fatal(err)
	}

	if forgot {
		t.Error("U2 forgot an entry of U1's")
	}

	if got := memoryTexts(ctx, t, st, "U1"); len(got) != 1 {
		t.Errorf("U1's memories = %v after U2 named one, want it kept", got)
	}

	forgot, err = st.Forget(ctx, "U2", mine.ID)
	if err != nil || !forgot {
		t.Fatalf("Forget of its own entry: forgot=%v err=%v", forgot, err)
	}

	if got := memoryTexts(ctx, t, st, "U2"); len(got) != 0 {
		t.Errorf("U2's memories = %v after forgetting, want none", got)
	}

	forgot, err = st.Forget(ctx, "U2", mine.ID)
	if err != nil || forgot {
		t.Errorf("forgetting it twice: forgot=%v err=%v, want false", forgot, err)
	}
}

// TestForgetScopeDeletesEverythingOfOneScope: how a person is forgotten
// entirely, and only that person.
func (s suite) TestForgetScopeDeletesEverythingOfOneScope(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	remember(ctx, t, st, "U1", "one", 0)
	remember(ctx, t, st, "U1", "two", 0)
	remember(ctx, t, st, "U2", "kept", 0)

	gone, err := st.ForgetScope(ctx, "U1")
	if err != nil {
		t.Fatal(err)
	}

	if gone != 2 {
		t.Errorf("ForgetScope reported %d, want 2", gone)
	}

	if got := memoryTexts(ctx, t, st, "U1"); len(got) != 0 {
		t.Errorf("U1 = %v, want nothing", got)
	}

	if got := memoryTexts(ctx, t, st, "U2"); len(got) != 1 {
		t.Errorf("U2 = %v, want its entry kept", got)
	}
}

// TestTheMemoryCapEvictsTheOldest: per scope, oldest first, zero is no limit
// and a negative limit is the default.
func (s suite) TestTheMemoryCapEvictsTheOldest(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	remember(ctx, t, st, "U2", "another scope's", 2)

	for _, text := range []string{"a", "b", "c"} {
		remember(ctx, t, st, "U1", text, 2)
	}

	if got := strings.Join(memoryTexts(ctx, t, st, "U1"), ""); got != "cb" {
		t.Errorf("memories = %q under a cap of 2, want the newest two", got)
	}

	if got := memoryTexts(ctx, t, st, "U2"); len(got) != 1 {
		t.Errorf("another scope = %v, want it untouched by U1's cap", got)
	}

	remember(ctx, t, st, "U1", "d", 0)
	remember(ctx, t, st, "U1", "e", 0)

	if got := memoryTexts(ctx, t, st, "U1"); len(got) != 4 {
		t.Errorf("memories = %v under no limit, want all four", got)
	}

	for i := range store.DefaultMemoryEntries {
		remember(ctx, t, st, "U3", "fact "+strings.Repeat("x", i), -1)
	}

	remember(ctx, t, st, "U3", "the newest", -1)

	got := memoryTexts(ctx, t, st, "U3")
	if len(got) != store.DefaultMemoryEntries || got[0] != "the newest" || got[len(got)-1] != "fact x" {
		t.Errorf("default cap kept %d entries ending %q, want %d with the oldest gone", len(got), got[len(got)-1], store.DefaultMemoryEntries)
	}
}

// TestMemoriesBelongToTheirPipeline: two pipelines in one database, each with
// a user U1, are two different people.
func (s suite) TestMemoriesBelongToTheirPipeline(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	web := s.open(t, "web")
	infra := s.open(t, "infra")

	theirs := remember(ctx, t, web, "U1", "web's fact", 0)

	if got := memoryTexts(ctx, t, infra, "U1"); len(got) != 0 {
		t.Errorf("infra sees %v, want nothing of web's", got)
	}

	forgot, err := infra.Forget(ctx, "U1", theirs.ID)
	if err != nil || forgot {
		t.Errorf("infra forgetting web's entry: forgot=%v err=%v, want false", forgot, err)
	}

	gone, err := infra.ForgetScope(ctx, "U1")
	if err != nil || gone != 0 {
		t.Errorf("infra forgetting its U1: gone=%d err=%v, want 0", gone, err)
	}

	scopes, err := infra.MemoryScopes(ctx)
	if err != nil || len(scopes) != 0 {
		t.Errorf("infra's scopes = %+v err=%v, want none", scopes, err)
	}

	if got := memoryTexts(ctx, t, web, "U1"); len(got) != 1 {
		t.Errorf("web's U1 = %v, want its entry", got)
	}
}

// TestAMemoryOutlivesTheRunThatRememberedIt: run_history: reaps runs, and a
// fact about a person is not a run's history.
func (s suite) TestAMemoryOutlivesTheRunThatRememberedIt(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	build(ctx, t, st, "answer", "run-old", 1)

	memory, _, err := st.Remember(ctx, store.Memory{Scope: "U1", Text: "likes go", RunID: "run-old"}, 0)
	if err != nil {
		t.Fatal(err)
	}

	if memory.RunID != "run-old" || memory.CreatedAt == "" {
		t.Errorf("Remember = %+v, want the run and a time it was remembered", memory)
	}

	build(ctx, t, st, "answer", "run-new", 2)

	err = st.Prune(ctx, store.Retention{JobName: "answer", Runs: 1}, "")
	if err != nil {
		t.Fatal(err)
	}

	runs, err := st.ListRuns(ctx, "answer", 0)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v err=%v, want the old one reaped", runs, err)
	}

	memories, err := st.ListMemories(ctx, "U1", 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(memories) != 1 || memories[0].RunID != "" {
		t.Errorf("memories = %+v, want the entry kept with its reaped run cleared", memories)
	}
}

// TestAMemoryNamesOnlyItsOwnPipelinesRun: a sibling's run id satisfies the
// foreign key, and a memory claiming it would point at a run this pipeline
// cannot show.
func (s suite) TestAMemoryNamesOnlyItsOwnPipelinesRun(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	web := s.open(t, "web")
	infra := s.open(t, "infra")

	mustStartRuns(ctx, t, infra, "build", "infra-run")

	memory, _, err := web.Remember(ctx, store.Memory{Scope: "U1", Text: "likes go", RunID: "infra-run"}, 0)
	if err != nil {
		t.Fatal(err)
	}

	if memory.RunID != "" {
		t.Errorf("RunID = %q, want nothing: the run is another pipeline's", memory.RunID)
	}
}

// TestDeletingAPipelineForgetsItsMemories: destroy is one DELETE, and what a
// bot knew about people goes with it.
func (s suite) TestDeletingAPipelineForgetsItsMemories(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	doomed := s.open(t, "test")

	remember(ctx, t, doomed, "U1", "likes go", 0)

	err := doomed.Delete(ctx)
	if err != nil {
		t.Fatal(err)
	}

	again := s.open(t, "test")

	if got := memoryTexts(ctx, t, again, "U1"); len(got) != 0 {
		t.Errorf("a pipeline set again under the same name knows %v", got)
	}
}

// TestMemoryScopesSummarizeWhatIsKept: what `steps memory` lists first.
func (s suite) TestMemoryScopesSummarizeWhatIsKept(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	remember(ctx, t, st, "U2", "one", 0)
	remember(ctx, t, st, "U1", "one", 0)
	newest := remember(ctx, t, st, "U1", "two", 0)

	scopes, err := st.MemoryScopes(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(scopes) != 2 || scopes[0].Scope != "U1" || scopes[0].Entries != 2 || scopes[1].Scope != "U2" {
		t.Fatalf("scopes = %+v, want U1 (2) then U2 (1)", scopes)
	}

	if scopes[0].LastAt != newest.CreatedAt {
		t.Errorf("U1 last remembered at %q, want its newest entry's %q", scopes[0].LastAt, newest.CreatedAt)
	}
}

// TestRememberRefusesWhatNoDriverCanFile: the caps and the text rules are the
// contract's, so both drivers refuse the same things the same way.
func (s suite) TestRememberRefusesWhatNoDriverCanFile(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	for name, memory := range map[string]store.Memory{
		"no scope":      {Text: "likes go"},
		"no text":       {Scope: "U1"},
		"a long text":   {Scope: "U1", Text: strings.Repeat("x", store.MaxMemoryBytes+1)},
		"a long scope":  {Scope: strings.Repeat("U", store.MaxMemoryScopeBytes+1), Text: "likes go"},
		"a NUL":         {Scope: "U1", Text: "likes\x00go"},
		"invalid UTF-8": {Scope: "U1", Text: "likes \xff go"},
	} {
		_, _, err := st.Remember(ctx, memory, 0)
		if !errors.Is(err, store.ErrMemoryRefused) {
			t.Errorf("%s: err = %v, want ErrMemoryRefused", name, err)
		}
	}

	_, added, err := st.Remember(ctx, store.Memory{Scope: "U1", Text: strings.Repeat("x", store.MaxMemoryBytes)}, 0)
	if err != nil || !added {
		t.Errorf("a memory exactly at the cap: added=%v err=%v, want it filed", added, err)
	}

	if got := memoryTexts(ctx, t, st, "U1"); len(got) != 1 {
		t.Errorf("memories = %d entries, want only the one at the cap", len(got))
	}
}
