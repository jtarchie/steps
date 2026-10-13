package cli

// Uploading rather than pointing at a file, because what was wrong with a daemon that reads paths is all identity: process-wide vars, a name that was a filename, bytes somebody else had to deliver, and a change that applied because a file moved rather than because anybody asked (docs/web.md).

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

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

// TargetFlags is which daemon a verb talks to. No login and no saved targets, because there is nothing to log in to when the daemon binds the loopback this defaults to; a daemon that does ask takes its credentials from this URL's userinfo, which is a string an operator can keep in a password manager and a script can keep in one variable.
type TargetFlags struct {
	Target string `default:"http://127.0.0.1:8088" env:"STEPS_TARGET" help:"the steps web daemon to talk to, optionally http://user:password@host" name:"target"`
}

// Validate is kong's hook. A target that does not parse cannot have its credentials taken off, and every message after this one would quote it whole — url.Parse's own error included.
func (f TargetFlags) Validate() error {
	_, err := url.Parse(strings.TrimSuffix(f.Target, "/"))
	if err != nil {
		return errors.New("--target does not parse as a URL; check its syntax, percent-encoding any / : @ in the password (not repeated here, as it may carry one)")
	}

	return nil
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
	NonInteractive bool `help:"do not show a diff or ask; apply it"                                        name:"non-interactive" short:"n"`
	Pause          bool `help:"pause the pipeline as part of the set, so it serves nothing until unpaused" name:"pause"`
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
	cfg, err := p.Load(p.Config, name)
	if err != nil {
		return err
	}

	// The parsed config's own source, not a second read: two reads of one path have no ordering between them, so an editor saving in the window uploads bytes nobody validated.

	return p.upload(name, cfg.Revision.Source, cfg.Revision.Includes)
}

// namedPipeline refuses a name before it is concatenated into a URL: "#" truncates the path into a fragment nobody sends and "?" into a query, so `-p prod#old` asked the daemon about `prod` — which for destroy is the wrong pipeline, irrecoverably.
func namedPipeline(name, verb string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("steps %s needs -p <name>", verb)
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

	current, held, broken, err := client.get(name)
	if err != nil {
		return err
	}

	if held && current.Source == source && maps.Equal(convertIncludes[string](current.Includes), includes) {
		fmt.Printf("unchanged: %s is already serving this configuration (%s)\n", name, shortConfig(current.SHA))

		// Separately and after, because the pipeline already exists: the window --pause closes is a new pipeline's first poll.
		if p.Pause {
			return client.pause(name, "pause")
		}

		return nil
	}

	err = p.confirm(name, current, held, broken, source, includes)
	if err != nil {
		return err
	}

	from, err := filepath.Abs(p.Config)
	if err != nil {
		from = p.Config
	}

	result, err := client.set(name, web.SetRequest{
		Source:    source,
		Includes:  convertIncludes[[]byte](includes),
		ExpectSHA: current.SHA,
		From:      from,
		Pause:     p.Pause,
	})
	if err != nil {
		return err
	}

	fmt.Printf("%s: %s is now serving %s\n", setVerb(result), name, shortConfig(result.SHA))

	return nil
}

func setVerb(result web.SetResult) string {
	switch {
	case result.Repaired:
		return "repaired"
	case result.Created:
		return "created"
	default:
		return "updated"
	}
}

// confirm shows what would change and asks, unless told not to.
func (p *PipelineSetCmd) confirm(
	name string, current web.PipelineConfig, held bool, broken, source string, includes map[string]string,
) error {
	if p.NonInteractive {
		return nil
	}

	switch {
	case broken != "":
		fmt.Printf("%s is not being served (%s); this set will repair it.\n", name, broken)
	case !held && p.Pause:
		fmt.Printf("%s is new to this daemon; it will be created paused.\n", name)
	case !held:
		fmt.Printf("%s is new to this daemon; it will be created and start running.\n", name)
	default:
		fmt.Print(diffSource(current.Source, source))
		fmt.Print(diffIncludes(convertIncludes[string](current.Includes), includes))
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

func convertIncludes[To, From ~string | ~[]byte](includes map[string]From) map[string]To {
	converted := make(map[string]To, len(includes))
	for path, content := range includes {
		converted[path] = To(content)
	}

	return converted
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
	client := newDaemonClient(p.Target)

	rows, err := client.list()
	if err != nil {
		return err
	}

	if len(rows) == 0 {
		fmt.Printf("no pipelines set on %s — upload one with: steps pipeline set -c pipeline.yml\n", client.target)

		return nil
	}

	writer := newTabWriter()
	_, _ = fmt.Fprintln(writer, "PIPELINE\tJOBS\tCONFIG\tSTATE\tSET FROM")

	for _, row := range rows {
		state := "running"

		switch {
		case row.Broken != "":
			state = "broken"
		case row.Paused:
			state = "paused"
		}

		from := row.From
		if from == "" {
			from = "-"
		}

		_, _ = fmt.Fprintf(writer, "%s\t%d\t%s\t%s\t%s\n", row.Name, row.Jobs, shortConfig(row.SHA), state, from)
	}

	err = flush(writer)
	if err != nil {
		return err
	}

	// A report, not a failure: the list succeeded, and /api/pipelines is the form a script reads.
	for _, row := range rows {
		if row.Broken != "" {
			fmt.Printf("%s is not being served: %s\n", row.Name, row.Broken)
		}
	}

	return nil
}

// PipelineGetCmd prints what a daemon serves, which after a local edit is the only place that configuration still exists.
type PipelineGetCmd struct {
	TargetFlags      `embed:""`
	PipelineNameFlag `embed:""`
}

// Run prints the served source and names the files it carried.
func (p *PipelineGetCmd) Run() error {
	name, err := namedPipeline(p.Pipeline, "pipeline get")
	if err != nil {
		return err
	}

	client := newDaemonClient(p.Target)

	current, held, broken, err := client.get(name)
	if err != nil {
		return err
	}

	if broken != "" {
		return fmt.Errorf("%s holds %q and is not serving it: %s", client.target, name, broken)
	}

	if !held {
		return fmt.Errorf("%s is not serving a pipeline called %q", client.target, name)
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
	name, err := namedPipeline(p.Pipeline, "pipeline destroy")
	if err != nil {
		return err
	}

	// Asked because it is not recoverable: history, versions and the merkle cache go with the row, and nothing here is a soft delete.
	if !p.NonInteractive {
		err = ask("destroy "+name+", everything recorded under it, and its mcp logins?", "not destroyed")
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
	name, err := namedPipeline(pipeline, "pipeline "+verb)
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
	name, err := namedPipeline(p.Pipeline, "pipeline rename")
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
