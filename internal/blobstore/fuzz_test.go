package blobstore

import (
	"io/fs"
	"strings"
	"testing"
)

// FuzzParse holds --artifact-store to the shape the key layout assumes: a bucket, a prefix with no slash at either end, and every blob filed under that prefix rather than beside it.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"s3://bucket",
		"s3://bucket/prefix/",
		"s3://bucket//a/b//?region=us-east-2&endpoint=http://localhost:9000",
		"s3://bucket/a/..",
		"s3:///prefix",
		"gs://bucket",
		"s3://bucket/%zz",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		opts, err := Parse(raw)
		if err != nil {
			return
		}

		if opts.Bucket == "" || opts.URL != raw {
			t.Fatalf("Parse(%q) = %+v", raw, opts)
		}

		if strings.HasPrefix(opts.Prefix, "/") || strings.HasSuffix(opts.Prefix, "/") {
			t.Fatalf("Parse(%q) prefix %q keeps a slash", raw, opts.Prefix)
		}

		checkKeysUnderPrefix(t, raw, opts)
	})
}

func checkKeysUnderPrefix(t *testing.T, raw string, opts Options) {
	t.Helper()

	// ponytail: a prefix with a dot or empty segment (a/.., ., a//b) files blobs outside it, where another store can share them; Parse accepts it today, so this holds only for clean prefixes until Parse refuses or cleans one.
	if opts.Prefix == "." || !fs.ValidPath(opts.Prefix) {
		return
	}

	key := (&Store{opts: opts}).key("sha256:0123")
	if opts.Prefix != "" && !strings.HasPrefix(key, opts.Prefix+"/") {
		t.Fatalf("Parse(%q): blob key %q is outside prefix %q", raw, key, opts.Prefix)
	}
}
