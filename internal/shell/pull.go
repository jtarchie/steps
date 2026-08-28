package shell

// Making sure a pipeline's images are on the daemon before any step runs.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jtarchie/steps/internal/dockerapi"
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
// Present images are skipped by asking the daemon, which is a local call: a
// warm run adds a few milliseconds and no network at all. That check is also
// what keeps a locally-built image (one that exists in no registry) working —
// it is found, so nothing is pulled.
//
// Progress goes to the terminal rather than being captured. It is startup
// output for the operator, not a command's result.
//
// One client for the whole sweep rather than one per image: connecting is the
// expensive half of asking about an image that is already there.
func PrepareImages(ctx context.Context, images []string) error {
	if len(images) == 0 {
		return nil
	}

	client, err := dockerapi.New("")
	if err != nil {
		return fmt.Errorf("preparing images: %w", err)
	}

	defer func() { _ = client.Close() }()

	for _, image := range images {
		if client.ImagePresent(ctx, image) {
			slog.Debug("shell.docker.image_present", "image", image)

			continue
		}

		err := pullImage(ctx, client, image)
		if err != nil {
			return err
		}
	}

	return nil
}

func pullImage(ctx context.Context, client *dockerapi.Client, image string) error {
	ctx, cancel := context.WithTimeout(ctx, dockerPullTimeout)
	defer cancel()

	fmt.Printf("pulling image: %s\n", image)
	slog.Debug("shell.docker.image_pull", "image", image)

	err := client.Pull(ctx, image, os.Stdout)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}
