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
package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"

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
	loaders := []func(*awsconfig.LoadOptions) error{}
	if opts.Region != "" {
		loaders = append(loaders, awsconfig.WithRegion(opts.Region))
	}

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
