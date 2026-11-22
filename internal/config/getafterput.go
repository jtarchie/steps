package config

// A get placed after a put to the same resource: it fetches the version that
// put recorded in this build, by running the type's in: with it, and no check
// runs for it. Concourse fetches after every put on its own (the implicit get);
// steps keeps that out and binds only the get a plan writes.

import (
	"fmt"
	"slices"
	"strings"
)

// PutBinding is a get that fetches what an earlier put in its own plan
// recorded, rather than a version a check found.
type PutBinding struct {
	// Get is the get's name, which is its artifact.
	Get      string
	Resource string
	// Put is the name of the put it follows.
	Put string
	// PutStep is the plan step that runs that put: the put itself, or the
	// block holding it.
	PutStep int
}

// PutBindings is every plan get that follows a put to its resource, by get
// name. A get inside a block is left out: only a plan step fetches.
func (j Job) PutBindings() map[string]PutBinding {
	at, _ := j.putBindings()
	bindings := map[string]PutBinding{}

	for i := range j.Plan {
		if binding, ok := at[&j.Plan[i]]; ok {
			bindings[binding.Get] = binding
		}
	}

	return bindings
}

// planPosition is one step of a plan, hooks left out, in the order the plan
// reaches it.
type planPosition struct {
	step  *Step
	label string
	top   int
	// within is every step holding this one, outermost first, and the step
	// itself last.
	within []*Step
}

// planPositions walks a plan the way walkStepTree does, without the hooks: a
// hook only reacts to the step it hangs on, so a put there is never the put a
// get follows.
func planPositions(j Job) []planPosition {
	var positions []planPosition

	var walk func(label string, step *Step, top int, within []*Step)

	walk = func(label string, step *Step, top int, within []*Step) {
		here := append(slices.Clone(within), step)
		positions = append(positions, planPosition{step: step, label: label, top: top, within: here})

		for _, spec := range stepKinds {
			if spec.children == nil {
				continue
			}

			for i, child := range spec.children(step) {
				walk(spec.childLabel(label, i), child, top, here)
			}
		}
	}

	for i := range j.Plan {
		walk(j.stepLabel(i), &j.Plan[i], i, nil)
	}

	return positions
}

// sharedBlock is the innermost step holding both a and b, nil when only the
// plan does.
func sharedBlock(a, b planPosition) *Step {
	var shared *Step

	for i := 0; i < len(a.within) && i < len(b.within) && a.within[i] == b.within[i]; i++ {
		shared = a.within[i]
	}

	return shared
}

// concurrentKind names the block when it runs what it holds at once, "" when
// it runs them one after another.
func concurrentKind(block *Step) string {
	if block == nil {
		return ""
	}

	if len(block.Across) > 0 {
		return "across"
	}

	kind, _ := branchesOf(block)

	return string(kind)
}

// putBindings finds the put each get follows, keyed by the get, and the first
// reason a get cannot follow its put. A get with no put to its resource before
// it is not bound and resolves through its check as before.
func (j Job) putBindings() (map[*Step]PutBinding, error) {
	positions := planPositions(j)
	bindings := map[*Step]PutBinding{}

	var first error

	for g, get := range positions {
		if get.step.Get == "" {
			continue
		}

		binding, bound, err := bindGet(positions, g)
		if err != nil && first == nil {
			first = err
		}

		if bound {
			bindings[get.step] = binding
		}
	}

	return bindings, first
}

// bindGet finds the put positions[g] follows: the nearest put to the same
// resource before it, which must finish before the get starts.
func bindGet(positions []planPosition, g int) (PutBinding, bool, error) {
	get := positions[g]
	resource := get.step.GetResourceName()

	var puts []int

	for p, put := range positions {
		if put.step.Put == "" || put.step.PutResourceName() != resource {
			continue
		}

		if kind := concurrentKind(sharedBlock(put, get)); kind != "" {
			return PutBinding{}, false, fmt.Errorf(
				"%s: get %q runs beside put %q in the same %s, so it cannot fetch what that put records; move the get after the %s block",
				get.label, get.step.Get, put.step.Put, kind, kind)
		}

		if p < g {
			puts = append(puts, p)
		}
	}

	if len(puts) == 0 {
		return PutBinding{}, false, nil
	}

	nearest := positions[puts[len(puts)-1]]
	binding := PutBinding{Get: get.step.Get, Resource: resource, Put: nearest.step.Put, PutStep: nearest.top}

	return binding, true, refuseUnboundPut(positions, puts, get, nearest)
}

// refuseUnboundPut refuses a put that does not leave exactly one version for
// the get after it: one run per across: cell, one a race: may stop, or one
// beside another put to the same resource.
func refuseUnboundPut(positions []planPosition, puts []int, get, nearest planPosition) error {
	shared := sharedBlock(nearest, get)
	below := nearest.within

	if i := slices.Index(below, shared); i >= 0 {
		below = below[i+1:]
	}

	for _, block := range below {
		if len(block.Across) > 0 {
			return fmt.Errorf(
				"%s: get %q follows put %q, which runs once per across: cell and records a version in each; the get cannot tell which to fetch, so put once outside the across:, or fetch in a step inside it",
				get.label, get.step.Get, nearest.step.Put)
		}

		if block.Race != nil {
			return fmt.Errorf(
				"%s: get %q follows put %q inside a race:, which stops the branches that lose, so the put may never record a version; move the put out of the race block",
				get.label, get.step.Get, nearest.step.Put)
		}
	}

	for _, p := range puts {
		other := positions[p]
		if other.step == nearest.step {
			continue
		}

		if kind := concurrentKind(sharedBlock(other, nearest)); kind != "" {
			return fmt.Errorf(
				"%s: get %q follows two puts to %q that run at once in the same %s; it cannot tell whose version to fetch",
				get.label, get.step.Get, get.step.GetResourceName(), kind)
		}
	}

	return nil
}

// validateGetsAfterPuts refuses a get that cannot fetch what the put before it
// records: one that runs beside the put, one whose put leaves no single
// version, one that also asks for a version some other way, one whose type
// cannot fetch, and one a to: route can reach without the put.
func (c *Config) validateGetsAfterPuts() error {
	for _, job := range c.Jobs {
		bindings, err := job.putBindings()
		if err != nil {
			return err
		}

		err = c.validateBoundGets(job, bindings)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Config) validateBoundGets(job Job, bindings map[*Step]PutBinding) error {
	for _, position := range planPositions(job) {
		binding, bound := bindings[position.step]
		if !bound {
			continue
		}

		err := c.validateBoundGet(job, position, binding)
		if err != nil {
			return err
		}
	}

	return refuseRoutesPastPuts(job, bindings)
}

func (c *Config) validateBoundGet(job Job, position planPosition, binding PutBinding) error {
	step := position.step
	follows := fmt.Sprintf("%s: get %q follows put %q, so it fetches the version that put records", position.label, step.Get, binding.Put)

	for _, field := range []struct {
		set  bool
		name string
	}{
		{step.Trigger, "trigger"},
		{step.Version != nil, "version"},
		{len(step.Passed) > 0, "passed"},
	} {
		if field.set {
			return fmt.Errorf("%s; %s: asks for a version some other way, so drop it", follows, field.name)
		}
	}

	if others := job.getsNamed(step.Get); len(others) > 1 {
		return fmt.Errorf("%s, and another get in this job is also named %q; name one differently, as get: <name> with resource: %s",
			follows, step.Get, binding.Resource)
	}

	resource, err := c.FindResource(binding.Resource)
	if err != nil {
		return nil //nolint:nilerr // an unresolvable resource is validateStepReferences' to report
	}

	resourceType, err := c.FindResourceType(resource.Type)
	if err != nil {
		return nil //nolint:nilerr // an unresolvable type is validateStepReferences' to report
	}

	if !resourceType.Fetches() {
		return fmt.Errorf("%s, but resource type %q has no in: to fetch it with; drop the get, or read what the put reported in its log",
			follows, resourceType.Name)
	}

	return nil
}

// getsNamed is every get in the job, hooks included, called name.
func (j Job) getsNamed(name string) []Step {
	var gets []Step

	_ = j.visitSteps(func(_ string, step *Step) error {
		if step.Get == name {
			gets = append(gets, *step)
		}

		return nil
	})

	return gets
}

// refuseRoutesPastPuts refuses a to: route that jumps from before a get's put
// to after it: the get would then run on a path the put never took.
func refuseRoutesPastPuts(job Job, bindings map[*Step]PutBinding) error {
	for g := range job.Plan {
		binding, bound := bindings[&job.Plan[g]]
		if !bound {
			continue
		}

		start := binding.PutStep
		for start > 0 && job.Plan[start-1].Get == "" {
			start--
		}

		for s := start; s < binding.PutStep; s++ {
			for _, entry := range job.Plan[s].RouteEntries() {
				for t := binding.PutStep + 1; t < g; t++ {
					if stepName(job.Plan[t]) == entry.Target {
						return fmt.Errorf("%s: %s routes to %q, past put %q, so get %q could run without the version that put records",
							job.stepLabel(s), entry.Name, entry.Target, binding.Put, binding.Get)
					}
				}
			}
		}
	}

	return nil
}

// Fetches reports whether the type has an in: to fetch a given version with,
// which a get after a put needs where an ordinary get needs a check as well.
func (rt ResourceType) Fetches() bool {
	switch rt.Config.Backend() {
	case BackendMCP:
		return rt.Config.MCP.In != nil
	case BackendExpr:
		return strings.TrimSpace(rt.Config.Expr.In) != ""
	case BackendShell, BackendConcourse:
		return strings.TrimSpace(rt.Config.In) != ""
	case BackendGitHub:
		return GitHubFetches(rt.Config.GitHub)
	case BackendLinear:
		return LinearFetches(rt.Config.Linear)
	case BackendWebhook, BackendCron, BackendRegistry, BackendService:
	}

	return true
}
