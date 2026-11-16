package cli

import (
	"fmt"
	"path/filepath"

	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/web"
	"github.com/jtarchie/steps/internal/workspace"
)

// workspaceFor is the provider this set needs, nil when the pipeline already has one: where trees live is the daemon's, so no set moves them, and a re-set changes no build directory underneath a run.
func (d *daemon) workspaceFor(st store.Store, existing *servedPipeline) (workspace.Provider, error) {
	if existing != nil {
		return nil, nil //nolint:nilnil // "no new provider needed" is the answer, not a missing one
	}

	return d.provider(st)
}

// provider is the workspace a pipeline's runs materialize in, refused when this machine cannot supply it.
func (d *daemon) provider(st store.Store) (workspace.Provider, error) {
	settings := d.exec.workspace(daemonWorkspaceRoot(d.state))

	provider, err := workspace.NewProvider(settings, d.exec.KeepWorkspace)
	if err != nil {
		return nil, fmt.Errorf("%w: workspace: %w", web.ErrRefused, err)
	}

	check := func(provider workspace.Provider) error {
		root, swept := d.sweptRoot(settings.Root)
		if swept {
			return provider.Probe()
		}

		provider.Spare(d.spared)

		err := provider.Validate()
		if err == nil && root != "" {
			d.swept[root] = true
		}

		return err //nolint:wrapcheck // readyProvider wraps it
	}

	// readyProvider also attaches --artifact-store, the half the setup() path this replaced did: without it the flag mirrors nothing and warns about nothing, which reads as configured and binds nothing.
	err = readyProvider(provider, check, st, d.exec.ArtifactStore)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", web.ErrRefused, err)
	}

	return provider, nil
}

// sweptRoot names the daemon's root and whether this process swept it already, because the sweep removes EVERY build directory under the root with no ownership check: a crashed process's leftovers the first time, another served pipeline's run in flight any time after. A root it cannot name counts as swept, so it never is.
func (d *daemon) sweptRoot(configured string) (string, bool) {
	if configured == "" {
		return "", false
	}

	// ponytail: one directory reached through two symlinked spellings is two roots here, so the second sweeps; key by filepath.EvalSymlinks if roots are ever shared that way.
	root, err := filepath.Abs(configured)
	if err != nil {
		return "", true
	}

	return root, d.swept[root]
}
