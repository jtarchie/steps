package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/redact"
)

// TestTranscriptPublishesCallsWithArgsAndDepth: the live view is how a watcher sees a call while it happens, and depth is the only thing marking a sub-agent's call as nested.
func TestTranscriptPublishesCallsWithArgsAndDepth(t *testing.T) {
	t.Parallel()

	bus := events.New(nil)
	t.Cleanup(bus.Close)

	var (
		mu  sync.Mutex
		got []events.Event
	)

	cancel := bus.Observe(func(e events.Event) {
		mu.Lock()
		defer mu.Unlock()

		got = append(got, e)
	})
	t.Cleanup(cancel)

	parent := &transcriptRecorder{live: liveContext{bus: bus, stepName: "review"}}
	parent.call("run_shell", map[string]any{"command": "true"})
	parent.childRecorder().call("read_file", nil)

	mu.Lock()
	defer mu.Unlock()

	if len(got) != 2 {
		t.Fatalf("published %d events; want one per call", len(got))
	}

	if got[0].Name != "run_shell" || got[0].Detail != `{"command":"true"}` || got[0].Status != "" {
		t.Errorf("parent call = %+v; want its args as detail and no depth", got[0])
	}

	if got[1].Name != "read_file" || got[1].Detail != "" || got[1].Status != "depth:1" {
		t.Errorf("child call = %+v; want no detail for no args and depth:1", got[1])
	}
}

func TestRenderResultContentIsJSON(t *testing.T) {
	t.Parallel()

	if got := renderResultContent(map[string]any{"stdout": "hi", "exit_code": 0}); got != `{"exit_code":0,"stdout":"hi"}` {
		t.Errorf("renderResultContent = %q; want the result as JSON", got)
	}
}

type transcriptSaver struct{ saved string }

func (s *transcriptSaver) SaveNodeTranscript(_ context.Context, _, transcript string) error {
	s.saved = transcript

	return nil
}

// TestTranscriptMasksTheRunsSecrets: a hidden load_var substituted into messages: is masked in step output, so the turn a viewer reads live and the transcript the store keeps must mask it too. Carried from the run's context through bindRun, which is where a recorder that never learned the run's secrets would go unnoticed.
func TestTranscriptMasksTheRunsSecrets(t *testing.T) {
	t.Parallel()

	const secret = "dummy-hidden-value-456"

	bus := events.New(nil)
	t.Cleanup(bus.Close)

	var (
		mu  sync.Mutex
		got []events.Event
	)

	cancel := bus.Observe(func(e events.Event) {
		mu.Lock()
		defer mu.Unlock()

		got = append(got, e)
	})
	t.Cleanup(cancel)

	secrets := &redact.Secrets{}
	ctx := events.WithBus(redact.WithSecrets(t.Context(), secrets), bus)

	var prepared preparedAgentStep

	bindRun(ctx, &prepared, nil, "job", 0, "review")

	// Registered after the recorder exists: a run resolves secrets as it goes.
	secrets.Add(secret)

	rec := prepared.conv.recorder
	rec.system("system " + secret)
	rec.user("check " + secret)
	rec.text("it is " + secret)
	rec.call("run_shell", map[string]any{"command": "echo " + secret, "env": []any{secret}})
	rec.result("run_shell", `{"stdout":"`+secret+`"}`)
	rec.compaction("summary", "earlier: "+secret)
	rec.insertUserAt(0, "again "+secret)

	child := rec.childRecorder()
	child.user("child " + secret)
	rec.subagent("helper", "ask "+secret, child.recorded())

	store := &transcriptSaver{}
	saveAgentTranscript(ctx, store, "hash", "job", conversationResult{transcript: rec.recorded()})

	if strings.Contains(store.saved, secret) || strings.Count(store.saved, "***") != 10 {
		t.Errorf("saved transcript = %s; want the secret masked in all ten places", store.saved)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(got) != 9 {
		t.Fatalf("published %d events; want nine", len(got))
	}

	for _, event := range got {
		if strings.Contains(event.Text+event.Detail, secret) || !strings.Contains(event.Text+event.Detail, "***") {
			t.Errorf("published %s = text %q detail %q; want the secret masked", event.Type, event.Text, event.Detail)
		}
	}
}
