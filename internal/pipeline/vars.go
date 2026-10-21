package pipeline

// The run-time half of pipeline vars: capturing a load_var: value and
// substituting it into the steps that follow.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
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

	return step.RenderVars(vars.snapshot())
}

// runLoadVarStep captures a file's contents into a pipeline var.
//
// The value is trimmed of surrounding whitespace, because the overwhelmingly
// common way to produce one is `git describe > version.txt`, which leaves a
// trailing newline that would otherwise be substituted into the middle of a
// command.
func runLoadVarStep(ctx context.Context, r stepRunner, i int, step config.Step, parentHash string) (stepResult, error) {
	space, err := r.bw.TaskSpace(ctx, step.LoadVar, step.InputNames(), nil, nil, nil)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (load_var %q): workspace: %w", i, step.LoadVar, err)
	}

	defer workspace.CloseSpace(space, step.LoadVar)

	body, err := readVarFile(space.Dir(), step.VarFile)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (load_var %q) file %q: %w", i, step.LoadVar, step.VarFile, err)
	}

	value := strings.TrimSpace(string(body))

	if vars := runVarsFrom(ctx); vars != nil {
		vars.set(step.LoadVar, value)
	}

	slog.InfoContext(ctx, "job.load_var", "job", r.jobName, "var", step.LoadVar, "bytes", len(value))

	content := map[string]any{"load_var": step.LoadVar, "file": step.VarFile, "value": value}

	hash, err := merkle.HashNode(merkle.NodeKindLoadVar, content, parentHash)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (load_var %q): %w", i, step.LoadVar, err)
	}

	node := merkle.Node{
		Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindLoadVar,
		StepIndex: i, Resource: step.LoadVar, Content: content,
	}
	bestEffort(ctx, "RecordNode", r.st.RecordNode(context.WithoutCancel(ctx), nodeRecord(node), r.jobName, "succeeded", nil, nil))

	return ran(hash), nil
}

// maxVarFileBytes caps what a load_var: reads, for the reason from_file:'s cap
// exists: a var is a value substituted into later steps, not a data transfer.
const maxVarFileBytes = maxAcrossFileBytes

// readVarFile reads a load_var: file from inside dir.
func readVarFile(dir, file string) ([]byte, error) {
	return readConfinedFile(dir, file, maxVarFileBytes)
}

// errFileTooLarge marks a confined read that hit its cap, so a caller can word it for its own field.
var errFileTooLarge = errors.New("is above the limit")

// readConfinedFile reads rel from inside dir, at most limit bytes. Load confines a literal path, but a rendered one can be steered by a captured var and a producing step controls what the tree holds, so the lexical check repeats here; OpenInRoot also refuses a symlink in the tree that points out of it, and opening once closes the stat-then-read race.
func readConfinedFile(dir, rel string, limit int) ([]byte, error) {
	if _, ok := config.ConfinedPath(".", rel); !ok {
		return nil, errors.New("escapes the workspace; it must be a path inside a declared input")
	}

	f, err := os.OpenInRoot(dir, filepath.FromSlash(rel))
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}

	defer func() { _ = f.Close() }()

	body, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}

	if len(body) > limit {
		return nil, fmt.Errorf("%w of %d bytes", errFileTooLarge, limit)
	}

	return body, nil
}
