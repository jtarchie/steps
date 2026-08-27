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
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/jtarchie/steps/internal/compress"
	"github.com/jtarchie/steps/internal/wire"
)

// errNoURL is a transfer frame that named no URL on a session that negotiated
// the URL plane — an orchestrator bug, not a worker condition.
var errNoURL = errors.New("the data plane is urls and the frame carried no url")

// storeClient is what the shim reaches the artifact store with.
//
// Its own, because http.DefaultClient bounds nothing: a store that accepts the
// connection and then stalls parks this call inside the session's frame loop,
// so the shim stops reading — no cancel, no goodbye, not even the
// orchestrator's EOF is heard, because the goroutine that would hear them is
// the one that is blocked. Every other network call in this package and the
// venue is bounded on purpose; this one is on the far end, where nobody can
// see it hang.
//
// Header timeouts rather than a whole-request Timeout: a tree can legitimately
// take minutes to move, and what has to be bounded is a peer that says
// nothing, not a transfer that is making progress.
//
//nolint:gochecknoglobals // one client for the process, as net/http intends
var storeClient = &http.Client{
	CheckRedirect: sameHostOnly,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	},
}

// errOffsiteRedirect is a store sending this transfer somewhere else.
var errOffsiteRedirect = errors.New("the artifact store redirected to another host")

// sameHostOnly refuses a redirect that leaves the host the URL named.
//
// net/http's default follows up to ten redirects anywhere, and GetBody above
// makes the upload REPLAYABLE — so a 307 from the store, or one injected on a
// plain-HTTP endpoint or by the worker's own HTTP_PROXY, would re-PUT the
// whole outputs tree to whatever host answered. A presigned URL redirects
// within its own service (an S3 region hint) or not at all, so the host is
// the right boundary and a cross-host hop is the shape of an exfiltration.
func sameHostOnly(request *http.Request, via []*http.Request) error {
	if len(via) >= maxStoreRedirects {
		return fmt.Errorf("%w: %d hops", errOffsiteRedirect, len(via))
	}

	if request.URL.Host != via[0].URL.Host {
		return fmt.Errorf("%w: %s to %s", errOffsiteRedirect, via[0].URL.Host, request.URL.Host)
	}

	return nil
}

// maxStoreRedirects bounds a store's own in-service hops.
const maxStoreRedirects = 3

// storeError strips the query from a store URL before the failure is reported.
//
// A *url.Error's message embeds the whole request URL, and a presigned one
// carries X-Amz-Signature — a live, object-scoped credential (write-scoped on
// the upload side). The shim's errors become a FrameError, which the venue
// prints, streams to the web UI and writes to the state database, so an
// ordinary store outage put a working credential in the build log.
func storeError(action string, err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		redacted := *urlErr

		parsed, parseErr := url.Parse(urlErr.URL)
		if parseErr == nil {
			parsed.RawQuery = ""
			redacted.URL = parsed.String()
		} else {
			redacted.URL = "(the store url)"
		}

		return fmt.Errorf("%s: %w", action, &redacted)
	}

	return fmt.Errorf("%s: %w", action, err)
}

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

	// No artifacts is an empty tree, not a malformed frame: a step with no
	// declared inputs or outputs has nothing to carry, and the work
	// directory the hello already made is the whole of what it needs.
	cache := s.artifactCacheDir()

	for _, artifact := range upload.Artifacts {
		err = s.placeArtifact(ctx, cache, artifact)
		if err != nil {
			return err
		}
	}

	return sweepArtifactCache(cache)
}

// placeArtifact puts one entry in the work directory, fetching it only if
// this worker does not already hold it.
//
// The whole point of the artifact grain: two steps of one job share their
// inputs and differ only in their outputs, so the second step's big input is
// already here. Verified by unpacking into the cache under its digest — what
// arrives is re-read from disk, never trusted because a URL said so.
func (s *session) placeArtifact(ctx context.Context, cache string, artifact wire.UploadArtifact) error {
	if artifact.Name == "" || artifact.Digest == "" {
		return errNoURL
	}

	held := filepath.Join(cache, artifact.Digest)

	_, err := os.Stat(held)
	if err != nil {
		if artifact.URL == "" {
			return errNoURL
		}

		err = fetchArtifact(ctx, artifact.URL, held)
		if err != nil {
			return err
		}
	}

	// Touched on use, so the sweep evicts what is coldest rather than what is
	// merely oldest — a base image everybody depends on would otherwise be
	// the first thing thrown away.
	now := time.Now()
	_ = os.Chtimes(held, now, now)

	return copyTree(filepath.Join(held, artifact.Name), filepath.Join(s.workdir, artifact.Name))
}

// fetchArtifact downloads one artifact and unpacks it under its digest.
//
// Into a temporary directory and renamed, so a fetch that dies halfway cannot
// leave a partial tree under a digest that claims to be complete — the next
// step would find it, skip the download, and run against half its input.
func fetchArtifact(ctx context.Context, url, held string) error {
	err := os.MkdirAll(filepath.Dir(held), 0o700)
	if err != nil {
		return fmt.Errorf("making the artifact cache: %w", err)
	}

	staging, err := os.MkdirTemp(filepath.Dir(held), ".partial-*")
	if err != nil {
		return fmt.Errorf("staging an artifact: %w", err)
	}

	defer func() { _ = os.RemoveAll(staging) }()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	response, err := storeClient.Do(request)
	if err != nil {
		return storeError("fetching an artifact", err)
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("the store answered %s", response.Status) //nolint:err113 // carries a status only the far end can act on
	}

	err = compress.Unpack(response.Body, true, func(r io.Reader) error {
		return wire.UnpackTree(r, staging)
	})
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	err = os.Rename(staging, held)
	if err != nil && !os.IsExist(err) {
		// A racing session may have completed the same digest first, which is
		// the cache working rather than a failure.
		_, statErr := os.Stat(held)
		if statErr != nil {
			return fmt.Errorf("placing an artifact in the cache: %w", err)
		}
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
	// net/http fills GetBody in only for its own in-memory readers, so without
	// this a presigned URL that answers 307 — or a connection reused into a
	// GOAWAY — cannot replay the body and fails the fetch with a length
	// mismatch rather than shipping the outputs.
	//
	// A fresh handle, NOT a Seek on this one: an *os.File passed as the body
	// is already an io.ReadCloser, so net/http keeps it verbatim and CLOSES
	// it after writing it. Seeking the closed file answered os.ErrClosed, so
	// the replay this exists for failed with "file already closed" and the
	// outputs still did not ship.
	path := staged.Name()
	request.GetBody = func() (io.ReadCloser, error) {
		replay, openErr := os.Open(path) //nolint:gosec // the path is this process's own os.CreateTemp, three lines up
		if openErr != nil {
			return nil, fmt.Errorf("%w", openErr)
		}

		return replay, nil
	}

	response, err := storeClient.Do(request)
	if err != nil {
		return storeError("shipping the step outputs", err)
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode >= 300 {
		return fmt.Errorf("the store answered %s", response.Status) //nolint:err113 // carries a status only the far end can act on
	}

	return nil
}
