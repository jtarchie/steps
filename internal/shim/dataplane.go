package shim

// The URL data plane: tree bytes over plain HTTP against URLs the
// orchestrator minted, so the tunnel carries control frames only. The shim
// holds no credentials — the URL is the whole authority, and this end speaks
// nothing but net/http.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/jtarchie/steps/internal/compress"
	"github.com/jtarchie/steps/internal/wire"
)

// errNoURL is a transfer frame that named no URL on a session that negotiated
// the URL plane — an orchestrator bug, not a worker condition.
var errNoURL = errors.New("the data plane is urls and the frame carried no url")

// downloadTree streams the blob at the frame's URL into the work directory.
// The blob format is fixed — zstd over the tar codec's stream — independent
// of the tunnel's own compression negotiation: it describes the object, not
// the pipe.
func (s *session) downloadTree(ctx context.Context, frame wire.Frame) error {
	var upload wire.Upload

	err := wire.DecodeJSON(frame, &upload)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	if upload.URL == "" {
		return errNoURL
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, upload.URL, nil)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("the store answered %s", response.Status) //nolint:err113 // carries a status only the far end can act on
	}

	err = compress.Unpack(response.Body, true, func(r io.Reader) error {
		return wire.UnpackTree(r, s.workdir)
	})
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// uploadOutputs packs the named outputs and PUTs them to the fetch's URL.
// Packed to a file first: S3 rejects a PUT without a length, and outputs can
// be larger than memory.
func (s *session) uploadOutputs(ctx context.Context, fetch wire.Fetch) error {
	if fetch.URL == "" {
		return errNoURL
	}

	staged, err := os.CreateTemp("", "steps-outputs-*")
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	defer func() {
		_ = staged.Close()
		_ = os.Remove(staged.Name())
	}()

	err = compress.Pack(staged, true, func(w io.Writer) error {
		return wire.PackPaths(w, s.workdir, fetch.Paths)
	})
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	size, err := staged.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	_, err = staged.Seek(0, io.SeekStart)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPut, fetch.URL, staged)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	request.ContentLength = size

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode >= 300 {
		return fmt.Errorf("the store answered %s", response.Status) //nolint:err113 // carries a status only the far end can act on
	}

	return nil
}
