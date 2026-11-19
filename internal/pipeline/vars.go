package pipeline

// The run-time half of ((.:name)): capturing a load_var: value or a get's
// facts, and substituting them into the steps that follow.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/redact"
	"github.com/jtarchie/steps/internal/workspace"
)

// runVars holds what one job run captured: load_var: values and gets' facts.
// An across: cell gets a child scope, which reads its parent's values and
// keeps its own captures to itself — Concourse's scope for a cell.
type runVars struct {
	mu     sync.Mutex
	parent *runVars
	by     map[string]capture
}

// capture is one ((.:name))'s value; facts marks a get's, which nothing
// before the run can know, so a step reading one is never reused.
type capture struct {
	value any
	facts bool
}

type runVarsKey struct{}

func withRunVars(ctx context.Context) context.Context {
	return context.WithValue(ctx, runVarsKey{}, &runVars{by: map[string]capture{}})
}

// withCellVars gives an across: cell its own scope under the run's.
func withCellVars(ctx context.Context) context.Context {
	return context.WithValue(ctx, runVarsKey{}, &runVars{parent: runVarsFrom(ctx), by: map[string]capture{}})
}

func runVarsFrom(ctx context.Context) *runVars {
	vars, _ := ctx.Value(runVarsKey{}).(*runVars)

	return vars
}

func (v *runVars) set(name string, c capture) {
	if v == nil {
		return
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	v.by[name] = c
}

func (v *runVars) lookup(name string) (capture, bool) {
	for scope := v; scope != nil; scope = scope.parent {
		scope.mu.Lock()
		c, ok := scope.by[name]
		scope.mu.Unlock()

		if ok {
			return c, true
		}
	}

	return capture{}, false
}

// setFacts makes a get's facts readable as ((.:get.name)); a name said twice keeps its last value.
func setFacts(ctx context.Context, get string, facts []events.Fact) {
	values := make(map[string]any, len(facts))
	for _, fact := range facts {
		values[fact.Name] = fact.Value
	}

	runVarsFrom(ctx).set(get, capture{value: values, facts: true})
}

type reusableKey struct{}

// withoutReuse marks a step whose result no cache may stand in for.
func withoutReuse(ctx context.Context) context.Context {
	return context.WithValue(ctx, reusableKey{}, false)
}

// reusable reports whether an earlier run's result may stand in for this
// step's: not for a step reading a get's facts.
func reusable(ctx context.Context) bool {
	reuse, set := ctx.Value(reusableKey{}).(bool)

	return !set || reuse
}

// renderStepVars substitutes every ((.:name)) a step's own fields carry from
// what the run has captured so far, and reports whether one was a get's facts.
//
// Applied BEFORE the step is hashed, deliberately: a captured value changes
// what the step actually runs, so two runs that captured different values must
// not share a cache entry. The alternative — hashing the unsubstituted text —
// would let a step that ran against v1.2.3 satisfy a run that meant v2.0.0.
func renderStepVars(ctx context.Context, step config.Step) (config.Step, bool, error) {
	// A matrix's cells render their own, after its {{ .vars }} pass: filled in here, a captured value would be read as a template.
	if len(step.Across) > 0 || len(step.LocalRefs()) == 0 {
		return step, false, nil
	}

	vars := runVarsFrom(ctx)
	readsFacts := false

	rendered, err := step.RenderLocalVars(func(ref config.LocalRef) (string, error) {
		c, ok := vars.lookup(ref.Root)
		if !ok {
			return "", fmt.Errorf("%s has no value in this run", ref)
		}

		readsFacts = readsFacts || c.facts

		return config.LocalValue(c.value, ref)
	})
	if err != nil {
		return step, false, fmt.Errorf("%s: %w", executedStepName(step), err)
	}

	return rendered, readsFacts, nil
}

// runLoadVarStep captures a file's contents, read in the step's format, as
// ((.:name)), and hides each string of it in what the run prints unless the
// step says reveal: true.
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

	format := step.LoadVarFormat()

	value, err := config.ParseLoadVar(format, body)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (load_var %q) file %q: %w", i, step.LoadVar, step.VarFile, err)
	}

	// Hidden from what the run prints and from every node the store keeps (nodeRecord masks them).
	if !step.Reveal {
		for _, text := range config.LocalStrings(value) {
			// A short value still masks, and masks unrelated output too; said so a mangled log has an explanation.
			if len(text) < shortSecret {
				slog.WarnContext(ctx, "load_var.short", "var", step.LoadVar, "bytes", len(text), "note", "masking it may also mask unrelated output; reveal: true shows a value that is not secret")
			}

			redact.SecretsFrom(ctx).Add(text)
			redact.SecretsFrom(ctx).Add(jsonSpelling(text))
		}
	}

	runVarsFrom(ctx).set(step.LoadVar, capture{value: value})

	content := map[string]any{"load_var": step.LoadVar, "file": step.VarFile, "format": format, "value": value}

	slog.InfoContext(ctx, "job.load_var", "job", r.jobName, "var", step.LoadVar, "format", format, "bytes", len(body))

	hash, err := merkle.HashNode(merkle.NodeKindLoadVar, content, parentHash)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (load_var %q): %w", i, step.LoadVar, err)
	}

	node := merkle.Node{
		Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindLoadVar,
		StepIndex: i, Resource: step.LoadVar, Content: content,
	}
	bestEffort(ctx, "RecordNode", recordSucceeded(context.WithoutCancel(ctx), r, node, nil))

	return ran(hash), nil
}

// jsonSpelling is text as a whole value substituted as JSON spells it (a tab
// as \t, a quote as \"), which a mask of the raw text does not match.
func jsonSpelling(text string) string {
	var spelled strings.Builder

	encoder := json.NewEncoder(&spelled)
	encoder.SetEscapeHTML(false)

	err := encoder.Encode(text)
	if err != nil {
		return text
	}

	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSuffix(spelled.String(), "\n"), `"`), `"`)
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
