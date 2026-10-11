package agent

import (
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/events"
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
