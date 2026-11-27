package e2e

// The built-in s3 type against MinIO, a real S3 implementation that refuses a wrong signature. Skipped where no minio binary is on PATH.

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const (
	minioKey    = "steps-test"
	minioSecret = "steps-test-secret" //nolint:gosec // the login of a MinIO this file starts and stops
)

// startMinio serves a fresh MinIO on host (an address, port 0 chosen here) and answers its endpoint.
func startMinio(t *testing.T, host string) string {
	t.Helper()

	binary, err := exec.LookPath("minio")
	if err != nil {
		t.Skip("minio is not on PATH")
	}

	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skipf("cannot listen on %s: %v", host, err)
	}

	addr := listener.Addr().String()
	_ = listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	server := exec.CommandContext(ctx, binary, "server", "--quiet", "--address", addr, t.TempDir()) //nolint:gosec // the binary on PATH, args this test chose
	server.Env = append(os.Environ(), "MINIO_ROOT_USER="+minioKey, "MINIO_ROOT_PASSWORD="+minioSecret, "MINIO_BROWSER=off", "MINIO_SITE_REGION=us-east-1")

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
	client := &http.Client{Transport: &http.Transport{}}

	defer client.CloseIdleConnections()

	deadline := time.Now().Add(30 * time.Second)

	for {
		response, getErr := client.Get(endpoint + "/minio/health/live") //nolint:noctx // a local server this test started
		if getErr == nil {
			_ = response.Body.Close()

			if response.StatusCode == http.StatusOK {
				return endpoint
			}
		}

		if time.Now().After(deadline) {
			t.Fatalf("minio never became healthy: %v", getErr)
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// minioDo sends one signed request and answers the body of a 2xx.
func minioDo(t *testing.T, method, target string, body []byte) []byte {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	request.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")

	err = v4.NewSigner().SignHTTP(t.Context(), aws.Credentials{AccessKeyID: minioKey, SecretAccessKey: minioSecret}, request, "UNSIGNED-PAYLOAD", "s3", "us-east-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}

	defer func() { _ = response.Body.Close() }()

	answer, _ := io.ReadAll(response.Body)
	if response.StatusCode/100 != 2 {
		t.Fatalf("%s %s: %s: %s", method, target, response.Status, answer)
	}

	return answer
}

// s3Source is the source: lines every test's resource shares, indented for a resource.
func s3Source(endpoint, bucket string) string {
	return `
    bucket: ` + bucket + `
    endpoint: ` + endpoint + `
    use_path_style: true
    access_key_id_env: STEPS_TEST_S3_KEY
    secret_access_key_env: STEPS_TEST_S3_SECRET`
}

// TestEndToEndS3PutsThenGetsTheNewestVersion: a put uploads the one file its glob matches beside the others the regexp names, and a get fetches the highest version, ordered as numbers (1.10.0 over 1.9.0), with Concourse's files beside it.
func TestEndToEndS3PutsThenGetsTheNewestVersion(t *testing.T) {
	t.Parallel()

	endpoint := startMinio(t, "127.0.0.1")
	minioDo(t, http.MethodPut, endpoint+"/releases", nil)

	dir := t.TempDir()
	seen, next := filepath.Join(dir, "seen.txt"), filepath.Join(dir, "next.txt")

	path := writePipeline(t, dir, `
resources:
- name: release
  type: s3
  source:
    regexp: builds/app-(.*).tgz`+s3Source(endpoint, "releases")+`

jobs:
- name: publish
  plan:
  - task: build
    outputs: [out]
    run: |
      VERSION=$(cat `+next+`)
      printf 'bytes of %s' "$VERSION" > "out/app-$VERSION.tgz"
  - put: release
    inputs: [out]
    params: {file: out/app-*.tgz}
- name: consume
  plan:
  - get: release
  - task: look
    inputs: [release]
    run: |
      { ls release | tr '\n' ' '; echo; cat release/version; echo; cat release/s3_uri; echo; cat release/url; echo; cat release/app-*.tgz; } > `+seen+`
`)

	for _, version := range []string{"1.9.0", "1.10.0"} {
		err := os.WriteFile(next, []byte(version), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		mustRun(t, "run", path, "--job", "publish", "--force")
	}

	versions := putVersions(t, path, "release")
	if len(versions) != 2 || versions[1]["path"] != "builds/app-1.10.0.tgz" {
		t.Errorf("recorded versions %v, want the two uploads by path", versions)
	}

	mustRun(t, "run", path, "--job", "consume")

	got := readFileString(t, seen)

	want := "app-1.10.0.tgz s3_uri url version \n1.10.0\ns3://releases/builds/app-1.10.0.tgz\n" + endpoint + "/releases/builds/app-1.10.0.tgz\nbytes of 1.10.0"
	if strings.TrimSpace(got) != want {
		t.Errorf("the get laid down\n%s\nwant\n%s", got, want)
	}
}

// reachableHost is an address of this machine that a container can also dial, which a loopback one is not.
func reachableHost(t *testing.T) string {
	t.Helper()

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("no interface addresses: %v", err)
	}

	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil && !ipnet.IP.IsLinkLocalUnicast() {
			return ipnet.IP.String()
		}
	}

	t.Skip("this machine has no address a container could dial")

	return ""
}

// TestEndToEndS3TransfersOnTheWorker crosses the seam the type is built around: a URL signed in this process is fetched by curl in a container on the worker, so a megabyte goes bucket to worker and worker to bucket without passing through here.
func TestEndToEndS3TransfersOnTheWorker(t *testing.T) {
	t.Parallel()
	requireDockerE2E(t)
	dockerSlot(t)

	endpoint := startMinio(t, reachableHost(t))
	minioDo(t, http.MethodPut, endpoint+"/releases", nil)

	dir := t.TempDir()
	seen := filepath.Join(dir, "seen.txt")

	path := writePipeline(t, dir, `
resources:
- name: release
  type: s3
  tags: [a]
  source:
    regexp: builds/app-(.*).bin`+s3Source(endpoint, "releases")+`

jobs:
- name: publish
  plan:
  - task: build
    tags: [a]
    image: `+dockerE2EImage+`
    outputs: [out]
    run: head -c `+strconv.Itoa(payloadBytes)+` /dev/urandom > out/app-1.0.0.bin
  - put: release
    inputs: [out]
    params: {file: out/app-*.bin}
- name: consume
  plan:
  - get: release
  - task: measure
    tags: [a]
    image: `+dockerE2EImage+`
    inputs: [release]
    outputs: [size]
    run: |
      wc -c < release/app-1.0.0.bin | tr -d ' ' > size/size.txt
      cat release/version >> size/size.txt
  - task: publish
    inputs: [size]
    run: cp size/size.txt `+seen+`
`)

	workers := []string{"--worker", "a=local:" + filepath.Join(t.TempDir(), "a")}

	mustRun(t, append([]string{"run", path, "--job", "publish"}, workers...)...)

	if put := placementNamed(t, runPlacements(t, path), "release"); put.BytesSent >= payloadBytes {
		t.Errorf("the put sent %d bytes to the worker; want under the payload — the file was already there", put.BytesSent)
	}

	mustRun(t, append([]string{"run", path, "--job", "consume"}, workers...)...)

	if got := readFileString(t, seen); got != strconv.Itoa(payloadBytes)+"\n1.0.0" {
		t.Errorf("the worker's task saw %q, want the payload's size and its version", got)
	}

	if get := placementNamed(t, runPlacements(t, path), "release"); get.BytesReceived >= payloadBytes {
		t.Errorf("the get brought %d bytes home; want under the payload — the object should have stayed on the worker", get.BytesReceived)
	}
}

// TestEndToEndS3VersionedFile: under versioned_file each put is a new version of one key, named by the id the bucket gave it, and a get fetches the newest; skip_download names the object without fetching it.
func TestEndToEndS3VersionedFile(t *testing.T) {
	t.Parallel()

	endpoint := startMinio(t, "127.0.0.1")
	minioDo(t, http.MethodPut, endpoint+"/config", nil)
	minioDo(t, http.MethodPut, endpoint+"/config?versioning", []byte(`<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`))

	dir := t.TempDir()
	seen, next := filepath.Join(dir, "seen.txt"), filepath.Join(dir, "next.txt")

	path := writePipeline(t, dir, `
resources:
- name: settings
  type: s3
  source:
    versioned_file: prod/settings.json`+s3Source(endpoint, "config")+`

jobs:
- name: publish
  plan:
  - task: write
    outputs: [out]
    run: cp `+next+` "out/my settings (draft).json"
  - put: settings
    inputs: [out]
    params: {file: "out/*.json", content_type: application/json}
- name: consume
  plan:
  - get: settings
  - get: named
    resource: settings
    params: {skip_download: true}
  - task: look
    inputs: [settings, named]
    run: |
      { cat settings/settings.json; echo; ls named | tr '\n' ' '; } > `+seen+`
      test "$(cat settings/version)" = "$(cat named/version)"
`)

	for _, body := range []string{"first", "second"} {
		err := os.WriteFile(next, []byte(body), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		mustRun(t, "run", path, "--job", "publish", "--force")
	}

	versions := putVersions(t, path, "settings")
	if len(versions) != 2 || versions[0]["version_id"] == versions[1]["version_id"] || versions[1]["version_id"] == "" {
		t.Fatalf("recorded versions %v, want two distinct version ids", versions)
	}

	mustRun(t, "run", path, "--job", "consume")

	if got := readFileString(t, seen); got != "second\ns3_uri url version " {
		t.Errorf("the gets laid down %q, want the newest upload's bytes and, for skip_download, only the naming files", got)
	}
}

// TestEndToEndS3PutRefusesAnAmbiguousFile: a glob matching two files uploads neither and names both, and one matching nothing says so.
func TestEndToEndS3PutRefusesAnAmbiguousFile(t *testing.T) {
	t.Parallel()

	endpoint := startMinio(t, "127.0.0.1")
	minioDo(t, http.MethodPut, endpoint+"/releases", nil)

	path := writePipeline(t, t.TempDir(), `
resources:
- name: release
  type: s3
  source:
    regexp: builds/app-(.*).tgz`+s3Source(endpoint, "releases")+`

jobs:
- name: two
  plan:
  - task: build
    outputs: [out]
    run: touch out/app-1.tgz out/app-2.tgz
  - put: release
    inputs: [out]
    params: {file: out/app-*.tgz}
- name: none
  plan:
  - task: build
    outputs: [out]
    run: touch out/other.txt
  - put: release
    inputs: [out]
    params: {file: out/app-*.tgz}
`)

	for job, want := range map[string]string{"two": "matches more than one file", "none": "matches no file"} {
		stderr := captureStderr(t)

		err := stepsRun(t, []string{"run", path, "--job", job})
		if err == nil {
			t.Errorf("job %s succeeded; want the put refused", job)
		}

		out := stderr()

		if !strings.Contains(out, want) {
			t.Errorf("job %s said %q, want %q", job, out, want)
		}
	}

	if listing := string(minioDo(t, http.MethodGet, endpoint+"/releases?list-type=2", nil)); strings.Contains(listing, "<Key>") {
		t.Errorf("a refused put uploaded something: %s", listing)
	}
}

// TestValidateRefusesS3Mistakes: what a load can know is refused at load, naming the field.
func TestValidateRefusesS3Mistakes(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ source, put, want string }{
		"no mode":           {`{bucket: b}`, `{file: out/x}`, "exactly one of source.regexp"},
		"both modes":        {`{bucket: b, regexp: "a-(.*)", versioned_file: a}`, `{file: out/x}`, "exactly one of source.regexp"},
		"no capture group":  {`{bucket: b, regexp: "a-.*"}`, `{file: out/x}`, "needs a capture group"},
		"a literal key":     {`{bucket: b, regexp: "a-(.*)", access_key_id_env: "AKIA/literal", secret_access_key_env: S}`, `{file: out/x}`, "must be the NAME of an environment variable"},
		"half a login":      {`{bucket: b, regexp: "a-(.*)", access_key_id_env: K}`, `{file: out/x}`, "go together"},
		"concourse's key":   {`{bucket: b, regexp: "a-(.*)", access_key_id: AKIA}`, `{file: out/x}`, `unknown key "access_key_id"`},
		"no file":           {`{bucket: b, regexp: "a-(.*)"}`, `{}`, "params.file is required"},
		"file outside":      {`{bucket: b, regexp: "a-(.*)"}`, `{file: ../x}`, "inside the put's inputs"},
		"file not an input": {`{bucket: b, regexp: "a-(.*)"}`, `{file: other/x}`, `names artifact "other"`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := writePipeline(t, t.TempDir(), `
resources:
- name: r
  type: s3
  source: `+tc.source+`
jobs:
- name: j
  plan:
  - task: t
    outputs: [out]
    run: "true"
  - put: r
    inputs: [out]
    params: `+tc.put+`
`)

			err := stepsRun(t, []string{"validate", path})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("validate = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}
