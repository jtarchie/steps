package cli

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	stepsmcp "github.com/jtarchie/steps/internal/mcp"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/web"
	"github.com/jtarchie/steps/internal/workspace"
)

// The handle is rebuilt rather than relabelled: the route, the pipelines row and the scope an agent pin is keyed by are ONE string, so moving only the row leaves two of the three answering to a name nothing else uses. Rebuilding cancels whatever the drain is running, so a rename is refused while a build of the pipeline runs; queued builds are rows keyed by the pipeline's id, and run under the new name.
//
//nolint:contextcheck // as restore: the validation reads the daemon's context on purpose, and opening a store takes none
func (d *daemon) Rename(ctx context.Context, from, to string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	served, err := d.renamable(from, to)
	if err != nil {
		return err
	}

	// Where it was set from travels with the identity: a rename moves the name, not the file somebody uploaded.
	setFrom := served.target.Path()
	current := served.target.Config().Revision

	// Everything that can refuse runs while the old name is still served: refused after letting go, a rename left the pipeline served under NEITHER name while the database, and so the next restart, named the new one.
	cfg, err := d.accept(to, current.Source, current.Includes)
	if err != nil {
		return fmt.Errorf("could not rename %q to %q: %w", from, to, err)
	}

	// Merging would hand this pipeline somebody else's accounts unnoticed, and replacing would destroy them.
	if dir, found := stepsmcp.LoginsExist(cfg.Name); found {
		return fmt.Errorf("%w: mcp logins for %q already exist at %s; remove that directory to rename onto this name", web.ErrRefused, to, dir)
	}

	// The last thing before anything durable, so a build the drain claimed while the checks above ran is still seen.
	// ponytail: a build the drain claims between this check and detach is still cancelled; upgrade: stop the pipeline's drain claiming while the rename holds d.mu.
	err = d.refuseWhileRunning(from)
	if err != nil {
		return err
	}

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), daemonWriteBound)
	defer cancel()

	// The UPDATE first, and every scoped row reaches the pipeline by id so the loops go on working under the old name: the served map is not the whole file, and a name a `steps run` row already holds used to fail the UNIQUE constraint AFTER the source pipeline had been torn down and its handle closed — lost until a restart.
	err = served.target.Store.Rename(writeCtx, to)
	if err != nil {
		return fmt.Errorf("%w: could not rename %q to %q: %w", web.ErrRefused, from, to, err)
	}

	st, provider, err := d.open(to, cfg)
	if err != nil {
		return d.undoRename(ctx, served, from, to, err)
	}

	// Held since the lookup above, under the lock this still holds, so it cannot miss.
	_, _ = d.detach(from)

	served.closer(false)
	d.runner.RemoveProvider(from)

	// A rename keeps history, so it keeps the logins. Started regardless of the move: the database already names the new one.
	// ponytail: a crash between the Store.Rename above and this move leaves the logins under the old name. Upgrade: sweep for login directories no pipeline row names on restart.
	oldLogins := served.target.Config().Name
	d.forget(oldLogins)
	d.dropProbes(from)
	moveErr := stepsmcp.MoveLogins(oldLogins, cfg.Name)

	d.start(to, cfg, st, provider, setFrom)

	if moveErr != nil {
		stay, _ := stepsmcp.LoginsExist(oldLogins)
		dest, _ := stepsmcp.LoginsExist(cfg.Name)

		return fmt.Errorf("renamed %q to %q, but its mcp logins stay at %s: %w; move them to %s or log in again", from, to, stay, moveErr, dest)
	}

	slog.InfoContext(ctx, "web.pipeline.renamed", "from", from, "to", to)

	return nil
}

// renamable is the served pipeline a rename may move. A broken pipeline is refused either way round: onto one the rename hits the name's UNIQUE row, and from one there is no served pipeline to move.
func (d *daemon) renamable(from, to string) (*servedPipeline, error) {
	if _, taken := d.served[to]; taken {
		return nil, fmt.Errorf("%w: this daemon already serves a pipeline called %q", web.ErrRefused, to)
	}

	if _, broken := d.server.BrokenReason(to); broken {
		return nil, fmt.Errorf("%w: a pipeline this daemon could not restore is already called %q; destroy or repair it first", web.ErrRefused, to)
	}

	if reason, broken := d.server.BrokenReason(from); broken {
		return nil, fmt.Errorf("%w: %q is not being served (%s); repair it with steps pipeline set first", web.ErrRefused, from, reason)
	}

	served, held := d.served[from]
	if !held || served == nil {
		return nil, fmt.Errorf("%w: %s", web.ErrNoSuchPipeline, from)
	}

	return served, nil
}

func (d *daemon) refuseWhileRunning(name string) error {
	builds := d.runner.Running(name)
	if len(builds) == 0 {
		return nil
	}

	listed := make([]string, 0, runningListed)
	for _, build := range builds[:min(len(builds), runningListed)] {
		listed = append(listed, build.JobName+"/"+build.RunID)
	}

	more := ""
	if len(builds) > runningListed {
		more = fmt.Sprintf(" (and %d more)", len(builds)-runningListed)
	}

	return fmt.Errorf("%w: %q has builds running: %s%s; wait for them to finish, or stop them with: steps runs abort -p %s <run-id>",
		web.ErrRefused, name, strings.Join(listed, ", "), more, name)
}

// Split from start because all of it can still refuse, which a rename must hear while the old name is still served.
func (d *daemon) open(name string, cfg *config.Config) (store.Store, workspace.Provider, error) {
	st, err := openState(d.state, name)
	if err != nil {
		return nil, nil, fmt.Errorf("could not open state for %q: %w", name, err)
	}

	provider, err := d.provider(cfg, st)
	if err != nil {
		release(st)

		return nil, nil, err
	}

	return st, provider, nil
}

// undoRename puts the row back under the name still being served, whose loops never stopped, so a refused rename is one that did not happen.
func (d *daemon) undoRename(ctx context.Context, served *servedPipeline, from, to string, cause error) error {
	undoCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), daemonWriteBound)
	defer cancel()

	err := served.target.Store.Rename(undoCtx, from)
	if err != nil {
		return fmt.Errorf("could not rename %q to %q: %w; nor could the rename be undone, so a restart serves it as %q: %w", from, to, cause, to, err)
	}

	return fmt.Errorf("could not rename %q to %q: %w", from, to, cause)
}
