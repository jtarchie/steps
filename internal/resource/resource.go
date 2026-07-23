// Package resource runs a resource type's check/in/out shell commands and
// selects among the versions a check returns.
package resource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/template"
)

// CheckVersions renders rt.Config.Check against {"source": source}, runs it,
// and parses stdout as a JSON array into []map[string]any. Ordering
// (oldest-first) is entirely the check command's responsibility, per
// Concourse convention — no sorting happens here (see docs/conformance.md
// and internal/resource/resource_test.go's TestSelectVersion/"latest when
// unpinned"; Concourse doc: concourse-ci.org/docs/resource-types/
// implementing/, "check" section).
//
// When rt.Config.MCP is set, this calls its check: tool instead (see
// mcpCheckVersions) — cfg is needed only for that path, to resolve the
// referenced mcp_servers: entry; the shell path below ignores it, so a nil
// cfg is fine whenever the caller knows rt isn't mcp-backed.
func CheckVersions(ctx context.Context, cfg *config.Config, rt config.ResourceType, source map[string]any) ([]map[string]any, error) {
	if rt.Config.MCP != nil {
		return mcpCheckVersions(ctx, cfg, rt, source)
	}

	slog.Debug("resource.check", "resource_type", rt.Name, "source", source)

	command, err := template.Render(rt.Config.Check, map[string]any{"source": source})
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	runner, err := shell.NewRunner(rt.Image, "")
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	out, err := runner.RunCapture(ctx, command)
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	var versions []map[string]any

	err = json.Unmarshal(out, &versions)
	if err != nil {
		slog.Debug("resource.check", "resource_type", rt.Name, "output", string(out))

		return nil, fmt.Errorf("check %q: could not parse JSON output: %w", rt.Name, err)
	}

	slog.Info("resource.checked", "resource_type", rt.Name, "versions", len(versions))

	return versions, nil
}

// VersionMode returns a get step's version selection mode ("latest",
// "every", or "pinned" with its pinned map), per Concourse's get.version
// field convention: unset or the string "latest" means latest, the string
// "every" means every version, and a map means pinned to a specific version.
func VersionMode(step config.Step) (mode string, pinned map[string]string) {
	switch v := step.Version.(type) {
	case string:
		if v == "every" {
			return "every", nil
		}

		return "latest", nil
	case map[string]any:
		m := make(map[string]string, len(v))
		for k, val := range v {
			m[k] = fmt.Sprint(val)
		}

		return "pinned", m
	case map[string]string:
		return "pinned", v
	default:
		return "latest", nil
	}
}

// SelectVersion returns the version matching all key/values in pinned, or
// (if pinned is empty) the last element of versions (latest, by convention).
func SelectVersion(versions []map[string]any, pinned map[string]string) (map[string]any, error) {
	slog.Debug("resource.select_version", "versions", len(versions), "pinned", pinned)

	if len(versions) == 0 {
		return nil, errors.New("no versions available")
	}

	if len(pinned) == 0 {
		version := versions[len(versions)-1]
		slog.Debug("resource.select_version", "version", version, "reason", "latest")

		return version, nil
	}

	for _, version := range versions {
		if matchesPin(version, pinned) {
			slog.Debug("resource.select_version", "version", version, "reason", "pinned", "pinned", pinned)

			return version, nil
		}
	}

	return nil, fmt.Errorf("no version matches pin %v", pinned)
}

func matchesPin(version map[string]any, pinned map[string]string) bool {
	for key, want := range pinned {
		got, ok := version[key]
		if !ok {
			return false
		}

		if fmt.Sprint(got) != want {
			return false
		}
	}

	return true
}

// RunIn renders rt.Config.In against {"source": source, "version": version}
// and executes it with cwd = destDir (caller ensures destDir exists).
//
// When rt.Config.MCP is set, this calls mcpRunIn instead — see its doc
// comment for the in:-omitted default (writing version.json) and the
// materialization convention when in: is set.
func RunIn(ctx context.Context, cfg *config.Config, rt config.ResourceType, source, version map[string]any, destDir string) error {
	if rt.Config.MCP != nil {
		return mcpRunIn(ctx, cfg, rt, source, version, destDir)
	}

	slog.Debug("resource.in", "resource_type", rt.Name, "source", source, "version", version, "dest_dir", destDir)

	command, err := template.Render(rt.Config.In, map[string]any{"source": source, "version": version})
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	runner, err := shell.NewRunner(rt.Image, destDir)
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	err = runner.Run(ctx, command)
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	slog.Info("resource.fetched", "resource_type", rt.Name, "dest_dir", destDir)

	return nil
}

// RunOut renders rt.Config.Out against {"source": source, "params": params}
// and executes it with cwd = srcDir. If stdout parses as a JSON object it's
// returned as result (loosely mirroring check's convention of emitting the
// version produced); unparsable or empty stdout is not an error — result
// is simply nil, since many out scripts won't emit anything (see
// docs/conformance.md and TestConformanceRunOutUnparsableStdoutIsNilNotError
// in resource_test.go).
//
// When rt.Config.MCP is set, this calls mcpRunOut instead. rt.Config.MCP.Out
// is itself optional (see validateMCPResourcePuts, which rejects a put step
// targeting an mcp-backed type with no out: at load time), so this is only
// ever reached with it set.
func RunOut(ctx context.Context, cfg *config.Config, rt config.ResourceType, source, params map[string]any, srcDir string) (map[string]any, error) {
	if rt.Config.MCP != nil {
		return mcpRunOut(ctx, cfg, rt, source, params)
	}

	slog.Debug("resource.out", "resource_type", rt.Name, "source", source, "params", params, "src_dir", srcDir)

	command, err := template.Render(rt.Config.Out, map[string]any{"source": source, "params": params})
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", rt.Name, err)
	}

	runner, err := shell.NewRunner(rt.Image, srcDir)
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", rt.Name, err)
	}

	out, err := runner.RunCapture(ctx, command)
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", rt.Name, err)
	}

	var result map[string]any

	unmarshalErr := json.Unmarshal(out, &result)
	if unmarshalErr != nil {
		slog.Debug("resource.out", "resource_type", rt.Name, "output", string(out), "parse_error", unmarshalErr)

		return nil, nil //nolint:nilnil // unparsable/empty stdout is not an error; nil result means "no version produced"
	}

	slog.Info("resource.put", "resource_type", rt.Name, "src_dir", srcDir, "result", result)

	return result, nil
}

// ResolveVersions determines which version(s) of a get step's resource to
// fetch. An explicit CLI pin always wins (a single version); otherwise the
// step's own version: field decides between latest (default, a single
// version), every (all versions returned by check), or a YAML-pinned
// version. Both the merkle planner and the executor call ResolveVersions so
// plan-time hashing and run-time execution stay in lockstep.
func ResolveVersions(ctx context.Context, cfg *config.Config, step config.Step, cliPinned map[string]string) (*config.Resource, *config.ResourceType, []map[string]any, error) {
	res, err := cfg.FindResource(step.GetResourceName())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get %q: %w", step.Get, err)
	}

	resourceType, err := cfg.FindResourceType(res.Type)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get %q: %w", step.Get, err)
	}

	versions, err := CheckVersions(ctx, cfg, *resourceType, res.Source)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get %q: %w", step.Get, err)
	}

	// A CLI --version pin always wins; otherwise the step's own version:
	// field decides. version:every is the only path that fans out to more
	// than one version — every other path narrows to a single pin (an empty
	// pin meaning "latest").
	pin := cliPinned
	if len(pin) == 0 {
		mode, stepPinned := VersionMode(step)
		slog.Debug("resource.version_mode", "resource", res.Name, "mode", mode)

		if mode == "every" {
			return res, resourceType, versions, nil
		}

		pin = stepPinned
	}

	version, err := SelectVersion(versions, pin)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get %q: %w", step.Get, err)
	}

	return res, resourceType, []map[string]any{version}, nil
}
