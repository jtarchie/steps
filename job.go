package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// RunJob executes job's plan steps in order inside workspaceDir (a fresh
// temp directory). pinned applies to any `get` step's version selection.
func RunJob(ctx context.Context, cfg *Config, job *Job, pinned map[string]string, workspaceDir string) error {
	slog.Info("job.run", "job", job.Name, "steps", len(job.Plan), "workspace_dir", workspaceDir)

	for i, step := range job.Plan {
		switch {
		case step.Get != "":
			slog.Debug("job.step", "job", job.Name, "index", i, "kind", "get", "resource", step.Get)

			err := runGetStep(ctx, cfg, step, pinned, workspaceDir)
			if err != nil {
				err = fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
				slog.Error("job.step", "job", job.Name, "index", i, "kind", "get", "resource", step.Get, "error", err)

				return err
			}
		case step.Task != "":
			slog.Debug("job.step", "job", job.Name, "index", i, "kind", "task", "task", step.Task, "run", step.Run)

			fmt.Printf("task: %s\n", step.Task)

			err := RunShell(ctx, step.Run, workspaceDir)
			if err != nil {
				err = fmt.Errorf("step %d (task %q): %w", i, step.Task, err)
				slog.Error("job.step", "job", job.Name, "index", i, "kind", "task", "task", step.Task, "error", err)

				return err
			}
		default:
			err := fmt.Errorf("step %d: unrecognized step (must be get or task)", i)
			slog.Error("job.step", "job", job.Name, "index", i, "error", err)

			return err
		}
	}

	slog.Info("job.done", "job", job.Name)

	return nil
}

func runGetStep(ctx context.Context, cfg *Config, step Step, pinned map[string]string, workspaceDir string) error {
	resource, err := cfg.FindResource(step.Get)
	if err != nil {
		return err
	}

	resourceType, err := cfg.FindResourceType(resource.Type)
	if err != nil {
		return err
	}

	versions, err := CheckVersions(ctx, *resourceType, resource.Source)
	if err != nil {
		return err
	}

	version, err := SelectVersion(versions, pinned)
	if err != nil {
		return err
	}

	fmt.Printf("get: %s (version: %v)\n", resource.Name, version)

	destDir := filepath.Join(workspaceDir, resource.Name)

	err = os.MkdirAll(destDir, 0o755)
	if err != nil {
		err = fmt.Errorf("could not create resource dir %q: %w", destDir, err)
		slog.Error("step.get", "resource", resource.Name, "dest_dir", destDir, "error", err)

		return err
	}

	return RunIn(ctx, *resourceType, resource.Source, version, destDir)
}
