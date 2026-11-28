package resource

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/redact"
	"github.com/jtarchie/steps/internal/shell"
)

// fakeBucket answers ListObjectsV2 for keys, a page of two at a time so a listing has to follow the continuation token, and remembers each query it was sent.
type fakeBucket struct {
	keys    []string
	mu      sync.Mutex
	queries []string
}

func (b *fakeBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.queries = append(b.queries, r.URL.RawQuery)
	b.mu.Unlock()

	type content struct {
		Key string `xml:"Key"`
	}

	var page struct {
		XMLName               xml.Name  `xml:"ListBucketResult"`
		IsTruncated           bool      `xml:"IsTruncated"`
		NextContinuationToken string    `xml:"NextContinuationToken,omitempty"`
		Contents              []content `xml:"Contents"`
	}

	var matching []string

	for _, key := range b.keys {
		if strings.HasPrefix(key, r.URL.Query().Get("prefix")) {
			matching = append(matching, key)
		}
	}

	start := 0
	if token := r.URL.Query().Get("continuation-token"); token != "" {
		start = len(token)
	}

	end := min(start+2, len(matching))
	for _, key := range matching[start:end] {
		page.Contents = append(page.Contents, content{Key: key})
	}

	if end < len(matching) {
		page.IsTruncated, page.NextContinuationToken = true, strings.Repeat("x", end)
	}

	_ = xml.NewEncoder(w).Encode(page)
}

func s3Check(t *testing.T, bucket *fakeBucket, source map[string]any, cursor map[string]any) ([]string, error) {
	t.Helper()

	server := httptest.NewServer(bucket)
	t.Cleanup(server.Close)

	source["bucket"], source["endpoint"], source["use_path_style"] = "b", server.URL, true

	versions, err := s3Backend{}.check(t.Context(), Call{Resource: config.Resource{Name: "r", Source: source}, Version: cursor})

	paths := make([]string, 0, len(versions))
	for _, version := range versions {
		paths = append(paths, versionField(version, s3PathField))
	}

	return paths, err
}

// TestS3CheckOrdersByVersionNotName is Concourse's check: versions sort as numbers, the first check reports only the newest, and a later one reports the cursor and everything after it.
func TestS3CheckOrdersByVersionNotName(t *testing.T) {
	t.Parallel()

	keys := []string{"builds/app-1.10.0.tgz", "builds/app-1.2.0.tgz", "builds/app-1.9.0.tgz", "builds/app-1.9.0-rc.1.tgz", "builds/notes.txt", "builds/app-9/9.9.9.tgz", "other/app-9.9.9.tgz"}

	for name, tc := range map[string]struct {
		cursor map[string]any
		want   []string
	}{
		"first check":                 {nil, []string{"builds/app-1.10.0.tgz"}},
		"from the cursor":             {map[string]any{"path": "builds/app-1.9.0.tgz"}, []string{"builds/app-1.9.0.tgz", "builds/app-1.10.0.tgz"}},
		"from a prerelease":           {map[string]any{"path": "builds/app-1.9.0-rc.1.tgz"}, []string{"builds/app-1.9.0-rc.1.tgz", "builds/app-1.9.0.tgz", "builds/app-1.10.0.tgz"}},
		"a cursor now deleted":        {map[string]any{"path": "builds/app-1.5.0.tgz"}, []string{"builds/app-1.9.0-rc.1.tgz", "builds/app-1.9.0.tgz", "builds/app-1.10.0.tgz"}},
		"a cursor of another pattern": {map[string]any{"path": "elsewhere/x"}, []string{"builds/app-1.10.0.tgz"}},
		"a cursor past the newest":    {map[string]any{"path": "builds/app-2.0.0.tgz"}, []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			bucket := &fakeBucket{keys: keys}

			got, err := s3Check(t, bucket, map[string]any{"regexp": "builds/app-(.*).tgz"}, tc.cursor)
			if err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("check = %v, want %v", got, tc.want)
			}

			// Only the directory the pattern names is listed, never the whole bucket.
			for _, query := range bucket.queries {
				if !strings.Contains(query, "prefix=builds%2F") {
					t.Errorf("listed with %q, want the prefix builds/", query)
				}
			}

			if len(bucket.queries) < 2 {
				t.Errorf("listed in %d request(s), want every page followed", len(bucket.queries))
			}
		})
	}
}

// TestS3CheckNamedGroupAndUnorderableVersion: the group named version wins over the first, and a matched file whose capture is no version fails by name where Concourse panics.
func TestS3CheckNamedGroupAndUnorderableVersion(t *testing.T) {
	t.Parallel()

	got, err := s3Check(t, &fakeBucket{keys: []string{"linux-1.0.0.tgz", "darwin-2.0.0.tgz"}}, map[string]any{"regexp": "(linux|darwin)-(?P<version>.*).tgz"}, nil)
	if err != nil || !reflect.DeepEqual(got, []string{"darwin-2.0.0.tgz"}) {
		t.Errorf("check = %v, %v; want the highest by the group named version", got, err)
	}

	_, err = s3Check(t, &fakeBucket{keys: []string{"memo-New Recording 12.m4a"}}, map[string]any{"regexp": "memo-(.*).m4a"}, nil)
	if err == nil || !strings.Contains(err.Error(), `"memo-New Recording 12.m4a"`) {
		t.Errorf("check = %v, want an error naming the file whose capture is not a version", err)
	}
}

// recordingRunner records the secret environment each command was run under and answers canned output, one per command.
type recordingRunner struct {
	answers []string
	envs    *[]map[string]string
}

func (r recordingRunner) Exec(ctx context.Context, _ shell.Command) (shell.Result, error) {
	*r.envs = append(*r.envs, shell.SecretEnv(ctx))

	return shell.Result{Stdout: r.answers[len(*r.envs)-1]}, nil
}

func (r recordingRunner) WithLabel(string) shell.Runner { return r }
func (recordingRunner) Close() error                    { return nil }

// TestS3TransfersGetALinkNeverTheKeys crosses the seam the type exists for: the commands a get and a put run are handed a signed URL for the one object, and neither the resource's keys nor the variables holding them.
func TestS3TransfersGetALinkNeverTheKeys(t *testing.T) {
	t.Parallel()

	const keyID, secret = "AKIDEXAMPLE", "the-secret-access-key"

	var envs []map[string]string

	ctx := shell.WithSecretEnv(t.Context(), map[string]string{"K": keyID, "S": secret})
	ctx = WithHere(ctx, func(context.Context, shell.RunnerSpec) (shell.Runner, error) {
		return recordingRunner{answers: []string{"", "out/app-2.0.0.tgz\n12\n", ""}, envs: &envs}, nil
	})

	resource := config.Resource{Name: "r", Source: map[string]any{"bucket": "b", "regexp": "builds/app-(.*).tgz", "access_key_id_env": "K", "secret_access_key_env": "S"}}

	err := s3Backend{}.in(ctx, Call{Resource: resource, Version: map[string]any{"path": "builds/app-1.0.0.tgz"}, Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("in: %v", err)
	}

	version, err := s3Backend{}.out(ctx, Call{Resource: resource, Params: map[string]any{"file": "out/app-*.tgz"}, Dir: t.TempDir()})
	if err != nil || version["path"] != "builds/app-2.0.0.tgz" {
		t.Fatalf("out = %v, %v; want the uploaded key", version, err)
	}

	if len(envs) != 3 {
		t.Fatalf("ran %d commands, want the download, the match and the upload", len(envs))
	}

	assertLinksNotKeys(t, envs, keyID, secret)
}

func assertLinksNotKeys(t *testing.T, envs []map[string]string, keyID, secret string) {
	t.Helper()

	for i, want := range map[int]string{0: "https://b.s3.us-east-1.amazonaws.com/builds/app-1.0.0.tgz?", 2: "https://b.s3.us-east-1.amazonaws.com/builds/app-2.0.0.tgz?"} {
		signed := envs[i][s3EnvURL]
		if !strings.HasPrefix(signed, want) || !strings.Contains(signed, "X-Amz-Signature=") || !strings.Contains(signed, "X-Amz-Credential="+keyID) {
			t.Errorf("command %d was handed %q, want a signed link to %s", i, signed, want)
		}
	}

	for i, env := range envs {
		for name, value := range env {
			if name == "K" || name == "S" || strings.Contains(value, secret) {
				t.Errorf("command %d was run with %s, which carries the resource's credential", i, name)
			}
		}
	}
}

// TestS3FolderPutBatchesLinksAndNeverTheKeys: a folder's files go up a bounded batch of signed links per command, every file exactly once under the prefix, and no command is handed the keys.
func TestS3FolderPutBatchesLinksAndNeverTheKeys(t *testing.T) {
	t.Parallel()

	const keyID, secret, files = "AKIDEXAMPLE", "the-secret-access-key", 200

	var listing strings.Builder
	for i := range files {
		fmt.Fprintf(&listing, "3\t./sub dir/f%03d.txt\n", i)
	}

	var envs []map[string]string

	ctx := shell.WithSecretEnv(t.Context(), map[string]string{"K": keyID, "S": secret})
	ctx = WithHere(ctx, func(context.Context, shell.RunnerSpec) (shell.Runner, error) {
		return recordingRunner{answers: append([]string{listing.String()}, make([]string, files)...), envs: &envs}, nil
	})

	resource := config.Resource{Name: "r", Source: map[string]any{"bucket": "b", "prefix": "drop/", "access_key_id_env": "K", "secret_access_key_env": "S"}}

	version, err := s3Backend{}.out(ctx, Call{Resource: resource, Params: map[string]any{"dir": "deliver"}, Dir: t.TempDir()})
	if err != nil || version["prefix"] != "drop/" || version["files"] != "200" {
		t.Fatalf("out = %v, %v; want the prefix and the count", version, err)
	}

	if len(envs) < 3 {
		t.Fatalf("ran %d commands, want the listing and several batches", len(envs))
	}

	assertNoKeys(t, envs, keyID, secret)
	assertFolderBatches(t, envs[1:], files)
}

func assertNoKeys(t *testing.T, envs []map[string]string, keyID, secret string) {
	t.Helper()

	for i, env := range envs {
		for name, value := range env {
			if strings.Contains(value, secret) || value == keyID || name == "K" || name == "S" {
				t.Errorf("command %d was handed the credential in $%s", i, name)
			}
		}
	}
}

func assertFolderBatches(t *testing.T, envs []map[string]string, files int) {
	t.Helper()

	uploads := 0

	for i, env := range envs {
		batch := env[s3EnvConfig]
		if len(batch) > s3BatchBytes+2048 {
			t.Errorf("command %d was handed %d bytes of links, want about %d at most", i, len(batch), s3BatchBytes)
		}

		uploads += strings.Count(batch, "\nupload-file = \"./sub dir/f")

		if strings.Count(batch, "url = \"https://b.s3.us-east-1.amazonaws.com/drop/sub%20dir/f") != strings.Count(batch, "upload-file = ") || !strings.Contains(batch, "X-Amz-Signature=") {
			t.Errorf("command %d: every file wants a signed link under the prefix: %.300s", i, batch)
		}
	}

	if uploads != files {
		t.Errorf("%d files were handed to curl, want each of the %d once", uploads, files)
	}
}

// TestS3ListedRefusesWhatItCannotCarry: a listing line that is not a size and a path, an oversized file, and an empty folder are each refused rather than half-published.
func TestS3ListedRefusesWhatItCannotCarry(t *testing.T) {
	t.Parallel()

	for answer, want := range map[string]string{
		"":                           "holds no files",
		"3\t./a\nb.txt\n":            "line break",
		"6000000000\t./big.bin\n":    "at most 5 GB",
		"3\t./a\n3\t../escape.txt\n": "line break",
	} {
		_, err := s3Listed("deliver", answer)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("s3Listed(%q) = %v, want an error naming %q", answer, err, want)
		}
	}

	files, err := s3Listed("deliver", "1\t./b\tc\n2\t./a\n")
	if err != nil || !reflect.DeepEqual(files, []string{"a", "b\tc"}) {
		t.Errorf("s3Listed = %v, %v; want both paths, sorted, the tab kept", files, err)
	}

	if got := curlQuote("a\"b\\c\td"); got != `"a\"b\\c\td"` {
		t.Errorf("curlQuote = %s", got)
	}
}

// TestS3PutKeyIsOneTheCheckFinds crosses put to check: a regexp's leading ^ anchors the pattern and is no part of the key the put writes, or the version it records would never match again.
func TestS3PutKeyIsOneTheCheckFinds(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"builds/app-(.*).tgz", "^builds/app-(.*).tgz", "^builds/app-(.*).tgz$"} {
		key := s3PutKey(config.S3Source{Regexp: raw}, "out/app-1.2.0.tgz")
		if key != "builds/app-1.2.0.tgz" {
			t.Errorf("put under %q writes %q, want builds/app-1.2.0.tgz", raw, key)
		}

		pattern, err := newS3Pattern(raw)
		if err != nil {
			t.Fatal(err)
		}

		if _, matched := pattern.version(key); !matched {
			t.Errorf("the check under %q does not find the key its put wrote, %q", raw, key)
		}
	}

	if key := s3PutKey(config.S3Source{VersionedFile: "prod/app.tgz"}, "out/x.tgz"); key != "prod/app.tgz" {
		t.Errorf("a versioned_file put writes %q, want the versioned file", key)
	}
}

// TestS3DottedBucketGoesInThePath: a bucket with a dot in its name cannot be a host name under S3's wildcard certificate, so over https it is addressed in the path.
func TestS3DottedBucketGoesInThePath(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ bucket, endpoint, want string }{
		{"acme", "", "https://acme.s3.us-east-1.amazonaws.com/"},
		{"acme.releases", "", "https://s3.us-east-1.amazonaws.com/acme.releases/"},
		{"acme.releases", "http://minio.internal:9000", "http://acme.releases.minio.internal:9000/"},
	} {
		client := &s3Client{source: config.S3Source{Bucket: tc.bucket, Endpoint: tc.endpoint}}
		if got := client.bucketURL().String(); got != tc.want {
			t.Errorf("bucket %q at %q is addressed %s, want %s", tc.bucket, tc.endpoint, got, tc.want)
		}
	}
}

// versionsBucket answers ListObjectVersions with ids, newest first, for one key.
func versionsBucket(key string, ids ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		type version struct {
			Key       string `xml:"Key"`
			VersionID string `xml:"VersionId"`
		}

		page := struct {
			XMLName  xml.Name  `xml:"ListVersionsResult"`
			Versions []version `xml:"Version"`
		}{}

		for _, id := range ids {
			page.Versions = append(page.Versions, version{Key: key, VersionID: id})
		}

		_ = xml.NewEncoder(w).Encode(page)
	}
}

// TestS3VersionsSkipPreVersioningHistory: an object written before versioning was turned on lists as "null" behind the real ids, which is history to skip; "null" as the newest is a bucket keeping no versions, which fails.
func TestS3VersionsSkipPreVersioningHistory(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		ids     []string
		want    []string
		refused bool
	}{
		"versioned after the fact": {ids: []string{"v2", "v1", "null"}, want: []string{"v2", "v1"}},
		"never versioned":          {ids: []string{"null"}, refused: true},
		"versioning suspended":     {ids: []string{"null", "v1"}, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(versionsBucket("app.tgz", tc.ids...))
			t.Cleanup(server.Close)

			client := &s3Client{source: config.S3Source{Bucket: "b", Endpoint: server.URL, UsePathStyle: true}, http: server.Client()}

			got, err := client.versions(t.Context(), "app.tgz")
			if tc.refused {
				if !errors.Is(err, errS3Unversioned) {
					t.Errorf("versions = %v, %v; want the bucket refused as unversioned", got, err)
				}

				return
			}

			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("versions = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// TestS3SignedLinksAreSecretsPlainOnesAreNot: a signed link is a credential for an hour and is masked out of what a transfer prints; an anonymous source's link is the object's public address and is left readable.
func TestS3SignedLinksAreSecretsPlainOnesAreNot(t *testing.T) {
	t.Parallel()

	target := (&s3Client{source: config.S3Source{Bucket: "b"}}).objectURL("builds/app-1.0.0.tgz")

	secrets := &redact.Secrets{}
	ctx := redact.WithSecrets(t.Context(), secrets)

	plain, err := (&s3Client{source: config.S3Source{Bucket: "b"}}).transferURL(ctx, http.MethodGet, target)
	if err != nil {
		t.Fatal(err)
	}

	if !secrets.Empty() {
		t.Errorf("an anonymous link %s was registered as a secret", plain)
	}

	signer := &s3Client{source: config.S3Source{Bucket: "b"}, creds: &aws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "s"}}

	signed, err := signer.transferURL(ctx, http.MethodGet, target)
	if err != nil {
		t.Fatal(err)
	}

	if got := secrets.String("curl " + signed); strings.Contains(got, "X-Amz-Signature") {
		t.Errorf("a signed link survives masking: %s", got)
	}
}

// TestS3MatchedKeepsTheNameAsPrinted: the matched file is read up to its line break, a name that starts with a space included.
func TestS3MatchedKeepsTheNameAsPrinted(t *testing.T) {
	t.Parallel()

	file, err := s3Matched("*", " app.tgz\n      12\n")
	if err != nil || file != " app.tgz" {
		t.Errorf("s3Matched = %q, %v; want the name with its leading space", file, err)
	}
}
