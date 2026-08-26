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
// Deliberately GET/PUT by key over the pure-Go SDK, never a mounted
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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

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
type Store struct {
	client *s3.Client
	opts   Options
}

// New opens the store. Opening reads only local configuration — the first
// network round trip is the first blob operation, so a misconfigured store
// fails on the run that uses it, with the operation in the error.
func New(ctx context.Context, opts Options) (*Store, error) {
	loaders := []func(*awsconfig.LoadOptions) error{
		// Checksums OFF unless the operation requires them, and this is not a
		// preference — it is what makes a presigned URL usable by the only
		// thing that ever fetches one.
		//
		// The SDK's default (WhenSupported) adds x-amz-checksum-mode to a
		// GetObject and folds it into SignedHeaders. A presigned URL is then
		// only valid for a client that sends that header — and every client
		// that matters here sends nothing but Host: curl in the SSM bootstrap
		// script, and net/http in the shim's data plane. Real S3 answers those
		// with SignatureDoesNotMatch, 403, every time.
		//
		// It survived review and a full fake-backed test suite because the
		// httptest fake never verified a signature; only real S3 could say so.
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	}

	if opts.Region != "" {
		loaders = append(loaders, awsconfig.WithRegion(opts.Region))
	}

	// A transport that cannot sit on a black hole: the SDK's defaults bound
	// dialing and TLS but never the wait for a response, and the blob mirror
	// runs OUTSIDE any step's timeout — so an endpoint that accepted the
	// connection and then said nothing hung a build indefinitely, after the
	// step had already printed success. Response-header only, deliberately:
	// an overall client timeout would kill legitimate large transfers.
	loaders = append(loaders, awsconfig.WithHTTPClient(&http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}))

	cfg, err := awsconfig.LoadDefaultConfig(ctx, loaders...)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", ErrStore, opts.URL, err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if opts.Endpoint != "" {
			o.BaseEndpoint = &opts.Endpoint
			o.UsePathStyle = true
		}
	})

	return &Store{client: client, opts: opts}, nil
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

	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.opts.Bucket, Key: &full})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("checking %s in %q: %w", key, s.opts.URL, err)
	}

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

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.opts.Bucket, Key: &full, Body: file})
	if err != nil {
		return fmt.Errorf("uploading %s to %q: %w", key, s.opts.URL, err)
	}

	return nil
}

// Get streams the object at key. The caller closes it.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	full := s.rawKey(key)

	object, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.opts.Bucket, Key: &full})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s is not in %q", ErrMissingBlob, key, s.opts.URL)
		}

		return nil, fmt.Errorf("fetching %s from %q: %w", key, s.opts.URL, err)
	}

	return object.Body, nil
}

// Delete removes the object at key. Absence is success — the object being
// gone is the outcome asked for.
func (s *Store) Delete(ctx context.Context, key string) error {
	full := s.rawKey(key)

	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.opts.Bucket, Key: &full})
	if err != nil {
		return fmt.Errorf("deleting %s from %q: %w", key, s.opts.URL, err)
	}

	return nil
}

// PresignGet mints a URL that reads key with no credentials but the URL
// itself — how a venue with zero AWS identity is handed exactly the blobs
// its job needs and nothing else.
func (s *Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	full := s.rawKey(key)

	request, err := s3.NewPresignClient(s.client).PresignGetObject(ctx,
		&s3.GetObjectInput{Bucket: &s.opts.Bucket, Key: &full},
		s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presigning a read of %s in %q: %w", key, s.opts.URL, err)
	}

	return request.URL, nil
}

// PresignPut is PresignGet for writing key.
func (s *Store) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	full := s.rawKey(key)

	request, err := s3.NewPresignClient(s.client).PresignPutObject(ctx,
		&s3.PutObjectInput{Bucket: &s.opts.Bucket, Key: &full},
		s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presigning a write of %s in %q: %w", key, s.opts.URL, err)
	}

	return request.URL, nil
}

// HasTree reports whether the store already holds digest, so a Put can be
// skipped — the point of content addressing is that a key never needs
// re-uploading.
func (s *Store) HasTree(ctx context.Context, digest string) (bool, error) {
	key := s.key(digest)

	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.opts.Bucket, Key: &key})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("checking %s in %q: %w", digest, s.opts.URL, err)
	}

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

	key := s.key(digest)

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.opts.Bucket, Key: &key, Body: staged})
	if err != nil {
		return fmt.Errorf("uploading %s to %q: %w", digest, s.opts.URL, err)
	}

	return nil
}

// GetTree materializes digest's blob into dir, which must not already exist.
// The caller owns verifying that what arrived digests to what was asked for —
// the digest function lives beside the step cache, not here.
func (s *Store) GetTree(ctx context.Context, digest, dir string) error {
	key := s.key(digest)

	object, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.opts.Bucket, Key: &key})
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("%w: blob %s is not in %q", ErrMissingBlob, digest, s.opts.URL)
		}

		return fmt.Errorf("fetching %s from %q: %w", digest, s.opts.URL, err)
	}

	defer func() { _ = object.Body.Close() }()

	err = os.MkdirAll(dir, 0o750)
	if err != nil {
		return fmt.Errorf("materializing blob %s: %w", digest, err)
	}

	err = compress.Unpack(object.Body, true, func(r io.Reader) error {
		return wire.UnpackTree(r, dir)
	})
	if err != nil {
		return fmt.Errorf("unpacking blob %s from %q: %w", digest, s.opts.URL, err)
	}

	return nil
}

// ErrMissingBlob is a digest the store does not hold. Distinct so a caller can
// treat it as an ordinary cache miss rather than a broken store.
var ErrMissingBlob = errors.New("blob not in the artifact store")

// isNotFound recognizes S3's spellings of an absent object. HeadObject answers
// a bare 404 (NotFound), GetObject a NoSuchKey — the smithy error codes are
// the stable surface for both.
func isNotFound(err error) bool {
	var apiErr interface{ ErrorCode() string }
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()

		return code == "NotFound" || code == "NoSuchKey"
	}

	return false
}
