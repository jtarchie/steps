package agent

// Deciding which tree a step's file tools work against, and telling the model where it is.

import (
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// fileToolNames are the builtins that read or write the step's directory, and so the ones that have to follow run_shell into a container. ask_user, web_fetch, the verdict and a sub-agent's own conversation have no filesystem of their own to disagree about.
var fileToolNames = map[string]bool{
	"read_file":    true,
	"list_dir":     true,
	"write_file":   true,
	"edit_file":    true,
	"search_files": true,
}

// reachesTheContainer reports whether any grant would run inside the step's container — a file tool, run_shell, or a custom tool, whose run: is a shell command like any other.
//
// run_shell counts even though it needs no tree of its own, because the probe answers a second question: what the model is TOLD its working directory is. A placed agent granted only run_shell would otherwise be handed the orchestrator's spelling of a directory that only exists on the worker.
//
// An agent granted none of them (web_fetch and a verdict, say) starts no container at all, which is what it did before any of this.
func reachesTheContainer(specs []config.ToolSpec) bool {
	for _, spec := range specs {
		if fileToolNames[spec.Builtin] || spec.Builtin == "run_shell" || spec.Run != "" {
			return true
		}
	}

	return false
}

// resolveStepTree answers the tree a step's file tools should use and the working directory the model should be told about. With no image: both are this process's own; with one, the file tools move into the container so that they and run_shell address a single copy of the tree, and the directory named is the CONTAINER's — every path handed back to the model now comes from there, and for a placed step this machine's spelling of it names a path on the wrong host.
func resolveStepTree(ctx context.Context, runner shell.Runner, image, dir string, specs []config.ToolSpec, lost *lostTree) (tree, string, error) {
	if image == "" || !reachesTheContainer(specs) {
		return hostTree{dir: dir}, dir, nil
	}

	caps, err := probeUserland(ctx, runner, image)
	if err != nil {
		return nil, "", fmt.Errorf("%w", err)
	}

	return containerTree{runner: runner, dir: caps.pwd, lost: lost}, caps.pwd, nil
}
