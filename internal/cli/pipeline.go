package cli

// Uploading rather than pointing at a file, because what was wrong with a daemon that reads paths is all identity: process-wide vars, a name that was a filename, bytes somebody else had to deliver, and a change that applied because a file moved rather than because anybody asked (docs/web.md).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/web"
)

// PipelineCmd groups the verbs that tell a daemon what to serve.
type PipelineCmd struct {
	Set     PipelineSetCmd     `cmd:"" help:"upload a pipeline's configuration to a daemon"`
	List    PipelineListCmd    `cmd:"" default:"withargs"                                          help:"what the daemon is serving"`
	Get     PipelineGetCmd     `cmd:"" help:"print the configuration the daemon is serving"`
	Destroy PipelineDestroyCmd `cmd:"" help:"forget a pipeline and everything recorded under it"`
	Pause   PipelinePauseCmd   `cmd:"" help:"stop polling, admitting and triggering this pipeline"`
	Unpause PipelineUnpauseCmd `cmd:"" help:"let it run again"`
	Rename  PipelineRenameCmd  `cmd:"" help:"give a pipeline a new name, keeping its history"`
}

// TargetFlags is which daemon a verb talks to. No login and no saved targets, because there is nothing to log in to: the daemon has no authentication and binds the loopback this defaults to.
type TargetFlags struct {
	Target string `default:"http://127.0.0.1:8088" env:"STEPS_TARGET" help:"the steps web daemon to talk to" name:"target"`
}

// PipelineNameFlag is which pipeline on that daemon a verb is about.
type PipelineNameFlag struct {
	Pipeline string `help:"the pipeline's name on the daemon" name:"pipeline" short:"p"`
}

// PipelineSetCmd uploads a configuration, which is the only way a served pipeline changes.
type PipelineSetCmd struct {
	TargetFlags      `embed:""`
	PipelineNameFlag `embed:""`
	VarFlags         `embed:""`
	Config           string `help:"the pipeline YAML to upload" name:"config" required:"" short:"c" type:"path"`
	// The prompt exists because a set is a deploy, and a deploy nobody looked at is the file watcher this command replaced.
	NonInteractive bool `help:"do not show a diff or ask; apply it" name:"non-interactive" short:"n"`
}

// Run substitutes, bundles, diffs, asks, and uploads.
func (p *PipelineSetCmd) Run() error {
	name := p.Pipeline
	if name == "" {
		name = config.Slugify(p.Config)
	}

	err := config.ValidPipelineName(name)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	// Substituted and parsed HERE: a var never reaches the daemon, and the parse is what resolves the includes that travel with it.
	vars, err := resolveVars(p.Var, p.VarsFile)
	if err != nil {
		return err
	}

	// The parsed config's own source, not a second read: two reads of one path have no ordering between them, so an editor saving in the window uploads bytes nobody validated.
	cfg, err := config.Load(p.Config, name, vars)
	if err != nil {
		return fmt.Errorf("could not load pipeline: %w", err)
	}

	return p.upload(name, cfg.Revision.Source, cfg.Revision.Includes)
}

// namedPipeline refuses a name before it is concatenated into a URL: "#" truncates the path into a fragment nobody sends and "?" into a query, so `-p prod#old` asked the daemon about `prod` — which for destroy is the wrong pipeline, irrecoverably.
func namedPipeline(name, verb string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("steps pipeline %s needs -p <name>", verb)
	}

	err := config.ValidPipelineName(name)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	return name, nil
}

// upload is the half that talks to the daemon: read what it serves, diff,
// ask, and send the sha that diff was against.
func (p *PipelineSetCmd) upload(name, source string, includes map[string]string) error {
	client := newDaemonClient(p.Target)

	current, held, err := client.get(name)
	if err != nil {
		return err
	}

	if held && current.Source == source && maps.Equal(current.Includes, includes) {
		fmt.Printf("unchanged: %s is already serving this configuration (%s)\n", name, shortConfig(current.SHA))

		return nil
	}

	err = p.confirm(name, current, held, source, includes)
	if err != nil {
		return err
	}

	from, err := filepath.Abs(p.Config)
	if err != nil {
		from = p.Config
	}

	result, err := client.set(name, web.SetRequest{
		Source:    source,
		Includes:  includes,
		ExpectSHA: current.SHA,
		From:      from,
	})
	if err != nil {
		return err
	}

	verb := "updated"
	if result.Created {
		verb = "created"
	}

	fmt.Printf("%s: %s is now serving %s\n", verb, name, shortConfig(result.SHA))

	return nil
}

// confirm shows what would change and asks, unless told not to.
func (p *PipelineSetCmd) confirm(
	name string, current web.PipelineConfig, held bool, source string, includes map[string]string,
) error {
	if p.NonInteractive {
		return nil
	}

	if !held {
		fmt.Printf("%s is new to this daemon; it will be created and start running.\n", name)
	} else {
		fmt.Print(diffSource(current.Source, source))
		fmt.Print(diffIncludes(current.Includes, includes))
	}

	return ask("apply configuration?", "not applied")
}

// The includes are as much of a deploy as the YAML is — a run_file: decides what a step executes — so an edit that only moved one of them was shown a pipeline reported as unchanged, and confirmed on that.
func diffIncludes(before, after map[string]string) string {
	var out strings.Builder

	for _, path := range slices.Sorted(maps.Keys(after)) {
		old, had := before[path]

		switch {
		case !had:
			fmt.Fprintf(&out, "+ %s (%d bytes, newly included)\n", path, len(after[path]))
		case old != after[path]:
			fmt.Fprintf(&out, "~ %s (%d bytes, was %d)\n", path, len(after[path]), len(old))
		}
	}

	for _, path := range slices.Sorted(maps.Keys(before)) {
		if _, kept := after[path]; !kept {
			fmt.Fprintf(&out, "- %s (no longer included)\n", path)
		}
	}

	return out.String()
}

// ask is the one confirmation both destructive verbs read, so a closed stdin is a no rather than a crash in one of them and a crash in the other.
func ask(question, refusal string) error {
	fmt.Print(question + " [y/N] ")

	var answer string

	_, err := fmt.Scanln(&answer)
	if err != nil && !errors.Is(err, io.EOF) {
		answer = ""
	}

	if !strings.EqualFold(strings.TrimSpace(answer), "y") {
		return errors.New(refusal)
	}

	return nil
}

// A whole-line walk rather than an LCS, because the only question here is whether the change you made is the change being sent — a repository is what `git diff` is for.
func diffSource(before, after string) string {
	// Silence rather than "the same lines, reordered", which is what an include-only edit used to be told about a file it had not touched.
	if before == after {
		return ""
	}

	old := strings.Split(before, "\n")
	next := strings.Split(after, "\n")

	var out strings.Builder

	for _, line := range old {
		if !slices.Contains(next, line) {
			fmt.Fprintf(&out, "- %s\n", line)
		}
	}

	for _, line := range next {
		if !slices.Contains(old, line) {
			fmt.Fprintf(&out, "+ %s\n", line)
		}
	}

	if out.Len() == 0 {
		return "the same lines, reordered\n"
	}

	return out.String()
}

// PipelineListCmd is what the daemon holds.
type PipelineListCmd struct {
	TargetFlags `embed:""`
}

// Run prints one row per served pipeline.
func (p *PipelineListCmd) Run() error {
	rows, err := newDaemonClient(p.Target).list()
	if err != nil {
		return err
	}

	if len(rows) == 0 {
		fmt.Printf("no pipelines set on %s — upload one with: steps pipeline set -c pipeline.yml\n", p.Target)

		return nil
	}

	writer := newTabWriter()
	_, _ = fmt.Fprintln(writer, "PIPELINE\tJOBS\tCONFIG\tSTATE\tSET FROM")

	for _, row := range rows {
		state := "running"
		if row.Paused {
			state = "paused"
		}

		from := row.From
		if from == "" {
			from = "-"
		}

		_, _ = fmt.Fprintf(writer, "%s\t%d\t%s\t%s\t%s\n", row.Name, row.Jobs, shortConfig(row.SHA), state, from)
	}

	return flush(writer)
}

// PipelineGetCmd prints what a daemon serves, which after a local edit is the only place that configuration still exists.
type PipelineGetCmd struct {
	TargetFlags      `embed:""`
	PipelineNameFlag `embed:""`
}

// Run prints the served source and names the files it carried.
func (p *PipelineGetCmd) Run() error {
	name, err := namedPipeline(p.Pipeline, "get")
	if err != nil {
		return err
	}

	current, held, err := newDaemonClient(p.Target).get(name)
	if err != nil {
		return err
	}

	if !held {
		return fmt.Errorf("%s is not serving a pipeline called %q", p.Target, name)
	}

	fmt.Print(current.Source)

	for path, content := range current.Includes {
		fmt.Printf("\n# --- %s (%d bytes) ---\n%s", path, len(content), content)
	}

	return nil
}

// PipelineDestroyCmd forgets a pipeline and everything recorded under it.
type PipelineDestroyCmd struct {
	TargetFlags      `embed:""`
	PipelineNameFlag `embed:""`
	NonInteractive   bool `help:"do not ask" name:"non-interactive" short:"n"`
}

// Run destroys the named pipeline.
func (p *PipelineDestroyCmd) Run() error {
	name, err := namedPipeline(p.Pipeline, "destroy")
	if err != nil {
		return err
	}

	// Asked because it is not recoverable: history, versions and the merkle cache go with the row, and nothing here is a soft delete.
	if !p.NonInteractive {
		err = ask("destroy "+name+" and everything recorded under it?", "not destroyed")
		if err != nil {
			return err
		}
	}

	err = newDaemonClient(p.Target).destroy(name)
	if err != nil {
		return err
	}

	fmt.Printf("destroyed: %s\n", name)

	return nil
}

// PipelinePauseCmd throws the pipeline-level circuit breaker.
type PipelinePauseCmd struct {
	TargetFlags      `embed:""`
	PipelineNameFlag `embed:""`
}

// Run pauses the named pipeline.
func (p *PipelinePauseCmd) Run() error { return pauseVerb(p.Target, p.Pipeline, "pause") }

// PipelineUnpauseCmd releases it.
type PipelineUnpauseCmd struct {
	TargetFlags      `embed:""`
	PipelineNameFlag `embed:""`
}

// Run unpauses the named pipeline.
func (p *PipelineUnpauseCmd) Run() error { return pauseVerb(p.Target, p.Pipeline, "unpause") }

func pauseVerb(target, pipeline, verb string) error {
	name, err := namedPipeline(pipeline, verb)
	if err != nil {
		return err
	}

	err = newDaemonClient(target).pause(name, verb)
	if err != nil {
		return err
	}

	fmt.Printf("%sd: %s\n", verb, name)

	return nil
}

// PipelineRenameCmd moves a pipeline's identity, keeping its history.
type PipelineRenameCmd struct {
	TargetFlags      `embed:""`
	PipelineNameFlag `embed:""`
	To               string `help:"the new name" name:"to" required:""`
}

// Run renames the named pipeline.
func (p *PipelineRenameCmd) Run() error {
	name, err := namedPipeline(p.Pipeline, "rename")
	if err != nil {
		return err
	}

	err = config.ValidPipelineName(p.To)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	err = newDaemonClient(p.Target).rename(name, p.To)
	if err != nil {
		return err
	}

	fmt.Printf("renamed: %s is now %s\n", name, p.To)

	return nil
}

// daemonClient is the HTTP half of every verb above.
type daemonClient struct {
	target string
	http   *http.Client
}

// A set runs every validator the daemon has, so it is not instant — and a daemon that stopped answering must say so rather than hang a terminal.
const daemonTimeout = 30 * time.Second

func newDaemonClient(target string) *daemonClient {
	return &daemonClient{
		target: strings.TrimSuffix(target, "/"),
		// Keep-alives off: a verb makes one or two requests and the process
		// exits, so a pooled connection buys nothing and leaves the daemon
		// holding a socket for a terminal that has gone.
		http: &http.Client{
			Timeout:   daemonTimeout,
			Transport: &http.Transport{DisableKeepAlives: true},
		},
	}
}

// held is false for a pipeline the daemon does not hold, which is a set creating one rather than an error.
func (c *daemonClient) get(name string) (web.PipelineConfig, bool, error) {
	var current web.PipelineConfig

	status, body, err := c.do(http.MethodGet, "/api/pipelines/"+name, nil)
	if err != nil {
		return current, false, err
	}

	if status == http.StatusNotFound {
		return current, false, nil
	}

	if status != http.StatusOK {
		return current, false, daemonError(c.target, status, body)
	}

	err = json.Unmarshal(body, &current)
	if err != nil {
		return current, false, fmt.Errorf("could not read the daemon's answer: %w", err)
	}

	return current, true, nil
}

func (c *daemonClient) list() ([]web.PipelineSummary, error) {
	status, body, err := c.do(http.MethodGet, "/api/pipelines", nil)
	if err != nil {
		return nil, err
	}

	if status != http.StatusOK {
		return nil, daemonError(c.target, status, body)
	}

	var rows []web.PipelineSummary

	err = json.Unmarshal(body, &rows)
	if err != nil {
		return nil, fmt.Errorf("could not read the daemon's answer: %w", err)
	}

	return rows, nil
}

func (c *daemonClient) set(name string, req web.SetRequest) (web.SetResult, error) {
	var result web.SetResult

	payload, err := json.Marshal(req)
	if err != nil {
		return result, fmt.Errorf("could not encode the configuration: %w", err)
	}

	status, body, err := c.do(http.MethodPut, "/api/pipelines/"+name, payload)
	if err != nil {
		return result, err
	}

	if status != http.StatusOK {
		return result, daemonError(c.target, status, body)
	}

	err = json.Unmarshal(body, &result)
	if err != nil {
		return result, fmt.Errorf("could not read the daemon's answer: %w", err)
	}

	return result, nil
}

func (c *daemonClient) destroy(name string) error {
	status, body, err := c.do(http.MethodDelete, "/api/pipelines/"+name, nil)
	if err != nil {
		return err
	}

	if status != http.StatusNoContent {
		return daemonError(c.target, status, body)
	}

	return nil
}

func (c *daemonClient) pause(name, verb string) error {
	status, body, err := c.do(http.MethodPost, "/api/pipelines/"+name+"/"+verb, nil)
	if err != nil {
		return err
	}

	if status != http.StatusNoContent {
		return daemonError(c.target, status, body)
	}

	return nil
}

func (c *daemonClient) rename(from, to string) error {
	payload, err := json.Marshal(map[string]string{"to": to})
	if err != nil {
		return fmt.Errorf("could not encode the new name: %w", err)
	}

	status, body, err := c.do(http.MethodPost, "/api/pipelines/"+from+"/rename", payload)
	if err != nil {
		return err
	}

	if status != http.StatusNoContent {
		return daemonError(c.target, status, body)
	}

	return nil
}

// do is one request, with the body read whatever the status.
func (c *daemonClient) do(method, path string, payload []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), daemonTimeout)
	defer cancel()

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.target+path, body)
	if err != nil {
		return 0, nil, fmt.Errorf("could not build the request: %w", err)
	}

	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("could not reach the steps daemon at %s: %w (start one with: steps web)", c.target, err)
	}

	defer func() { _ = resp.Body.Close() }()

	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("could not read the daemon's answer: %w", err)
	}

	return resp.StatusCode, answer, nil
}

// The refusal becomes the sentence the terminal that asked prints, which is the whole reason this transport is HTTP rather than a row write.
func daemonError(target string, status int, body []byte) error {
	message := strings.TrimSpace(string(body))

	// echo wraps a refusal as {"message": ...}, and the message is the part a person acts on.
	var wrapped struct {
		Message string `json:"message"`
	}

	if json.Unmarshal(body, &wrapped) == nil && wrapped.Message != "" {
		message = wrapped.Message
	}

	if message == "" {
		message = http.StatusText(status)
	}

	if status == http.StatusConflict {
		return fmt.Errorf("%s refused it: %s — re-read it with `steps pipeline get` and try again", target, message)
	}

	return fmt.Errorf("%s refused it: %s", target, message)
}
