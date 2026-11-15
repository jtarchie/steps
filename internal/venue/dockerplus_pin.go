package venue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/shell"
)

const (
	// pinLabel names the alias a pin volume keeps from eviction; a volume of its own, since docker's labels on the alias cannot change.
	pinLabel = "steps.pin"
	// runLabel is the run a pin is for, which releases it by this label from any process, a resumed one included.
	runLabel  = "steps.run"
	pinPrefix = "steps-pin-"
)

// ponytail: pinLifetime bounds a pin so a dead run cannot hold the cache over its bound forever; fixed at twice the default approval wait, since a held output can sit through one; a live run re-stamping its pins would let this shrink to minutes.
var pinLifetime = 48 * time.Hour //nolint:gochecknoglobals // a test seam beside cacheBytes and orphanAge

// pin keeps digest's alias from eviction until the run this session is part of releases it or the pin outlives pinLifetime. Made before the alias is looked up or published, so no eviction between the two can take it unpinned.
func (s *plusSession) pin(ctx context.Context, digest string) error {
	labels := shell.OwnershipLabels()
	labels[cacheLabel] = "pin"
	labels[pinLabel] = s.aliasName(digest)
	labels[runLabel] = s.runID

	_, err := s.docker.CreateVolume(ctx, pinPrefix+randomSuffix(), labels, nil)
	if err != nil {
		return fmt.Errorf("pinning: %w", err)
	}

	return nil
}

// pinned are the aliases a live pin names. A pin past pinLifetime is removed instead; a zero CreatedAt is a daemon that did not say, taken as live as reclaim takes it.
func (s *plusSession) pinned(ctx context.Context, volumes []dockerapi.Volume) map[string]bool {
	pinned := map[string]bool{}

	for _, volume := range volumes {
		alias := volume.Labels[pinLabel]

		switch {
		case alias == "":
		case volume.CreatedAt.IsZero() || time.Since(volume.CreatedAt) <= pinLifetime:
			pinned[alias] = true
		default:
			_ = s.docker.RemoveVolume(ctx, volume.Name)
		}
	}

	return pinned
}

// Unpin releases every pin run made on holder, for a run that finished and needs none of what it held there. The caller bounds ctx: this is cleanup, likeliest run as its caller's context ends.
func Unpin(ctx context.Context, holder, runID string) error {
	worker, err := ParseWorker(holder)
	if err != nil {
		return err
	}

	if !worker.dockerPlus() || runID == "" {
		return nil
	}

	s := newPlusSession(worker, shell.RunnerSpec{})
	//nolint:contextcheck // close runs under its own bound
	defer func() { _ = s.close() }()

	_, err = s.dialDaemon(ctx)
	if err != nil {
		return fmt.Errorf("worker %s: %w", holderAddress(holder), err)
	}

	pins, err := s.docker.ListVolumes(ctx, map[string]string{"steps.owner": "steps", cacheLabel: "pin", runLabel: runID})
	if err != nil {
		return fmt.Errorf("worker %s: %w", holderAddress(holder), err)
	}

	errs := make([]error, 0, len(pins))
	for _, pin := range pins {
		errs = append(errs, s.docker.RemoveVolume(ctx, pin.Name))
	}

	err = errors.Join(errs...)
	if err != nil {
		return fmt.Errorf("worker %s: unpinning: %w", holderAddress(holder), err)
	}

	return nil
}
