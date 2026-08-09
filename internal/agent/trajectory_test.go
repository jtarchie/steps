package agent

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

func call(name string, args map[string]any) recordedToolCall {
	return recordedToolCall{name: name, args: args}
}

func expect(name string, args map[string]string) config.ExpectedToolCall {
	return config.ExpectedToolCall{Name: name, Args: args}
}

func TestMatchToolCallTrajectory(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		want    []config.ExpectedToolCall
		got     []recordedToolCall
		wantErr bool
	}{
		{
			name: "empty expectation passes trivially",
			want: nil,
			got:  []recordedToolCall{call("read_file", nil)},
		},
		{
			name: "exact single match",
			want: []config.ExpectedToolCall{expect("read_file", nil)},
			got:  []recordedToolCall{call("read_file", nil)},
		},
		{
			name: "exact order, adjacent",
			want: []config.ExpectedToolCall{expect("read_file", nil), expect("post_review", nil)},
			got:  []recordedToolCall{call("read_file", nil), call("post_review", nil)},
		},
		{
			name: "gaps allowed - extra calls between matches",
			want: []config.ExpectedToolCall{expect("read_file", nil), expect("post_review", nil)},
			got: []recordedToolCall{
				call("read_file", nil),
				call("list_dir", nil),
				call("run_shell", nil),
				call("post_review", nil),
			},
		},
		{
			name: "extra calls before and after are ignored",
			want: []config.ExpectedToolCall{expect("post_review", nil)},
			got: []recordedToolCall{
				call("list_dir", nil),
				call("post_review", nil),
				call("run_shell", nil),
			},
		},
		{
			name:    "out of order fails",
			want:    []config.ExpectedToolCall{expect("post_review", nil), expect("read_file", nil)},
			got:     []recordedToolCall{call("read_file", nil), call("post_review", nil)},
			wantErr: true,
		},
		{
			name:    "missing expected call fails",
			want:    []config.ExpectedToolCall{expect("read_file", nil), expect("post_review", nil)},
			got:     []recordedToolCall{call("read_file", nil)},
			wantErr: true,
		},
		{
			name:    "no calls at all fails a non-empty expectation",
			want:    []config.ExpectedToolCall{expect("read_file", nil)},
			got:     nil,
			wantErr: true,
		},
		{
			name: "subset args - listed key matches, extras ignored",
			want: []config.ExpectedToolCall{expect("post_review", map[string]string{"action": "comment"})},
			got:  []recordedToolCall{call("post_review", map[string]any{"action": "comment", "body": "looks good"})},
		},
		{
			name:    "missing asserted arg key fails",
			want:    []config.ExpectedToolCall{expect("post_review", map[string]string{"action": "comment"})},
			got:     []recordedToolCall{call("post_review", map[string]any{"body": "looks good"})},
			wantErr: true,
		},
		{
			name:    "wrong arg value fails",
			want:    []config.ExpectedToolCall{expect("post_review", map[string]string{"action": "comment"})},
			got:     []recordedToolCall{call("post_review", map[string]any{"action": "approve"})},
			wantErr: true,
		},
		{
			name: "arg mismatch skips to a later matching call",
			want: []config.ExpectedToolCall{expect("post_review", map[string]string{"action": "comment"})},
			got: []recordedToolCall{
				call("post_review", map[string]any{"action": "approve"}),
				call("post_review", map[string]any{"action": "comment"}),
			},
		},
		{
			name: "non-string arg values compare as strings",
			want: []config.ExpectedToolCall{expect("wait", map[string]string{"seconds": "30"})},
			got:  []recordedToolCall{call("wait", map[string]any{"seconds": 30})},
		},
		{
			name: "repeated expectation needs two distinct calls",
			want: []config.ExpectedToolCall{expect("read_file", nil), expect("read_file", nil)},
			got:  []recordedToolCall{call("read_file", nil), call("read_file", nil)},
		},
		{
			name:    "repeated expectation fails with only one call",
			want:    []config.ExpectedToolCall{expect("read_file", nil), expect("read_file", nil)},
			got:     []recordedToolCall{call("read_file", nil)},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := matchToolCallTrajectory(tc.want, tc.got)
			if tc.wantErr && err == nil {
				t.Fatal("expected a mismatch error, got nil")
			}

			if !tc.wantErr && err != nil {
				t.Fatalf("expected a match, got error: %v", err)
			}
		})
	}
}

// TestMatchToolCallTrajectoryErrorMessage proves a mismatch names the
// unmatched expectation and prints the observed trajectory, so a failing
// fixture is debuggable from the message alone.
func TestMatchToolCallTrajectoryErrorMessage(t *testing.T) {
	t.Parallel()

	err := matchToolCallTrajectory(
		[]config.ExpectedToolCall{expect("post_review", map[string]string{"action": "comment"})},
		[]recordedToolCall{call("read_file", nil), call("list_dir", nil)},
	)
	if err == nil {
		t.Fatal("expected a mismatch error")
	}

	msg := err.Error()
	for _, want := range []string{"post_review", `action="comment"`, "read_file", "list_dir"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q missing %q", msg, want)
		}
	}
}

func TestAssertAgentResponseToolCalls(t *testing.T) {
	t.Parallel()

	trajectory := []recordedToolCall{
		call("read_file", nil),
		call("post_review", map[string]any{"action": "comment"}),
	}
	res := conversationResult{text: "posted the review", trajectory: trajectory}

	t.Run("matching trajectory passes", func(t *testing.T) {
		t.Parallel()

		assert := &config.Assert{ToolCalls: []config.ExpectedToolCall{
			expect("read_file", nil),
			expect("post_review", map[string]string{"action": "comment"}),
		}}

		err := assertAgentResponse(assert, res)
		if err != nil {
			t.Errorf("expected the assert to pass, got %v", err)
		}
	})

	t.Run("mismatched trajectory fails", func(t *testing.T) {
		t.Parallel()

		assert := &config.Assert{ToolCalls: []config.ExpectedToolCall{expect("never_called", nil)}}

		err := assertAgentResponse(assert, res)
		if err == nil {
			t.Error("expected a mismatch failure")
		}
	})

	t.Run("stdout and tool_calls are ANDed", func(t *testing.T) {
		t.Parallel()

		stdout := "posted"
		matching := []config.ExpectedToolCall{expect("read_file", nil)}

		// Both satisfied -> pass.
		err := assertAgentResponse(&config.Assert{Stdout: &stdout, ToolCalls: matching}, res)
		if err != nil {
			t.Errorf("both satisfied should pass, got %v", err)
		}

		// stdout satisfied, tool_calls not -> fail.
		bad := []config.ExpectedToolCall{expect("never_called", nil)}

		err = assertAgentResponse(&config.Assert{Stdout: &stdout, ToolCalls: bad}, res)
		if err == nil {
			t.Error("a tool_calls mismatch must fail even when stdout matches")
		}

		// tool_calls satisfied, stdout not -> fail.
		missing := "not in the response"

		err = assertAgentResponse(&config.Assert{Stdout: &missing, ToolCalls: matching}, res)
		if err == nil {
			t.Error("a stdout mismatch must fail even when tool_calls match")
		}
	})

	t.Run("nil assert is a no-op", func(t *testing.T) {
		t.Parallel()

		err := assertAgentResponse(nil, res)
		if err != nil {
			t.Errorf("nil assert should pass, got %v", err)
		}
	})
}

// TestRunAgentConversationRecordsTrajectory proves the loop records every
// requested call, in order, with the model's own arguments — and that a retry
// reports only its own attempt's calls, not an accumulation.
func TestRunAgentConversationRecordsTrajectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	fake := &fakeLLM{
		responses: []*model.LLMResponse{
			{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{ID: "1", Name: "run_shell", Args: map[string]any{"command": "echo one"}}},
			}}},
			{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{ID: "2", Name: "list_dir", Args: map[string]any{"path": "."}}},
			}}},
			{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}}},
		},
	}

	res, err := runAgentConversation(context.Background(), fake, newTestConversation(t, "go", dir))
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	if len(res.trajectory) != 2 {
		t.Fatalf("trajectory = %+v, want 2 calls", res.trajectory)
	}

	if res.trajectory[0].name != "run_shell" || res.trajectory[1].name != "list_dir" {
		t.Errorf("trajectory names = %q/%q, want run_shell/list_dir", res.trajectory[0].name, res.trajectory[1].name)
	}

	if res.trajectory[0].args["command"] != "echo one" {
		t.Errorf("recorded args = %#v, want the model-authored command", res.trajectory[0].args)
	}
}

// TestTrajectoryRecordsBudgetRejectedCall proves a call rejected by a
// max_calls: budget still appears in the trajectory: the model did make it
// (and saw the rejection as data), so its behavior is judged on it.
func TestTrajectoryRecordsBudgetRejectedCall(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	specs := []config.ToolSpec{{Name: "post_review", Run: "true", MaxCalls: 1}}

	decls, registry, _, err := buildAgentTools(context.Background(), nil, specs, "")
	if err != nil {
		t.Fatal(err)
	}

	runner, err := shell.NewRunner(shell.RunnerSpec{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	toolCall := &model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
		{FunctionCall: &genai.FunctionCall{ID: "1", Name: "post_review"}},
	}}}

	fake := &fakeLLM{responses: []*model.LLMResponse{
		toolCall,
		toolCall, // second call: over budget, rejected as data
		{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}}},
	}}

	conv := agentConversation{
		prompt:   "review",
		env:      toolEnv{dir: dir, runner: runner},
		tools:    agentTools{decls: decls, registry: registry, required: requiredToolNames(specs), maxCalls: maxCallsByName(specs)},
		maxTurns: testMaxTurns,
	}

	res, err := runAgentConversation(context.Background(), fake, conv)
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	if len(res.trajectory) != 2 {
		t.Fatalf("trajectory = %+v, want both calls recorded (including the budget-rejected one)", res.trajectory)
	}
}
