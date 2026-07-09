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

	err := runSteps(ctx, cfg, job.Name, job.Plan, pinned, workspaceDir)
	if err != nil {
		return err
	}

	slog.Info("job.done", "job", job.Name)

	return nil
}

// runSteps executes steps in order. A `get` step fans out: for each version
// it selects (a single version normally, or every version returned by
// check when version:every is set), that version triggers its own build
// of the remainder of the plan — see runTriggeredBuild.
func runSteps(ctx context.Context, cfg *Config, jobName string, steps []Step, pinned map[string]string, workspaceDir string) error {
	for i, step := range steps {
		switch {
		case step.Get != "":
			resource, resourceType, versions, err := resolveGetVersions(ctx, cfg, step, pinned)
			if err != nil {
				return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
			}

			slog.Debug("job.step", "job", jobName, "index", i, "kind", "get", "resource", step.Get, "versions", len(versions))

			for _, version := range versions {
				err := runTriggeredBuild(ctx, cfg, jobName, *resource, *resourceType, version, steps[i+1:], pinned)
				if err != nil {
					return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
				}
			}

			return nil
		case step.Task != "":
			slog.Debug("job.step", "job", jobName, "index", i, "kind", "task", "task", step.Task, "run", step.Run)

			fmt.Printf("task: %s\n", step.Task)

			err := RunShell(ctx, step.Run, workspaceDir)
			if err != nil {
				return fmt.Errorf("step %d (task %q): %w", i, step.Task, err)
			}
		default:
			return fmt.Errorf("step %d: unrecognized step (must be get or task)", i)
		}
	}

	return nil
}

// resolveGetVersions determines which version(s) of a get step's resource
// to fetch. An explicit CLI pin always wins (a single version); otherwise
// the step's own version: field decides between latest (default, a single
// version), every (all versions returned by check), or a YAML-pinned
// version.
func resolveGetVersions(ctx context.Context, cfg *Config, step Step, cliPinned map[string]string) (*Resource, *ResourceType, []map[string]any, error) {
	resource, err := cfg.FindResource(step.Get)
	if err != nil {
		return nil, nil, nil, err
	}

	resourceType, err := cfg.FindResourceType(resource.Type)
	if err != nil {
		return nil, nil, nil, err
	}

	versions, err := CheckVersions(ctx, *resourceType, resource.Source)
	if err != nil {
		return nil, nil, nil, err
	}

	// A CLI --version pin always wins; otherwise the step's own version:
	// field decides. version:every is the only path that fans out to more
	// than one version — every other path narrows to a single pin (an empty
	// pin meaning "latest").
	pin := cliPinned
	if len(pin) == 0 {
		mode, stepPinned := VersionMode(step)
		slog.Debug("resource.version_mode", "resource", resource.Name, "mode", mode)

		if mode == "every" {
			return resource, resourceType, versions, nil
		}

		pin = stepPinned
	}

	version, err := SelectVersion(versions, pin)
	if err != nil {
		return nil, nil, nil, err
	}

	return resource, resourceType, []map[string]any{version}, nil
}

// runTriggeredBuild runs the build that a single resource version triggers:
// per Concourse's model, the version triggering a get is what starts a
// build, and every build gets its own isolated working directory. So this
// creates a fresh workspace for just this version, fetches the version
// into it, runs the remainder of the plan inside it, and tears the
// workspace down afterward — never sharing it with any other triggered
// build, including sibling versions fanned out by version:every.
func runTriggeredBuild(ctx context.Context, cfg *Config, jobName string, resource Resource, resourceType ResourceType, version map[string]any, remainder []Step, pinned map[string]string) error {
	buildWorkspace, err := os.MkdirTemp("", "steps-*")
	if err != nil {
		return fmt.Errorf("could not create workspace for %q: %w", resource.Name, err)
	}

	slog.Debug("workspace.create", "dir", buildWorkspace, "resource", resource.Name, "version", version)

	defer func() {
		removeErr := os.RemoveAll(buildWorkspace)
		if removeErr != nil {
			slog.Error("workspace.remove", "dir", buildWorkspace, "error", removeErr)

			return
		}

		slog.Debug("workspace.remove", "dir", buildWorkspace)
	}()

	err = fetchGetStep(ctx, resource, resourceType, version, buildWorkspace)
	if err != nil {
		return err
	}

	return runSteps(ctx, cfg, jobName, remainder, pinned, buildWorkspace)
}

// fetchGetStep places one version of a resource into workspaceDir/<name>.
func fetchGetStep(ctx context.Context, resource Resource, resourceType ResourceType, version map[string]any, workspaceDir string) error {
	fmt.Printf("get: %s (version: %v)\n", resource.Name, version)

	destDir := filepath.Join(workspaceDir, resource.Name)

	err := os.MkdirAll(destDir, 0o750)
	if err != nil {
		return fmt.Errorf("could not create resource dir %q: %w", destDir, err)
	}

	return RunIn(ctx, resourceType, resource.Source, version, destDir)
}
