package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
)

func modelCall(id, name string, args map[string]any) *genai.Part {
	return &genai.Part{FunctionCall: &genai.FunctionCall{ID: id, Name: name, Args: args}}
}

// TestReadOnlyRunInOneTurnKeepsSerialOrderAndBudgets drives a whole conversation: one turn of five calls, a write between reads, and a read_file budget of three. Whatever order the reads finish in, results, transcript and the budget rejection must be what the serial loop produced.
func TestReadOnlyRunInOneTurnKeepsSerialOrderAndBudgets(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for name, body := range map[string]string{"a.txt": "old-a", "b.txt": "old-b"} {
		err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	fake := &fakeLLM{responses: []*model.LLMResponse{
		{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
			modelCall("c1", "read_file", map[string]any{"path": "a.txt"}),
			modelCall("c2", "read_file", map[string]any{"path": "b.txt"}),
			modelCall("c3", "write_file", map[string]any{"path": "a.txt", "content": "new-a"}),
			modelCall("c4", "read_file", map[string]any{"path": "a.txt"}),
			modelCall("c5", "read_file", map[string]any{"path": "b.txt"}),
		}}},
		{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}}},
	}}

	conv := newTestConversation(t, "go", dir)

	built, _, err := buildAgentTools(context.Background(), nil, []config.ToolSpec{
		{Builtin: "read_file", MaxCalls: 3},
		{Builtin: "write_file"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	conv.tools = built

	res, err := runAgentConversation(context.Background(), fake, conv)
	if err != nil {
		t.Fatal(err)
	}

	sent := fake.requests[1].Contents
	ids, bodies := responseIDsAndBodies(sent[len(sent)-1].Parts)

	if want := "c1,c2,c3,c4,c5"; strings.Join(ids, ",") != want {
		t.Fatalf("result order = %v, want %s", ids, want)
	}

	assertContains(t, bodies, "old-a", "old-b", "", "new-a", "call budget (3) exhausted")
	assertTranscriptOrder(t, res.transcript)
}

func assertContains(t *testing.T, bodies []string, wants ...string) {
	t.Helper()

	for i, want := range wants {
		if !strings.Contains(bodies[i], want) {
			t.Errorf("result %d = %s, want it to contain %q", i+1, bodies[i], want)
		}
	}
}

func assertTranscriptOrder(t *testing.T, transcript []transcriptEvent) {
	t.Helper()

	var calls []string

	for _, ev := range transcript {
		if ev.Type == "call" || ev.Type == "result" {
			calls = append(calls, ev.Type+":"+ev.Name)
		}
	}

	want := "call:read_file,call:read_file,call:write_file,call:read_file,call:read_file,result:read_file,result:read_file,result:write_file,result:read_file,result:read_file"
	if strings.Join(calls, ",") != want {
		t.Errorf("transcript = %v, want %s", calls, want)
	}
}

func responseIDsAndBodies(parts []*genai.Part) (ids, bodies []string) {
	ids = make([]string, 0, len(parts))
	bodies = make([]string, 0, len(parts))

	for _, part := range parts {
		ids = append(ids, part.FunctionResponse.ID)
		bodies = append(bodies, fmt.Sprint(part.FunctionResponse.Response))
	}

	return ids, bodies
}

type readLog struct {
	mu     sync.Mutex
	events []string
}

func (l *readLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.events = append(l.events, e)
}

func (l *readLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return slices.Clone(l.events)
}

// TestReadOnlyCallsOverlapAndABarrierDoesNot proves two reads are in flight together (each waits for the other, so a serial loop never gets past the first) and that the non-read call between them and the next read overlaps with neither.
func TestReadOnlyCallsOverlapAndABarrierDoesNot(t *testing.T) {
	t.Parallel()

	var (
		log      readLog
		inFlight sync.WaitGroup
		both     = make(chan struct{})
	)

	inFlight.Add(2)

	go func() {
		inFlight.Wait()
		close(both)
	}()

	reader := func(name string, rendezvous bool) toolImpl {
		return func(ctx context.Context, _ map[string]any, _ toolEnv) map[string]any {
			log.add("start:" + name)

			if rendezvous {
				inFlight.Done()

				select {
				case <-both:
				case <-ctx.Done():
					return errorResult(name + ": never overlapped with its sibling")
				}
			}

			log.add("end:" + name)

			return map[string]any{"name": name}
		}
	}

	registry := map[string]toolImpl{
		"r1":      reader("r1", true),
		"r2":      reader("r2", true),
		"barrier": reader("barrier", false),
		"r3":      reader("r3", false),
	}
	readOnly := map[string]bool{"r1": true, "r2": true, "r3": true}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	calls := []*genai.FunctionCall{{ID: "1", Name: "r1"}, {ID: "2", Name: "r2"}, {ID: "3", Name: "barrier"}, {ID: "4", Name: "r3"}}

	parts := toolResponseParts(ctx, calls, testEnv(t.TempDir()), registry, readOnly, nil, map[string]int{})

	for i, part := range parts {
		if want := strconv.Itoa(i + 1); part.FunctionResponse.ID != want {
			t.Errorf("part %d has ID %s, want %s", i, part.FunctionResponse.ID, want)
		}

		if msg, bad := part.FunctionResponse.Response["error"]; bad {
			t.Errorf("part %d: %v", i, msg)
		}
	}

	events := log.snapshot()
	if len(events) != 8 {
		t.Fatalf("events = %v", events)
	}

	if !slices.Equal(events[4:], []string{"start:barrier", "end:barrier", "start:r3", "end:r3"}) {
		t.Errorf("the barrier overlapped with a neighbour: %v", events)
	}
}

// TestConcurrentReadsAreBounded checks no more than maxConcurrentReads run at once.
func TestConcurrentReadsAreBounded(t *testing.T) {
	t.Parallel()

	var (
		mu          sync.Mutex
		active, top int
		gate        = make(chan struct{})
		once        sync.Once
	)

	impl := func(ctx context.Context, _ map[string]any, _ toolEnv) map[string]any {
		mu.Lock()
		active++
		top = max(top, active)

		if active == maxConcurrentReads {
			once.Do(func() { close(gate) })
		}

		mu.Unlock()

		select {
		case <-gate:
		case <-ctx.Done():
		}

		mu.Lock()
		active--
		mu.Unlock()

		return map[string]any{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	calls := make([]*genai.FunctionCall, maxConcurrentReads*2)
	for i := range calls {
		calls[i] = &genai.FunctionCall{ID: strconv.Itoa(i), Name: "r"}
	}

	toolResponseParts(ctx, calls, testEnv(t.TempDir()), map[string]toolImpl{"r": impl}, map[string]bool{"r": true}, nil, map[string]int{})

	if top != maxConcurrentReads {
		t.Errorf("peak concurrency = %d, want %d", top, maxConcurrentReads)
	}
}

// TestAPanickingReadBecomesThatCallsError keeps one bad read from taking the process down or its siblings with it.
func TestAPanickingReadBecomesThatCallsError(t *testing.T) {
	t.Parallel()

	registry := map[string]toolImpl{
		"boom": func(context.Context, map[string]any, toolEnv) map[string]any { panic("kaput") },
		"ok":   func(context.Context, map[string]any, toolEnv) map[string]any { return map[string]any{"fine": true} },
	}
	readOnly := map[string]bool{"boom": true, "ok": true}
	calls := []*genai.FunctionCall{{ID: "1", Name: "ok"}, {ID: "2", Name: "boom"}, {ID: "3", Name: "ok"}}

	parts := toolResponseParts(context.Background(), calls, testEnv(t.TempDir()), registry, readOnly, nil, map[string]int{})

	if msg, _ := parts[1].FunctionResponse.Response["error"].(string); !strings.Contains(msg, "kaput") {
		t.Errorf("panicking call = %v, want an error naming the panic", parts[1].FunctionResponse.Response)
	}

	if parts[0].FunctionResponse.Response["fine"] != true || parts[2].FunctionResponse.Response["fine"] != true {
		t.Errorf("siblings were disturbed: %v %v", parts[0].FunctionResponse.Response, parts[2].FunctionResponse.Response)
	}
}

// TestOnlyTheTreeBoundBuiltinsAreReadOnly pins which tools may overlap: the three that cannot write or leave the tree, and not a rule-guarded one.
func TestOnlyTheTreeBoundBuiltinsAreReadOnly(t *testing.T) {
	t.Parallel()

	built, _, err := buildAgentTools(context.Background(), nil, []config.ToolSpec{
		{Builtin: "read_file"}, {Builtin: "list_dir"}, {Builtin: "search_files"},
		{Builtin: "write_file"}, {Builtin: "edit_file"}, {Builtin: "run_shell"}, {Builtin: "web_fetch"},
		{Name: "custom", Run: "true"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	for name := range built.registry {
		want := name == "read_file" || name == "list_dir" || name == "search_files"
		if built.readOnly[name] != want {
			t.Errorf("readOnly[%s] = %v, want %v", name, built.readOnly[name], want)
		}
	}
}
