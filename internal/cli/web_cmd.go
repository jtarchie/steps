package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/pipeline"
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
	DB            DB                `help:"state database: a sqlite file path or sqlite:// url (default: .steps/steps.db)"                                          name:"db"                                                          placeholder:"URL"`
	Listen        string            `default:"127.0.0.1:8088"                                                                                                       help:"address to serve on"`
	Interval      time.Duration     `default:"30s"                                                                                                                  help:"how often to check trigger: true resources"`
	MaxConcurrent int               `default:"1"                                                                                                                    help:"maximum number of queued jobs running at once, per pipeline"`
	Pin           map[string]string `help:"pin a version field, e.g. number=87 (repeatable)"                                                                        name:"pin"`
	Force         bool              `help:"ignore the step cache and re-run every step, even if unchanged (version: every still takes only versions not yet built)"`
	// A statement about the BROWSER's surface only; `steps pipeline set` is the deployment path and is deliberately not withheld — see docs/web.md.
	ReadOnly bool `help:"serve the pages without trigger, approval, answer, resume or abort controls" name:"read-only"`
	// Both or neither, refused below: half a pair is a daemon somebody believes is protected. Env vars because a password on a command line is in every ps listing and every shell history.
	BasicAuthUsername string `env:"STEPS_BASIC_AUTH_USERNAME" help:"require this HTTP Basic username on every route but POST /p/<pipeline>/hooks/<resource>" name:"basic-auth-username"`
	BasicAuthPassword string `env:"STEPS_BASIC_AUTH_PASSWORD" help:"the password that goes with it"                                                          name:"basic-auth-password"`
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

	if w.BasicAuthUsername == "" {
		return nil, nil
	}

	return []web.Option{web.WithBasicAuth(w.BasicAuthUsername, w.BasicAuthPassword)}, nil
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
	_, err := w.authOptions()
	if err != nil {
		return err
	}

	_, err = externalURL(w.Listen, w.ExternalURL)
	if err != nil {
		return err
	}

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	// Read once, before anything is spawned: every build and poll the daemon runs writes where it was started, rather than reading the process's streams from a goroutine while something else reassigns them.
	ctx = events.WithOutput(ctx, events.Output{Stdout: os.Stdout, Stderr: os.Stderr})

	ctx, err = w.ExecFlags.Apply(ctx)
	if err != nil {
		return err
	}

	return w.serve(ctx)
}

// serve runs the UI, the set endpoint, and a poller and drainer per served
// pipeline, until the process is stopped.
//
// One process fills and drains every queue, because there is one daemon: a
// front end that drains a queue nothing fills is a runner that looks alive
// and notices nothing, and a second `steps web` on the same state database is
// the deployment mistake the one-process-per-file rule already names.
func (w *WebCmd) serve(ctx context.Context) error {
	local := web.NewLocalRunner(nil, w.Pin, w.MaxConcurrent, w.Force)
	local.StopWith(ctx)

	var runner web.Runner
	if !w.ReadOnly {
		runner = local
	}

	opts, err := w.authOptions()
	if err != nil {
		return err
	}

	ctx, err = w.withExternalURL(ctx)
	if err != nil {
		return err
	}

	server, err := web.New(nil, runner, append(opts, web.WithVersion(BuildVersion))...)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	state := DaemonStatePath(w.DB)

	held := newDaemon(ctx, server, local, state, w.ExecFlags, w.HistoryFlags, w.Interval)
	defer held.Close()

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

	fmt.Printf("steps web: http://%s (state: %s)\n", w.Listen, state)

	// A pipeline is arbitrary commands and a set is the endpoint that installs them, so an address anybody can reach with no credentials is worth saying out loud once.
	if len(opts) == 0 && !loopbackOnly(w.Listen) {
		fmt.Printf("steps web: WARNING serving %s with no authentication — anyone who can reach it can set a pipeline, which is arbitrary command execution; set --basic-auth-username and --basic-auth-password\n", w.Listen)
	}

	if len(server.Served()) == 0 {
		fmt.Println("steps web: no pipelines set — upload one with: steps pipeline set -c pipeline.yml")
	}

	err = server.Start(ctx, w.Listen)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	return nil
}

// withExternalURL carries STEPS_URL to every run this daemon starts: each drain loop derives from newDaemon's base, so browser-, webhook- and trigger-started runs all see it.
func (w *WebCmd) withExternalURL(ctx context.Context) (context.Context, error) {
	published, err := externalURL(w.Listen, w.ExternalURL)
	if err != nil {
		return ctx, err
	}

	if published == "" {
		fmt.Printf("steps web: %s is a wildcard address, so STEPS_URL is unset for steps; set --external-url\n", w.Listen)
	}

	return pipeline.WithExternalURL(ctx, published), nil
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
