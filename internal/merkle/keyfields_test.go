package merkle

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// leafPaths is every settable leaf under v: struct fields are walked into, and so are the elements a base value already has, so a caller seeds a slice with one valid element to have its fields probed.
func leafPaths(v reflect.Value, path string, out *[]string) {
	if inner, ok := walkable(v); ok {
		if v.Kind() == reflect.Slice {
			path += "[0]"
		}

		for i := range inner.NumField() {
			if field := inner.Type().Field(i); field.IsExported() {
				leafPaths(inner.Field(i), strings.TrimPrefix(path+"."+field.Name, "."), out)
			}
		}

		return
	}

	*out = append(*out, path)
}

// walkable is the struct to descend into, if v holds one: itself, the first element of a seeded slice, or what a non-nil pointer points at.
func walkable(v reflect.Value) (reflect.Value, bool) {
	//nolint:exhaustive // every other kind is a leaf, which is the caller's default
	switch v.Kind() {
	case reflect.Struct:
		return v, true
	case reflect.Slice:
		if v.Len() > 0 && v.Index(0).Kind() == reflect.Struct {
			return v.Index(0), true
		}
	case reflect.Pointer:
		if !v.IsNil() && v.Elem().Kind() == reflect.Struct {
			return v.Elem(), true
		}
	}

	return reflect.Value{}, false
}

// resolve walks a leafPaths path back to the settable value it names.
func resolve(v reflect.Value, path string) reflect.Value {
	for _, part := range strings.Split(path, ".") {
		name, indexed := strings.CutSuffix(part, "[0]")

		if v.Kind() == reflect.Pointer {
			v = v.Elem()
		}

		v = v.FieldByName(name)

		if indexed {
			v = v.Index(0)
		}
	}

	return v
}

// perturb gives v a value it did not have: the smallest change of each kind that a cache key could be expected to notice.
func perturb(v reflect.Value) { perturbTo(v, 0) }

// maxPerturbDepth stops at the first nested struct's own leaves: config.Step holds []Step, so filling structs all the way down never ends.
const maxPerturbDepth = 2

func perturbTo(v reflect.Value, depth int) {
	if depth > maxPerturbDepth || perturbScalar(v) {
		return
	}

	//nolint:exhaustive // the scalar kinds are perturbScalar's, and a kind neither handles (a chan, a func) is not something a pipeline can write
	switch v.Kind() {
	case reflect.Slice:
		element := reflect.New(v.Type().Elem()).Elem()
		perturbTo(element, depth+1)
		v.Set(reflect.Append(v, element))
	case reflect.Map:
		grown := reflect.MakeMap(v.Type())

		for _, key := range v.MapKeys() {
			grown.SetMapIndex(key, v.MapIndex(key))
		}

		key, value := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		perturbTo(key, depth+1)
		perturbTo(value, depth+1)
		grown.SetMapIndex(key, value)
		v.Set(grown)
	case reflect.Pointer:
		fresh := reflect.New(v.Type().Elem())
		perturbTo(fresh.Elem(), depth+1)
		v.Set(fresh)
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				perturbTo(v.Field(i), depth+1)
			}
		}
	}
}

func perturbScalar(v reflect.Value) bool {
	//nolint:exhaustive // the composite kinds are perturbTo's
	switch v.Kind() {
	case reflect.String:
		v.SetString(v.String() + "-changed")
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(v.Int() + 7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(v.Uint() + 7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(v.Float() + 0.25)
	case reflect.Interface:
		v.Set(reflect.ValueOf("changed"))
	default:
		return false
	}

	return true
}

// render is build's answer as the bytes a key is hashed from, or the error it gave.
func render[T any](t *testing.T, value T, build func(T) (any, error)) string {
	t.Helper()

	built, err := build(value)
	if err != nil {
		return "error: " + err.Error()
	}

	raw, err := json.Marshal(built)
	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}

// unkeyed is every leaf of base that can change without build's answer changing — the fields a cached step would NOT be re-run for.
func unkeyed[T any](t *testing.T, base T, build func(T) (any, error)) []string {
	t.Helper()

	// The base goes through the same copy as every variant, so a difference is the perturbation and never the round trip.
	before := render(t, deepCopy(t, base), build)

	var paths []string

	leafPaths(reflect.ValueOf(&base).Elem(), "", &paths)

	var silent []string

	for _, path := range paths {
		changed := deepCopy(t, base)
		perturb(resolve(reflect.ValueOf(&changed).Elem(), path))

		if render(t, changed, build) == before {
			silent = append(silent, path)
		}
	}

	sort.Strings(silent)

	return silent
}

// deepCopy goes through JSON so that perturbing a slice or map in the copy cannot reach back into base.
func deepCopy[T any](t *testing.T, value T) T {
	t.Helper()

	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}

	var out T

	err = json.Unmarshal(raw, &out)
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// Every reason below says where it comes from. "source:" is the code's own account (a field doc or a builder's comment). "observed:" means nothing in the tree says the field was left out on purpose: it is recorded as found on 2026-09-18 so that the NEXT silent field fails this test, and each one was reported rather than decided here — folding a field in changes the key for every cached step, which is not a test's call to make.
const (
	offTheStep   = "source: valid only on a job's or the pipeline's assert (requireExecutionOnly), never on the step this builder hashes"
	notThisVerb  = "source: a template this verb never renders, so it cannot change what the step does"
	derivedFlag  = "source: yaml:\"-\" — set by the loader for a built-in Go-backed type (webhook, cron), which no pipeline can write"
	operational  = "source: the field's own doc says never hashed — a deadline, retry, budget or history-size knob is not part of what a step asks for"
	secretNamed  = "source: AgentContentMap — nothing secret-adjacent belongs in hashed content"
	loadedInto   = "source: resolved at load into a field that IS keyed (system_file: into system:, file: into every field it supplies, an agent's description onto the grant that names it) — a Config built by hand, as here, skips the loader"
	otherForm    = "source: not a field of this grant form (validateSubAgentToolShape and its siblings refuse it at load)"
	undocumented = "observed: nothing says this was left out on purpose"
)

type keyProbe struct {
	name   string
	silent func(t *testing.T) []string
	// filled is the builder's bytes with every keyed leaf but unfilled set at once (fillKeyed), which keyfields.golden pins.
	filled func(t *testing.T, p keyProbe) string
	// unfilled are the leaves that select ANOTHER form (a grant's agent: or mcp:): setting one would hash that form instead of this one.
	unfilled []string
	// enums are the leaves only a listed value is accepted for, given one here instead of their path.
	enums map[string]any
	want  map[string]string
}

func probeOf[T any](name string, base T, want map[string]string, build func(T) (any, error)) keyProbe {
	return keyProbe{
		name:   name,
		want:   want,
		silent: func(t *testing.T) []string { return unkeyed(t, base, build) },
		filled: func(t *testing.T, p keyProbe) string {
			skip := maps.Clone(want)
			for _, leaf := range p.unfilled {
				skip[leaf] = "selects another form"
			}

			filled := fillKeyed(t, base, skip)

			for path, value := range p.enums {
				resolve(reflect.ValueOf(&filled).Elem(), path).Set(reflect.ValueOf(value))
			}

			return render(t, filled, build)
		},
	}
}

func (p keyProbe) leaving(leaves ...string) keyProbe {
	p.unfilled = leaves

	return p
}

func (p keyProbe) choosing(enums map[string]any) keyProbe {
	p.enums = enums

	return p
}

func keyProbes() []keyProbe {
	rtype := config.ResourceType{Name: "git", Limits: &config.ContainerLimits{}}
	// The server is named what fillKeyed writes into Config.MCP.Server, so the filled type's mcp: stage resolves and is pinned too.
	rtypeCfg := &config.Config{MCPServers: []config.MCPServer{{Name: "Config.MCP.Server", Endpoint: "http://localhost:2"}}}
	server := config.MCPServer{Name: "github", Endpoint: "http://localhost:1"}
	source := config.AgentSource{Model: "lmstudio/qwen"}
	step := config.Step{Agent: "reviewer", Messages: []string{"do it"}}

	agentContent := func(agents ...config.Agent) (any, error) {
		// The rule is named what fillKeyed writes into the first tool's rules:, so the grant resolves.
		cfg := &config.Config{Agents: agents, Rules: []config.Rule{{Name: "Tools[0].Rules[0]", When: "true"}}}

		ri, err := cfg.ResolveAgentInvocation(step)
		if err != nil {
			return nil, fmt.Errorf("resolving: %w", err)
		}

		return AgentContentMap(cfg, step, ri)
	}

	agentEnums := map[string]any{"ReasoningEffort": "medium"}

	grants := &config.Config{
		MCPServers: []config.MCPServer{server},
		Agents:     []config.Agent{{Name: "extra", Source: source, Description: "a helper"}},
		Rules:      []config.Rule{{Name: "Rules[0]", When: "true", Accept: "true"}},
	}

	grant := func(name string, spec config.ToolSpec, want map[string]string) keyProbe {
		return probeOf("ToolSpec/"+name, spec, want, func(spec config.ToolSpec) (any, error) {
			return toolSpecsContent(grants, []config.ToolSpec{spec})
		})
	}

	resourceType := map[string]string{"Name": "source: config.ResourceType — a step is keyed by what the type DOES, so a rename re-runs nothing and two types doing the same thing share an entry", "Config.Check": notThisVerb, "Config.Webhook": derivedFlag, "Config.Cron": derivedFlag}

	agentSilent := map[string]string{
		"Attempts": operational, "Budget": operational, "DelegateBudgetPercent": operational, "Timeout": operational,
		"CompactAfterTokens": operational, "MaxContextBytes": operational,
		"ContextWindow":           "source: config.Agent.ContextWindow — its only consumer is resolveCompactionBudget, the operational budget compact_after_tokens: also sets",
		"Preflight":               "source: config.Agent.Preflight — it decides whether a health probe runs before the step, nothing about the conversation",
		"Fallback":                "source: config.Agent.Fallback — the primary is what the step asks for and what the key names; this is outage handling",
		"Settings":                "source: refused at load on a hosted agent (validateCLIAgents), and keyed as cli_settings on a CLI one — the 'Agent/cli' probe holds it to that. This probe's agent is hosted, which is why an earlier reading of it wrongly reported settings: as outside the key",
		"Source.StringToolChoice": "source: config.AgentSource — how one request is spelled for a server that cannot parse the precise form, not what is asked",
		"Source.APIKeyEnv":        secretNamed,
		"File":                    loadedInto, "SystemFile": loadedInto, "Description": loadedInto,
		"Caches": "source: config.TaskCache — a speed-up only, never hashed, or a warm cache would make identical work look different",
	}

	with := func(base map[string]string, more map[string]string) map[string]string {
		out := map[string]string{}

		for k, v := range base {
			out[k] = v
		}

		for k, v := range more {
			out[k] = v
		}

		return out
	}

	toolEnv := "source: config.ToolSpec.Env — refused at load on every grant and selection (validateToolEnvShape); only an mcp_exports: command carries one, and an export is never hashed"

	unusedByGrant := func(fields ...string) map[string]string {
		out := map[string]string{"Timeout": operational, "Env": toolEnv}

		for _, field := range fields {
			out[field] = otherForm
		}

		return out
	}

	return []keyProbe{
		probeOf("ContainerLimits", config.ContainerLimits{}, map[string]string{}, func(l config.ContainerLimits) (any, error) {
			content := map[string]any{}
			withIsolation(false, &l, content)

			return content, nil
		}),
		probeOf("Assert", config.Assert{ToolCalls: []config.ExpectedToolCall{{Name: "read_file"}}}, map[string]string{
			"Execution": offTheStep, "Outcome": offTheStep,
		}, func(a config.Assert) (any, error) {
			return assertContent(&a), nil
		}),
		probeOf("ResourceType/get", rtype, with(resourceType, map[string]string{"Config.Out": notThisVerb}), func(rt config.ResourceType) (any, error) {
			return GetNodeContent(rtypeCfg, config.Step{Get: "repo"}, rt, nil, nil, nil)
		}),
		probeOf("ResourceType/put", rtype, with(resourceType, map[string]string{"Config.In": notThisVerb}), func(rt config.ResourceType) (any, error) {
			return PutNodeContent(rtypeCfg, config.Step{Put: "repo"}, rt, nil, nil, nil, nil, false)
		}),
		probeOf("ResourceType/cache key", rtype, with(resourceType, map[string]string{"Config.Out": notThisVerb}), func(rt config.ResourceType) (any, error) {
			return ResourceCacheKey(rtypeCfg, rt, nil, nil, nil, nil)
		}),
		probeOf("MCPServer", server, map[string]string{
			"Auth.CallbackPort": "source: config.MCPServerAuth — read only by `steps mcp login`, so no step ever runs with it",
		}, func(srv config.MCPServer) (any, error) {
			return mcpServerContent(&config.Config{MCPServers: []config.MCPServer{srv}}, "github")
		}),
		grant("builtin", config.ToolSpec{Builtin: "run_shell"}, unusedByGrant("MCPTool", "MCPTools")).leaving("Agent", "MCP"),
		grant("custom", config.ToolSpec{Name: "lint", Description: "lints", Run: "golangci-lint run"}, unusedByGrant("MCPTool", "MCPTools")).leaving("Agent", "MCP"),
		grant("sub-agent", config.ToolSpec{Agent: "extra", Description: "a helper"}, unusedByGrant(
			"Allow", "AnsweredBy", "Args", "Builtin", "Default", "MCP", "MCPTool", "MCPTools", "MaxCalls", "MaxOutputBytes", "Name", "OptionsRequired", "Required", "Run")),
		grant("mcp", config.ToolSpec{MCP: "github"}, unusedByGrant("Allow", "AnsweredBy", "Builtin", "Default", "Name", "OptionsRequired", "Run")).leaving("Agent"),
		// ask_user is granted so that max_questions:, which is keyed only for an agent that can ask, is probed rather than skipped.
		probeOf("Agent", config.Agent{Name: "reviewer", Source: source, Tools: []config.ToolSpec{{Builtin: "ask_user"}}}, with(agentSilent, map[string]string{
			"Tools[0].Timeout": operational, "Tools[0].Env": toolEnv, "Tools[0].MCPTool": otherForm, "Tools[0].MCPTools": otherForm,
		}), func(a config.Agent) (any, error) { return agentContent(a) }).leaving("Tools[0].Agent", "Tools[0].MCP").choosing(agentEnums),
		probeOf("Agent/cli", config.Agent{Name: "reviewer", Source: config.AgentSource{Model: "@claude/sonnet"}}, func() map[string]string {
			// Everything a hosted agent leaves out, MINUS settings: — the one field that exists only here, and must move the key.
			out := with(agentSilent, map[string]string{"MaxQuestions": "source: AgentContentMap keys max_questions: only for an agent that grants ask_user, and this one grants nothing"})
			delete(out, "Settings")

			return out
		}(), func(a config.Agent) (any, error) { return agentContent(a) }).choosing(agentEnums).leaving("Source.Endpoint", "Tools"),
		probeOf("Agent/as a sub-agent", config.Agent{Name: "extra", Source: source, Description: "a helper"}, with(agentSilent, map[string]string{
			"MaxQuestions": "source: AgentContentMap keys max_questions: only for an agent that grants ask_user, and this child grants nothing",
		}), func(a config.Agent) (any, error) {
			return agentContent(config.Agent{Name: "reviewer", Source: source, Tools: []config.ToolSpec{{Agent: "extra"}}}, a)
		}).choosing(agentEnums).leaving("Tools"),
	}
}

// A field that can change without the key changing is a step that will NOT be re-run when it should be: the cached result is reported green having never seen the new configuration. Every builder's comments worry about exactly this, field by field, and until now nothing held a NEW field to it — 36 mutants that stopped folding a field in all survived this package's tests.
func TestEveryConfigFieldMovesTheKeyOrIsNamedAsNotMeantTo(t *testing.T) {
	t.Parallel()

	for _, probe := range keyProbes() {
		t.Run(probe.name, func(t *testing.T) {
			t.Parallel()

			silent := probe.silent(t)

			for _, field := range silent {
				if _, named := probe.want[field]; !named {
					t.Errorf("%s changes without changing the key: a cached step would not re-run for it. Fold it into the builder, or name it in keyProbes with the reason it is safe", field)
				}
			}

			for field := range probe.want {
				if !slices.Contains(silent, field) {
					t.Errorf("%s is listed as outside the key, but changing it now moves the key — delete the entry", field)
				}
			}
		})
	}
}

// Value-gating is the other half of the promise: a field folds in only when SET, so a pipeline that does not use it hashes byte-identically to before the field existed and keeps its cache. These are those bytes. A boundary slip (> 0 becoming >= 0) folds a zero in and silently busts every cache there is, and no test that only CHANGES a field can see it.
func TestUnsetFieldsLeaveTheKeyExactlyAsItWas(t *testing.T) {
	t.Parallel()

	rtype := config.ResourceType{Name: "git"}
	server := config.MCPServer{Name: "github", Endpoint: "http://localhost:1"}
	cfg := &config.Config{MCPServers: []config.MCPServer{server}}

	limits := map[string]any{}
	withIsolation(false, &config.ContainerLimits{}, limits)

	get, err := GetNodeContent(&config.Config{}, config.Step{Get: "repo"}, rtype, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	put, err := PutNodeContent(&config.Config{}, config.Step{Put: "repo"}, rtype, nil, nil, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	cacheKey, err := ResourceCacheKey(&config.Config{}, rtype, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	mcpServer, err := mcpServerContent(cfg, "github")
	if err != nil {
		t.Fatal(err)
	}

	mcpTool, err := mcpToolSpecContent(cfg, config.ToolSpec{MCP: "github"})
	if err != nil {
		t.Fatal(err)
	}

	for name, pair := range map[string]struct {
		got  any
		want string
	}{
		"container limits": {limits, `{}`},
		"assert":           {assertContent(&config.Assert{ToolCalls: []config.ExpectedToolCall{{Name: "read_file"}}}), `{"tool_calls":[{"name":"read_file"}]}`},
		"get":              {get, `{"in_template":"","source":null,"version":null}`},
		"put":              {put, `{"inputs":[],"out_template":"","params":null,"source":null}`},
		"resource cache":   {cacheKey, `"a5b2e0153d0c681245b09a513ba4b769e0d3b2bf6a51736d5031dc7dc3815d02"`},
		"mcp server":       {mcpServer, `{"auth_type":"","endpoint":"http://localhost:1","name":"github"}`},
		"mcp grant":        {mcpTool, `{"mcp":"github","server":{"auth_type":"","endpoint":"http://localhost:1","name":"github"}}`},
	} {
		raw, err := json.Marshal(pair.got)
		if err != nil {
			t.Fatal(err)
		}

		if string(raw) != pair.want {
			t.Errorf("%s with every optional field unset:\n got %s\nwant %s\nIf this is deliberate it invalidates every cached step of this kind — say so in the commit", name, raw, pair.want)
		}
	}
}

// A sub-agent's content is held by its KEYS rather than its bytes: the values include a provider's default endpoint and turn limit, which are not this test's to pin. The keys are enough — an isolation field folded in while unset shows up as a key that should not be there.
func TestASubAgentWithNoIsolationKeysNoIsolation(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Agents: []config.Agent{{Name: "extra", Source: config.AgentSource{Model: "lmstudio/qwen"}}}}

	content, err := subAgentInvocationContent(cfg, "extra")
	if err != nil {
		t.Fatal(err)
	}

	got := slices.Sorted(maps.Keys(content))
	want := []string{"agent", "endpoint", "max_tokens", "max_turns", "model", "reasoning_effort", "system", "temperature", "tools", "top_p"}

	if !slices.Equal(got, want) {
		t.Errorf("keys = %v\nwant   %v", got, want)
	}
}
