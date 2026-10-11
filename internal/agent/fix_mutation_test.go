package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestRunFixAsksTheModelWithTheBuiltinsAndTheTask: a fix: with no tools: granted gets the default built-ins plus a tool re-running the failed task, and it actually reaches the model.
func TestRunFixAsksTheModelWithTheBuiltinsAndTheTask(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		bodies []string
	)

	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"fix","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"fixed"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(llm.Close)

	cfg := &config.Config{Agents: []config.Agent{{Name: "fixer", Source: config.AgentSource{Model: "test-model", Endpoint: llm.URL + "/v1/"}}}}
	task := config.ResolvedTask{Name: "build_it", Run: "true", Fix: &config.FixSpec{Agent: "fixer"}}

	ctx, _ := captured()

	err := RunFix(ctx, cfg, "job", 0, task, nil, "boom", t.TempDir())
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(bodies) == 0 {
		t.Fatal("RunFix returned without asking the model anything")
	}

	for _, want := range []string{`"read_file"`, `"build_it"`} {
		if !strings.Contains(bodies[0], want) {
			t.Errorf("request does not offer %s", want)
		}
	}
}
