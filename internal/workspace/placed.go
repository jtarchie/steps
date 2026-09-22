package workspace

// A step space for a step that runs on a worker, with a store to carry trees
// between workers (steps#138, rung 3).
//
// An input another worker holds is left where it is: the space names it and
// its holder, and the venue offers it to the step's worker by digest, served
// from the store. Only an input whose declared name IS its artifact name can
// travel that way — the digest names the tree under the name it was filed
// as, and a renamed input would hash differently — so a mapped input is
// pulled here and materialized under its declared name as always.

import (
	"context"
)

// PlacedSpaces is the optional BuildWorkspace capability behind a placed
// step's space when a store is configured.
type PlacedSpaces interface {
	// PlacedTaskSpace is TaskSpace leaving remote inputs on their holders,
	// reported by declared name.
	PlacedTaskSpace(ctx context.Context, label string, inputs, outputs []string, inputMapping, outputMapping map[string]string) (StepSpace, map[string]RemoteArtifact, error)
	// PlacedPutSpace is PutSpace, the same way.
	PlacedPutSpace(ctx context.Context, label string, inputs []string, all bool) (StepSpace, map[string]RemoteArtifact, error)
}

// PlacedTaskSpace implements PlacedSpaces.
func (b *isolatingBuild) PlacedTaskSpace(ctx context.Context, label string, inputs, outputs []string, inputMapping, outputMapping map[string]string) (StepSpace, map[string]RemoteArtifact, error) {
	remote := b.leaveRemote(inputs, inputMapping)

	space, err := b.newSpaceLeaving(ctx, label, inputs, outputs, inputMapping, outputMapping, remote)
	if err != nil {
		return nil, nil, err
	}

	return space, remote, nil
}

// PlacedPutSpace implements PlacedSpaces.
func (b *isolatingBuild) PlacedPutSpace(ctx context.Context, label string, inputs []string, all bool) (StepSpace, map[string]RemoteArtifact, error) {
	if all {
		names, err := b.allArtifacts()
		if err != nil {
			return nil, nil, err
		}

		inputs = names
	}

	remote := b.leaveRemote(inputs, nil)

	space, err := b.newSpaceLeaving(ctx, label, inputs, nil, nil, nil, remote)
	if err != nil {
		return nil, nil, err
	}

	return space, remote, nil
}

// leaveRemote picks the inputs that can stay on their holders: held
// elsewhere, and declared under the artifact's own name.
func (b *isolatingBuild) leaveRemote(inputs []string, inputMapping map[string]string) map[string]RemoteArtifact {
	remote := map[string]RemoteArtifact{}

	for _, in := range inputs {
		if mappedName(in, inputMapping) != in {
			continue
		}

		if held, ok := b.remoteArtifact(in); ok {
			remote[in] = held
		}
	}

	return remote
}
