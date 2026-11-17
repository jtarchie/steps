package mcpread

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeSource answers from memory and remembers what was recorded.
type fakeSource struct {
	runs    []Run
	output  string
	steps   []Step
	waiting map[string][]Waiting

	mu       sync.Mutex
	recorded []string
}

func (f *fakeSource) Pipelines(context.Context) ([]Pipeline, error) {
	return []Pipeline{{Name: "web"}, {Name: "api"}}, nil
}

func (f *fakeSource) Pipeline(_ context.Context, name string) (PipelineDetail, error) {
	if name != "api" && name != "web" {
		return PipelineDetail{}, ErrNotFound
	}

	return PipelineDetail{Name: name, Jobs: []Job{{Name: "build"}}}, nil
}

func (f *fakeSource) Config(context.Context, string, string, string) (Config, error) {
	return Config{SHA: "abc", Source: "jobs: []\n", Includes: map[string]string{"task.sh": "echo hi"}}, nil
}

func (f *fakeSource) Runs(_ context.Context, pipeline string, q RunQuery) ([]Run, error) {
	if pipeline != "api" {
		return nil, ErrNotFound
	}

	start := 0

	if q.Before != "" {
		start = len(f.runs)

		for i, run := range f.runs {
			if run.ID == q.Before {
				start = i + 1
			}
		}
	}

	return f.runs[start:min(start+q.Limit, len(f.runs))], nil
}

func (f *fakeSource) Run(_ context.Context, _, id string) (RunDetail, error) {
	if id != "r1" {
		return RunDetail{}, ErrNotFound
	}

	return RunDetail{Run: Run{ID: "r1", Status: "failed"}, Steps: f.steps}, nil
}

func (f *fakeSource) StepOutput(context.Context, string, string, int64) (string, error) {
	return f.output, nil
}

func (f *fakeSource) Search(context.Context, string, string) ([]Hit, error) { return nil, nil }

func (f *fakeSource) Waiting(_ context.Context, pipeline string) ([]Waiting, error) {
	return f.waiting[pipeline], nil
}

func (f *fakeSource) Cost(context.Context, string, string) (Cost, error) { return Cost{}, nil }

func (f *fakeSource) Resources(context.Context, string) ([]Resource, error) { return nil, nil }

func (f *fakeSource) Versions(context.Context, string, string) ([]string, error) {
	return nil, fmt.Errorf("the database is locked at /secret/path: %w", context.DeadlineExceeded)
}

func (f *fakeSource) Record(_ context.Context, tool string, pipelines []string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.recorded = append(f.recorded, tool+" "+strings.Join(pipelines, ","))
}

func connect(t *testing.T, source Source) *sdkmcp.ClientSession {
	t.Helper()

	handler := Handler("test")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(With(r.Context(), source)))
	}))
	t.Cleanup(server.Close)

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "v0"}, nil)

	session, err := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = session.Close() })

	return session
}

func call(t *testing.T, session *sdkmcp.ClientSession, name string, args map[string]any, out any) *sdkmcp.CallToolResult {
	t.Helper()

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}

	if result.IsError || out == nil {
		return result
	}

	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}

	err = json.Unmarshal(encoded, out)
	if err != nil {
		t.Fatal(err)
	}

	return result
}

func toolText(result *sdkmcp.CallToolResult) string {
	var parts []string

	for _, content := range result.Content {
		if text, ok := content.(*sdkmcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}

	return strings.Join(parts, "\n")
}

func resultSize(t *testing.T, result *sdkmcp.CallToolResult) int {
	t.Helper()

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}

	return len(encoded)
}

// A step's output comes back in pages that add up to it exactly, never split a character, and never exceed the cap however the output escapes.
func TestOutputPagesAreWholeAndBounded(t *testing.T) {
	t.Parallel()

	for name, output := range map[string]string{
		"plain":     strings.Repeat("a line of build output\n", 4000),
		"multibyte": strings.Repeat("ビルド出力の一行\n", 4000),
		// Every byte of this escapes to six in JSON, which is what a page sized by raw bytes alone would overflow on.
		"escaping": strings.Repeat("<>&", 30000),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			session := connect(t, &fakeSource{output: output})

			var (
				whole  strings.Builder
				offset int
			)

			for pages := 0; ; pages++ {
				var page outputOut

				result := call(t, session, "get_step_output", map[string]any{"pipeline": "api", "run": "r1", "step": 1, "offset": offset, "limit": maxTextLimit}, &page)
				if result.IsError {
					t.Fatalf("page %d: %s", pages, toolText(result))
				}

				if size := resultSize(t, result); size > responseCap {
					t.Fatalf("page %d is %d bytes, over the %d cap", pages, size, responseCap)
				}

				if !strings.HasPrefix(toolText(result), frame) {
					t.Fatal("the text is not framed as data")
				}

				whole.WriteString(page.Text)

				if page.NextOffset == 0 {
					break
				}

				if page.NextOffset <= offset || pages > 1000 {
					t.Fatalf("page %d did not advance: %d after %d", pages, page.NextOffset, offset)
				}

				offset = page.NextOffset
			}

			if whole.String() != output {
				t.Errorf("the pages are %d bytes and differ from the %d-byte output", whole.Len(), len(output))
			}
		})
	}
}

// An offset inside a character is moved back to its start, and the page says where it really starts, so offset+len(text) is where the next page begins.
func TestAnOffsetInsideACharacterSaysWhereThePageStarts(t *testing.T) {
	t.Parallel()

	session := connect(t, &fakeSource{output: "ビルド"})

	var page outputOut

	result := call(t, session, "get_step_output", map[string]any{"pipeline": "api", "run": "r1", "step": 1, "offset": 4}, &page)
	if result.IsError {
		t.Fatal(toolText(result))
	}

	if page.Offset != 3 || page.Text != "ルド" {
		t.Errorf("page = offset %d, text %q; want offset 3, text %q", page.Offset, page.Text, "ルド")
	}
}

// Free text loses its escape sequences before an agent reads it: a terminal's OSC in a step's output is not the agent's to pass on.
func TestOutputIsScrubbed(t *testing.T) {
	t.Parallel()

	session := connect(t, &fakeSource{output: "before\x1b]52;c;cHduZWQ=\x07after\x1b[31m red\n"})

	var page outputOut

	call(t, session, "get_step_output", map[string]any{"pipeline": "api", "run": "r1", "step": 1}, &page)

	if page.Text != "beforeafter red\n" {
		t.Errorf("output = %q, want its escape sequences gone", page.Text)
	}
}

// A status filter pages by a cursor that resumes where the last page stopped, so every matching run is read once.
func TestListRunsPagesAFilteredHistory(t *testing.T) {
	t.Parallel()

	source := &fakeSource{}
	for i := range 30 {
		status := "succeeded"
		if i%3 == 0 {
			status = "failed"
		}

		source.runs = append(source.runs, Run{ID: fmt.Sprintf("r%02d", i), Status: status})
	}

	session := connect(t, source)

	var (
		seen   []string
		cursor string
	)

	for range 20 {
		var page listRunsOut

		call(t, session, "list_runs", map[string]any{"pipeline": "api", "status": "failed", "limit": 3, "cursor": cursor}, &page)

		for _, run := range page.Runs {
			if run.Status != "failed" {
				t.Errorf("a %s run came back filtered to failed", run.Status)
			}

			seen = append(seen, run.ID)
		}

		if page.NextCursor == "" {
			break
		}

		cursor = page.NextCursor
	}

	if got := strings.Join(seen, ","); got != "r00,r03,r06,r09,r12,r15,r18,r21,r24,r27" {
		t.Errorf("paged failed runs = %s", got)
	}
}

// A list pages by its cursor too, and a cursor nobody gave out is refused rather than read as a position.
func TestListsPageByCursor(t *testing.T) {
	t.Parallel()

	session := connect(t, &fakeSource{})

	var first listPipelinesOut

	call(t, session, "list_pipelines", map[string]any{"limit": 1}, &first)

	if len(first.Pipelines) != 1 || first.Pipelines[0].Name != "api" || first.NextCursor == "" {
		t.Fatalf("first page = %+v", first)
	}

	var second listPipelinesOut

	call(t, session, "list_pipelines", map[string]any{"limit": 1, "cursor": first.NextCursor}, &second)

	if len(second.Pipelines) != 1 || second.Pipelines[0].Name != "web" || second.NextCursor != "" {
		t.Errorf("second page = %+v", second)
	}

	if result := call(t, session, "list_pipelines", map[string]any{"cursor": "1"}, nil); !result.IsError || !strings.Contains(toolText(result), "cursor") {
		t.Errorf("a made-up cursor = %s; want refused", toolText(result))
	}
}

// A list whose items are too big for one response is cut short, with a cursor for the rest, rather than answered whole.
func TestABigListIsCutToTheCap(t *testing.T) {
	t.Parallel()

	steps := make([]Step, 200)
	for i := range steps {
		steps[i] = Step{ID: int64(i), Name: fmt.Sprintf("step-%d", i), Status: "failed", Error: strings.Repeat("e", fieldCap*2)}
	}

	session := connect(t, &fakeSource{steps: steps})

	var page getRunOut

	result := call(t, session, "get_run", map[string]any{"pipeline": "api", "run": "r1"}, &page)

	if size := resultSize(t, result); size > responseCap {
		t.Errorf("get_run answered %d bytes, over the %d cap", size, responseCap)
	}

	if page.NextCursor == "" || len(page.Steps) == 0 || len(page.Steps) == len(steps) {
		t.Errorf("get_run returned %d of %d steps, cursor %q; want a page and a cursor", len(page.Steps), len(steps), page.NextCursor)
	}

	if !strings.Contains(page.Steps[0].Error, "more bytes]") {
		t.Errorf("a long error was not capped: %d bytes", len(page.Steps[0].Error))
	}
}

// What the agent is not told goes to the daemon's log, or an operator could not see /mcp failing at all. Serial: it swaps the process's default logger.
func TestAFailedReadIsLogged(t *testing.T) {
	var logged bytes.Buffer

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	session := connect(t, &fakeSource{})
	call(t, session, "list_versions", map[string]any{"pipeline": "api", "resource": "repo"}, nil)

	if !strings.Contains(logged.String(), "mcpread.read_failed") || !strings.Contains(logged.String(), "/secret/path") {
		t.Errorf("the failure was not logged with its cause:\n%s", logged.String())
	}
}

// Not found says only that, and a failure inside the daemon says it could not read — never the error behind it, which is SQL and paths.
func TestRefusalsSayNoMoreThanTheyMust(t *testing.T) {
	t.Parallel()

	session := connect(t, &fakeSource{})

	if text := toolText(call(t, session, "get_pipeline", map[string]any{"pipeline": "hidden"}, nil)); text != "no such pipeline: not found" {
		t.Errorf("a pipeline the source will not show = %q", text)
	}

	if text := toolText(call(t, session, "list_versions", map[string]any{"pipeline": "api", "resource": "repo"}, nil)); strings.Contains(text, "/secret/path") {
		t.Errorf("an internal failure leaked its detail: %q", text)
	}
}

// Each call is recorded by tool and pipeline, never by what the agent sent; a refused call records nothing, since nothing was read.
func TestCallsAreRecordedByToolAndPipeline(t *testing.T) {
	t.Parallel()

	source := &fakeSource{waiting: map[string][]Waiting{"api": {{Pipeline: "api", Kind: "approval", Message: "ship it?"}}}}
	session := connect(t, source)

	call(t, session, "get_pipeline", map[string]any{"pipeline": "api", "cursor": ""}, &getPipelineOut{})
	call(t, session, "list_waiting", nil, &waitingOut{})
	call(t, session, "get_pipeline", map[string]any{"pipeline": "hidden"}, nil)

	source.mu.Lock()
	defer source.mu.Unlock()

	if got := strings.Join(source.recorded, "; "); got != "get_pipeline api; list_waiting api,web" {
		t.Errorf("recorded %q", got)
	}
}

// Every tool says it only reads, and reaches nothing outside the daemon.
func TestEveryToolIsReadOnly(t *testing.T) {
	t.Parallel()

	session := connect(t, &fakeSource{})

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(tools.Tools) != 11 {
		t.Errorf("%d tools", len(tools.Tools))
	}

	for _, tool := range tools.Tools {
		if a := tool.Annotations; a == nil || !a.ReadOnlyHint || a.OpenWorldHint == nil || *a.OpenWorldHint || a.DestructiveHint == nil || *a.DestructiveHint {
			t.Errorf("%s: %+v", tool.Name, tool.Annotations)
		}
	}
}

// A request that reached the tools with nobody to answer as is refused, not answered as nobody.
func TestNoSourceNoAnswer(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(Handler("test"))
	t.Cleanup(server.Close)

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "v0"}, nil)

	session, err := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = session.Close() }()

	if result := call(t, session, "list_pipelines", nil, nil); !result.IsError {
		t.Error("a call with no source was answered")
	}
}
