package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// CheckVersions renders rt.Config.Check against {"source": source}, runs it,
// and parses stdout as a JSON array into []map[string]any. Ordering
// (oldest-first) is entirely the check command's responsibility, per
// Concourse convention — no sorting happens here.
func CheckVersions(ctx context.Context, rt ResourceType, source map[string]any) ([]map[string]any, error) {
	command, err := Render(rt.Config.Check, map[string]any{"source": source})
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	out, err := RunShellCapture(ctx, command, "")
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	var versions []map[string]any

	err = json.Unmarshal(out, &versions)
	if err != nil {
		return nil, fmt.Errorf("check %q: could not parse JSON output: %w", rt.Name, err)
	}

	return versions, nil
}

// SelectVersion returns the version matching all key/values in pinned, or
// (if pinned is empty) the last element of versions (latest, by convention).
func SelectVersion(versions []map[string]any, pinned map[string]string) (map[string]any, error) {
	if len(versions) == 0 {
		return nil, fmt.Errorf("no versions available")
	}

	if len(pinned) == 0 {
		return versions[len(versions)-1], nil
	}

	for _, version := range versions {
		if matchesPin(version, pinned) {
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
func RunIn(ctx context.Context, rt ResourceType, source, version map[string]any, destDir string) error {
	command, err := Render(rt.Config.In, map[string]any{"source": source, "version": version})
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	err = RunShell(ctx, command, destDir)
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	return nil
}
