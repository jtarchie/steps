package e2e

// The built-in s3 type against MinIO, a real S3 implementation that refuses a wrong signature. Skipped where no minio binary is on PATH.

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
		response, getErr := client.Get(endpoint + "/minio/health/cluster") //nolint:noctx // a local server this test started
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

	// S3 signs the path as sent; the default would escape a key's %20 a second time.
	err = v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true }).SignHTTP(t.Context(), aws.Credentials{AccessKeyID: minioKey, SecretAccessKey: minioSecret}, request, "UNSIGNED-PAYLOAD", "s3", "us-east-1", time.Now())
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

	// putVersions answers in no order.
	versions := putVersions(t, path, "release")
	slices.SortFunc(versions, func(a, b map[string]any) int { return strings.Compare(fmt.Sprint(a["path"]), fmt.Sprint(b["path"])) })

	if len(versions) != 2 || versions[0]["path"] != "builds/app-1.10.0.tgz" || versions[1]["path"] != "builds/app-1.9.0.tgz" {
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
- name: drop
  type: s3
  tags: [a]
  source:
    prefix: drops/`+s3Source(endpoint, "releases")+`

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
  - put: drop
    inputs: [out]
    params: {dir: out}
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

	if drop := placementNamed(t, runPlacements(t, path), "drop"); drop.BytesSent >= payloadBytes {
		t.Errorf("the folder put sent %d bytes to the worker; want under the payload", drop.BytesSent)
	}

	if keys := minioKeys(t, endpoint, "releases", "drops/"); !slices.Equal(keys, []string{"drops/app-1.0.0.bin"}) {
		t.Errorf("the folder put from the worker left %v, want the one file under drops/", keys)
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

// minioKeys is every key in bucket under prefix, in the bucket's (sorted) order.
func minioKeys(t *testing.T, endpoint, bucket, prefix string) []string {
	t.Helper()

	var keys []string

	token := ""

	for {
		query := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			query.Set("continuation-token", token)
		}

		var page struct {
			NextContinuationToken string `xml:"NextContinuationToken"`
			Contents              []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
		}

		err := xml.Unmarshal(minioDo(t, http.MethodGet, endpoint+"/"+bucket+"?"+query.Encode(), nil), &page)
		if err != nil {
			t.Fatal(err)
		}

		for _, object := range page.Contents {
			keys = append(keys, object.Key)
		}

		if page.NextContinuationToken == "" {
			return keys
		}

		token = page.NextContinuationToken
	}
}

// sigV4Escape is key as S3 expects a signed path spelled: everything but letters, digits, -_.~ and the slashes percent-encoded, which is stricter than net/url.
func sigV4Escape(key string) string {
	var out strings.Builder

	for _, b := range []byte(key) {
		if strings.IndexByte("/-_.~0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ", b) >= 0 {
			out.WriteByte(b)
		} else {
			fmt.Fprintf(&out, "%%%02X", b)
		}
	}

	return out.String()
}

// TestEndToEndS3PutsAFolder: under source.prefix a put uploads every file under params.dir, relative paths kept, whatever the files are named, and more of them than one curl is handed at once.
func TestEndToEndS3PutsAFolder(t *testing.T) {
	t.Parallel()

	endpoint := startMinio(t, "127.0.0.1")
	minioDo(t, http.MethodPut, endpoint+"/memos", nil)

	path := writePipeline(t, t.TempDir(), `
resources:
- name: cuts
  type: s3
  source:
    prefix: memo-cut/`+s3Source(endpoint, "memos")+`
- name: gone
  type: s3
  source:
    prefix: memo-cut/`+s3Source(endpoint, "no-such-bucket")+`
- name: gone-file
  type: s3
  source:
    regexp: memo-cut/(.*).txt`+s3Source(endpoint, "no-such-bucket")+`

jobs:
- name: refused
  plan:
  - task: lay-out
    outputs: [deliver]
    run: touch deliver/a.txt
  - put: gone
    inputs: [deliver]
    params: {dir: deliver}
- name: refused-file
  plan:
  - task: lay-out
    outputs: [deliver]
    run: touch deliver/a.txt
  - put: gone-file
    inputs: [deliver]
    params: {file: deliver/a.txt}
- name: deliver
  plan:
  - task: lay-out
    outputs: [deliver]
    run: |
      mkdir -p "deliver/New Recording 12/notes" deliver/many
      printf cut > "deliver/New Recording 12/New Recording 12.cut.m4a"
      printf why > "deliver/New Recording 12/notes/rationale [v1] {a,b} \"q\".md"
      i=0; while [ $i -lt 300 ]; do printf '%s' $i > deliver/many/$i.txt; i=$((i+1)); done
      ln -s many deliver/link
  - put: cuts
    inputs: [deliver]
    params: {dir: deliver}
- name: empty
  plan:
  - task: lay-out
    outputs: [deliver]
    run: mkdir deliver/nothing
  - put: cuts
    inputs: [deliver]
    params: {dir: deliver}
- name: missing
  plan:
  - task: lay-out
    outputs: [deliver]
    run: "true"
  - put: cuts
    inputs: [deliver]
    params: {dir: deliver/absent}
`)

	mustRun(t, "run", path, "--job", "deliver")

	keys := minioKeys(t, endpoint, "memos", "")
	if len(keys) != 302 {
		t.Fatalf("the bucket holds %d objects, want the 302 files and nothing for the symlink: %v", len(keys), keys)
	}

	for key, want := range map[string]string{
		"memo-cut/New Recording 12/New Recording 12.cut.m4a":          "cut",
		`memo-cut/New Recording 12/notes/rationale [v1] {a,b} "q".md`: "why",
		"memo-cut/many/299.txt":                                       "299",
	} {
		if got := string(minioDo(t, http.MethodGet, endpoint+"/memos/"+sigV4Escape(key), nil)); got != want {
			t.Errorf("%s holds %q, want %q", key, got, want)
		}
	}

	versions := putVersions(t, path, "cuts")
	if len(versions) != 1 || versions[0]["prefix"] != "memo-cut/" || versions[0]["files"] != "302" {
		t.Errorf("recorded versions %v, want the prefix and the count", versions)
	}

	// The bucket's own refusal fails the put: curl is told to treat a 404 as a failure.
	for job, want := range map[string]string{"empty": "holds no files", "missing": "is not a directory", "refused": "404", "refused-file": "404"} {
		stderr := captureStderr(t)

		err := stepsRun(t, []string{"run", path, "--job", job})
		if err == nil {
			t.Errorf("job %s succeeded; want the put refused", job)
		}

		if out := stderr(); !strings.Contains(out, want) {
			t.Errorf("job %s said %q, want %q", job, out, want)
		}
	}
}

// TestValidateRefusesS3Mistakes: what a load can know is refused at load, naming the field.
func TestValidateRefusesS3Mistakes(t *testing.T) {
	t.Parallel()

	t.Run("a get of a folder", func(t *testing.T) {
		t.Parallel()

		path := writePipeline(t, t.TempDir(), `
resources:
- name: r
  type: s3
  source: {bucket: b, prefix: a/}
jobs:
- name: j
  plan:
  - get: r
`)

		err := stepsRun(t, []string{"validate", path})
		if err == nil || !strings.Contains(err.Error(), "only publishes") {
			t.Errorf("validate = %v, want the get refused as publish-only", err)
		}
	})

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
		"three modes":       {`{bucket: b, regexp: "a-(.*)", prefix: a/}`, `{file: out/x}`, "exactly one of source.regexp"},
		"prefix, no slash":  {`{bucket: b, prefix: a}`, `{dir: out}`, "must end in /"},
		"prefix, rooted":    {`{bucket: b, prefix: /a/}`, `{dir: out}`, "must not start with /"},
		"prefix, no get":    {`{bucket: b, prefix: a/, skip_download: true}`, `{dir: out}`, "skip_download has no effect"},
		"prefix wants dir":  {`{bucket: b, prefix: a/}`, `{file: out/x}`, "params.dir is required"},
		"dir wants prefix":  {`{bucket: b, regexp: "a-(.*)"}`, `{dir: out}`, "params.dir needs source.prefix"},
		"file and dir":      {`{bucket: b, prefix: a/}`, `{dir: out, file: out/x}`, "not both"},
		"dir outside":       {`{bucket: b, prefix: a/}`, `{dir: ../out}`, "inside the put's inputs"},
		"dir not an input":  {`{bucket: b, prefix: a/}`, `{dir: other}`, `names artifact "other"`},
		"typed folder":      {`{bucket: b, prefix: a/}`, `{dir: out, content_type: text/plain}`, "content_type applies to one file"},
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
