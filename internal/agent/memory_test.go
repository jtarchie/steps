package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

func memoryStore(t *testing.T) *sqlite.Store {
	t.Helper()

	st, err := sqlite.OpenStore(filepath.Join(t.TempDir(), "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	return st
}

func writeScope(t *testing.T, content string) string {
	t.Helper()

	dir := t.TempDir()

	err := os.MkdirAll(filepath.Join(dir, "who"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "who", "user"), []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return dir
}

var scopedStep = config.Step{Agent: "bot", Memory: &config.StepMemory{ScopeFrom: "who/user"}}

// TestAScopeFileThatNamesNothingFailsTheStep: falling back to a scope every
// user shares is the one failure memory must not have.
func TestAScopeFileThatNamesNothingFailsTheStep(t *testing.T) {
	t.Parallel()

	for name, dir := range map[string]string{
		"empty":    writeScope(t, " \n"),
		"missing":  t.TempDir(),
		"too long": writeScope(t, strings.Repeat("U", store.MaxMemoryScopeBytes+1)),
	} {
		_, err := loadStepMemory(t.Context(), &config.Config{}, scopedStep, dir, memoryStore(t), false)
		if err == nil {
			t.Errorf("%s scope file: loaded, want the step to fail", name)
		}
	}

	loaded, err := loadStepMemory(t.Context(), &config.Config{}, scopedStep, writeScope(t, "U1\n"), memoryStore(t), false)
	if err != nil {
		t.Fatal(err)
	}

	if loaded.env.scope != "U1" || loaded.preload != nil {
		t.Errorf("loaded = %+v, want scope U1 trimmed and no preload for an empty scope", loaded)
	}
}

// TestTheDigestCoversEveryEntry: an entry that fell off the preload is still
// a change in what the scope holds.
func TestTheDigestCoversEveryEntry(t *testing.T) {
	t.Parallel()

	base := []store.Memory{{ID: 2, Text: "b"}, {ID: 1, Text: "a"}}

	for name, other := range map[string]string{
		"another scope": memoryDigest("U2", base),
		"one fewer":     memoryDigest("U1", base[:1]),
		"other text":    memoryDigest("U1", []store.Memory{{ID: 2, Text: "b"}, {ID: 1, Text: "c"}}),
		"other id":      memoryDigest("U1", []store.Memory{{ID: 2, Text: "b"}, {ID: 3, Text: "a"}}),
	} {
		if other == memoryDigest("U1", base) {
			t.Errorf("%s digests the same", name)
		}
	}
}

// TestThePreloadStopsAtItsCap: newest first, and it says how much it left out.
func TestThePreloadStopsAtItsCap(t *testing.T) {
	t.Parallel()

	fact := strings.Repeat("x", store.MaxMemoryBytes-10)

	var entries []store.Memory
	for id := int64(20); id > 0; id-- {
		entries = append(entries, store.Memory{ID: id, Text: fact})
	}

	rendered := renderMemoryPreload(entries, false)

	if !strings.Contains(rendered, "[20] ") || strings.Contains(rendered, "[1] ") {
		t.Errorf("preload should keep the newest and drop the oldest:\n%.200s", rendered)
	}

	if len(rendered) > maxMemoryPreloadBytes+512 {
		t.Errorf("preload is %d bytes, over its %d cap", len(rendered), maxMemoryPreloadBytes)
	}

	if !strings.Contains(rendered, "older entries did not fit") {
		t.Error("a truncated preload does not say so")
	}

	if strings.Contains(rendered, "forget takes") {
		t.Error("the preload offers forget to a step not granted it")
	}

	if !strings.Contains(renderMemoryPreload(entries[:1], true), "forget takes the id") {
		t.Error("a step granted forget is not told the ids are what it takes")
	}
}

// TestMemoryToolsWithoutAScopeAnswerAsData: a conversation that is not a
// memory: step gets told so, rather than filing under an empty scope.
func TestMemoryToolsWithoutAScopeAnswerAsData(t *testing.T) {
	t.Parallel()

	for name, result := range map[string]map[string]any{
		"remember": execRemember(t.Context(), map[string]any{"text": "x"}, toolEnv{}),
		"forget":   execForget(t.Context(), map[string]any{"id": 1.0}, toolEnv{}),
	} {
		if _, ok := result["error"]; !ok {
			t.Errorf("%s with no memory: = %v, want an error result", name, result)
		}
	}
}

func TestRememberAndForgetFileUnderTheStepsScope(t *testing.T) {
	t.Parallel()

	st := memoryStore(t)
	ctx := events.WithRunID(t.Context(), "run-1")
	env := toolEnv{memory: memoryEnv{st: st, scope: "U1", limit: 0}}

	saved := execRemember(ctx, map[string]any{"text": "  prefers code samples  "}, env)
	if saved["remembered"] != true {
		t.Fatalf("remember = %v", saved)
	}

	again := execRemember(ctx, map[string]any{"text": "prefers code samples"}, env)
	if again["remembered"] != false || again["id"] != saved["id"] {
		t.Errorf("remembering it again = %v, want the same entry back, not a copy", again)
	}

	if refused := execRemember(ctx, map[string]any{"text": ""}, env); refused["error"] == nil {
		t.Errorf("remembering nothing = %v, want an error result", refused)
	}

	other := toolEnv{memory: memoryEnv{st: st, scope: "U2"}}

	id, _ := saved["id"].(int64)

	if result := execForget(ctx, map[string]any{"id": float64(id)}, other); result["error"] == nil {
		t.Errorf("U2 forgetting U1's entry = %v, want an error result", result)
	}

	// The string form, as a model copying "[1]" out of the preload sends it.
	if result := execForget(ctx, map[string]any{"id": " 1 "}, env); result["forgotten"] != true {
		t.Errorf("forget by string id = %v", result)
	}

	if result := execForget(ctx, map[string]any{}, env); result["error"] == nil {
		t.Errorf("forget with no id = %v, want an error result", result)
	}

	left, err := st.ListMemories(ctx, "U1", 0)
	if err != nil || len(left) != 0 {
		t.Errorf("U1 = %+v err=%v, want nothing left", left, err)
	}
}
