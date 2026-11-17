package e2e

import (
	"archive/tar"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// fakeRegistry is an in-process OCI registry on loopback, optionally behind basic auth; host is what a repository names it by.
type fakeRegistry struct {
	host string
}

func newFakeRegistry(t *testing.T, username, password string) *fakeRegistry {
	t.Helper()

	handler := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	if username != "" {
		inner := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(user+":"+pass), []byte(username+":"+password)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)

				return
			}

			inner.ServeHTTP(w, r)
		})
	}

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return &fakeRegistry{host: strings.TrimPrefix(server.URL, "http://")}
}

func (f *fakeRegistry) repo(path string) string { return f.host + "/" + path }

// digestOf is the digest the registry serves for ref, read the way the resource reads it.
func (f *fakeRegistry) digestOf(t *testing.T, ref string, auth authn.Authenticator) string {
	t.Helper()

	parsed, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}

	descriptor, err := remote.Head(parsed, remote.WithAuth(auth))
	if err != nil {
		t.Fatalf("HEAD %s: %v", ref, err)
	}

	return descriptor.Digest.String()
}

// seed pushes a random image to ref, returning its digest.
func (f *fakeRegistry) seed(t *testing.T, ref string, auth authn.Authenticator) string {
	t.Helper()

	image, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}

	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatal(err)
	}

	err = remote.Write(tag, image, remote.WithAuth(auth))
	if err != nil {
		t.Fatalf("seed %s: %v", ref, err)
	}

	digest, err := image.Digest()
	if err != nil {
		t.Fatal(err)
	}

	return digest.String()
}

// ociLayout writes an OCI layout holding one image, or one multi-platform index, and returns its directory and that top-level digest.
func ociLayout(t *testing.T, index bool) (string, string) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "layout")

	path, err := layout.Write(dir, empty.Index)
	if err != nil {
		t.Fatal(err)
	}

	var digest v1.Hash

	if index {
		digest, err = appendRandomIndex(path)
	} else {
		digest, err = appendRandomImage(path)
	}

	if err != nil {
		t.Fatal(err)
	}

	return dir, digest.String()
}

func appendRandomIndex(path layout.Path) (v1.Hash, error) {
	built, err := random.Index(64, 1, 2)
	if err != nil {
		return v1.Hash{}, fmt.Errorf("index: %w", err)
	}

	err = path.AppendIndex(built)
	if err != nil {
		return v1.Hash{}, fmt.Errorf("append: %w", err)
	}

	return built.Digest() //nolint:wrapcheck // test fixture
}

func appendRandomImage(path layout.Path) (v1.Hash, error) {
	built, err := random.Image(64, 2)
	if err != nil {
		return v1.Hash{}, fmt.Errorf("image: %w", err)
	}

	err = path.AppendImage(built)
	if err != nil {
		return v1.Hash{}, fmt.Errorf("append: %w", err)
	}

	return built.Digest() //nolint:wrapcheck // test fixture
}

// tarDir packs dir into a tarball the way BuildKit's type=oci output is one.
func tarDir(t *testing.T, dir string) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "image.tar")

	file, err := os.Create(out) //nolint:gosec // a t.TempDir() path
	if err != nil {
		t.Fatal(err)
	}

	writer := tar.NewWriter(file)

	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}

		return addToTar(writer, dir, path, entry)
	})
	if err != nil {
		t.Fatal(err)
	}

	err = errors.Join(writer.Close(), file.Close())
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func addToTar(writer *tar.Writer, dir, path string, entry fs.DirEntry) error {
	rel, _ := filepath.Rel(dir, path)
	info, _ := entry.Info()

	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("header: %w", err)
	}

	header.Name = filepath.ToSlash(rel)

	err = writer.WriteHeader(header)
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}

	if entry.IsDir() {
		return nil
	}

	content, err := os.Open(path) //nolint:gosec // walking our own temp dir
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}

	defer func() { _ = content.Close() }()

	_, err = io.Copy(writer, content)
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}

	return nil
}

// registryPipeline publishes what is at image (copied into the build as image/<base>) to repository, with source extra appended, then a second job fetches it back and prints the digest it names.
func registryPipeline(repository, image, sourceExtra string) string {
	return `
resources:
- name: app
  type: registry-image
  source:
    repository: ` + repository + `
` + sourceExtra + `
jobs:
- name: publish
  plan:
  - task: build
    outputs: [image]
    run: |
      cp -R '` + image + `' image/
      echo v1 v1.2 > image/tags
  - put: app
    inputs: [image]
    params:
      image: image/` + filepath.Base(image) + `
      additional_tags: image/tags
- name: consume
  plan:
  - get: app
  - task: show
    inputs: [app]
    run: cat app/repository app/tag app/digest
`
}

// TestEndToEndRegistryImagePublishesThenFetches: an OCI tarball a build made is pushed to source.tag and each additional tag, the put records the digest, and a later get names the same digest for a step's image:.
func TestEndToEndRegistryImagePublishesThenFetches(t *testing.T) {
	t.Parallel()

	fake := newFakeRegistry(t, "", "")
	dir, want := ociLayout(t, false)
	repository := fake.repo("team/app")

	path := writePipeline(t, t.TempDir(), registryPipeline(repository, tarDir(t, dir), ""))
	mustRun(t, "run", path, "--job", "publish")

	for _, tag := range []string{"latest", "v1", "v1.2"} {
		if got := fake.digestOf(t, repository+":"+tag, authn.Anonymous); got != want {
			t.Errorf("%s:%s = %s, want the pushed image %s", repository, tag, got, want)
		}
	}

	versions := putVersions(t, path, "app")
	if len(versions) != 1 || versions[0]["digest"] != want {
		t.Errorf("recorded versions %v, want one {digest: %s}", versions, want)
	}

	out := captureStdout(t, func() { mustRun(t, "run", path, "--job", "consume") })
	if !strings.Contains(out, repository+"latest"+want) {
		t.Errorf("consume printed %q, want repository, tag and digest %s%s%s", out, repository, "latest", want)
	}
}

// TestEndToEndRegistryImagePushesAMultiPlatformIndex: a layout whose one entry is an index (a multi-platform build) is pushed whole, and the tag names the index, which every platform's daemon resolves to its own image.
func TestEndToEndRegistryImagePushesAMultiPlatformIndex(t *testing.T) {
	t.Parallel()

	fake := newFakeRegistry(t, "", "")
	dir, want := ociLayout(t, true)
	repository := fake.repo("team/multi")

	path := writePipeline(t, t.TempDir(), registryPipeline(repository, dir, ""))
	mustRun(t, "run", path, "--job", "publish")

	if got := fake.digestOf(t, repository+":latest", authn.Anonymous); got != want {
		t.Errorf("latest = %s, want the index %s", got, want)
	}
}

// TestEndToEndRegistryImageLogsInWithTheResourceCredential: source.username and password_env are the login for check, get and put alike.
func TestEndToEndRegistryImageLogsInWithTheResourceCredential(t *testing.T) {
	t.Setenv("FAKE_REGISTRY_PASSWORD", "s3cret")

	fake := newFakeRegistry(t, "robot", "s3cret")
	dir, want := ociLayout(t, false)
	repository := fake.repo("private/app")

	path := writePipeline(t, t.TempDir(), registryPipeline(repository, dir, "    username: robot\n    password_env: FAKE_REGISTRY_PASSWORD\n"))
	mustRun(t, "run", path, "--job", "publish")
	mustRun(t, "run", path, "--job", "consume")

	if got := fake.digestOf(t, repository+":latest", &authn.Basic{Username: "robot", Password: "s3cret"}); got != want {
		t.Errorf("latest = %s, want %s", got, want)
	}
}

// TestEndToEndRegistryImageNeverReadsDockerConfig: a login in the operator's docker config is not used. Without one in source:, a private registry refuses the check, which says the login is what to fix.
func TestEndToEndRegistryImageNeverReadsDockerConfig(t *testing.T) {
	fake := newFakeRegistry(t, "robot", "s3cret")
	fake.seed(t, fake.repo("private/app")+":latest", &authn.Basic{Username: "robot", Password: "s3cret"})

	dockerConfig := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dockerConfig)

	err := os.WriteFile(filepath.Join(dockerConfig, "config.json"), []byte(`{"auths":{"`+fake.host+`":{"username":"robot","password":"s3cret"}}}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	path := writePipeline(t, t.TempDir(), `
resources:
- name: app
  type: registry-image
  source:
    repository: `+fake.repo("private/app")+`
jobs:
- name: consume
  plan:
  - get: app
`)

	err = stepsRun(t, []string{"run", path, "--job", "consume"})
	if err == nil || !strings.Contains(err.Error(), "refused the login") {
		t.Errorf("run: err = %v, want the registry's refusal, named as a login problem", err)
	}
}

// TestEndToEndRegistryImageMissingTagFailsTheCheck: a tag the registry does not have is an error naming it, never "no versions yet".
func TestEndToEndRegistryImageMissingTagFailsTheCheck(t *testing.T) {
	t.Parallel()

	fake := newFakeRegistry(t, "", "")
	fake.seed(t, fake.repo("team/app")+":latest", authn.Anonymous)

	path := writePipeline(t, t.TempDir(), `
resources:
- name: app
  type: registry-image
  source:
    repository: `+fake.repo("team/app")+`
    tag: lastest
jobs:
- name: consume
  plan:
  - get: app
`)

	err := stepsRun(t, []string{"run", path, "--job", "consume"})
	if err == nil || !strings.Contains(err.Error(), fake.repo("team/app")+":lastest does not exist") {
		t.Errorf("run: err = %v, want the missing tag named", err)
	}
}

// TestValidateRefusesRegistryImageMistakes: each is a load error naming what to change.
func TestValidateRefusesRegistryImageMistakes(t *testing.T) {
	t.Parallel()

	cases := map[string]struct{ pipeline, want string }{
		"literal password": {
			pipeline: "resources:\n- name: app\n  type: registry-image\n  source: {repository: ghcr.io/o/app, username: me, password_env: 'ghp_not a name'}\n",
			want:     "source.password_env must be the NAME",
		},
		"username alone": {
			pipeline: "resources:\n- name: app\n  type: registry-image\n  source: {repository: ghcr.io/o/app, username: me}\n",
			want:     "source.username and source.password_env go together",
		},
		"tag in repository": {
			pipeline: "resources:\n- name: app\n  type: registry-image\n  source: {repository: 'ghcr.io/o/app:v1'}\n",
			want:     "a tag goes in source.tag",
		},
		"unknown key": {
			pipeline: "resources:\n- name: app\n  type: registry-image\n  source: {repository: ghcr.io/o/app, password: hunter2}\n",
			want:     `unknown key "password"`,
		},
		"get params": {
			pipeline: "resources:\n- name: app\n  type: registry-image\n  source: {repository: ghcr.io/o/app}\njobs:\n- name: j\n  plan:\n  - get: app\n    params: {format: rootfs}\n",
			want:     "a registry-image get takes no params",
		},
		"put outside inputs": {
			pipeline: "resources:\n- name: app\n  type: registry-image\n  source: {repository: ghcr.io/o/app}\njobs:\n- name: j\n  plan:\n  - put: app\n    inputs: [built]\n    params: {image: other/image.tar}\n",
			want:     `params.image "other/image.tar" names artifact "other"`,
		},
		"put without image": {
			pipeline: "resources:\n- name: app\n  type: registry-image\n  source: {repository: ghcr.io/o/app}\njobs:\n- name: j\n  plan:\n  - put: app\n    params: {}\n",
			want:     "params.image is required",
		},
	}

	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			t.Parallel()

			pipeline := tc.pipeline
			if !strings.Contains(pipeline, "jobs:") {
				pipeline += "jobs:\n- name: j\n  plan:\n  - get: app\n"
			}

			path := writePipeline(t, t.TempDir(), pipeline)

			err := stepsRun(t, []string{"validate", path})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("validate: err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestEndToEndRegistryImageGetOfAGoneDigestFails: a get asks the registry for the digest it names, so an image deleted since its check fails at the get, naming it, rather than as a step that cannot pull.
func TestEndToEndRegistryImageGetOfAGoneDigestFails(t *testing.T) {
	t.Parallel()

	fake := newFakeRegistry(t, "", "")
	ref := fake.repo("team/app") + ":latest"
	gone := fake.seed(t, ref, authn.Anonymous)

	pipeline := `
resources:
- name: app
  type: registry-image
  source:
    repository: ` + fake.repo("team/app") + `
jobs:
- name: consume
  plan:
  - get: app
    version: {digest: "` + gone + `"}
`
	path := writePipeline(t, t.TempDir(), pipeline)
	mustRun(t, "run", path, "--job", "consume")

	digest, err := name.NewDigest(fake.repo("team/app") + "@" + gone)
	if err != nil {
		t.Fatal(err)
	}

	err = remote.Delete(digest)
	if err != nil {
		t.Fatalf("deleting %s: %v", digest, err)
	}

	fake.seed(t, ref, authn.Anonymous)

	err = stepsRun(t, []string{"run", path, "--job", "consume", "--force"})
	if err == nil || !strings.Contains(err.Error(), fake.repo("team/app")+"@"+gone+" does not exist") {
		t.Errorf("run: err = %v, want the gone digest named", err)
	}
}

// TestValidateNamesAnUnsetRegistryPassword: a login whose variable is not set is reported before a run, not as a refused check.
func TestValidateNamesAnUnsetRegistryPassword(t *testing.T) {
	t.Parallel()

	path := writePipeline(t, t.TempDir(), `
resources:
- name: app
  type: registry-image
  source: {repository: ghcr.io/o/app, username: me, password_env: STEPS_TEST_REGISTRY_PASSWORD_UNSET}
jobs:
- name: j
  plan:
  - get: app
`)

	err := stepsRun(t, []string{"validate", path})
	if err == nil || !strings.Contains(err.Error(), "$STEPS_TEST_REGISTRY_PASSWORD_UNSET is not set (source.password_env)") {
		t.Errorf("validate: err = %v, want the unset variable named", err)
	}
}

// TestPipelineOwnRegistryImageTypeReplacesTheBuiltIn: declaring a type under the built-in's name is the pipeline's choice, as for git; its source: is that type's business and its own commands run.
func TestPipelineOwnRegistryImageTypeReplacesTheBuiltIn(t *testing.T) {
	t.Parallel()

	schema := loadSchema(t)

	path := writePipeline(t, t.TempDir(), `
resource_types:
- name: registry-image
  config:
    check: echo '[{"digest":"sha256:`+strings.Repeat("a", 64)+`"}]'
    in: echo "own type" > said
resources:
- name: app
  type: registry-image
  source: {repository: ghcr.io/o/app, anything: goes}
jobs:
- name: j
  plan:
  - get: app
  - task: show
    inputs: [app]
    run: cat app/said
`)

	err := schema.Validate(yamlAsJSONValue(t, path))
	if err != nil {
		t.Errorf("schema refused a source: for a pipeline's own registry-image type: %v", err)
	}

	out := captureStdout(t, func() { mustRun(t, "run", path, "--job", "j") })
	if !strings.Contains(out, "own type") {
		t.Errorf("run printed %q, want the pipeline's own in: to have run", out)
	}
}

// TestPipelineOwnCronAndGitHubTypesReplaceTheBuiltIns: the schema and the loader both leave a replaced built-in's source: open, and the pipeline's own commands run.
func TestPipelineOwnCronAndGitHubTypesReplaceTheBuiltIns(t *testing.T) {
	t.Parallel()

	schema := loadSchema(t)

	path := writePipeline(t, t.TempDir(), `
resource_types:
- name: cron
  config:
    check: echo '[{"at":"now"}]'
    in: echo "own cron" > said
- name: github-prs
  config:
    check: echo '[{"n":"1"}]'
    in: echo "own prs" > said
resources:
- name: tick
  type: cron
  source: {every: tuesday}
- name: pr
  type: github-prs
  source: {project: elsewhere}
jobs:
- name: j
  plan:
  - get: tick
  - get: pr
  - task: show
    inputs: [tick, pr]
    run: cat tick/said pr/said
`)

	err := schema.Validate(yamlAsJSONValue(t, path))
	if err != nil {
		t.Errorf("schema refused a source: for a pipeline's own cron or github-prs type: %v", err)
	}

	err = stepsRun(t, []string{"validate", path})
	if err != nil {
		t.Errorf("validate: %v, want the pipeline's own cron type's source: left to it", err)
	}

	out := captureStdout(t, func() { mustRun(t, "run", path, "--job", "j") })
	if !strings.Contains(out, "own cron") || !strings.Contains(out, "own prs") {
		t.Errorf("run printed %q, want both of the pipeline's own in: commands to have run", out)
	}
}
