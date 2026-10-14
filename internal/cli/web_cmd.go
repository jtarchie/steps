package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/scheduler"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/telemetry"
	"github.com/jtarchie/steps/internal/web"
)

// WebCmd is the daemon: it serves the pipeline UI, holds whatever pipelines
// have been set into it, polls their trigger: true resources, and runs the
// jobs both of those enqueue.
//
// It takes no pipeline arguments, and that is the whole shape of it: a
// pipeline arrives by `steps pipeline set` and by nothing else, so vars are
// per-set rather than process-wide, a pipeline's identity is a name somebody
// chose rather than a filename, and a change happens when somebody asks for
// it rather than when a file moves. Nothing here watches a file.
//
// It binds loopback by default and has no authentication, because there is
// nothing to authenticate against — this is the local runner's own front end,
// in the same trust domain as the shell that started it. `steps pipeline set`
// makes that sharper than it was: a pipeline is arbitrary commands, so the
// set endpoint is a remote shell. --listen exists for the person who has
// decided that is what they want, not as a default.
type WebCmd struct {
	ExecFlags    `embed:""`
	HistoryFlags `embed:""`
	// Its own --db rather than StateFlags, whose --name binds nothing here: a name is chosen by `steps pipeline set -p`, and a flag that parses and threads nowhere reads as configured.
	DB            DB                `help:"state database: a sqlite file path, sqlite:// or postgres:// url (default: .steps/steps.db)"                             name:"db"                                                                   placeholder:"URL"`
	Listen        string            `default:"127.0.0.1:8088"                                                                                                       help:"address to serve on"`
	Interval      time.Duration     `default:"30s"                                                                                                                  help:"how often to check trigger: true resources"`
	MaxConcurrent int               `default:"1"                                                                                                                    help:"maximum number of queued jobs running at once, per pipeline"`
	MaxRuns       int               `default:"0"                                                                                                                    help:"maximum number of runs at once across every pipeline (0 is no limit)"`
	Pin           map[string]string `help:"pin a version field, e.g. number=87 (repeatable)"                                                                        name:"pin"`
	Force         bool              `help:"ignore the step cache and re-run every step, even if unchanged (version: every still takes only versions not yet built)"`
	// A statement about the BROWSER's surface only; `steps pipeline set` is the deployment path and is deliberately not withheld — see docs/web.md.
	ReadOnly bool `help:"serve the pages without trigger, approval, answer, resume or abort controls" name:"read-only"`
	// Both or neither, refused below: half a pair is a daemon somebody believes is protected. Env vars because a password on a command line is in every ps listing and every shell history.
	BasicAuthUsername string   `env:"STEPS_BASIC_AUTH_USERNAME"                                                                                                                                                     help:"require this HTTP Basic username on every route but POST /p/<pipeline>/hooks/<resource>" name:"basic-auth-username"`
	BasicAuthPassword string   `env:"STEPS_BASIC_AUTH_PASSWORD"                                                                                                                                                     help:"the password that goes with it"                                                          name:"basic-auth-password"`
	LocalUser         []string `help:"a local account as name:bcrypt-hash, the line htpasswd -nbB writes (repeatable)"                                                                                              name:"local-user"                                                                              placeholder:"NAME:HASH"`
	ActivityHistory   int      `default:"10000"                                                                                                                                                                     help:"how many entries the activity log keeps, across every pipeline (0 is no limit)"          name:"activity-history"`
	Role              []string `help:"who may do what, as role=rule: viewer, operator or admin, and an expression over Provider, UserID, Email, Organizations and Groups (repeatable; with any, admin is required)" name:"role"                                                                                    placeholder:"ROLE=RULE"`
	GitHubClientID    string   `help:"sign people in with GitHub: the OAuth app's client id"                                                                                                                        name:"github-client-id"                                                                        placeholder:"ID"`
	GitHubSecretEnv   string   `default:"STEPS_GITHUB_CLIENT_SECRET"                                                                                                                                                help:"the environment variable holding that app's client secret"                               name:"github-client-secret-env" placeholder:"NAME"`
	GoogleClientID    string   `help:"sign people in with Google: the OAuth client id"                                                                                                                              name:"google-client-id"                                                                        placeholder:"ID"`
	GoogleSecretEnv   string   `default:"STEPS_GOOGLE_CLIENT_SECRET"                                                                                                                                                help:"the environment variable holding that client's secret"                                   name:"google-client-secret-env" placeholder:"NAME"`
	LocalUsersFile    string   `help:"an htpasswd file of local accounts, bcrypt hashes only"                                                                                                                       name:"local-users-file"                                                                        placeholder:"PATH"              type:"existingfile"`
	// Opt-in because a reverse proxy forwards the client's Host, so only the operator knows this daemon's names; see docs/web.md#security.
	AllowedHost []string `env:"STEPS_ALLOWED_HOSTS" help:"refuse any request whose Host is not this name (repeatable; loopback names are always allowed)" name:"allowed-host" placeholder:"NAME"`
	// No env: binding: a STEPS_*-shaped name invites confusion with the variable this one sets.
	ExternalURL string `help:"the URL steps publish to every step as STEPS_URL (default http://<listen>)" name:"external-url"`
}

// externalURL is the STEPS_URL a daemon publishes: the flag, validated, or http://<listen>. Empty means unset — a wildcard listen address is no address a link could use.
func externalURL(listen, flag string) (string, error) {
	if flag != "" {
		return validExternalURL(flag)
	}

	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("web: --listen %q: %w", listen, err)
	}

	addr, err := netip.ParseAddr(host)
	if host == "" || (err == nil && addr.IsUnspecified()) {
		return "", nil
	}

	return "http://" + listen, nil
}

func validExternalURL(flag string) (string, error) {
	u, err := url.Parse(flag)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("web: --external-url %q must be an absolute http or https URL", flag)
	}

	// Userinfo would put credentials in every step's environment, every agent's env and every placed worker.
	if u.User != nil || strings.ContainsAny(flag, "?#") {
		return "", fmt.Errorf("web: --external-url %q must not carry credentials, a query or a fragment", flag)
	}

	return strings.TrimRight(flag, "/"), nil
}

// authOptions is the server's auth setting, and the refusal when only half of one was given.
func (w *WebCmd) authOptions() ([]web.Option, error) {
	if (w.BasicAuthUsername == "") != (w.BasicAuthPassword == "") {
		return nil, errors.New("web: --basic-auth-username and --basic-auth-password are both or neither — half a pair serves open access to a daemon somebody believes is protected")
	}

	var opts []web.Option

	if w.BasicAuthUsername != "" {
		opts = append(opts, web.WithBasicAuth(w.BasicAuthUsername, w.BasicAuthPassword))
	}

	users, err := w.localUsers()
	if err != nil {
		return nil, err
	}

	if len(users) > 0 {
		opts = append(opts, web.WithLocalUsers(users))
	}

	return opts, nil
}

// accessOptions adds the sign-in providers and the roles to the accounts authOptions read: what lets people in, then what they may do once in.
func (w *WebCmd) accessOptions(opts []web.Option, published string) ([]web.Option, error) {
	providers, err := w.signInProviders(published)
	if err != nil {
		return nil, err
	}

	opts = append(opts, providers...)

	if len(w.Role) == 0 {
		return opts, nil
	}

	if len(opts) == 0 {
		return nil, errors.New("web: --role decides what signed-in people may do, and nobody can sign in: add --local-user or a sign-in provider")
	}

	roles, err := web.CompileRoles(w.Role)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}

	return append(opts, web.WithRoles(roles)), nil
}

// signInProviders is the GitHub and Google sign-in a client id asks for. Each needs its secret, read from the environment so it is never on a command line, and the URL a provider sends the browser back to.
func (w *WebCmd) signInProviders(published string) ([]web.Option, error) {
	var providers []web.SignInProvider

	for _, want := range []struct {
		flag, id, secretEnv string
		make                func(id, secret, base string) web.SignInProvider
	}{
		{"--github-client-id", w.GitHubClientID, w.GitHubSecretEnv, func(id, secret, base string) web.SignInProvider { return web.GitHubSignIn(id, secret, base, nil) }},
		{"--google-client-id", w.GoogleClientID, w.GoogleSecretEnv, func(id, secret, base string) web.SignInProvider { return web.GoogleSignIn(id, secret, base, nil) }},
	} {
		if want.id == "" {
			continue
		}

		secret := os.Getenv(want.secretEnv)
		if secret == "" {
			return nil, fmt.Errorf("web: %s needs its client secret in $%s, which is unset", want.flag, want.secretEnv)
		}

		if published == "" {
			return nil, fmt.Errorf("web: %s needs --external-url, the address a provider sends the browser back to", want.flag)
		}

		providers = append(providers, want.make(want.id, secret, published))
	}

	if len(providers) == 0 {
		return nil, nil
	}

	return []web.Option{web.WithSignInProviders(providers...)}, nil
}

// localUsers is every account --local-user and --local-users-file name; a name given twice is refused, since which password won would be an accident of order.
func (w *WebCmd) localUsers() ([]web.LocalUser, error) {
	users := make([]web.LocalUser, 0, len(w.LocalUser))

	for _, entry := range w.LocalUser {
		user, err := web.ParseLocalUser(entry)
		if err != nil {
			return nil, fmt.Errorf("web: --local-user: %w", err)
		}

		users = append(users, user)
	}

	if w.LocalUsersFile != "" {
		file, err := os.Open(w.LocalUsersFile)
		if err != nil {
			return nil, fmt.Errorf("web: --local-users-file: %w", err)
		}

		defer func() { _ = file.Close() }()

		fromFile, err := web.ParseLocalUsers(file)
		if err != nil {
			return nil, fmt.Errorf("web: --local-users-file %s: %w", w.LocalUsersFile, err)
		}

		users = append(users, fromFile...)
	}

	seen := map[string]bool{w.BasicAuthUsername: w.BasicAuthUsername != ""}

	for _, user := range users {
		if seen[user.Name] {
			return nil, fmt.Errorf("web: local user %q is given twice", user.Name)
		}

		seen[user.Name] = true
	}

	return users, nil
}

// allowedHosts refuses a value that is not a bare name: a URL would never equal a Host header, so every remote request would be refused and nothing would say why.
func (w *WebCmd) allowedHosts() error {
	for _, host := range w.AllowedHost {
		if host == "" || strings.ContainsAny(host, "/@?#") {
			return fmt.Errorf("web: --allowed-host %q must be a host name, optionally with a port, not a URL", host)
		}
	}

	return nil
}

// Run serves until canceled, holding whatever the state database says was set.
func (w *WebCmd) Run() error {
	// Rejected rather than shrugged at: a server that quietly served forever
	// without ever polling would be the exact confusion this command's
	// polling default exists to remove.
	if w.Interval <= 0 {
		return fmt.Errorf("web: --interval must be positive, got %s", w.Interval)
	}

	if len(w.Deliver) > 0 {
		return errors.New("web: --deliver is for steps run and steps test; a daemon receives deliveries at POST /p/<pipeline>/hooks/<resource>")
	}

	// Before anything is opened or bound, so a half-configured credential pair costs nothing.
	opts, err := w.authOptions()
	if err != nil {
		return err
	}

	err = w.allowedHosts()
	if err != nil {
		return err
	}

	published, err := externalURL(w.Listen, w.ExternalURL)
	if err != nil {
		return err
	}

	opts, err = w.accessOptions(opts, published)
	if err != nil {
		return err
	}

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	// A daemon's runs are read in the browser, from their record: its terminal is log lines, so a step's bytes, a note's fallback and a prompt reach nobody here.
	ctx = events.WithOutput(ctx, events.Output{Headless: true})

	ctx, base, err := w.ExecFlags.Apply(ctx)
	if err != nil {
		return err
	}

	return w.serve(ctx, opts, w.runBase(ctx, base, published))
}

// serve runs the UI, the set endpoint, and a poller and drainer per served
// pipeline, until the process is stopped.
//
// One process fills and drains every queue, because there is one daemon: a
// front end that drains a queue nothing fills is a runner that looks alive
// and notices nothing, and a second `steps web` on the same state database is
// the deployment mistake the one-process-per-file rule already names.
func (w *WebCmd) serve(ctx context.Context, opts []web.Option, base pipeline.Run) error {
	local := scheduler.NewLocalRunner(nil, base, w.MaxConcurrent)
	local.StopWith(ctx)
	local.LimitRuns(w.MaxRuns)

	var runner web.Runner
	if !w.ReadOnly {
		runner = local
	}

	state := DaemonStatePath(w.DB)
	readLog := func() (store.Reader, error) { return openStateReader(state) } //nolint:contextcheck // opening a store takes none

	// Only where somebody signs in: a browser on an open daemon has nothing to sign in as.
	if len(opts) > 0 {
		accounts, err := openAccounts(state) //nolint:contextcheck // opening a store takes none
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}

		defer func() { _ = accounts.Close() }()

		opts = append(opts, web.WithSessions(accounts))
	}

	server, err := web.New(nil, runner, append(opts, web.WithAllowedHosts(w.AllowedHost), web.WithVersion(BuildVersion), web.WithCapacity(local), web.WithMiddleware(telemetry.Handler(web.HealthPath)), web.WithActivityLog(readLog, w.ActivityHistory))...)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	held := newDaemon(ctx, server, local, state, w.ExecFlags, w.HistoryFlags, w.Interval)
	defer held.Close()

	// Deferred after Close, so it stops first: a collection must never read a store Close has shut.
	defer telemetry.ObserveQueue(held.pending)()

	// NOT withheld by --read-only, which has always been a statement about
	// the BROWSER's surface: `steps pipeline set` is the deployment path, and
	// a box that could never be configured is not a deployment. It is also
	// arbitrary command execution, which is why this binds loopback — see
	// docs/web.md.
	server.SetManager(held)

	err = held.load(ctx)
	if err != nil {
		return err
	}

	slog.InfoContext(ctx, "web.serving", "url", "http://"+w.Listen, "state", state)

	// A pipeline is arbitrary commands and a set is the endpoint that installs them, so an address anybody can reach with no credentials is worth saying out loud once.
	if len(opts) == 0 && !loopbackOnly(w.Listen) {
		slog.WarnContext(ctx, "web.no_auth", "listen", w.Listen,
			"risk", "anyone who can reach it can set a pipeline, which is arbitrary command execution",
			"fix", "set --local-user (or --basic-auth-username and --basic-auth-password), and --allowed-host to refuse names that are not this daemon's (DNS rebinding)")
	}

	if len(server.Served()) == 0 {
		slog.InfoContext(ctx, "web.no_pipelines", "hint", "steps pipeline set -c pipeline.yml")
	}

	err = server.Start(ctx, w.Listen)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	return nil
}

// runBase is what every run this daemon starts shares. Every one goes through the one LocalRunner, so browser-, webhook- and trigger-started runs all see STEPS_URL.
func (w *WebCmd) runBase(ctx context.Context, base pipeline.Run, published string) pipeline.Run {
	if published == "" {
		slog.WarnContext(ctx, "web.steps_url_unset", "listen", w.Listen, "reason", "a wildcard address", "fix", "set --external-url")
	}

	base.Pinned, base.Force, base.ExternalURL = w.Pin, w.Force, published

	return base
}

// loopbackOnly answers whether this listen address can only be reached from this machine. An unparseable or name-based address answers false: the warning is the safe side of a guess.
func loopbackOnly(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}

	// The empty host of ":8088" is every interface, which net.ParseIP reads as nothing at all.
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return strings.EqualFold(host, "localhost")
	}

	return addr.IsLoopback()
}
