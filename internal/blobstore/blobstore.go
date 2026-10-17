// Package blobstore is a content-addressed store for artifact trees on S3.
//
// A blob is one artifact packed exactly as it crosses a venue's wire — the
// tar codec's stream, zstd-wrapped — keyed by the digest internal/workspace
// computes over the tree. The key IS the identity: there is no index object,
// no manifest, and no state in the bucket. Which digests mean what stays in
// the orchestrator's own SQLite, which is why a lifecycle rule expiring
// untouched objects is always safe — the worst case is a re-upload, never a
// wrong skip.
//
// Deliberately GET/PUT by key over plain HTTP, never a mounted
// filesystem: every mount surveyed for #80 fails the working tree (dropped
// exec bits, missing rename, no-op flock under SQLite).
//
// One caveat to the safety claim above, recorded rather than hidden: a blob
// is never OVERWRITTEN (publishing HEAD-skips an existing key), so if a wrong
// tree ever lands under a digest — reachable only by racing two processes
// over one workspace.root, which the one-process-per-state-file doctrine
// already forbids — the verify-on-fetch turns it into a permanent miss for
// that digest rather than corruption, and only lifecycle expiry clears it.
package blobstore

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/jtarchie/steps/internal/compress"
	"github.com/jtarchie/steps/internal/wire"
)

// ErrStore is a store URL that cannot work as written.
var ErrStore = errors.New("invalid artifact store")

// Options say where the store is. Everything about the machine reaching it —
// credentials, proxies — comes from the ambient AWS configuration, the same
// chain every other AWS tool reads.
type Options struct {
	// URL is the option string as written, kept for error messages.
	URL    string
	Bucket string
	// Prefix namespaces this store's objects inside the bucket, so one bucket
	// can carry several stores. Blobs live under <prefix>/blobs/<digest>.
	Prefix string
	// Region overrides the ambient AWS region.
	Region string
	// Endpoint points at an S3-compatible server that is not AWS — minio, or
	// a test. Path-style addressing is used with it, because virtual-host
	// style needs DNS an alternative endpoint does not have.
	Endpoint string
	// Limits bound what GetTree unpacks; zero means wire.FetchLimits. A blob
	// is unpacked before its digest is checked, so the bound is all that
	// stands between a bucket writer and this machine's disk.
	Limits wire.UnpackOptions
}

// Parse reads one --artifact-store value: s3://bucket/prefix, with ?region=
// and ?endpoint= as the only knobs. Anything else about the bucket belongs to
// whatever provisioned it.
func Parse(raw string) (Options, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return Options{}, fmt.Errorf("%w %q: %w", ErrStore, raw, err)
	}

	if parsed.Scheme != "s3" {
		return Options{}, fmt.Errorf("%w %q: unknown scheme %q, want s3://bucket/prefix", ErrStore, raw, parsed.Scheme)
	}

	if parsed.Host == "" {
		return Options{}, fmt.Errorf("%w %q: s3 needs a bucket, as in s3://bucket/prefix", ErrStore, raw)
	}

	opts := Options{
		URL:      raw,
		Bucket:   parsed.Host,
		Prefix:   strings.Trim(parsed.Path, "/"),
		Region:   parsed.Query().Get("region"),
		Endpoint: parsed.Query().Get("endpoint"),
	}

	return opts, nil
}

// Store is one bucket-and-prefix, holding trees by digest.
//
// Five object verbs signed with the SDK's own SigV4 signer rather than the S3 service client: that client was ~4MB of the binary (every S3 operation is kept, because kong and text/template defeat the linker's dead-method pruning) for HEAD, GET, PUT, DELETE and a presign.
type Store struct {
	http        *http.Client
	credentials aws.CredentialsProvider
	signer      *v4.Signer
	region      string
	opts        Options
}

// New opens the store. Opening reads only local configuration — the first
// network round trip is the first blob operation, so a misconfigured store
// fails on the run that uses it, with the operation in the error.
func New(ctx context.Context, opts Options) (*Store, error) {
	var loaders []func(*awsconfig.LoadOptions) error

	if opts.Region != "" {
		loaders = append(loaders, awsconfig.WithRegion(opts.Region))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, loaders...)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", ErrStore, opts.URL, err)
	}

	region := cfg.Region
	if region == "" && opts.Endpoint != "" {
		region = "us-east-1"
	}

	// A transport that cannot sit on a black hole: Go's defaults bound
	// dialing and TLS but never the wait for a response, and the blob mirror
	// runs OUTSIDE any step's timeout — so an endpoint that accepted the
	// connection and then said nothing hung a build indefinitely, after the
	// step had already printed success. Response-header only, deliberately:
	// an overall client timeout would kill legitimate large transfers.
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}

	// S3 signs the path as sent, not escaped a second time.
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })

	if opts.Limits == (wire.UnpackOptions{}) {
		opts.Limits = wire.FetchLimits()
	}

	return &Store{http: client, credentials: cfg.Credentials, signer: signer, region: region, opts: opts}, nil
}

// unsignedPayload is S3's opt-out of hashing a body into the signature: an upload streams its file once instead of reading it twice, and a presigned URL cannot know the body it will carry.
const unsignedPayload = "UNSIGNED-PAYLOAD"

// objectURL addresses key: path-style under ?endpoint= (an S3-compatible server has no per-bucket DNS) and for a dotted bucket (whose virtual host no wildcard certificate covers), virtual-hosted on AWS otherwise.
//
// ponytail: aws partition only; a cn-/gov region's hostname suffix, or AWS_ENDPOINT_URL_S3, would need resolving here.
func (s *Store) objectURL(key string) (*url.URL, error) {
	if s.region == "" {
		return nil, fmt.Errorf("%w %q: no region — set ?region= or AWS_REGION", ErrStore, s.opts.URL)
	}

	segments := strings.Split(key, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}

	escaped := strings.Join(segments, "/")

	base := "https://" + s.opts.Bucket + ".s3." + s.region + ".amazonaws.com"
	prefix := ""

	switch {
	case s.opts.Endpoint != "":
		base = strings.TrimSuffix(s.opts.Endpoint, "/")
		prefix = "/" + url.PathEscape(s.opts.Bucket)
	case strings.Contains(s.opts.Bucket, "."):
		base = "https://s3." + s.region + ".amazonaws.com"
		prefix = "/" + url.PathEscape(s.opts.Bucket)
	}

	parsed, err := url.Parse(base + prefix + "/" + escaped)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", ErrStore, s.opts.URL, err)
	}

	return parsed, nil
}

// retryAttempts matches the SDK's standard retryer, which this replaced: S3 answers 503 SlowDown under load and expects the client to come back.
const retryAttempts = 3

// do sends a signed request for key and returns the response when S3 answered 2xx; anything else is an *s3Error carrying S3's error code. A 5xx or a transport failure is retried — every verb here is idempotent, a PUT included, because keys are content-addressed.
func (s *Store) do(ctx context.Context, method, key string, body *os.File) (*http.Response, error) {
	var err error

	for attempt := range retryAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("%s %s: %w (after %w)", method, key, context.Cause(ctx), err)
			case <-time.After(time.Duration(100<<attempt) * time.Millisecond):
			}
		}

		var response *http.Response

		response, err = s.send(ctx, method, key, body)
		if err == nil {
			return response, nil
		}

		var s3Err *s3Error
		if errors.As(err, &s3Err) && s3Err.status < http.StatusInternalServerError {
			return nil, err
		}

		if errors.Is(err, errUnsendable) {
			return nil, err
		}
	}

	return nil, fmt.Errorf("after %d attempts: %w", retryAttempts, err)
}

// errUnsendable is a request that never left this machine — retrying it changes nothing.
var errUnsendable = errors.New("request not sent")

func (s *Store) send(ctx context.Context, method, key string, body *os.File) (*http.Response, error) {
	target, err := s.objectURL(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUnsendable, err)
	}

	request, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: building %s %s: %w", errUnsendable, method, key, err)
	}

	if body != nil {
		info, statErr := body.Stat()
		if statErr != nil {
			return nil, fmt.Errorf("%w: sizing %s: %w", errUnsendable, key, statErr)
		}

		_, statErr = body.Seek(0, io.SeekStart)
		if statErr != nil {
			return nil, fmt.Errorf("%w: rewinding %s: %w", errUnsendable, key, statErr)
		}

		// NopCloser: the caller owns the file, and a retry reads it again.
		request.Body = io.NopCloser(body)
		request.ContentLength = info.Size()
	}

	creds, err := s.credentials.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: reading AWS credentials: %w", errUnsendable, err)
	}

	request.Header.Set("X-Amz-Content-Sha256", unsignedPayload)

	err = s.signer.SignHTTP(ctx, creds, request, unsignedPayload, "s3", s.region, time.Now())
	if err != nil {
		return nil, fmt.Errorf("%w: signing %s %s: %w", errUnsendable, method, key, err)
	}

	response, err := s.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, key, err)
	}

	if response.StatusCode/100 == 2 {
		return response, nil
	}

	defer func() { _ = response.Body.Close() }()

	return nil, readS3Error(response)
}

// presign mints a URL for method on key that signs Host and nothing else — see TestPresignedURLsSignOnlyHost for why that is load-bearing.
func (s *Store) presign(ctx context.Context, method, key string, ttl time.Duration) (string, error) {
	target, err := s.objectURL(key)
	if err != nil {
		return "", err
	}

	query := target.Query()
	query.Set("X-Amz-Expires", strconv.Itoa(int(ttl.Seconds())))
	target.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		return "", fmt.Errorf("building %s %s: %w", method, key, err)
	}

	creds, err := s.credentials.Retrieve(ctx)
	if err != nil {
		return "", fmt.Errorf("reading AWS credentials: %w", err)
	}

	signed, _, err := s.signer.PresignHTTP(ctx, creds, request, unsignedPayload, "s3", s.region, time.Now())
	if err != nil {
		return "", fmt.Errorf("signing %s %s: %w", method, key, err)
	}

	return signed, nil
}

// s3Error is a non-2xx answer: the code from S3's XML error body, or for a bodiless HEAD, the status spelled as S3 spells it.
type s3Error struct {
	status  int
	code    string
	message string
}

func (e *s3Error) Error() string {
	if e.message != "" {
		return fmt.Sprintf("%d %s: %s", e.status, e.code, e.message)
	}

	return fmt.Sprintf("%d %s", e.status, e.code)
}

func readS3Error(response *http.Response) error {
	var body struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}

	_ = xml.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&body)

	if body.Code == "" && response.StatusCode == http.StatusNotFound {
		body.Code = "NotFound"
	}

	if body.Code == "" {
		body.Code = http.StatusText(response.StatusCode)
	}

	return &s3Error{status: response.StatusCode, code: body.Code, message: body.Message}
}

// key is where one digest's blob lives.
func (s *Store) key(digest string) string {
	return path.Join(s.opts.Prefix, "blobs", digest)
}

// rawKey places a caller-chosen key under the store's prefix. The typed tree
// API above owns the blobs/ namespace; everything else — the venue data
// plane's wire/ objects — names its own key through the raw API below.
func (s *Store) rawKey(key string) string {
	return path.Join(s.opts.Prefix, key)
}

// Has reports whether the store holds key.
func (s *Store) Has(ctx context.Context, key string) (bool, error) {
	full := s.rawKey(key)

	response, err := s.do(ctx, http.MethodHead, full, nil)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("checking %s in %q: %w", key, s.opts.URL, err)
	}

	_ = response.Body.Close()

	return true, nil
}

// PutFile uploads one local file as the object at key. A file rather than a
// reader because S3 signs the payload: an upload needs a length and a
// seekable body.
func (s *Store) PutFile(ctx context.Context, key, name string) error {
	file, err := os.Open(name) //nolint:gosec // a path the caller staged
	if err != nil {
		return fmt.Errorf("uploading %s to %q: %w", key, s.opts.URL, err)
	}

	defer func() { _ = file.Close() }()

	full := s.rawKey(key)

	response, err := s.do(ctx, http.MethodPut, full, file)
	if err != nil {
		return fmt.Errorf("uploading %s to %q: %w", key, s.opts.URL, err)
	}

	_ = response.Body.Close()

	return nil
}

// Get streams the object at key. The caller closes it.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	full := s.rawKey(key)

	response, err := s.do(ctx, http.MethodGet, full, nil)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s is not in %q", ErrMissingBlob, key, s.opts.URL)
		}

		return nil, fmt.Errorf("fetching %s from %q: %w", key, s.opts.URL, err)
	}

	return response.Body, nil
}

// Delete removes the object at key. Absence is success — the object being
// gone is the outcome asked for.
func (s *Store) Delete(ctx context.Context, key string) error {
	full := s.rawKey(key)

	response, err := s.do(ctx, http.MethodDelete, full, nil)
	if err != nil {
		return fmt.Errorf("deleting %s from %q: %w", key, s.opts.URL, err)
	}

	_ = response.Body.Close()

	return nil
}

// PresignGet mints a URL that reads key with no credentials but the URL
// itself — how a venue with zero AWS identity is handed exactly the blobs
// its job needs and nothing else.
func (s *Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	full := s.rawKey(key)

	signed, err := s.presign(ctx, http.MethodGet, full, ttl)
	if err != nil {
		return "", fmt.Errorf("presigning a read of %s in %q: %w", key, s.opts.URL, err)
	}

	return signed, nil
}

// PresignPut is PresignGet for writing key.
func (s *Store) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	full := s.rawKey(key)

	signed, err := s.presign(ctx, http.MethodPut, full, ttl)
	if err != nil {
		return "", fmt.Errorf("presigning a write of %s in %q: %w", key, s.opts.URL, err)
	}

	return signed, nil
}

// HasTree reports whether the store already holds digest, so a Put can be
// skipped — the point of content addressing is that a key never needs
// re-uploading.
func (s *Store) HasTree(ctx context.Context, digest string) (bool, error) {
	response, err := s.do(ctx, http.MethodHead, s.key(digest), nil)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("checking %s in %q: %w", digest, s.opts.URL, err)
	}

	_ = response.Body.Close()

	return true, nil
}

// PutTree uploads dir as one blob under digest.
//
// Packed to a temporary file first, not streamed: S3 signs the payload, so an
// upload needs a length and a seekable body, and a tree can be larger than
// memory. The file lives beside nothing and is removed before return.
func (s *Store) PutTree(ctx context.Context, digest, dir string) error {
	staged, err := os.CreateTemp("", "steps-blob-*")
	if err != nil {
		return fmt.Errorf("staging blob %s: %w", digest, err)
	}

	defer func() {
		_ = staged.Close()
		_ = os.Remove(staged.Name())
	}()

	err = compress.Pack(staged, true, func(w io.Writer) error {
		return wire.PackTree(w, dir)
	})
	if err != nil {
		return fmt.Errorf("packing blob %s: %w", digest, err)
	}

	_, err = staged.Seek(0, io.SeekStart)
	if err != nil {
		return fmt.Errorf("staging blob %s: %w", digest, err)
	}

	response, err := s.do(ctx, http.MethodPut, s.key(digest), staged)
	if err != nil {
		return fmt.Errorf("uploading %s to %q: %w", digest, s.opts.URL, err)
	}

	_ = response.Body.Close()

	return nil
}

// GetTree materializes digest's blob into dir, which must not already exist.
// The caller owns verifying that what arrived digests to what was asked for —
// the digest function lives beside the step cache, not here.
func (s *Store) GetTree(ctx context.Context, digest, dir string) error {
	response, err := s.do(ctx, http.MethodGet, s.key(digest), nil)
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("%w: blob %s is not in %q", ErrMissingBlob, digest, s.opts.URL)
		}

		return fmt.Errorf("fetching %s from %q: %w", digest, s.opts.URL, err)
	}

	defer func() { _ = response.Body.Close() }()

	err = os.MkdirAll(dir, 0o750)
	if err != nil {
		return fmt.Errorf("materializing blob %s: %w", digest, err)
	}

	err = compress.Unpack(response.Body, true, func(r io.Reader) error {
		return wire.UnpackTree(r, dir, s.opts.Limits)
	})
	if err != nil {
		return fmt.Errorf("unpacking blob %s from %q: %w", digest, s.opts.URL, err)
	}

	return nil
}

// ErrMissingBlob is a digest the store does not hold. Distinct so a caller can
// treat it as an ordinary cache miss rather than a broken store.
var ErrMissingBlob = errors.New("blob not in the artifact store")

// isNotFound recognizes S3's spellings of an absent object. HEAD answers a
// bare 404 (NotFound), GET a NoSuchKey in its XML body.
func isNotFound(err error) bool {
	var s3Err *s3Error
	if errors.As(err, &s3Err) {
		return s3Err.code == "NotFound" || s3Err.code == "NoSuchKey"
	}

	return false
}
