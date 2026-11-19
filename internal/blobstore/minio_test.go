package blobstore

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	minioAwkwardUploads(t, store)

	http.DefaultClient.CloseIdleConnections()

	// The server is not just answering: a wrong secret is refused.
	forged := minioStore(t, endpoint, "not-the-secret")

	_, err = forged.HasTree(context.Background(), "anything")

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

// minioAwkwardUploads: keys holding characters url.PathEscape leaves bare and SigV4 encodes (signed one way, checked the other: 403).
func minioAwkwardUploads(t *testing.T, store *Store) {
	t.Helper()

	ctx := context.Background()

	for _, key := range []string{"plain", "a+b", "a=b:c@d$e&f"} {
		err := store.PutTree(ctx, key, t.TempDir())
		if err != nil {
			t.Errorf("PutTree(%q): %v", key, err)

			continue
		}

		has, err := store.HasTree(ctx, key)
		if err != nil || !has {
			t.Errorf("HasTree(%q) after PutTree = %v, %v", key, has, err)
		}
	}
}
