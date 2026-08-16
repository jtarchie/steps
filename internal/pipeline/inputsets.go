package pipeline

// Input sets: which versions one build binds, for every get in the plan.

import (
	"context"
	"fmt"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	rsrc "github.com/jtarchie/steps/internal/resource"
)

// setResolution is everything the planner and the executor need to agree on:
// the sets themselves, which resources fan out, and — when there are no sets
// — why.
type setResolution struct {
	// sets, in build order. One per unconsumed step of the widest every-get;
	// exactly one for a plan with no every-get at all. Empty means nothing
	// to build.
	sets []merkle.InputSet
	// everyResources names the resources that fan out, so the executor knows
	// whose cursor each set advances.
	everyResources []string
	// blocking names every-resources that could bind nothing at all — no
	// unconsumed version, no held version — which is what stops sets being
	// built and what the "no versions" report should say.
	blocking []string
}

// resolveInputSets computes the input sets ONE run will build, Concourse's
// way: each every-get advances one step per set through its unconsumed
// versions, in lockstep with its siblings; a get whose versions run out
// HOLDS — at its last unconsumed from this round, else at the newest version
// its cursor has already covered. Non-every gets bind their single resolved
// version into every set.
//
// The hold rule is what makes the common case right, not just the edge.
// Updates rarely arrive in matched pairs; the steady state is one resource
// moving at a time. b2 arriving alone means a has zero unconsumed and b has
// one — exactly one set, (a@held, b2); a2 arriving later gives (a2, b@held).
// The diagonal (a1,b1),(a2,b2),(a3,b3-held) is the same rule applied when
// several inputs happen to have backlogs at once.
//
// Resolution goes through the SAME cache the planner and executor share, so
// a set is made of exactly the versions either would have resolved alone —
// reimplementing selection here is how hashes would drift.
func resolveInputSets(
	ctx context.Context, cfg *config.Config, plan []config.Step, pinned map[string]string,
	cache *rsrc.Cache, cursor *versionCursor, history *resourceHistory,
) (setResolution, error) {
	inputs, resolution, err := gatherInputs(ctx, cfg, plan, pinned, cache, cursor, history)
	if err != nil {
		return setResolution{}, err
	}

	resolution.sets = assembleSets(inputs, resolution)

	return resolution, nil
}

// setInputs is the raw material sets are assembled from.
type setInputs struct {
	fixed      merkle.InputSet
	unconsumed map[string][]map[string]any
	held       map[string]map[string]any
}

func gatherInputs(
	ctx context.Context, cfg *config.Config, plan []config.Step, pinned map[string]string,
	cache *rsrc.Cache, cursor *versionCursor, history *resourceHistory,
) (setInputs, setResolution, error) {
	inputs := setInputs{
		fixed:      merkle.InputSet{},
		unconsumed: map[string][]map[string]any{},
		held:       map[string]map[string]any{},
	}

	var resolution setResolution

	for i := range plan {
		if plan[i].Get == "" {
			continue
		}

		err := gatherOneInput(ctx, cfg, plan[i], pinned, cache, cursor, history, &inputs, &resolution)
		if err != nil {
			return setInputs{}, setResolution{}, err
		}
	}

	return inputs, resolution, nil
}

func gatherOneInput(
	ctx context.Context, cfg *config.Config, step config.Step, pinned map[string]string,
	cache *rsrc.Cache, cursor *versionCursor, history *resourceHistory,
	inputs *setInputs, resolution *setResolution,
) error {
	res, _, versions, err := cache.ResolveVersionsCached(ctx, cfg, step, pinned)
	if err != nil {
		return err //nolint:wrapcheck // ResolveVersions names the get
	}

	// A CLI pin collapses every-mode to a single named version (see
	// resource.ResolveVersions), so a pinned run builds exactly one set and —
	// via the existing take exemption — consumes nothing.
	mode, _ := rsrc.VersionMode(step)
	if mode != "every" || len(pinned) > 0 {
		inputs.fixed[res.Name] = versions[len(versions)-1]

		return nil
	}

	if _, twice := inputs.unconsumed[res.Name]; twice {
		// Guarded at load once multi-every ships; kept here because a silent
		// second cursor over one resource is the failure mode.
		return fmt.Errorf(
			"get %q: two version: every gets resolve to resource %q, which would share one cursor", step.Get, res.Name)
	}

	resolution.everyResources = append(resolution.everyResources, res.Name)
	inputs.unconsumed[res.Name] = versions

	if len(versions) == 0 {
		hold := cursor.heldVersion(res.Name, history.get(res.Name))
		if hold == nil {
			resolution.blocking = append(resolution.blocking, res.Name)

			return nil
		}

		inputs.held[res.Name] = hold
	}

	return nil
}

// assembleSets pairs the gathered inputs into build order: set i binds each
// every-get's i-th unconsumed version, holding when a shorter input runs out.
func assembleSets(inputs setInputs, resolution setResolution) []merkle.InputSet {
	if len(resolution.everyResources) == 0 {
		return []merkle.InputSet{inputs.fixed}
	}

	setCount := 0
	for _, versions := range inputs.unconsumed {
		setCount = max(setCount, len(versions))
	}

	// Nothing unconsumed anywhere is idle, not an error; an input that can
	// bind nothing at all blocks the fan-out and is named.
	if setCount == 0 || len(resolution.blocking) > 0 {
		return nil
	}

	sets := make([]merkle.InputSet, 0, setCount)
	for i := range setCount {
		sets = append(sets, assembleOneSet(inputs, resolution.everyResources, i))
	}

	return sets
}

// assembleOneSet binds set i: fixed versions everywhere, and per every-get
// its i-th unconsumed version — holding at the last one, or at the held
// fallback, when it runs out.
func assembleOneSet(inputs setInputs, everyResources []string, i int) merkle.InputSet {
	set := merkle.InputSet{}

	for resource, version := range inputs.fixed {
		set[resource] = version
	}

	for _, resource := range everyResources {
		versions := inputs.unconsumed[resource]

		switch {
		case i < len(versions):
			set[resource] = versions[i]
		case len(versions) > 0:
			set[resource] = versions[len(versions)-1]
		default:
			set[resource] = inputs.held[resource]
		}
	}

	return set
}

// heldVersion is where an exhausted every-get holds: the newest candidate its
// cursor has already covered — Concourse's `check_order <=` fallback, which
// degrades gracefully when the exact marked version has been pruned.
//
// candidates is the same list resolution would choose from (green-filtered
// for a passed:-gated resource, so a held version has passed too). The
// cache's filtered entry cannot answer this: it has, by construction,
// removed exactly the versions the fallback needs.
func (c *versionCursor) heldVersion(resourceName string, candidates []map[string]any) map[string]any {
	if c == nil {
		return nil
	}

	mark := c.marks[resourceName]
	orders := c.orders[resourceName]

	var (
		best      map[string]any
		bestOrder int64
	)

	for _, version := range candidates {
		key, ok := encodeVersion(version)
		if !ok {
			continue
		}

		order, known := orders[key]
		if !known || order > mark {
			continue
		}

		if best == nil || order > bestOrder {
			best, bestOrder = version, order
		}
	}

	return best
}

// blockingReport names what stops a fan-out, for the "no versions" message.
func (r setResolution) blockingReport() string {
	return strings.Join(r.blocking, ", ")
}
