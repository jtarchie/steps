package dockerapi

// Making sure an image is on the daemon.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// ImagePresent reports whether the daemon already holds image.
//
// A local call, which is what makes it worth asking before every pull: a warm
// daemon costs milliseconds and no network. It is also what keeps a
// locally-BUILT image working — one that exists in no registry is found here,
// so nothing is pulled and nothing 404s.
//
// Two answers mean absent, and the second is easy to miss: an image name the
// daemon cannot even PARSE ("--privileged", "NOT A REF") is an invalid
// argument rather than a missing image. Reading that as present pushes the
// failure into whichever step first needs it, reported as a container that
// would not start; reading it as absent sends it to the pull, which names the
// value and fails startup. The docker CLI behaves the same way, because
// `docker image inspect` exits nonzero for both.
//
// Anything else — a daemon that is unreachable or unwell — is reported as
// present, deliberately. That is a problem the pull would hit too, and the
// pull says so far better than a message synthesised here would.
func (c *Client) ImagePresent(ctx context.Context, image string) bool {
	_, err := c.api.ImageInspect(ctx, image)
	if err == nil {
		return true
	}

	return !errdefs.IsNotFound(err) && !errdefs.IsInvalidArgument(err)
}

// IsImageNotFound reports whether err is the daemon saying it does not have
// the image asked for.
//
// Exported because creating a container is the one place a caller has to act
// on it rather than report it: `docker run` pulled implicitly, and the engine
// API does not, so the caller restores that by pulling and asking again.
func IsImageNotFound(err error) bool {
	return errdefs.IsNotFound(err)
}

// Pull fetches image, reporting the daemon's progress to progress.
//
// The reporting is not decoration. An image pull is minutes of silence
// otherwise, and it happens at startup where the operator is the only
// audience — the same reason the docker CLI draws one and the same reason this
// is not routed through a logger.
//
// The errors are the subtle part: a pull reports failure INSIDE the stream
// rather than on the HTTP call, so a caller that only checked what ImagePull
// returned would read "repository does not exist" as a successful pull and
// fail later, in a step, on an image that was never fetched.
func (c *Client) Pull(ctx context.Context, image string, progress io.Writer) error {
	response, err := c.api.ImagePull(ctx, image, client.ImagePullOptions{
		RegistryAuth: RegistryAuth(ctx, image),
	})
	if err != nil {
		return fmt.Errorf("pulling image %q: %w", image, err)
	}

	defer func() { _ = response.Close() }()

	err = reportPullProgress(response, progress)
	if err != nil {
		return fmt.Errorf("pulling image %q: %w", image, err)
	}

	return nil
}

// pullMessage is one line of the daemon's pull stream.
type pullMessage struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Progress string `json:"progress"`
	Error    string `json:"error"`
}

// reportPullProgress writes the stream out as lines and returns whatever the
// daemon said went wrong.
//
// Lines, not a redrawn progress bar. The bar the CLI draws needs to own the
// cursor, and this output is interleaved with the rest of a run: the same
// information arrives as `<layer>: <status>`, which is also exactly what
// docker itself prints when its stdout is not a terminal.
//
// The byte-by-byte updates are dropped for the same reason docker drops them
// without a terminal — they are the same status repeated a thousand times,
// and only the transitions carry information.
func reportPullProgress(stream io.Reader, progress io.Writer) error {
	decoder := json.NewDecoder(stream)

	for {
		var message pullMessage

		err := decoder.Decode(&message)
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("reading the daemon's progress: %w", err)
		}

		if message.Error != "" {
			return errors.New(message.Error)
		}

		if message.Progress != "" || message.Status == "" {
			continue
		}

		if message.ID == "" {
			_, _ = fmt.Fprintf(progress, "%s\n", message.Status)

			continue
		}

		_, _ = fmt.Fprintf(progress, "%s: %s\n", message.ID, message.Status)
	}
}
