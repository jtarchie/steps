package machine

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// distill: — rung 1.5 of the context ladder. A state declares what it needs
// from a large scope value; a cheap model extracts only that slice before the
// state runs, and inside the state the declared key IS the slice. Each entry
// lowers to a REAL implicit agent state (`consumer#key`) in the defaults
// pass — the same "sugar compiles to states" move the design reserves for
// action chains — so journal, memo, retry, and mock semantics all apply
// unchanged. `#` cannot appear in a JS identifier, which makes the implicit
// names collision-free and their outputs impossible to destructure from any
// other state.

// DistillEntry declares one model-extracted slice of a scope value.
type DistillEntry struct {
	Key  string // the scope name the consumer sees (From==Key shadows the source)
	From string // source scope key: a run input or a predecessor state (default Key)
	// For is the need: what this state requires from the source. A static
	// string or a function of the consumer's (pre-distill) scope — per-item
	// for forEach consumers.
	For Dyn
	// MaxTokens is the slice's output budget — it becomes the implicit
	// state's maxOutputTokens, so the existing cap enforcement bounds it.
	MaxTokens int
	Model     string // alias/ref; default: models.distiller, then the machine default
	Memo      bool   // default true: distillation is pure, replay is always safe
	// StateName is the lowered implicit state (`consumer#key`), filled by
	// ApplyDefaults.
	StateName string
}

// DefaultDistillMaxTokens bounds a slice whose entry declares no budget.
const DefaultDistillMaxTokens = 512

// DistillerAlias is the models: alias the lowering reaches for when an entry
// names no model — the intended shape is a cheap local ref.
const DistillerAlias = "distiller"

// DistillSystem is the engine-owned distiller instruction. An extractor, not
// a summarizer: the verbatim-quote bias is what makes a small local model
// trustworthy at the job. Not user-templatable — the escape hatch is a real
// state.
const DistillSystem = "From SOURCE, extract only what is relevant to NEED. " +
	"Prefer verbatim quotes. Preserve identifiers, signatures, paths, numbers, " +
	"and error text exactly. No commentary, no restructuring, no invention. " +
	"If nothing is relevant, reply exactly: (nothing relevant)"

// IsDistill reports whether the state was lowered from a distill entry.
func (s *State) IsDistill() bool { return s.DistillOf != "" }

// DistillEntryFor returns the consumer's entry that produced an implicit
// distill state, or nil. The engine uses it to resolve the entry's source
// (an absent source yields an empty slice without a model call).
func (m *Machine) DistillEntryFor(s *State) *DistillEntry {
	if !s.IsDistill() {
		return nil
	}
	consumer := m.State(s.DistillOf)
	if consumer == nil {
		return nil
	}
	for i := range consumer.Distill {
		if consumer.Distill[i].Key == s.DistillKey {
			return &consumer.Distill[i]
		}
	}
	return nil
}

// distillPrompt builds the implicit state's user message: the rendered need
// plus the raw source value (yaml-rendered when not a string). A Go-native
// Dyn — there is no JS source to carry, only engine-owned composition.
func distillPrompt(d DistillEntry) func(map[string]any) (any, error) {
	return func(scope map[string]any) (any, error) {
		need, err := d.For.String(scope)
		if err != nil {
			return nil, fmt.Errorf("distill.%s for: %w", d.Key, err)
		}
		src, ok := scope[d.From]
		if !ok || src == nil {
			return nil, fmt.Errorf("distill.%s: source %q is missing from scope", d.Key, d.From)
		}
		text, isStr := src.(string)
		if !isStr {
			raw, err := yaml.Marshal(src)
			if err != nil {
				return nil, fmt.Errorf("distill.%s: rendering source %q: %w", d.Key, d.From, err)
			}
			text = strings.TrimRight(string(raw), "\n")
		}
		return "NEED: " + strings.TrimSpace(need) + "\n\nSOURCE:\n" + text, nil
	}
}

// lowerDistill compiles every distill entry into an implicit agent state and
// rewires the graph: all in-edges of a consumer retarget to its chain head;
// the chain ends at the consumer. Runs inside ApplyDefaults after linear-flow
// wiring (so every edge exists) and before the per-state cascade (so the
// implicit states pick up model aliases, retry defaults, and the {text}
// output contract like any other state).
func lowerDistill(m *Machine) {
	heads := map[string]string{} // consumer name -> chain head name
	var lowered []*State

	for _, s := range m.States {
		if len(s.Distill) == 0 || s.Terminal {
			lowered = append(lowered, s)
			continue
		}

		chain := make([]*State, 0, len(s.Distill))
		for i := range s.Distill {
			d := &s.Distill[i]
			if d.From == "" {
				d.From = d.Key // shadow: inside the state, the key IS the slice
			}
			if d.MaxTokens == 0 {
				d.MaxTokens = DefaultDistillMaxTokens
			}
			d.StateName = s.Name + "#" + d.Key

			// Model resolution: entry -> models.distiller -> machine default
			// (the cascade below fills an empty model from defaults).
			model := d.Model
			if model == "" {
				if _, ok := m.Models[DistillerAlias]; ok {
					model = DistillerAlias
				}
			}
			var modelDyn Dyn
			if model != "" {
				modelDyn = Dyn{Static: model}
			}

			imp := &State{
				Name:       d.StateName,
				DistillOf:  s.Name,
				DistillKey: d.Key,
				Memo:       d.Memo,
				Agent: &AgentSpec{
					Model:  modelDyn,
					System: Dyn{Static: DistillSystem},
					Prompt: Dyn{
						Native: distillPrompt(*d),
						Src:    fmt.Sprintf("distill(%s from %s)", d.Key, d.From),
					},
					MaxTurns:        1, // one call, no tools — semantic retries reset the budget
					MaxOutputTokens: d.MaxTokens,
					Reasoning:       "low", // extraction needs precision, not thought
				},
				// Distill failures are the consumer's failures: same catch
				// edges, same retry policy (nil falls through to defaults).
				Catch: append([]CatchClause(nil), s.Catch...),
			}
			if s.Retry != nil {
				imp.Retry = append([]RetryPolicy{}, s.Retry...)
			}
			// forEach consumers distill per item: the implicit state inherits
			// the fan-out and the consumer zips slices back by index.
			// OnItemFailure pins to fail — a missing slice must never
			// silently misalign the zip.
			if f := s.ForEach; f != nil {
				imp.ForEach = &ForEachSpec{Over: f.Over, As: f.As, Concurrency: f.Concurrency, OnItemFailure: "fail"}
			}
			chain = append(chain, imp)
		}

		for i, imp := range chain {
			next := s.Name
			if i+1 < len(chain) {
				next = chain[i+1].Name
			}
			imp.Transitions = []Transition{{To: next}}
		}
		heads[s.Name] = chain[0].Name
		lowered = append(lowered, chain...)
		lowered = append(lowered, s)
	}

	if len(heads) == 0 {
		return
	}
	m.States = lowered

	// Retarget every in-edge that points at a consumer to its chain head —
	// including the consumer's own loop-backs (a revisit's need may have
	// changed; memo makes unchanged pairs free). The chain's own final hop
	// into its consumer is the one edge that must stay.
	for _, s := range m.States {
		for i := range s.Transitions {
			if head, ok := heads[s.Transitions[i].To]; ok && s.DistillOf != s.Transitions[i].To {
				s.Transitions[i].To = head
			}
		}
		for i := range s.Catch {
			if head, ok := heads[s.Catch[i].To]; ok && s.DistillOf != s.Catch[i].To {
				s.Catch[i].To = head
			}
		}
		if s.Human != nil {
			if head, ok := heads[s.Human.OnTimeout]; ok {
				s.Human.OnTimeout = head
			}
		}
	}
	if head, ok := heads[m.Initial]; ok {
		m.Initial = head
	}
	m.buildIndex()
}
