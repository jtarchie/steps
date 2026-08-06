package pipeline

// The run-time half of pipeline vars: capturing a load_var: value and
// substituting it into the steps that follow.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// runVars holds the values load_var: steps captured during one job run.
type runVars struct {
	mu sync.Mutex
	by map[string]string
}

type runVarsKey struct{}

func withRunVars(ctx context.Context) context.Context {
	return context.WithValue(ctx, runVarsKey{}, &runVars{by: map[string]string{}})
}

func runVarsFrom(ctx context.Context) *runVars {
	vars, _ := ctx.Value(runVarsKey{}).(*runVars)

	return vars
}

func (v *runVars) set(name, value string) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.by[name] = value
}

func (v *runVars) snapshot() map[string]string {
	v.mu.Lock()
	defer v.mu.Unlock()

	out := make(map[string]string, len(v.by))
	for name, value := range v.by {
		out[name] = value
	}

	return out
}

// renderStepVars substitutes any ((name)) a step still carries from the values
// load_var: steps have captured so far.
//
// Applied BEFORE the step is hashed, deliberately: a captured value changes
// what the step actually runs, so two runs that captured different values must
// not share a cache entry. The alternative — hashing the unsubstituted text —
// would let a step that ran against v1.2.3 satisfy a run that meant v2.0.0.
func renderStepVars(ctx context.Context, step config.Step) config.Step {
	vars := runVarsFrom(ctx)
	if vars == nil {
		return step
	}

	values := vars.snapshot()
	if len(values) == 0 {
		return step
	}

	step.Run = config.RenderVars(step.Run, values)
	step.Prompt = config.RenderVars(step.Prompt, values)
	step.Image = config.RenderVars(step.Image, values)
	step.Dir = config.RenderVars(step.Dir, values)
	step.VarFile = config.RenderVars(step.VarFile, values)

	// params: is where a captured version most often belongs — it is what a
	// put hands the resource. Rendering it is not optional garnish: the
	// literal text `((version))` used to reach the out: command and publish a
	// release by that name.
	if rendered, ok := config.RenderValue(step.Params, values).(map[string]any); ok {
		step.Params = rendered
	}

	return step
}

// runLoadVarStep captures a file's contents into a pipeline var.
//
// The value is trimmed of surrounding whitespace, because the overwhelmingly
// common way to produce one is `git describe > version.txt`, which leaves a
// trailing newline that would otherwise be substituted into the middle of a
// command.
func runLoadVarStep(
	ctx context.Context, jobName string, i int, step config.Step,
	bw workspace.BuildWorkspace, st *store.Store, parentHash string,
) (string, stepDisposition, nonGetOutcome, error) {
	space, err := bw.TaskSpace(ctx, step.LoadVar, step.InputNames(), nil, nil, nil)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (load_var %q): workspace: %w", i, step.LoadVar, err)
	}

	defer workspace.CloseSpace(space, step.LoadVar)

	path := filepath.Join(space.Dir(), step.VarFile)

	body, err := os.ReadFile(path) //nolint:gosec // the path is a pipeline-declared file inside this step's own workspace
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (load_var %q): %w", i, step.LoadVar, err)
	}

	value := strings.TrimSpace(string(body))

	if vars := runVarsFrom(ctx); vars != nil {
		vars.set(step.LoadVar, value)
	}

	fmt.Printf("load_var: %s\n", step.LoadVar)
	slog.Info("job.load_var", "job", jobName, "var", step.LoadVar, "bytes", len(value))

	content := map[string]any{"load_var": step.LoadVar, "file": step.VarFile, "value": value}

	hash, err := merkle.HashNode(merkle.NodeKindLoadVar, content, parentHash)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (load_var %q): %w", i, step.LoadVar, err)
	}

	node := merkle.Node{
		Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindLoadVar,
		StepIndex: i, Resource: step.LoadVar, Content: content,
	}
	_ = st.RecordNode(context.WithoutCancel(ctx), nodeRecord(node), jobName, "succeeded", nil, nil)

	return hash, stepRan, nonGetOutcome{}, nil
}
