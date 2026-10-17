package blobstore

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startMinio runs a real S3 implementation, which — unlike fakeS3 — refuses a request whose SigV4 signature is wrong. The signing is this package's own now, so this is the test that can see it break. Skipped where no minio binary is on PATH.
func startMinio(t *testing.T) string {
	t.Helper()

	binary, err := exec.LookPath("minio")
	if err != nil {
		t.Skip("minio is not on PATH")
	}

	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	addr := listener.Addr().String()
	_ = listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	server := exec.CommandContext(ctx, binary, "server", "--quiet", "--address", addr, t.TempDir()) //nolint:gosec // the binary on PATH, args this test chose
	server.Env = append(os.Environ(), "MINIO_ROOT_USER=steps-test", "MINIO_ROOT_PASSWORD=steps-test-secret", "MINIO_BROWSER=off", "MINIO_SITE_REGION=us-east-1")

	err = server.Start()
	if err != nil {
		cancel()
		t.Fatalf("starting minio: %v", err)
	}

	t.Cleanup(func() {
		cancel()
		_ = server.Wait()
	})

	endpoint := "http://" + addr

	deadline := time.Now().Add(20 * time.Second)
	for {
		response, getErr := http.Get(endpoint + "/minio/health/live") //nolint:noctx // a local server this test started
		if getErr == nil {
			_ = response.Body.Close()

			if response.StatusCode == http.StatusOK {
				break
			}
		}

		if time.Now().After(deadline) {
			t.Fatalf("minio never became healthy: %v", getErr)
		}

		time.Sleep(50 * time.Millisecond)
	}

	http.DefaultClient.CloseIdleConnections()

	return endpoint
}

func minioStore(t *testing.T, endpoint, secret string) *Store {
	t.Helper()

	// Nothing ambient: no shared config means no region, which is the ?endpoint= default this exercises.
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "steps-test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", secret)

	opts, err := Parse("s3://cas/team?endpoint=" + endpoint)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	store, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(store.http.CloseIdleConnections)

	return store
}

// TestAgainstARealS3 drives every verb through a server that verifies signatures: what fakeS3 cannot say is whether real S3 would have accepted the request at all.
func TestAgainstARealS3(t *testing.T) {
	endpoint := startMinio(t)
	store := minioStore(t, endpoint, "steps-test-secret")

	// CreateBucket is not a verb the store has; the signed PUT on the bucket path is it.
	created, err := store.do(context.Background(), http.MethodPut, "", nil)
	if err != nil {
		t.Fatalf("creating the bucket: %v", err)
	}

	_ = created.Body.Close()

	minioTreeRoundTrip(t, store)
	minioPresignedRoundTrip(t, store)

	http.DefaultClient.CloseIdleConnections()

	// The server is not just answering: a wrong secret is refused.
	forged := minioStore(t, endpoint, "not-the-secret")

	_, err = forged.Has(context.Background(), "wire/anything")

	var s3Err *s3Error
	if !errors.As(err, &s3Err) || s3Err.status != http.StatusForbidden {
		t.Fatalf("a wrongly signed HEAD = %v, want a 403", err)
	}
}

func minioTreeRoundTrip(t *testing.T, store *Store) {
	t.Helper()

	ctx := context.Background()
	src := t.TempDir()

	err := os.WriteFile(filepath.Join(src, "result.txt"), []byte("payload\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	if hasTree(t, store, "def456") {
		t.Fatal("HasTree before upload = true")
	}

	err = store.PutTree(ctx, "def456", src)
	if err != nil {
		t.Fatalf("PutTree: %v", err)
	}

	if !hasTree(t, store, "def456") {
		t.Fatal("HasTree after upload = false")
	}

	dst := filepath.Join(t.TempDir(), "restored")

	err = store.GetTree(ctx, "def456", dst)
	if err != nil {
		t.Fatalf("GetTree: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "result.txt")) //nolint:gosec // a path this test just built
	if err != nil || string(got) != "payload\n" {
		t.Fatalf("restored = %q, %v", got, err)
	}

	err = store.GetTree(ctx, "nope", filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, ErrMissingBlob) {
		t.Fatalf("GetTree of an absent digest = %v, want ErrMissingBlob", err)
	}
}

// minioPresignedRoundTrip uses a key with a space in it, which is where S3's own path escaping and the signer's would disagree.
func minioPresignedRoundTrip(t *testing.T, store *Store) {
	t.Helper()

	ctx := context.Background()

	putURL, err := store.PresignPut(ctx, "wire/out 1", time.Minute)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, strings.NewReader("worker-bytes"))
	if err != nil {
		t.Fatal(err)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("PUT to the presigned URL: %v", err)
	}

	_ = response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("presigned PUT status = %d", response.StatusCode)
	}

	getURL, err := store.PresignGet(ctx, "wire/out 1", time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}

	response, err = http.Get(getURL) //nolint:noctx,gosec // a URL this test minted
	if err != nil {
		t.Fatalf("GET from the presigned URL: %v", err)
	}

	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()

	if string(body) != "worker-bytes" {
		t.Fatalf("presigned GET = %d %q", response.StatusCode, body)
	}

	minioRawReadAndDelete(t, store, "wire/out 1")
}

func minioRawReadAndDelete(t *testing.T, store *Store, key string) {
	t.Helper()

	ctx := context.Background()

	reader, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get of the presigned upload: %v", err)
	}

	_ = reader.Close()

	err = store.Delete(ctx, key)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	has, err := store.Has(ctx, key)
	if err != nil || has {
		t.Fatalf("Has after Delete = %v, %v", has, err)
	}
}
