package machine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
)

// The destructured parameter list IS the state's declared input contract:
// `({article, critique}) => ...` names exactly what the function reads.
// CheckContracts extracts those keys from each function's source and checks
// them against the state's derived scope — misspellings fail the load with
// the available keys listed, before any proxy dry-run even runs.

// scopeReserved are keys the engine owns; inputs and states cannot shadow
// them (the flat scope would become ambiguous).
var scopeReserved = []string{
	"output", "event", "visits", "run", "attempt",
	"args", "calls", "turn", "index", "total", "ctx",
}

// CheckContracts validates every function's destructured parameters.
func CheckContracts(m *Machine) error {
	if len(m.Input) == 0 {
		// Run inputs are unknowable without an input: block — any unknown
		// key could be one. Declaring input: buys strict checking.
		return nil
	}
	base := m.scopeKeys()
	var errs []string
	for _, s := range m.States {
		if s.Terminal {
			continue
		}
		errs = append(errs, checkStateContract(s, base)...)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	return nil
}

// contractCheck reports a destructured-parameter error for one function call
// site, naming the state, site, and offending key against the currently
// available scope (base + whatever extras that site adds).
type contractCheck struct {
	stateName string
	base      []string
	errs      []string
}

func (c *contractCheck) check(site string, d Dyn, extra ...string) {
	if !d.IsFn() {
		return
	}
	keys, ok := destructuredKeys(d.Src)
	if !ok {
		return // non-destructuring param (s => ...) — proxy dry-run covers it
	}
	allowed := append(append([]string{}, c.base...), extra...)
	for _, k := range keys {
		if !contains(allowed, k) {
			sort.Strings(allowed)
			c.errs = append(c.errs, fmt.Sprintf(
				"state %q %s destructures {%s} — unknown; available: %s",
				c.stateName, site, k, strings.Join(allowed, ", ")))
		}
	}
}

// checkStateContract validates one non-terminal state's functions'
// destructured parameters against the scope available at each call site.
func checkStateContract(s *State, base []string) []string {
	c := &contractCheck{stateName: s.Name, base: base}

	handlerExtras := checkForEachAndDistill(c, s)
	if a := s.Agent; a != nil {
		checkAgentContract(c, a, handlerExtras)
	}
	if h := s.Human; h != nil {
		c.check("human", h.Prompt, handlerExtras...)
		if h.Choices != nil {
			c.check("choices.multi", h.Choices.Dynamic, handlerExtras...)
		}
	}
	checkInputContract(c, s, handlerExtras)
	// Guards run after the state completes — they see output/event but
	// never a per-item variable (foreach guards judge the aggregate). The
	// verdict: is one such guard.
	if !s.Verdict.IsZero() {
		c.check("verdict", s.Verdict, "output", "event")
	}
	for i, t := range s.Transitions {
		c.check(fmt.Sprintf("transitions[%d].when", i), t.When, "output", "event")
	}
	return c.errs
}

// checkForEachAndDistill checks forEach.over and each distill.for function,
// and computes handlerExtras — the extra scope keys every handler site
// below sees: the fan-out variable/index/total (if any), plus the
// distilled keys (distill: for: functions themselves see only the
// pre-distill scope; shadow keys are already state/input names, so
// duplicates are harmless).
func checkForEachAndDistill(c *contractCheck, s *State) (handlerExtras []string) {
	var itemExtras []string
	if f := s.ForEach; f != nil {
		c.check("forEach.over", f.Over)
		itemExtras = []string{f.As, "index", "total"}
	}
	handlerExtras = itemExtras
	if len(s.Distill) > 0 {
		handlerExtras = append([]string{}, itemExtras...)
		for i := range s.Distill {
			d := &s.Distill[i]
			c.check("distill."+d.Key+".for", d.For, itemExtras...)
			handlerExtras = append(handlerExtras, d.Key)
		}
	}
	return handlerExtras
}

func checkAgentContract(c *contractCheck, a *AgentSpec, handlerExtras []string) {
	historyExtras := handlerExtras
	if a.History != nil {
		historyExtras = append(append([]string{}, handlerExtras...), a.History.As)
	}
	c.check("model", a.Model, handlerExtras...)
	c.check("prompt", a.Prompt, historyExtras...)
	c.check("system", a.System, historyExtras...)
	toolExtras := append(append([]string{}, handlerExtras...), "args", "calls", "turn")
	for _, tr := range a.Tools {
		c.check("tool "+tr.Name+" when", tr.When, toolExtras...)
		c.check("tool "+tr.Name+" args", tr.Args, handlerExtras...)
	}
}

func checkInputContract(c *contractCheck, s *State, handlerExtras []string) {
	c.check("input", s.Input, handlerExtras...)
	if inputs, ok := s.Input.Static.(map[string]any); ok {
		for k, v := range inputs {
			if nested, isDyn := v.(Dyn); isDyn {
				c.check("input."+k, nested, handlerExtras...)
			}
		}
	}
}

// scopeKeys is the flat root every function may destructure: run inputs +
// state names + the always-present engine keys.
func (m *Machine) scopeKeys() []string {
	keys := []string{"visits", "run", "attempt"}
	for name := range m.Input {
		keys = append(keys, name)
	}
	for _, st := range m.States {
		if st.IsDistill() {
			continue // `name#key` is not an identifier — never destructurable
		}
		keys = append(keys, st.Name)
	}
	return keys
}

// destructuredKeys parses a function's source and returns the keys of its
// first parameter's object pattern. ok=false when the parameter is not a
// destructuring pattern (or the source cannot be parsed).
func destructuredKeys(src string) (keys []string, ok bool) {
	program, err := parser.ParseFile(nil, "", "("+src+")", 0)
	if err != nil {
		return nil, false
	}
	if len(program.Body) == 0 {
		return nil, false
	}
	stmt, isExpr := program.Body[0].(*ast.ExpressionStatement)
	if !isExpr {
		return nil, false
	}

	var params *ast.ParameterList
	switch fn := stmt.Expression.(type) {
	case *ast.ArrowFunctionLiteral:
		params = fn.ParameterList
	case *ast.FunctionLiteral:
		params = fn.ParameterList
	default:
		return nil, false
	}
	if params == nil || len(params.List) == 0 {
		return nil, false
	}
	pattern, isPattern := params.List[0].Target.(*ast.ObjectPattern)
	if !isPattern {
		return nil, false
	}
	for _, prop := range pattern.Properties {
		switch p := prop.(type) {
		case *ast.PropertyShort:
			keys = append(keys, p.Name.Name.String())
		case *ast.PropertyKeyed:
			if lit, isString := p.Key.(*ast.StringLiteral); isString {
				keys = append(keys, lit.Value.String())
			} else if ident, isIdent := p.Key.(*ast.Identifier); isIdent {
				keys = append(keys, ident.Name.String())
			}
		}
	}
	return keys, true
}

// Dedent strips the common leading whitespace of non-empty lines and trims
// leading/trailing blank lines — prompts indent naturally in machine files
// without leaking indentation to the model.
func Dedent(s string) string {
	lines := strings.Split(s, "\n")

	minIndent := -1
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" {
			continue
		}
		indent := len(line) - len(trimmed)
		if minIndent < 0 || indent < minIndent {
			minIndent = indent
		}
	}
	if minIndent > 0 {
		for i, line := range lines {
			if len(line) >= minIndent {
				lines[i] = line[minIndent:]
			} else {
				lines[i] = strings.TrimLeft(line, " \t")
			}
		}
	}

	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return strings.Join(lines[start:end], "\n")
}
