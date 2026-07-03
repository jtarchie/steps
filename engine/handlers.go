package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jtarchie/steps/journal"
	"github.com/jtarchie/steps/machine"
	"github.com/jtarchie/steps/provider"
)

// runAction resolves the input block into args and calls the registered
// tool. extra carries foreach item data ({as}: item, index, total). Static
// values pass through with their real types — numbers stay numbers.
func (e *Engine) runAction(ctx context.Context, st *machine.State, rs *journal.RunState, extra map[string]any, attempt int) (*HandlerResult, error) {
	scope := baseScope(rs)
	scope["attempt"] = attempt
	for k, v := range extra {
		scope[k] = v
	}
	args, err := machine.ResolveInputs(st.Input, scope)
	if err != nil {
		return nil, &provider.ClassifiedError{Class: machine.ClassActionError, Msg: err.Error()}
	}

	e.Listener.ToolCalled(st.Name, st.Action.Name, args)
	out, err := e.Tools.Call(ctx, st.Action.Name, args)
	if err != nil {
		return nil, &provider.ClassifiedError{Class: machine.ClassActionError, Msg: err.Error()}
	}
	if out == nil {
		out = map[string]any{}
	}
	e.Listener.ToolResult(st.Name, st.Action.Name, out)

	// Actions only validate when the state declares a contract.
	if st.Output.Compiled != nil && len(st.Output.Schema) > 0 && !st.Output.DefaultOutput() {
		if err := st.Output.Compiled.Validate(normalizeJSON(out)); err != nil {
			return nil, &provider.ClassifiedError{Class: machine.ClassSchemaViolation, Msg: err.Error()}
		}
	}
	return &HandlerResult{Output: out}, nil
}

// runHuman resolves the gate prompt and requests a park.
func (e *Engine) runHuman(st *machine.State, rs *journal.RunState) (*HandlerResult, error) {
	prompt, err := st.Human.Prompt.String(baseScope(rs))
	if err != nil {
		return nil, err
	}
	prompt = machine.Dedent(prompt)
	return &HandlerResult{Park: &parkRequest{
		Prompt:    prompt,
		Timeout:   st.Human.Timeout,
		OnTimeout: st.Human.OnTimeout,
	}}, nil
}

// sortedKeys keeps rendering deterministic: map iteration order is random,
// and nondeterministic prompts defeat both journaled reproducibility and
// provider prefix caches.
func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// renderHistory produces the rung-2 journal projection: a plain-text record
// of what a prior state did, injected as data — never as conversation.
func renderHistory(msgs []journal.Message, spec *machine.HistorySpec) string {
	if len(msgs) == 0 {
		return "(no recorded history)"
	}
	includeMessages := false
	includeTools := false
	includeThoughts := false
	for _, inc := range spec.Include {
		switch inc {
		case "messages":
			includeMessages = true
		case "tool_calls":
			includeTools = true
		case "thoughts":
			includeThoughts = true
		}
	}

	if spec.LastTurns > 0 && len(msgs) > spec.LastTurns {
		msgs = msgs[len(msgs)-spec.LastTurns:]
	}

	var b strings.Builder
	for _, m := range msgs {
		if m.Thought {
			if includeThoughts && m.Text != "" {
				fmt.Fprintf(&b, "[%s thinking] %s\n", m.Role, m.Text)
			}
			continue
		}
		if includeMessages && m.Text != "" {
			fmt.Fprintf(&b, "[%s] %s\n", m.Role, m.Text)
		}
		if includeTools {
			for _, tc := range m.ToolCalls {
				args, _ := json.Marshal(tc.Args)
				fmt.Fprintf(&b, "[tool_call] %s(%s)\n", tc.Name, args)
			}
			for _, tr := range m.ToolResults {
				res, _ := json.Marshal(tr.Result)
				fmt.Fprintf(&b, "[tool_result] %s -> %s\n", tr.Name, res)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// normalizeJSON round-trips a map through JSON so schema validation sees
// canonical types (float64 numbers etc.).
func normalizeJSON(m map[string]any) map[string]any {
	raw, err := json.Marshal(m)
	if err != nil {
		return m
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return m
	}
	return out
}
