package resource

// A Concourse resource image's own scripts (steps#237): /opt/resource/check, in and out, each handed Concourse's request as JSON on stdin — never on a command line, since source: is where these images take credentials — and in and out answering {version, metadata} on stdout.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
)

type concourseBackend struct{}

// concourseRequest is what a Concourse script reads on stdin. The resource's resolved secrets: are laid over its source:, which is how a credential reaches an image that only knows source:; a first check carries no version at all, as Concourse sends none.
func concourseRequest(ctx context.Context, c Call, version, params map[string]any) (string, error) {
	source := maps.Clone(c.Resource.Source)
	if source == nil {
		source = map[string]any{}
	}

	for name, value := range shell.SecretEnv(ctx) {
		source[name] = value
	}

	request := map[string]any{"source": source}
	if len(version) > 0 {
		request["version"] = version
	}

	if len(params) > 0 {
		request["params"] = params
	}

	encoded, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("encoding the request: %w", err)
	}

	return string(encoded), nil
}

func (concourseBackend) check(ctx context.Context, c Call) ([]map[string]any, error) {
	rt := c.Type

	request, err := concourseRequest(ctx, c, c.Version, nil)
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	spec := runnerSpec(rt, c.Resource.Env)

	// Keyed on the request, not the script: every Concourse type runs the same script, and what tells two checks apart is the source: they ask about.
	share, key := sharedCheckFor(ctx, rt.Config.Check+"\x00"+request, spec)

	run := func() ([]map[string]any, error) { return runConcourseCheck(ctx, c, spec, request) }

	if share != nil && len(c.Resource.Secrets) == 0 {
		return share.do(ctx, key, run)
	}

	return run()
}

func runConcourseCheck(ctx context.Context, c Call, spec shell.RunnerSpec, request string) ([]map[string]any, error) {
	rt := c.Type

	runner, err := newRunner(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	runner = runner.WithLabel(rt.Name + " check")
	defer shell.CloseRunner(runner, rt.Name+" check")

	out, err := shell.RunCaptureInput(shell.WithBuildMetadata(ctx, shell.BuildMetadata{}), runner, rt.Config.Check, request)
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	versions, err := DecodeVersions(out)
	if err != nil {
		return nil, fmt.Errorf("check %q: could not parse JSON output: %w", rt.Name, err)
	}

	slog.InfoContext(ctx, "resource.checked", "resource_type", rt.Name, "versions", len(versions))

	return versions, nil
}

func (concourseBackend) in(ctx context.Context, c Call) error {
	rt := c.Type

	request, err := concourseRequest(ctx, c, c.Version, c.Params)
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	spec := runnerSpec(rt, c.Resource.Env)
	spec.Cwd, spec.Workdir, spec.FetchAll = c.Dir, shell.GetDir, true

	_, err = runConcourseScript(ctx, spec, rt.Name+" in", rt.Config.In, request)
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	return nil
}

func (concourseBackend) out(ctx context.Context, c Call) (map[string]any, error) {
	rt := c.Type

	request, err := concourseRequest(ctx, c, nil, c.Params)
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", rt.Name, err)
	}

	spec := runnerSpec(rt, c.Resource.Env)
	spec.Cwd, spec.Workdir = c.Dir, shell.PutDir

	version, err := runConcourseScript(ctx, spec, rt.Name+" out", rt.Config.Out, request)
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", rt.Name, err)
	}

	return version, nil
}

// runConcourseScript runs in or out and unwraps what it answered.
func runConcourseScript(ctx context.Context, spec shell.RunnerSpec, label, script, request string) (map[string]any, error) {
	runner, err := newRunner(ctx, spec)
	if err != nil {
		return nil, err
	}

	defer shell.CloseRunner(runner, label)

	out, err := shell.RunCaptureInput(ctx, runner, script, request)
	if err != nil {
		return nil, err //nolint:wrapcheck // the caller names the stage
	}

	return unwrapConcourseAnswer(ctx, out)
}

// concourseAnswer is what in and out print: the version, and name/value pairs Concourse shows on the build.
type concourseAnswer struct {
	Version  map[string]any `json:"version"`
	Metadata []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"metadata"`
}

// errNullVersion is Concourse's own refusal: a script that answered with no version produced nothing a later step could name.
var errNullVersion = errors.New("the script answered with no version")

// unwrapConcourseAnswer takes the version out of {version, metadata} and reports the metadata as the stage's facts.
func unwrapConcourseAnswer(ctx context.Context, out []byte) (map[string]any, error) {
	var answer concourseAnswer

	_, err := decodeExact(out, &answer)
	if err != nil {
		return nil, fmt.Errorf("could not parse the script's answer as {version, metadata}: %w", err)
	}

	if len(answer.Version) == 0 {
		return nil, errNullVersion
	}

	facts := make([]events.Fact, 0, len(answer.Metadata))
	for _, field := range answer.Metadata {
		facts = append(facts, events.Fact{Name: field.Name, Value: field.Value})
	}

	reportFacts(ctx, facts)

	return answer.Version, nil
}
