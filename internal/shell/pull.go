package shell

// Making sure a pipeline's images are on the daemon before any step runs.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

// dockerPullTimeout bounds one image pull. Generous: a large image on a slow
// link is normal, and the alternative to waiting here is waiting inside a step
// whose own timeout was never sized for a download.
const dockerPullTimeout = 30 * time.Minute

// PrepareImages makes sure every image the pipeline can execute in is present
// on the daemon, pulling the ones that aren't.
//
// This exists because an implicit pull is charged to the wrong place. Docker
// pulls on first use, so without this the first command needing an uncached
// image pays for the download inside its own step: the progress output goes
// wherever that command's stderr goes — mixed into a resource check's parsed
// output, or into an agent's tool result — and the time counts against the
// step's timeout, so a big image on a cold daemon can burn a budget meant for
// the work. Pulling up front makes it startup cost, visible as such.
//
// Present images are skipped via `docker image inspect`, which is a local
// call: a warm run adds a few milliseconds and no network at all. That check
// is also what keeps a locally-built image (one that exists in no registry)
// working — it is found, so nothing is pulled.
//
// Progress streams to the terminal rather than being captured. It is startup
// output for the operator, not a command's result.
func PrepareImages(ctx context.Context, images []string) error {
	for _, image := range images {
		if imagePresent(ctx, image) {
			slog.Debug("shell.docker.image_present", "image", image)

			continue
		}

		err := pullImage(ctx, image)
		if err != nil {
			return err
		}
	}

	return nil
}

// imagePresent reports whether the daemon already has image locally.
func imagePresent(ctx context.Context, image string) bool {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", "--", image) //nolint:gosec // image is validated at load time (checkImageValue) and passed positionally
	cmd.Stdout = nil
	cmd.Stderr = nil

	return cmd.Run() == nil
}

func pullImage(ctx context.Context, image string) error {
	ctx, cancel := context.WithTimeout(ctx, dockerPullTimeout)
	defer cancel()

	fmt.Printf("pulling image: %s\n", image)
	slog.Debug("shell.docker.image_pull", "image", image)

	cmd := exec.CommandContext(ctx, "docker", "pull", "--", image) //nolint:gosec // image is validated at load time (checkImageValue) and passed positionally
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("pulling image %q: %w", image, err)
	}

	return nil
}
