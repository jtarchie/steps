package tree

// Deciding which tree a step's file tools work against, and telling the model where it is.

import (
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// ForStep answers the tree a step's file tools should use and the working directory the model should be told about. With no image: both are this process's own; with one, the file tools move into the container so that they and run_shell address a single copy of the tree, and the directory named is the CONTAINER's — every path handed back to the model now comes from there, and for a placed step this machine's spelling of it names a path on the wrong host. An empty grant is the default file tools, so it goes into the container too; run_shell counts though it needs no tree, because the probe also settles the directory the model is TOLD.
func ForStep(ctx context.Context, runner shell.Runner, image, dir string, specs []config.ToolSpec, lost *Lost) (Tree, string, error) {
	if image == "" || !config.ReachesTree(specs) {
		return Host{Dir: dir}, dir, nil
	}

	caps, err := probeUserland(ctx, runner, image)
	if err != nil {
		return nil, "", fmt.Errorf("%w", err)
	}

	return Container{Runner: runner, dir: caps.pwd, Lost: lost}, caps.pwd, nil
}
