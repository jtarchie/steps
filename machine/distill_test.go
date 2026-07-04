package machine

import (
	"strings"
	"testing"
)

// TestDistillLowering: entries lower to implicit `name#key` agent states,
// every in-edge (including the consumer's own loop-back) retargets to the
// chain head, and forEach consumers hand the fan-out down to the chain.
func TestDistillLowering(t *testing.T) {
	src := `
const plan = {
  prompt: ({ spec }) => "plan " + spec,
  output: { files: [{ path: "string" }], contract: "string" },
};
const gen = {
  forEach: { over: ({ plan }) => plan.files, as: "target", concurrency: 2 },
  distill: {
    spec: { for: ({ target }) => "what " + target.path + " needs", maxTokens: 300 },
    contract_slice: { from: "plan", for: "the public contract only", memo: false },
  },
  prompt: ({ spec, target, contract_slice }) => spec + target.path + contract_slice,
};
export default {
  name: "lower",
  input: { spec: { type: "string", required: true } },
  models: { distiller: "mock" },
  model: "mock",
  states: { plan, gen },
  flow: pipe(plan, branch(gen, [
    when(({ visits }) => visits.gen < 2).to(gen),
    done,
  ])),
};`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	spec := m.State("gen#spec")
	if spec == nil || spec.Agent == nil {
		t.Fatal("gen#spec implicit state missing or not an agent")
	}
	if spec.DistillOf != "gen" || spec.DistillKey != "spec" {
		t.Errorf("gen#spec marks = %q/%q, want gen/spec", spec.DistillOf, spec.DistillKey)
	}
	if !spec.Memo {
		t.Error("distill memo should default to true")
	}
	if spec.Agent.MaxOutputTokens != 300 {
		t.Errorf("gen#spec maxOutputTokens = %d, want the 300 slice budget", spec.Agent.MaxOutputTokens)
	}
	if ref, _ := spec.Agent.Model.Static.(string); ref != "mock" {
		t.Errorf("gen#spec model = %v, want the distiller alias resolved to mock", spec.Agent.Model.Display())
	}
	if spec.Agent.MaxTurns != 1 || spec.Agent.Reasoning != "low" {
		t.Errorf("gen#spec turns/reasoning = %d/%q, want 1/low", spec.Agent.MaxTurns, spec.Agent.Reasoning)
	}
	if spec.ForEach == nil || spec.ForEach.As != "target" || spec.ForEach.Concurrency != 2 {
		t.Errorf("gen#spec forEach = %+v, want the consumer's fan-out inherited", spec.ForEach)
	}
	if spec.ForEach.OnItemFailure != "fail" {
		t.Errorf("gen#spec onItemFailure = %q, want fail (zip alignment)", spec.ForEach.OnItemFailure)
	}
	if !spec.Output.DefaultOutput() {
		t.Errorf("gen#spec output = %+v, want default {text: string}", spec.Output.Schema)
	}

	slice := m.State("gen#contract_slice")
	if slice == nil {
		t.Fatal("gen#contract_slice implicit state missing")
	}
	if slice.Memo {
		t.Error("memo: false on the entry must survive lowering")
	}
	if slice.Agent.MaxOutputTokens != DefaultDistillMaxTokens {
		t.Errorf("default slice budget = %d, want %d", slice.Agent.MaxOutputTokens, DefaultDistillMaxTokens)
	}

	// Chain wiring: plan -> gen#spec -> gen#contract_slice -> gen.
	if to := m.State("plan").Transitions[0].To; to != "gen#spec" {
		t.Errorf("plan flows to %q, want the chain head gen#spec", to)
	}
	if to := spec.Transitions[0].To; to != "gen#contract_slice" {
		t.Errorf("gen#spec flows to %q, want gen#contract_slice", to)
	}
	if to := slice.Transitions[0].To; to != "gen" {
		t.Errorf("gen#contract_slice flows to %q, want the consumer", to)
	}
	// The consumer's own loop-back re-enters the chain (revisits re-distill;
	// memo makes unchanged pairs free).
	gen := m.State("gen")
	if to := gen.Transitions[0].To; to != "gen#spec" {
		t.Errorf("gen loop-back targets %q, want gen#spec", to)
	}
	if to := gen.Transitions[1].To; to != "done" {
		t.Errorf("gen fallback targets %q, want done", to)
	}
}

// TestDistillInitialRetarget: a distilling initial state starts at its chain.
func TestDistillInitialRetarget(t *testing.T) {
	src := `
export default {
  name: "init",
  input: { spec: "string" },
  model: "mock",
  states: {
    use: { distill: { spec: { for: "the api only" } }, prompt: ({ spec }) => "u" + spec },
  },
};`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Initial != "use#spec" {
		t.Errorf("initial = %q, want use#spec", m.Initial)
	}
}

// TestDistillShadowDryRun: field access on a shadowed key fails the load
// naming the distillation; string use of the slice passes.
func TestDistillShadowDryRun(t *testing.T) {
	bad := `
export default {
  name: "shadow",
  input: { spec: "string" },
  model: "mock",
  states: {
    use: { distill: { spec: { for: "x" } }, prompt: ({ spec }) => "t" + spec.title },
  },
};`
	_, err := Parse([]byte(bad))
	if err == nil {
		t.Fatal("field access on a distilled (string) value should fail the load")
	}
	if !strings.Contains(err.Error(), "unknown field use.spec.title") || !strings.Contains(err.Error(), "distill") {
		t.Errorf("error should name the field and the distillation, got: %v", err)
	}

	good := `
export default {
  name: "shadow-ok",
  input: { spec: "string" },
  model: "mock",
  states: {
    use: { distill: { spec: { for: "x" } }, prompt: ({ spec }) => spec.trim() + " and " + spec },
  },
};`
	if _, err := Parse([]byte(good)); err != nil {
		t.Errorf("string use of a distilled value should load: %v", err)
	}
}

// TestDistillValidation: the fail-before-you-spend surface.
func TestDistillValidation(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{
			"missing for",
			`export default { name: "x", input: { spec: "string" }, model: "mock",
			  states: { s: { distill: { spec: {} }, prompt: ({ spec }) => spec } } };`,
			"needs for:",
		},
		{
			"reserved key",
			`export default { name: "x", input: { spec: "string" }, model: "mock",
			  states: { s: { distill: { output: { from: "spec", for: "x" } }, prompt: ({ spec }) => spec } } };`,
			"reserved scope key",
		},
		{
			"source not a predecessor",
			`const a = { prompt: "a" };
			 const b = { distill: { alpha: { from: "later", for: "x" } }, prompt: ({ alpha }) => alpha };
			 const later = { prompt: "c" };
			 export default { name: "x", input: { spec: "string" }, model: "mock", states: { a, b, later } };`,
			"not a predecessor",
		},
		{
			"unknown source",
			`export default { name: "x", input: { spec: "string" }, model: "mock",
			  states: { s: { distill: { alpha: { from: "nope", for: "x" } }, prompt: ({ alpha }) => alpha } } };`,
			"not a run input or a predecessor",
		},
		{
			"derived key shadows a state",
			`const a = { prompt: "a" };
			 const b = { distill: { a: { from: "spec", for: "x" } }, prompt: ({ a }) => a.x };
			 export default { name: "x", input: { spec: "string" }, model: "mock", states: { a, b } };`,
			"shadows",
		},
		{
			"key collides with forEach.as",
			`export default { name: "x", input: { spec: "string" }, model: "mock",
			  states: { s: {
			    forEach: { over: ({ spec }) => [spec], as: "item" },
			    distill: { item: { from: "spec", for: "x" } },
			    prompt: ({ item }) => "" + item,
			  } } };`,
			"collides with forEach.as",
		},
		{
			"unknown entry key",
			`export default { name: "x", input: { spec: "string" }, model: "mock",
			  states: { s: { distill: { spec: { for: "x", budget: 3 } }, prompt: ({ spec }) => spec } } };`,
			"unknown key",
		},
		{
			"for referencing an unknown scope key",
			`export default { name: "x", input: { spec: "string" }, model: "mock",
			  states: { s: { distill: { spec: { for: ({ nope }) => nope } }, prompt: ({ spec }) => spec } } };`,
			"nope",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatalf("expected load error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestStateNamesMustBeIdentifiers: the flat scope requires destructurable
// names — and `#` stays reserved for lowered distill states.
func TestStateNamesMustBeIdentifiers(t *testing.T) {
	src := `
export default {
  name: "x",
  model: "mock",
  states: { "my-state": "do a thing" },
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "valid identifiers") {
		t.Errorf("non-identifier state name should fail the load, got: %v", err)
	}
}

// TestDistillScopeDoc: steps context names the slice, its source, and budget.
func TestDistillScopeDoc(t *testing.T) {
	src := `
export default {
  name: "doc",
  input: { spec: "string" },
  model: "mock",
  states: {
    use: { distill: { spec: { for: "the api", maxTokens: 200 } }, prompt: ({ spec }) => spec },
  },
};`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	doc := ScopeDoc(m, m.State("use"))
	if !strings.Contains(doc, "distilled slice of spec") || !strings.Contains(doc, "200 tokens") {
		t.Errorf("scope doc should describe the slice, got:\n%s", doc)
	}
	if strings.Contains(doc, "use#spec\n") {
		t.Errorf("scope doc should not list the implicit state as a plain key:\n%s", doc)
	}
}
