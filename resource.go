package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// CheckVersions renders rt.Config.Check against {"source": source}, runs it,
// and parses stdout as a JSON array into []map[string]any. Ordering
// (oldest-first) is entirely the check command's responsibility, per
// Concourse convention — no sorting happens here.
func CheckVersions(ctx context.Context, rt ResourceType, source map[string]any) ([]map[string]any, error) {
	slog.Debug("resource.check", "resource_type", rt.Name, "source", source)

	command, err := Render(rt.Config.Check, map[string]any{"source": source})
	if err != nil {
		err = fmt.Errorf("check %q: %w", rt.Name, err)
		slog.Error("resource.check", "resource_type", rt.Name, "error", err)

		return nil, err
	}

	out, err := RunShellCapture(ctx, command, "")
	if err != nil {
		err = fmt.Errorf("check %q: %w", rt.Name, err)
		slog.Error("resource.check", "resource_type", rt.Name, "command", command, "error", err)

		return nil, err
	}

	var versions []map[string]any

	err = json.Unmarshal(out, &versions)
	if err != nil {
		err = fmt.Errorf("check %q: could not parse JSON output: %w", rt.Name, err)
		slog.Error("resource.check", "resource_type", rt.Name, "output", string(out), "error", err)

		return nil, err
	}

	slog.Info("resource.checked", "resource_type", rt.Name, "versions", len(versions))

	return versions, nil
}

// SelectVersion returns the version matching all key/values in pinned, or
// (if pinned is empty) the last element of versions (latest, by convention).
func SelectVersion(versions []map[string]any, pinned map[string]string) (map[string]any, error) {
	slog.Debug("resource.select_version", "versions", len(versions), "pinned", pinned)

	if len(versions) == 0 {
		err := fmt.Errorf("no versions available")
		slog.Error("resource.select_version", "pinned", pinned, "error", err)

		return nil, err
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

	err := fmt.Errorf("no version matches pin %v", pinned)
	slog.Error("resource.select_version", "pinned", pinned, "versions", versions, "error", err)

	return nil, err
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
func RunIn(ctx context.Context, rt ResourceType, source, version map[string]any, destDir string) error {
	slog.Debug("resource.in", "resource_type", rt.Name, "source", source, "version", version, "dest_dir", destDir)

	command, err := Render(rt.Config.In, map[string]any{"source": source, "version": version})
	if err != nil {
		err = fmt.Errorf("in %q: %w", rt.Name, err)
		slog.Error("resource.in", "resource_type", rt.Name, "error", err)

		return err
	}

	err = RunShell(ctx, command, destDir)
	if err != nil {
		err = fmt.Errorf("in %q: %w", rt.Name, err)
		slog.Error("resource.in", "resource_type", rt.Name, "command", command, "dest_dir", destDir, "error", err)

		return err
	}

	slog.Info("resource.fetched", "resource_type", rt.Name, "dest_dir", destDir)

	return nil
}
