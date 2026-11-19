package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jtarchie/steps/internal/blobstore"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// setup opens the state store and builds/validates the workspace provider
// shared by the commands that run steps, returning a cleanup func that closes both
// (logging, not returning, any close error — mirroring the deferred
// close-error handling the commands used inline before this helper
// existed).
func setup(
	cfg *config.Config, pipelinePath string, flags StateFlags, exec ExecFlags,
) (store.Store, workspace.Provider, func(), error) {
	name := resolvePipelineName(pipelinePath, flags.Name)

	// The identity the caller loaded the Config under has to be the identity
	// the store is opened under, because they are one identity: the store
	// scopes every row by it, the /p/<slug> route is it, and the Config
	// carries it to whatever else is keyed by pipeline (an agent pin). They
	// were computed independently and silently disagreed, which is the whole
	// of #94 — so a caller that resolves it differently, or forgets and takes
	// the file-name default while --name says otherwise, is told rather than
	// left to split.
	if cfg.Name != name {
		return nil, nil, nil, fmt.Errorf(
			"the pipeline was loaded as %q but its state is scoped to %q — these are one identity, so load it with resolvePipelineName(path, flags.Name)",
			cfg.Name, name)
	}

	st, err := openLocalState(pipelinePath, flags.DB, name)
	if err != nil {
		return nil, nil, nil, err
	}

	// This is the funnel every command that RUNS a local pipeline file comes
	// through (run, test), and the only place holding
	// both the YAML path and the handle to record it against.
	err = st.SetSourcePath(context.Background(), pipelinePath)
	if err != nil {
		_ = st.Close()

		return nil, nil, nil, fmt.Errorf("could not record the pipeline's source path: %w", err)
	}

	err = RecordRevision(context.Background(), st, cfg)
	if err != nil {
		_ = st.Close()

		return nil, nil, nil, err
	}

	err = pipeline.AdoptResourceConfigs(context.Background(), st, cfg)
	if err != nil {
		_ = st.Close()

		return nil, nil, nil, fmt.Errorf("pipeline %q: %w", cfg.Name, err)
	}

	provider, err := workspace.NewProvider(exec.workspace(""), exec.KeepWorkspace)
	if err != nil {
		_ = st.Close()

		return nil, nil, nil, fmt.Errorf("could not build workspace provider: %w", err)
	}

	err = readyProvider(provider, workspace.Provider.Validate, st, exec.ArtifactStore)
	if err != nil {
		_ = st.Close()

		return nil, nil, nil, err
	}

	cleanup := func() {
		closeErr := provider.Close()
		if closeErr != nil {
			slog.Error("workspace.close", "error", closeErr)
		}

		closeErr = st.Close()
		if closeErr != nil {
			slog.Error("store.close", "error", closeErr)
		}
	}

	return st, provider, cleanup, nil
}

// readyProvider checks a freshly built provider, indexes its caches in st and attaches --artifact-store to it, closing it on any failure: the provider owns a temp root it created and only its Close removes one, so a caller that closed its store alone left a steps-* directory behind on every attempt, with nothing to reap it.
func readyProvider(provider workspace.Provider, check func(workspace.Provider) error, st store.Blobs, artifactStore string) error {
	err := check(provider)
	if err != nil {
		_ = provider.Close()

		return fmt.Errorf("workspace: %w", err)
	}

	provider.AttachCacheIndex(cacheIndex{st})

	err = attachArtifactStore(provider, artifactStore)
	if err != nil {
		_ = provider.Close()

		return err
	}

	return nil
}

// attachArtifactStore mirrors the provider's step cache to --artifact-store. With no durable --workspace-root there is no step cache to mirror, which is warned about rather than refused, because the flag's other consumer, a placed step's data plane, works without one.
func attachArtifactStore(provider workspace.Provider, raw string) error {
	if raw == "" {
		return nil
	}

	opts, err := blobstore.Parse(raw)
	if err != nil {
		return err //nolint:wrapcheck // blobstore's own errors name the URL and the rule it broke
	}

	blobs, err := blobstore.New(context.Background(), opts)
	if err != nil {
		return err //nolint:wrapcheck // as above
	}

	if !provider.AttachArtifactStore(blobs) {
		slog.Warn("artifact_store.no_step_cache",
			"store", raw,
			"why", "no durable --workspace-root, so cached step outputs are not mirrored; placed steps still use the store as their data plane")
	}

	return nil
}

// cacheIndex is the state database's cache index in workspace's types, which may not name internal/store.
type cacheIndex struct{ st store.Blobs }

func (c cacheIndex) CacheEntry(ctx context.Context, kind, key string) (map[string]workspace.CachedTree, error) {
	trees, err := c.st.CacheEntry(ctx, store.CacheKind(kind), key)
	if err != nil {
		return nil, err //nolint:wrapcheck // the store names what it was reading
	}

	out := make(map[string]workspace.CachedTree, len(trees))
	for name, tree := range trees {
		out[name] = workspace.CachedTree{Name: tree.Name, Digest: tree.Digest, Holder: tree.Holder}
	}

	return out, nil
}

func (c cacheIndex) RecordCacheEntry(ctx context.Context, kind, key string, trees map[string]workspace.CachedTree, keep int) ([]string, error) {
	in := make(map[string]store.CachedTree, len(trees))
	for name, tree := range trees {
		in[name] = store.CachedTree{Name: tree.Name, Digest: tree.Digest, Holder: tree.Holder}
	}

	return c.st.RecordCacheEntry(ctx, store.CacheKind(kind), key, in, keep) //nolint:wrapcheck // the store names what it was writing
}

func (c cacheIndex) TouchCacheEntry(ctx context.Context, kind, key string) error {
	return c.st.TouchCacheEntry(ctx, store.CacheKind(kind), key) //nolint:wrapcheck // the store names what it was writing
}

// RecordRevision writes down WHICH configuration the runs opened against this
// handle will have executed.
//
// Called from the same funnel as SetSourcePath, and for the same reason: it
// is the only place holding both the configuration that was parsed and the
// handle its runs are recorded against. A command that only READS history
// never comes through there and records no revision, which is right — it
// resolved no configuration.
func RecordRevision(ctx context.Context, st store.Revisions, cfg *config.Config) error {
	if !cfg.Revision.Recorded() {
		return nil
	}

	err := st.RecordRevision(ctx, cfg.Revision.SHA, cfg.Revision.Source, cfg.Revision.Includes)
	if err != nil {
		return fmt.Errorf("could not record the pipeline's configuration: %w", err)
	}

	return nil
}
