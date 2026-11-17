package pipeline

// A get's and a put's facts — Concourse's resource metadata: said on the step,
// kept beside the version, and read back when a fetch is reused rather than
// repeated.

import (
	"context"
	"log/slog"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// sayFacts puts each fact on the step ctx names; published says they are a put's.
func sayFacts(ctx context.Context, facts []events.Fact, published bool) {
	for _, fact := range facts {
		events.FactNote(ctx, fact, published)
	}
}

// factsVersion is the version facts are kept under, or false for one that is not kept (see versionRecordable, whose warning this does not repeat).
func factsVersion(version map[string]any) (store.VersionKey, bool) {
	if len(version) == 0 {
		return "", false
	}

	encoded, err := store.EncodeVersion(version)
	if err != nil || len(encoded) > maxRecordedVersionBytes {
		return "", false
	}

	return encoded, true
}

// keepFacts files facts beside the version they describe. Best-effort, as every other version bookkeeping is: the step did its work.
func keepFacts(ctx context.Context, st store.Versions, resource string, version map[string]any, facts []events.Fact) {
	encoded, ok := factsVersion(version)
	if !ok || len(facts) == 0 {
		return
	}

	err := st.RecordVersionMetadata(context.WithoutCancel(ctx), resource, encoded, events.EncodeFacts(facts))
	if err != nil {
		slog.WarnContext(ctx, "job.metadata_unrecorded", "resource", resource, "error", err)
	}
}

// keptFacts is what an earlier fetch or publish of version said, for a fetch reused rather than repeated.
func keptFacts(ctx context.Context, st store.Versions, resource string, version map[string]any) []events.Fact {
	encoded, ok := factsVersion(version)
	if !ok {
		return nil
	}

	kept, err := st.VersionMetadata(ctx, resource, []store.VersionKey{encoded})
	if err != nil {
		slog.WarnContext(ctx, "job.metadata_unread", "resource", resource, "error", err)

		return nil
	}

	return events.DecodeFacts(kept[encoded])
}
