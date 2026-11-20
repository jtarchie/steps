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
//
// durable gives an unset --workspace-root the default steps web has, workspace/ beside the state database, so the step and resource caches outlive the run; without it the root is a temp directory removed at exit.
func setup(
	cfg *config.Config, pipelinePath string, flags StateFlags, exec ExecFlags, durable bool,
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

	err = pipeline.Adopt(context.Background(), st, cfg)
	if err != nil {
		_ = st.Close()

		return nil, nil, nil, fmt.Errorf("pipeline %q: %w", cfg.Name, err)
	}

	provider, err := localProvider(st, pipelinePath, flags, exec, durable)
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

// localProvider is setup's workspace provider, ready to build.
func localProvider(st store.Store, pipelinePath string, flags StateFlags, exec ExecFlags, durable bool) (workspace.Provider, error) {
	root, check := "", workspace.Provider.Validate
	if durable && exec.WorkspaceRoot == "" {
		// Probe, not Validate: the sweep takes every build tree under the root, and this one holds the trees failed runs keep for --resume, of this pipeline and of any other whose state sits beside it.
		root, check = daemonWorkspaceRoot(StatePath(pipelinePath, flags.DB)), workspace.Provider.Probe
	}

	provider, err := workspace.NewProvider(exec.workspace(root), exec.KeepWorkspace)
	if err != nil {
		return nil, fmt.Errorf("could not build workspace provider: %w", err)
	}

	provider.Spare(resumableTrees(st))

	err = readyProvider(provider, check, st, exec.ArtifactStore)
	if err != nil {
		return nil, err
	}

	return provider, nil
}

// resumableTrees are the build trees this pipeline's failed and interrupted runs kept for --resume, which the stale-build sweep must leave.
func resumableTrees(st store.Runs) []string {
	runs, err := st.ListRuns(context.Background(), "", 0)
	if err != nil {
		// ponytail: the sweep then may take a tree a later --resume needs; that resume refuses, naming the missing workspace.
		slog.Warn("workspace.resumable_unread", "error", err)

		return nil
	}

	var trees []string

	for _, run := range runs {
		if run.Status == store.RunFailed || run.Status == store.RunRunning {
			trees = append(trees, run.Workspace)
		}
	}

	return trees
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

// attachArtifactStore mirrors the provider's step cache to --artifact-store. With no durable --workspace-root there is no step cache to mirror, which is warned about rather than refused.
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
			"why", "no durable --workspace-root, so cached step outputs are not mirrored")
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
