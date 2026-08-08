package agent

// setcontext.go implements the WRITE half of the run context store: a step
// declaring context: write is granted a synthesized set_context tool, and
// every successful call records one fact for the steps that come after it.
//
// Facts are captured through a real tool rather than parsed out of the model's
// final answer. Free-text parsing is the tempting shortcut and the wrong one:
// it fails silently the moment a model formats its reply slightly differently,
// and a silently-lost fact is indistinguishable from one the model never
// learned. A tool call either happened or it did not, and the trajectory says
// which.
//
// Unlike the verdict and write_handoff tools this is never required: the step
// is offered somewhere to put a fact, not made to produce one.

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// setContextToolName is the fixed name of the tool a context: write step is
// granted. A custom tool of the same name is rejected at injection, the same
// treatment verdictToolName and writeHandoffToolName get.
const setContextToolName = "set_context"

// contextWriter records one fact. It is the seam internal/agent has on the
// store: RunStep closes over the run id and the writing step's name, so the
// tool implementation below never learns either.
type contextWriter func(ctx context.Context, key, value string) error

// buildSetContextTool synthesizes the set_context tool around write.
//
// Every failure comes back as data ({"error": ...}) rather than a Go error, so
// a bad key costs the model one turn and a correction instead of failing the
// step. That is the same contract every other tool here honors.
func buildSetContextTool(write contextWriter) (*genai.FunctionDeclaration, toolImpl) {
	decl := &genai.FunctionDeclaration{
		Name: setContextToolName,
		Description: "Record a fact for the later steps of this pipeline, which cannot see this conversation. " +
			"Use it for conclusions worth carrying forward — what you found, what you decided, what a later step needs " +
			"to act on. Writing the same key again replaces the previous value.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"key": {
					Type: genai.TypeString,
					Description: "Name for this fact, e.g. \"failure_cause\". Letters, digits, and _ - . only. " +
						"A later step reads it back by this exact name, so prefer a stable, descriptive one.",
				},
				"value": {
					Type:        genai.TypeString,
					Description: "The fact itself. Keep it to what a later step actually needs; it is quoted verbatim into their context.",
				},
			},
			Required: []string{"key", "value"},
		},
	}

	impl := func(ctx context.Context, args map[string]any, _ toolEnv) map[string]any {
		key := stringArg(args, "key")
		value := stringArg(args, "value")

		err := config.ValidateContextKey(key)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}

		if len(value) > config.MaxContextValueLen {
			return map[string]any{"error": fmt.Sprintf(
				"value for %q is %d characters, above the limit of %d; record a summary and leave the detail in a file",
				key, len(value), config.MaxContextValueLen)}
		}

		err = write(ctx, key, value)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}

		slog.Debug("agent.context.set", "key", key, "bytes", len(value))

		return map[string]any{"exit_code": 0, "stored": key}
	}

	return decl, impl
}

// contextWriterFor returns the writer a step records facts through, or nil
// when there is nowhere to record them: no store, or no run identity on the
// context (every non-pipeline caller — RunFix, sub-agents, most tests).
// injectSetContextTool reads that nil as "do not offer the tool".
func contextWriterFor(st *store.Store, runID, stepName string) contextWriter {
	if st == nil || runID == "" {
		return nil
	}

	return func(ctx context.Context, key, value string) error {
		return st.SetContext(ctx, runID, key, value, stepName)
	}
}

// injectSetContextTool appends the synthesized set_context tool when step
// declares context: write. A pre-existing tool of the same name is a conflict,
// rejected here rather than silently shadowed.
//
// A nil write is the "nowhere to record it" case — no store on this call path,
// which every non-pipeline caller and most tests are. The tool is then not
// offered at all, rather than offered and failing every call: a model that can
// see a tool will use it, and one that always errors burns turns learning that.
func injectSetContextTool(step config.Step, write contextWriter, decls *genai.Tool, registry map[string]toolImpl) error {
	if !step.WritesContext() || write == nil {
		return nil
	}

	if _, exists := registry[setContextToolName]; exists {
		return fmt.Errorf("declares context: write but already defines a tool named %q", setContextToolName)
	}

	decl, impl := buildSetContextTool(write)
	decls.FunctionDeclarations = append(decls.FunctionDeclarations, decl)
	registry[setContextToolName] = impl

	return nil
}
