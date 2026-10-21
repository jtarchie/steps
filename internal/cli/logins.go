package cli

// Where mcp oauth logins live: sealed into the state database, under STEPS_ENCRYPTION_KEY, and nowhere without one.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sync/atomic"

	stepsmcp "github.com/jtarchie/steps/internal/mcp"
	"github.com/jtarchie/steps/internal/sealbox"
	"github.com/jtarchie/steps/internal/store"
)

// sealedLogins is the mcp.Vault every command that dials an oauth server installs.
type sealedLogins struct {
	state State
	box   *sealbox.Box
	// register is a local command's: its file IS the pipeline, so a login done before the first run creates the row. A daemon saves only for a pipeline it holds, or a refresh landing after a destroy would leave a live token under a name the next set inherits.
	register bool
}

// loginSaves counts this process's saves of any login, which is every save a daemon's database sees (one process per state database), so a cached answer about a login is stale exactly when it moved.
//
//nolint:gochecknoglobals // one counter for the one vault a process installs
var loginSaves atomic.Int64

// useLogins installs the vault over state for the rest of the process. A malformed key fails now, not at the first login.
func useLogins(state State, register bool) (*sealedLogins, error) {
	box, err := sealbox.FromEnv(os.Getenv)
	if err != nil {
		return nil, err //nolint:wrapcheck // it names the variable, which is the whole fix
	}

	logins := &sealedLogins{state: state, box: box, register: register}
	stepsmcp.UseVault(logins)

	return logins, nil
}

// openLocalState opens a local command's state and installs the vault its logins are sealed into, which is the same database.
func openLocalState(pipelinePath string, db DB, name string) (store.Store, error) {
	state := StatePath(pipelinePath, db)

	st, err := openState(state, name)
	if err != nil {
		return nil, fmt.Errorf("could not open state store: %w", err)
	}

	_, err = useLogins(state, true)
	if err != nil {
		_ = st.Close()

		return nil, err
	}

	return st, nil
}

// loginsReady is whether a login finished now could be saved, asked of the environment the daemon was started with.
func loginsReady() error {
	box, err := sealbox.FromEnv(os.Getenv)
	if err != nil {
		return err //nolint:wrapcheck // it names the variable, which is the whole fix
	}

	return box.Ready() //nolint:wrapcheck // as above
}

// unopenable is a sealed login no key here opens, in sealbox's words but classed as stepsmcp.ErrNeedsLogin: no poll heals it, so preflight must neither wait on it as transient nor cache it past the login that fixes it.
type unopenable struct{ error }

func (u unopenable) Unwrap() []error { return []error{u.error, stepsmcp.ErrNeedsLogin} }

func credentialName(server string) string { return "mcp:" + server }

func credentialAAD(st store.Credentials, name string) []byte {
	return []byte(st.CredentialScope() + "|" + name)
}

// noSuchState is every way a database can lack the pipeline, which for a login is simply none saved.
func noSuchState(err error) bool {
	return errors.Is(err, store.ErrNoSuchPipeline) || errors.Is(err, store.ErrNoState) || errors.Is(err, fs.ErrNotExist)
}

// Load opens the pipeline's sealed login, re-sealing it under the current key when only the old one opened it.
//
//nolint:contextcheck // opening a store takes none
func (l *sealedLogins) Load(ctx context.Context, pipeline, server string) ([]byte, string, bool, error) {
	st, err := attachState(l.state, pipeline)
	if noSuchState(err) {
		return nil, "", false, nil
	}

	if err != nil {
		return nil, "", false, err
	}

	defer release(st)

	name := credentialName(server)
	scope := st.CredentialScope()

	sealed, found, err := st.Credential(ctx, name)
	if err != nil || !found {
		return nil, "", false, err //nolint:wrapcheck // the store names the credential and the pipeline
	}

	plain, stale, err := l.box.Open(sealed, credentialAAD(st, name))
	if err != nil {
		return nil, "", false, unopenable{err}
	}

	if stale {
		err = l.seal(ctx, st, name, plain)
		if err != nil {
			slog.WarnContext(ctx, "mcp.login.reseal_failed", "pipeline", pipeline, "server", server, "error", err,
				"hint", "it opens under "+sealbox.OldKeyEnv+" still; keep that set until a later read re-seals it")
		}
	}

	return plain, scope, true, nil
}

// Save seals data under the current key, bound to the pipeline, and refuses with no key before anything is written.
//
// Only a local command's fresh login may register the pipeline. Everything else attaches to a pipeline already there, so a write-back cannot resurrect one a destroy removed, and checks it is still the pipeline the login was read under, so it cannot land on a stranger that took the name; a destroy between the two is refused by the foreign key.
//
//nolint:contextcheck // opening a store takes none
func (l *sealedLogins) Save(ctx context.Context, pipeline, server, scope string, data []byte) error {
	err := l.box.Ready()
	if err != nil {
		return err //nolint:wrapcheck // sealbox names the variable to set
	}

	var st store.Store

	if l.register && scope == "" {
		st, err = openState(l.state, pipeline)
	} else {
		st, err = attachState(l.state, pipeline)
	}

	if err != nil {
		return fmt.Errorf("not saving a login for pipeline %q: %w", pipeline, err)
	}

	defer release(st)

	if scope != "" && st.CredentialScope() != scope {
		return fmt.Errorf("not saving a login for pipeline %q: %w", pipeline, stepsmcp.ErrLoginOrphaned)
	}

	return l.seal(ctx, st, credentialName(server), data)
}

func (l *sealedLogins) seal(ctx context.Context, st store.Store, name string, data []byte) error {
	sealed, err := l.box.Seal(data, credentialAAD(st, name))
	if err != nil {
		return err //nolint:wrapcheck // sealbox names the variable to set
	}

	err = st.SaveCredential(ctx, name, sealed)
	if err != nil {
		return err //nolint:wrapcheck // the store names the credential and the pipeline
	}

	loginSaves.Add(1)

	return nil
}
