package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/config"
	stepsmcp "github.com/jtarchie/steps/internal/mcp"
	"github.com/jtarchie/steps/internal/web"
)

// MCPCmd groups the two mcp_servers:-related subcommands: `tools` (list a
// server's tools — works for any auth type, and is the discovery/preflight
// step a pipeline author runs before writing a tool reference or a
// resource type's mcp: block) and `login` (the only interactive,
// state-writing command in this group — see internal/mcp/login.go). Neither
// `run` nor `watch` ever prompts; a headless process that hits an
// unauthorized oauth server just surfaces the actionable error naming this
// login command.
type MCPCmd struct {
	List  MCPListCmd  `cmd:"" help:"list the pipeline's mcp servers and whether each one answers"`
	Tools MCPToolsCmd `cmd:"" help:"list an mcp server's tools and their argument schemas"`
	Login MCPLoginCmd `cmd:"" help:"interactively authorize an oauth-configured mcp server"`
}

// MCPListCmd is the inventory: every mcp_servers: entry, how it connects, who
// consumes it, and — unless --offline — whether it answers right now.
//
// `mcp tools` answers "what can this one server do", which presumes you
// already know which servers exist and which are working. This is the step
// before that, and the reason it probes by default: a server is configured in
// YAML but broken on this machine (binary not on PATH, token never obtained,
// endpoint moved), and the file alone cannot tell you which.
//
// It reports rather than gates — a server that does not answer is a row with
// an ✗, not a non-zero exit. `steps validate --live` is the command that fails.
type MCPListCmd struct {
	Pipeline string            `arg:""                                                                                                          help:"path to the pipeline YAML file"`
	Offline  bool              `help:"list what the file declares without connecting to anything"                                               name:"offline"`
	Name     map[string]string `help:"name the pipeline a file's mcp logins are filed under, e.g. --name infra=infra/pipeline.yml (repeatable)" name:"name"`
}

// Run prints one row per configured server.
func (m *MCPListCmd) Run() error {
	cfg, err := config.Load(m.Pipeline, resolvePipelineName(m.Pipeline, m.Name), nil)
	if err != nil {
		return fmt.Errorf("could not load pipeline: %w", err)
	}

	if len(cfg.MCPServers) == 0 {
		fmt.Printf("no mcp_servers: entries in %s\n", m.Pipeline)

		return nil
	}

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	statuses := staticMCPStatuses(cfg)

	var probeErr error

	if !m.Offline {
		probeErr = probeMCPServers(ctx, cfg, statuses)
	}

	writer := newTabWriter()

	_, _ = fmt.Fprintln(writer, "NAME\tTRANSPORT\tTARGET\tAUTH\tUSED BY\tSTATUS")

	for i, srv := range cfg.MCPServers {
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s",
			srv.Name, srv.Transport(), srv.Target(), srv.AuthLabel(), cfg.MCPUsers(srv.Name), statuses[i])

		_, _ = fmt.Fprintln(writer, row)
	}

	err = flush(writer)
	if err != nil {
		return err
	}

	// An interrupted probe leaves rows reading "✗ context canceled", which is
	// indistinguishable from a pipeline whose every server is down — so the
	// exit status has to be the thing that separates them (130, not 0).
	if probeErr != nil {
		return fmt.Errorf("mcp list: %w", probeErr)
	}

	return nil
}

// staticMCPStatuses is the STATUS column before anything is dialled: a stdio command on PATH, a bearer variable set, an oauth server that needs a login. This is the whole column under --offline, and the starting point for the probe — see config.MCPServer.StaticStatus.
func staticMCPStatuses(cfg *config.Config) []string {
	statuses := make([]string, len(cfg.MCPServers))

	for i, srv := range cfg.MCPServers {
		status := srv.StaticStatus()

		switch status.Readiness {
		case config.MCPReady:
			statuses[i] = "✓ " + status.Detail
		case config.MCPMissing:
			statuses[i] = "✗ " + elideMiddle(status.Detail)
		case config.MCPUnknown:
			statuses[i] = "· " + status.Detail
		}
	}

	return statuses
}

// probeMCPServers connects to every server and reports what it found, one
// status per server in configuration order, plus the context's own error so an
// interrupted listing is not mistaken for a listing of broken servers.
//
// Concurrently, because the failure this command exists to surface is a server
// that does not answer — and doing that serially means the slowest possible
// listing is the one with the most broken servers in it, each waiting out its
// own timeout in turn.
//
// A server whose static status already FAILED is not dialled: a probe that
// cannot authenticate reports the missing credential a second time, in the
// vocabulary of whatever refused it, and one problem in two wordings reads as
// two problems.
func probeMCPServers(ctx context.Context, cfg *config.Config, statuses []string) error {
	var settings *config.Preflight
	if cfg.Defaults != nil {
		settings = cfg.Defaults.Preflight
	}

	timeout := settings.ProbeTimeout()

	var wait sync.WaitGroup

	for i, srv := range cfg.MCPServers {
		if srv.StaticStatus().Readiness != config.MCPReady {
			continue
		}

		wait.Add(1)

		go func() {
			defer wait.Done()

			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			tools, err := stepsmcp.ListServerTools(probeCtx, cfg.Name, srv)
			if err != nil {
				statuses[i] = "✗ " + elideMiddle(config.MCPStatusReason(srv.Name, err))

				return
			}

			statuses[i] = fmt.Sprintf("✓ %d %s", len(tools), pluralize(len(tools), "tool"))
		}()
	}

	wait.Wait()

	return ctx.Err() //nolint:wrapcheck // the caller names the command; a bare context.Canceled is what outcome.ExitCode reads
}

// MCPToolsCmd lists the tools a configured mcp_servers: entry exposes.
type MCPToolsCmd struct {
	Pipeline string            `arg:""                                                                                                          help:"path to the pipeline YAML file"`
	Server   string            `arg:""                                                                                                          help:"mcp_servers: entry name"`
	Name     map[string]string `help:"name the pipeline a file's mcp logins are filed under, e.g. --name infra=infra/pipeline.yml (repeatable)" name:"name"`
}

// Run loads the pipeline, resolves the named server, connects (per its
// configured auth), and prints each of its tools' name, description, and
// argument schema. An unauthorized oauth server surfaces
// oauthTokenSource's own actionable "run steps mcp login" error, unchanged.
func (m *MCPToolsCmd) Run() error {
	cfg, err := config.Load(m.Pipeline, resolvePipelineName(m.Pipeline, m.Name), nil)
	if err != nil {
		return fmt.Errorf("could not load pipeline: %w", err)
	}

	srv, err := cfg.FindMCPServer(m.Server)
	if err != nil {
		return fmt.Errorf("could not find mcp server: %w", err)
	}

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	tools, err := stepsmcp.ListServerTools(ctx, cfg.Name, *srv)
	if err != nil {
		return fmt.Errorf("could not list tools: %w", err)
	}

	printMCPTools(tools)

	return nil
}

// printMCPTools writes each tool's name, description, and argument schema
// to stdout in a simple, human-readable form.
func printMCPTools(tools []*sdkmcp.Tool) {
	if len(tools) == 0 {
		fmt.Println("(no tools)")

		return
	}

	for _, tool := range tools {
		fmt.Printf("%s\n", tool.Name)

		if tool.Description != "" {
			fmt.Printf("  %s\n", tool.Description)
		}

		schema, err := json.MarshalIndent(tool.InputSchema, "  ", "  ")
		if err == nil && len(schema) > 0 {
			fmt.Printf("  arguments: %s\n", schema)
		}

		fmt.Println()
	}
}

// MCPLoginCmd interactively authorizes an auth: {type: oauth} mcp_servers:
// entry — the only command in this CLI that opens a browser or blocks on
// user interaction outside a pipeline run.
type MCPLoginCmd struct {
	TargetFlags      `embed:""`
	PipelineNameFlag `embed:""`
	Server           string `arg:""                                                                             help:"mcp_servers: entry name to authorize"`
	Config           string `help:"authorize on THIS machine, for the server as this pipeline YAML declares it" name:"config"                               short:"c" type:"path"`
	// Name is which pipeline a -c login is filed under, the same name `steps run --name` gives its state.
	Name map[string]string `help:"name the pipeline a file's mcp logins are filed under, e.g. --name infra=infra/pipeline.yml (repeatable)" name:"name"`
}

// Run is one of two logins, chosen by which pipeline was named. -c is a file here, so the login runs here: a loopback listener catches the redirect and the token lands in this user's config dir. -p is a pipeline a DAEMON serves, so the daemon runs it (daemon_login.go) — it is the machine that will spend the token, and one with no browser cannot be the loopback a redirect comes back to.
func (m *MCPLoginCmd) Run() error {
	if (m.Config == "") == (m.Pipeline == "") {
		return errors.New("steps mcp login needs -c <pipeline.yml> to authorize this machine, or -p <name> (with --target) to authorize a daemon — and not both")
	}

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	if m.Pipeline != "" {
		// -p already names the pipeline the login is filed under; a --name beside it would be silently ignored.
		if len(m.Name) > 0 {
			return errors.New("steps mcp login: --name names a -c file's pipeline; with -p the pipeline is already named")
		}

		return m.remote(ctx)
	}

	cfg, err := config.Load(m.Config, resolvePipelineName(m.Config, m.Name), nil)
	if err != nil {
		return fmt.Errorf("could not load pipeline: %w", err)
	}

	srv, err := cfg.FindMCPServer(m.Server)
	if err != nil {
		return fmt.Errorf("could not find mcp server: %w", err)
	}

	if srv.Auth.Type != "oauth" {
		return fmt.Errorf("mcp server %q is not auth: {type: oauth}; nothing to log in to", m.Server)
	}

	fmt.Printf("→ opening browser to authorize %q…\n", m.Server)

	err = stepsmcp.Login(ctx, cfg.Name, *srv, openBrowser)
	if err != nil {
		return fmt.Errorf("mcp login: %w", err)
	}

	path, err := stepsmcp.TokenPath(cfg.Name, m.Server)
	if err != nil {
		return fmt.Errorf("mcp login: %w", err)
	}

	fmt.Printf("✓ authorized %q for pipeline %q (token saved to %s)\n", m.Server, cfg.Name, path)

	return nil
}

// loginPoll is how often the CLI asks a daemon where its login stands.
const loginPoll = 500 * time.Millisecond

// remote starts the login on the daemon and plays its browser half here. The daemon is sent the address it was reached on with the credentials ALREADY OFF it (daemonClient.target), because that address becomes the redirect URI the authorization server is given and keeps.
func (m *MCPLoginCmd) remote(ctx context.Context) error {
	name, err := namedPipeline(m.Pipeline, "mcp login")
	if err != nil {
		return err
	}

	client := newDaemonClient(m.Target)

	status, err := client.startLogin(ctx, name, m.Server)
	if err != nil {
		return err
	}

	fmt.Printf("→ %s is authorizing %q…\n", client.target, m.Server)

	announced := false

	for status.State == web.LoginPending {
		if status.AuthorizeURL != "" && !announced {
			announced = true

			stepsmcp.PrintAndOpen(os.Stdout, openBrowser, status.AuthorizeURL)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("mcp login: %w", ctx.Err())
		case <-time.After(loginPoll):
		}

		status, err = client.loginStatus(ctx, name, m.Server)
		if err != nil {
			return err
		}
	}

	if status.State != web.LoginAuthorized {
		return fmt.Errorf("mcp login: %s", status.Message)
	}

	fmt.Printf("✓ authorized %q on %s (token saved there, at %s)\n", m.Server, client.target, status.TokenPath)

	return nil
}

// openBrowser launches the OS's default browser at url. Its caller
// (internal/mcp's loopbackCallback.fetch) prints url to stdout regardless
// and reports any error this returns alongside it, so this only needs to
// cover the happy path per OS — a nil case (no known opener for GOOS) fails
// closed into "open the URL above" rather than guessing.
func openBrowser(url string) error {
	// A fire-and-forget subprocess launch (handing off to the OS's default-
	// app opener, which returns almost immediately) with nothing meaningful
	// to cancel — context.Background() rather than threading a caller ctx
	// through internal/mcp's open func(string) error callback type.
	ctx := context.Background()

	var cmd *exec.Cmd

	switch {
	// The convention xdg-open, gh and python's webbrowser all honour, and the only way to choose a browser on macOS, where `open` takes no such hint.
	case os.Getenv("BROWSER") != "":
		cmd = exec.CommandContext(ctx, os.Getenv("BROWSER"), url) //nolint:gosec // the operator's own choice of browser, and the URL is one steps built or a daemon they authenticated to returned
	case runtime.GOOS == "darwin":
		cmd = exec.CommandContext(ctx, "open", url) //nolint:gosec // url is the authorization URL steps itself just built via oauth2.Config.AuthCodeURL, not attacker-influenced input
	case runtime.GOOS == "linux":
		cmd = exec.CommandContext(ctx, "xdg-open", url) //nolint:gosec // same as above
	default:
		return fmt.Errorf("no known browser-open command for GOOS %q", runtime.GOOS)
	}

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}
