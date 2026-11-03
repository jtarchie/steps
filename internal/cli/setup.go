package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jtarchie/steps/internal/blobstore"
	"github.com/jtarchie/steps/internal/config"
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
	// Before anything is opened: the refusal costs nothing.
	err := refuseSecretsLocally(cfg)
	if err != nil {
		return nil, nil, nil, err
	}

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

	provider, err := workspace.NewProvider(cfg.Workspace, exec.KeepWorkspace)
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

// readyProvider checks a freshly built provider and attaches --artifact-store
// to it, closing it on any failure: the provider owns a temp root it created
// and only its Close removes one, so a caller that closed its store alone left
// a steps-* directory behind on every attempt, with nothing to reap it.
func readyProvider(provider workspace.Provider, check func(workspace.Provider) error, st store.Blobs, artifactStore string) error {
	err := check(provider)
	if err != nil {
		_ = provider.Close()

		return fmt.Errorf("workspace: %w", err)
	}

	err = attachArtifactStore(provider, st, artifactStore)
	if err != nil {
		_ = provider.Close()

		return err
	}

	return nil
}

// attachArtifactStore wires --artifact-store into the provider's step cache:
// the blob half from the URL, the index half from the state store the digests
// are truth in. A pipeline with no durable workspace.root: has no step cache
// to mirror — that half is warned about rather than refused, because the
// flag's other consumer, a placed step's data plane, works without one.
func attachArtifactStore(provider workspace.Provider, st store.Blobs, raw string) error {
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

	if !provider.AttachArtifactStore(blobs, st) {
		slog.Warn("artifact_store.no_step_cache",
			"store", raw,
			"why", "no durable workspace.root:, so cached step outputs are not mirrored; placed steps still use the store as their data plane")
	}

	return nil
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
