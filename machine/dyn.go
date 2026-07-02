package machine

import (
	"fmt"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// jsRT wraps the machine's goja runtime. One runtime per machine, guarded by
// a mutex: goja.Runtime is not goroutine-safe, and foreach items may resolve
// prompts/guards concurrently. JS calls are microseconds; LLM latency
// dominates, so contention is irrelevant.
type jsRT struct {
	mu sync.Mutex
	vm *goja.Runtime
}

// callTimeout bounds every user function: a while(true) in a guard becomes
// an error, never a hung run.
const callTimeout = time.Second

func (rt *jsRT) call(fn goja.Callable, src string, scope map[string]any) (goja.Value, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	timer := time.AfterFunc(callTimeout, func() {
		rt.vm.Interrupt(fmt.Sprintf("function exceeded %s", callTimeout))
	})
	defer func() {
		timer.Stop()
		rt.vm.ClearInterrupt()
	}()

	val, err := fn(goja.Undefined(), rt.vm.ToValue(scope))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", firstLine(src), err)
	}
	return val, nil
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i] + "…"
		}
		if i > 80 {
			return s[:i] + "…"
		}
	}
	return s
}

// Dyn is a machine value that is either static data or a JS function of one
// destructurable scope argument — the ONLY two kinds of value the config
// language has. Functions carry their source text for journaling, --print,
// and error messages.
type Dyn struct {
	Static any
	Src    string // function source (empty for static values)

	fn goja.Callable
	rt *jsRT
}

// IsZero reports an unset value.
func (d Dyn) IsZero() bool { return d.Static == nil && d.fn == nil }

// IsFn reports whether the value is computed.
func (d Dyn) IsFn() bool { return d.fn != nil }

// Display renders the value for humans (--print, journal).
func (d Dyn) Display() string {
	if d.fn != nil {
		return firstLine(d.Src)
	}
	if s, ok := d.Static.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", d.Static)
}

// Value resolves the raw value.
func (d Dyn) Value(scope map[string]any) (any, error) {
	if d.fn == nil {
		return d.Static, nil
	}
	v, err := d.rt.call(d.fn, d.Src, scope)
	if err != nil {
		return nil, err
	}
	return v.Export(), nil
}

// String resolves to a string.
func (d Dyn) String(scope map[string]any) (string, error) {
	v, err := d.Value(scope)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s: returned %T, want a string", d.Display(), v)
	}
	return s, nil
}

// Bool resolves a guard. Non-function guards are invalid by construction
// (the loader only accepts functions for when:).
func (d Dyn) Bool(scope map[string]any) (bool, error) {
	v, err := d.Value(scope)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("%s: returned %T, want a boolean", d.Display(), v)
	}
	return b, nil
}

// List resolves to a list (foreach.over).
func (d Dyn) List(scope map[string]any) ([]any, error) {
	v, err := d.Value(scope)
	if err != nil {
		return nil, err
	}
	l, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: returned %T, want an array", d.Display(), v)
	}
	return l, nil
}

// Map resolves to a string-keyed map (tool args, whole input maps).
func (d Dyn) Map(scope map[string]any) (map[string]any, error) {
	v, err := d.Value(scope)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: returned %T, want an object", d.Display(), v)
	}
	return m, nil
}

// ResolveInputs resolves an input block: either a whole-map function, or a
// static map whose individual values may be functions. Static values pass
// through with their real types — numbers stay numbers.
func ResolveInputs(d Dyn, scope map[string]any) (map[string]any, error) {
	if d.IsZero() {
		return map[string]any{}, nil
	}
	if d.IsFn() {
		return d.Map(scope)
	}
	static, ok := d.Static.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("input must be an object or a function returning one, got %T", d.Static)
	}
	out := make(map[string]any, len(static))
	for k, v := range static {
		if nested, ok := v.(Dyn); ok {
			resolved, err := nested.Value(scope)
			if err != nil {
				return nil, fmt.Errorf("input %s: %w", k, err)
			}
			out[k] = resolved
			continue
		}
		out[k] = v
	}
	return out, nil
}
