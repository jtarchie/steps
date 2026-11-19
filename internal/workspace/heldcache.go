package workspace

// Cache entries for trees docker holds: what the step and resource caches file for a container step's outputs or an image: get's tree, so the bytes stay in docker and nothing crosses to this disk to be filed.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// Holds is how this package reaches a tree docker holds, injected because it does not know how a worker is dialled.
type Holds struct {
	// Pull makes the RemoteArtifact.Pull for a tree holder files under digest.
	Pull func(holder, digest string) func(context.Context, string) error
	// Claim keeps holder's tree under digest for the run under ctx and fails when holder no longer has it; what a cache hit on a held tree checks before handing it on.
	Claim func(ctx context.Context, holder, digest string) error
}

type holdsKey struct{}

// WithHolds lets the caches under ctx file a held tree as a pointer rather than bringing it home. Without it they bring it home, as a cache with no way to reach docker must.
func WithHolds(ctx context.Context, holds Holds) context.Context {
	return context.WithValue(ctx, holdsKey{}, holds)
}

func holdsFrom(ctx context.Context) (Holds, bool) {
	holds, ok := ctx.Value(holdsKey{}).(Holds)

	return holds, ok && holds.Pull != nil && holds.Claim != nil
}

var errNoHolds = errors.New("a held tree in the cache, and no way to reach docker")

// claimAll asks every holder to keep its tree for this run; any one gone makes the entry a miss, since a partial restore would hand a step an input nobody has.
func claimAll(ctx context.Context, held map[string]heldRecord) error {
	held = remoteOnly(held)
	if len(held) == 0 {
		return nil
	}

	holds, ok := holdsFrom(ctx)
	if !ok {
		return errNoHolds
	}

	for name, record := range held {
		if record.Digest == "" {
			return fmt.Errorf("held output %q: an incomplete record", name)
		}

		err := holds.Claim(ctx, record.Holder, record.Digest)
		if err != nil {
			return fmt.Errorf("held output %q on %s: %w", name, record.Holder, err)
		}
	}

	return nil
}

// claimEntry is an entry's trees, each docker holds claimed for this run; an error is a miss.
func claimEntry(ctx context.Context, index CacheIndex, kind, key string) (map[string]heldRecord, error) {
	held, err := index.CacheEntry(ctx, kind, key)
	if err != nil {
		return nil, fmt.Errorf("reading the cache index: %w", err)
	}

	return held, claimAll(ctx, held)
}

// remoteOnly are the records a docker holds; one with no holder names a tree in this machine's store.
func remoteOnly(records map[string]heldRecord) map[string]heldRecord {
	remote := map[string]heldRecord{}

	for name, record := range records {
		if record.Holder != "" {
			remote[name] = record
		}
	}

	return remote
}

// unheld are req's outputs an entry keeps as bytes.
func unheld(req StepCacheRequest, held map[string]heldRecord) []string {
	local := make([]string, 0, len(req.Outputs))

	for _, out := range req.Outputs {
		if _, ok := held[out]; !ok {
			local = append(local, out)
		}
	}

	return local
}

// holdRestored hands each claimed tree on as the artifact it was filed for, as the step that made it did.
func (b *isolatingBuild) holdRestored(ctx context.Context, held map[string]heldRecord, mapping map[string]string) error {
	holds, _ := holdsFrom(ctx)

	for out, record := range held {
		err := b.HoldRemote(mappedName(out, mapping), RemoteArtifact{Name: record.Name, Digest: record.Digest, Holder: record.Holder, Pull: holds.Pull(record.Holder, record.Digest)})
		if err != nil {
			return err
		}
	}

	return nil
}

// heldOutputs is which of req's outputs docker holds, when this cache may file them as pointers: there is a way to reach docker, and no artifact store, whose off-machine mirror needs the bytes here.
func (b *isolatingBuild) heldOutputs(ctx context.Context, req StepCacheRequest) map[string]heldRecord {
	_, ok := holdsFrom(ctx)
	if !ok || b.stepCache.blobs != nil {
		return nil
	}

	held := map[string]heldRecord{}

	for _, out := range req.Outputs {
		remote, kept := b.remoteArtifact(mappedName(out, req.OutputMapping))
		if kept {
			held[out] = heldRecord{Name: remote.Name, Digest: remote.Digest, Holder: remote.Holder}
		}
	}

	return held
}

// fetchHeld answers a resource cache entry docker holds: the tree is claimed and held under name, nothing fetched. False is a miss of any kind, logged when it was a failure.
func (b *isolatingBuild) fetchHeld(ctx context.Context, key, name string) bool {
	held, err := b.cache.index.CacheEntry(ctx, kindResource, key)
	if err == nil && held[resourceHeldKey].Holder == "" {
		return false
	}

	if err == nil {
		err = claimAll(ctx, held)
	}

	if err == nil {
		err = b.holdRestored(ctx, map[string]heldRecord{name: held[resourceHeldKey]}, nil)
	}

	if err != nil {
		slog.WarnContext(ctx, "workspace.cache_held_miss", "key", key, "error", err)

		return false
	}

	b.cache.touch(ctx, key)

	slog.DebugContext(ctx, "workspace.cache_hit", "key", key, "held", true)

	return true
}

// resourceHeldKey is the one record a resource entry holds: a get fetches one tree.
const resourceHeldKey = "tree"

var errFiledHeld = errors.New("filed as held")

// storeHeld files a resource entry as a pointer at the tree docker holds for name.
func (c *resourceCache) storeHeld(ctx context.Context, key string, remote RemoteArtifact) {
	c.point(ctx, key, heldRecord{Name: remote.Name, Digest: remote.Digest, Holder: remote.Holder})
}
