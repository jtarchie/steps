package venue

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/dockerapi"
)

//nolint:gochecknoglobals // test seams for a bound on another machine's disk
var (
	// cacheBytes bounds what a docker+ worker keeps, by size not age: the shim's artifact cache bound, carried over.
	cacheBytes int64 = 8 << 30
	// orphanAge is how long a volume nothing names may sit before it is taken for a crashed session's: long past any session's first mount.
	orphanAge = time.Hour
	// evictScope selects the volumes eviction may touch.
	evictScope = map[string]string{"steps.owner": "steps"}
)

// sessionPrefixes are the volumes a session or the cache makes; eviction never touches another name.
var sessionPrefixes = []string{"steps-d-", "steps-out-", "steps-work-", "steps-in-", "steps-upper-", "steps-ovl-"} //nolint:gochecknoglobals // a fact about the names this file mints

// evict keeps the cache under its bound and reclaims what crashed sessions left. Best effort: a worker that cannot be tidied still runs the step, and the next session tries again.
//
// ponytail: oldest-created first, not least-recently-used; a hit cannot restamp an immutable label.
// ponytail: an entry found by a concurrent session between its lookup and its overlay's creation can still be evicted under it, which fails that mount by name.
func (s *plusSession) evict(ctx context.Context) {
	volumes, err := s.docker.ListVolumes(ctx, evictScope)
	if err != nil {
		slog.WarnContext(ctx, "venue.docker.evict_failed", "worker", s.worker.String(), "error", err)

		return
	}

	named, layered := map[string]bool{}, map[string]bool{}

	var aliases []dockerapi.Volume

	for _, volume := range volumes {
		if volume.Labels[cacheLabel] == "alias" {
			aliases = append(aliases, volume)
			named[volume.Labels[cacheData]] = true
		}

		if lower := volume.Labels[lowerLabel]; lower != "" {
			layered[lower] = true
		}
	}

	for _, volume := range volumes {
		if isSessionVolume(volume.Name) && !named[volume.Name] && !layered[volume.Name] && time.Since(volume.CreatedAt) > orphanAge {
			// Refused while mounted, which is the protection a live session needs.
			_ = s.docker.RemoveVolume(ctx, volume.Name)
		}
	}

	s.trim(ctx, aliases, layered)
}

func (s *plusSession) trim(ctx context.Context, aliases []dockerapi.Volume, layered map[string]bool) {
	var total int64
	for _, alias := range aliases {
		total += sizeOf(alias)
	}

	slices.SortFunc(aliases, func(a, b dockerapi.Volume) int { return a.CreatedAt.Compare(b.CreatedAt) })

	for _, alias := range aliases {
		if total <= cacheBytes {
			return
		}

		data := alias.Labels[cacheData]
		if layered[data] {
			continue
		}

		// The data first: refused while something mounts it, and then the alias must stay too.
		if s.docker.RemoveVolume(ctx, data) != nil {
			continue
		}

		_ = s.docker.RemoveVolume(ctx, alias.Name)
		total -= sizeOf(alias)
	}
}

func sizeOf(alias dockerapi.Volume) int64 {
	size, _ := strconv.ParseInt(alias.Labels[cacheSize], 10, 64)

	return size
}

func isSessionVolume(name string) bool {
	return slices.ContainsFunc(sessionPrefixes, func(prefix string) bool { return strings.HasPrefix(name, prefix) })
}
