// Package secret resolves ((source:name)) references through the commands a deployment's config file names: ordered lookup paths, first found wins, "missing" moves on and any other failure stops (Concourse's atc/creds/secret_var_lookup.go).
package secret

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"gopkg.in/yaml.v3"

	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/template"
)

// ErrNotFound is every lookup path answering "missing".
var ErrNotFound = errors.New("secret not found")

// defaultTimeout is longer than any other default in steps: `op read` can wait on a Touch ID prompt a person has to notice.
const defaultTimeout = 2 * time.Minute

const (
	maxValue  = 1 << 20
	maxStderr = 1 << 10
)

var sourceNamePattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

// Config is the `secret:` section of a `steps web --config` file.
type Config struct {
	Source map[string]Source `yaml:"source"`
}

// Source is one named place secrets come from.
type Source struct {
	Command string        `yaml:"command"`
	Timeout time.Duration `yaml:"timeout"`
	Missing string        `yaml:"missing"`
	Lookup  []Path        `yaml:"lookup"`
}

// Path is one lookup path, tried only for the references its when: allows.
type Path struct {
	Path string `yaml:"path"`
	When string `yaml:"when"`
}

// Ref is one reference as the step that needs it sees it.
type Ref struct {
	Pipeline, Job, Resource, Source, Name string
}

func (r Ref) String() string { return "((" + r.Source + ":" + r.Name + "))" }

// Resolver holds the compiled sources.
type Resolver struct {
	sources map[string]*source
}

type source struct {
	command string
	timeout time.Duration
	missing *vm.Program
	lookup  []lookupPath
}

type lookupPath struct {
	template string
	when     *vm.Program
	// usesJob and usesResource say which author-chosen names this path is built from, so only those are held to the segment rule.
	usesJob, usesResource bool
}

// lookupEnv is all a when: rule sees; only pipeline is admin-gated (through set), the rest are the pipeline author's to choose.
type lookupEnv struct {
	Pipeline string `expr:"pipeline"`
	Job      string `expr:"job"`
	Resource string `expr:"resource"`
	Name     string `expr:"name"`
}

type missingEnv struct {
	ExitCode int    `expr:"exit_code"`
	Stderr   string `expr:"stderr"`
}

// Parse refuses unknown keys because a misspelled missing: would quietly become a fall-through to the wrong credential.
func Parse(raw []byte) (*Resolver, error) {
	var config Config

	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)

	err := decoder.Decode(&config)
	if err != nil {
		return nil, fmt.Errorf("secret: %w", err)
	}

	if len(config.Source) == 0 {
		return nil, errors.New("secret: names no source")
	}

	resolver := &Resolver{sources: map[string]*source{}}

	for name, spec := range config.Source {
		compiled, err := compile(spec)
		if err != nil {
			return nil, fmt.Errorf("secret: source %q: %w", name, err)
		}

		if !sourceNamePattern.MatchString(name) {
			return nil, fmt.Errorf("secret: source %q: a name is lower-case letters, digits, _ and -", name)
		}

		resolver.sources[name] = compiled
	}

	return resolver, nil
}

// Has reports whether a source of that name is configured.
func (r *Resolver) Has(name string) bool {
	_, ok := r.sources[name]

	return ok
}

func compile(spec Source) (*source, error) {
	err := checkShape(spec)
	if err != nil {
		return nil, err
	}

	compiled := &source{command: spec.Command, timeout: spec.Timeout}
	if compiled.timeout == 0 {
		compiled.timeout = defaultTimeout
	}

	if spec.Missing != "" {
		compiled.missing, err = expr.Compile(spec.Missing, expr.Env(missingEnv{}), expr.AsBool())
		if err != nil {
			return nil, fmt.Errorf("missing: %w", err)
		}
	}

	for i, path := range spec.Lookup {
		entry, err := compilePath(path)
		if err != nil {
			return nil, fmt.Errorf("lookup[%d].%w", i, err)
		}

		compiled.lookup = append(compiled.lookup, entry)
	}

	return compiled, nil
}

func checkShape(spec Source) error {
	if spec.Command == "" {
		return errors.New("command: is required")
	}

	if len(spec.Lookup) == 0 {
		return errors.New("lookup: needs at least one path")
	}

	// Without it, `op read` exiting 1 for an expired session reads as "missing" and the build falls through to the shared path with the wrong credential.
	if len(spec.Lookup) > 1 && spec.Missing == "" {
		return errors.New("missing: is required with more than one lookup path — without it a failed login would fall through to the next path")
	}

	if spec.Timeout < 0 {
		return fmt.Errorf("timeout: %s is negative", spec.Timeout)
	}

	// A path is built from names a pipeline author chooses, so a command that drops it in unquoted is a shell injection on the daemon; a sentinel holding a quote shows whether it was quoted.
	rendered, err := template.Render(spec.Command, map[string]any{"path": unquotedSentinel})
	if err != nil {
		return fmt.Errorf("command: %w", err)
	}

	if strings.Contains(rendered, unquotedSentinel) {
		return errors.New("command: write {{ .path | shellquote }}; an unquoted path lets a job or resource name run as shell on the daemon")
	}

	return nil
}

const unquotedSentinel = "x'y"

func compilePath(path Path) (lookupPath, error) {
	_, err := template.Render(path.Path, pathData(Ref{}))
	if err != nil {
		return lookupPath{}, fmt.Errorf("path: %w", err)
	}

	entry := lookupPath{template: path.Path, usesJob: strings.Contains(path.Path, ".job"), usesResource: strings.Contains(path.Path, ".resource")}

	if path.When != "" {
		entry.when, err = expr.Compile(path.When, expr.Env(lookupEnv{}), expr.AsBool())
		if err != nil {
			return lookupPath{}, fmt.Errorf("when: %w", err)
		}
	}

	return entry, nil
}

func pathData(ref Ref) map[string]any {
	return map[string]any{"pipeline": ref.Pipeline, "job": ref.Job, "resource": ref.Resource, "name": ref.Name}
}

// Lookup resolves one reference: the first lookup path its source finds wins, "missing" tries the next, anything else stops. The value never appears in an error.
func (r *Resolver) Lookup(ctx context.Context, ref Ref) (string, error) {
	src, ok := r.sources[ref.Source]
	if !ok {
		return "", fmt.Errorf("%s: no secret source %q is configured", ref, ref.Source)
	}

	var tried []string

	for _, path := range src.lookup {
		applies, err := path.applies(ref)
		if err != nil {
			return "", fmt.Errorf("%s: %w", ref, err)
		}

		if !applies {
			continue
		}

		err = path.safeFor(ref)
		if err != nil {
			return "", fmt.Errorf("%s: %w", ref, err)
		}

		location, err := template.Render(path.template, pathData(ref))
		if err != nil {
			return "", fmt.Errorf("%s: %w", ref, err)
		}

		value, found, err := src.fetch(ctx, location)
		if err != nil {
			return "", fmt.Errorf("%s at %s: %w", ref, location, err)
		}

		if found {
			return value, nil
		}

		tried = append(tried, location)
	}

	if len(tried) == 0 {
		return "", fmt.Errorf("%w: %s: no lookup path applies to pipeline %q", ErrNotFound, ref, ref.Pipeline)
	}

	return "", fmt.Errorf("%w: %s is not at %s", ErrNotFound, ref, strings.Join(tried, ", "))
}

// safeFor refuses a job or resource name that would climb out of, or across, a path segment: those names are the pipeline author's, unlike the pipeline's own, which set already holds to this rule.
func (p lookupPath) safeFor(ref Ref) error {
	for _, field := range []struct {
		used        bool
		kind, value string
	}{{p.usesJob, "job", ref.Job}, {p.usesResource, "resource", ref.Resource}} {
		if field.used && (field.value == "." || field.value == ".." || strings.ContainsAny(field.value, "/\\")) {
			return fmt.Errorf("%s name %q cannot be used in a lookup path: it would leave its own path segment", field.kind, field.value)
		}
	}

	return nil
}

func (p lookupPath) applies(ref Ref) (bool, error) {
	if p.when == nil {
		return true, nil
	}

	out, err := expr.Run(p.when, lookupEnv{Pipeline: ref.Pipeline, Job: ref.Job, Resource: ref.Resource, Name: ref.Name})
	if err != nil {
		return false, fmt.Errorf("when: %w", err)
	}

	applies, _ := out.(bool)

	return applies, nil
}

// fetch keeps the daemon's whole environment because `op` and `vault` need theirs, which the step runner's allowlist would strip; the deadline is per attempt because a fall-through runs the source once per path.
func (s *source) fetch(ctx context.Context, location string) (string, bool, error) {
	command, err := template.Render(s.command, map[string]any{"path": location})
	if err != nil {
		return "", false, err //nolint:wrapcheck // Render names the template
	}

	parent := ctx

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	stdout := &capped{max: maxValue}
	stderr := &capped{max: maxStderr}

	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // the deployment's config file names this command; running it is the feature
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second

	// shell.Start puts a headless child in its own session and kills the whole group on cancel, so an abandoned prompt dies with the shell.
	release, err := shell.Start(ctx, cmd)
	if err != nil {
		return "", false, fmt.Errorf("start: %w", err)
	}

	defer release()

	err = cmd.Wait()

	if parent.Err() != nil {
		return "", false, fmt.Errorf("stopped by the step: %w", parent.Err())
	}

	if ctx.Err() != nil {
		return "", false, fmt.Errorf("gave up after %s: %w", s.timeout, ctx.Err())
	}

	if err != nil {
		return s.failed(err, stderr.String())
	}

	if stdout.over {
		return "", false, fmt.Errorf("value is longer than %d bytes", maxValue)
	}

	// Exactly one newline: `op read` without --no-newline returns value\n, and a step prints the trimmed form.
	value := strings.TrimSuffix(stdout.String(), "\n")
	if value == "" {
		return "", false, errors.New("the source printed an empty value")
	}

	return value, true, nil
}

// failed classifies a non-zero exit by the source's missing: rule; only the exit code and stderr are reported, never stdout.
func (s *source) failed(err error, stderr string) (string, bool, error) {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return "", false, fmt.Errorf("run: %w", err)
	}

	code := exitErr.ExitCode()

	if s.missing != nil {
		out, runErr := expr.Run(s.missing, missingEnv{ExitCode: code, Stderr: stderr})
		if runErr != nil {
			return "", false, fmt.Errorf("missing: %w", runErr)
		}

		if missing, _ := out.(bool); missing {
			return "", false, nil
		}
	}

	return "", false, fmt.Errorf("exit %d: %s", code, strings.TrimSpace(stderr))
}

// capped keeps the first max bytes and drops the rest, so a runaway source cannot grow the daemon.
type capped struct {
	bytes.Buffer

	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	room := c.max - c.Len()
	if len(p) > room {
		c.over = true
		_, _ = c.Buffer.Write(p[:max(room, 0)])

		return len(p), nil
	}

	_, _ = c.Buffer.Write(p)

	return len(p), nil
}

// Run memoizes one run's lookups, single-flight, so a step re-placed after a spot reclaim, or two steps asking for the same name, prompt once.
type Run struct {
	resolver *Resolver
	ttl      time.Duration

	mu      sync.Mutex
	entries map[Ref]*runEntry
}

type runEntry struct {
	done  chan struct{}
	value string
	err   error
	at    time.Time
	// ownerCancelled is whether the caller that ran the lookup had its own context end, which says nothing about the secret.
	ownerCancelled bool
}

// ForRun starts a run's memo.
func (r *Resolver) ForRun() *Run {
	return &Run{resolver: r, entries: map[Ref]*runEntry{}}
}

// ForChecks is a memo whose answers expire: a poll has no run to scope one to, and without it a trigger: true resource prompts every interval (Concourse's --secret-cache-duration).
func (r *Resolver) ForChecks(ttl time.Duration) *Run {
	return &Run{resolver: r, ttl: ttl, entries: map[Ref]*runEntry{}}
}

func (run *Run) expired(entry *runEntry) bool {
	if run.ttl == 0 {
		return false
	}

	select {
	case <-entry.done:
		return time.Since(entry.at) > run.ttl
	default:
		return false
	}
}

// Lookup answers from the memo; a failure is not kept, so a declined prompt can be retried by the next attempt.
func (run *Run) Lookup(ctx context.Context, ref Ref) (string, error) {
	for {
		run.mu.Lock()

		entry, cached := run.entries[ref]
		if entry == nil || run.expired(entry) {
			entry = &runEntry{done: make(chan struct{})}
			run.entries[ref] = entry
			cached = false
		}

		run.mu.Unlock()

		if !cached {
			return run.lead(ctx, ref, entry)
		}

		select {
		case <-entry.done:
			// The owner's cancellation is no answer about the secret: a waiter whose own context is live asks again rather than fail with it.
			if entry.ownerCancelled && ctx.Err() == nil {
				continue
			}

			return entry.value, entry.err
		case <-ctx.Done():
			return "", fmt.Errorf("%s: %w", ref, ctx.Err())
		}
	}
}

func (run *Run) lead(ctx context.Context, ref Ref, entry *runEntry) (string, error) {
	entry.value, entry.err = run.resolver.Lookup(ctx, ref)
	entry.at = time.Now()
	entry.ownerCancelled = ctx.Err() != nil

	if entry.err != nil {
		run.mu.Lock()
		delete(run.entries, ref)
		run.mu.Unlock()
	}

	close(entry.done)

	return entry.value, entry.err
}
