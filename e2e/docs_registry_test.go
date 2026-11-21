package e2e

// The fake registry each registry= doc example runs against. The reader sees ghcr.io; the test points the repository's host at an in-process registry on loopback, the one place the type reaches without TLS.

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"go.yaml.in/yaml/v4"

	"github.com/jtarchie/steps/docs"
)

type docRegistryFixture struct {
	username, password string
	// seed are the tags pushed before the block runs, each a random image.
	seed []string
	// pushed are the tags the block must leave in the registry, all naming one digest.
	pushed []string
	// base is the one repository path seeded, with a real image for this machine's platform holding baseMarker; every other repository the block pushes must carry that file, so the build took its FROM from the get.
	base string
}

const baseMarker = "base-marker"

var docRegistryFixtures = map[string]docRegistryFixture{ //nolint:gochecknoglobals // read-only fixture table, like docGitHubFixtures
	"private": {username: "robot", password: "fake-registry-token", seed: []string{"latest"}},
	// publish starts empty: the block pushes what its get then finds.
	"publish": {username: "robot", password: "fake-registry-token", pushed: []string{"latest", "v1"}},
	// rebuild seeds the base a build names in FROM; the image it pushes must be built on it.
	"rebuild": {username: "robot", password: "fake-registry-token", base: "acme/base", pushed: []string{"latest"}},
}

var docRegistryToken sync.Once //nolint:gochecknoglobals // one process-wide dummy, as docSlackToken

func injectDocRegistryFixture(t *testing.T, block docs.Block, body string) string {
	t.Helper()

	id := block.RegistryID()
	if id == "" {
		return body
	}

	fixture, ok := docRegistryFixtures[id]
	if !ok {
		t.Fatalf("fence names registry=%s but docs_registry_test.go has no such fixture", id)
	}

	fake := newFakeRegistry(t, fixture.username, fixture.password)

	var doc map[string]any

	err := yaml.Unmarshal([]byte(body), &doc)
	if err != nil {
		t.Fatalf("block is not valid YAML: %v", err)
	}

	pointed := 0

	eachOf(doc, "resources", func(entry any) {
		resource, _ := entry.(map[string]any)
		if resource["type"] != "registry-image" {
			return
		}

		source, _ := resource["source"].(map[string]any)
		pointDocRegistryResource(t, fake, fixture, source)
		checkDocRegistryPushes(t, fake, fixture, source)

		pointed++
	})

	if pointed == 0 {
		t.Fatalf("the fence names registry=%s, but the block declares no registry-image resource", id)
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	return string(out)
}

// pointDocRegistryResource moves one resource's repository onto the fake, sets its password variable, and seeds the tags it follows.
func pointDocRegistryResource(t *testing.T, fake *fakeRegistry, fixture docRegistryFixture, source map[string]any) {
	t.Helper()

	repository, _ := source["repository"].(string)
	_, path, _ := strings.Cut(repository, "/")
	source["repository"] = fake.repo(path)

	// The block runs in a parallel subtest, where t.Setenv panics; the value is the fake's own dummy, so one process-wide setting is harmless.
	if variable, _ := source["password_env"].(string); variable != "" {
		docRegistryToken.Do(func() {
			err := os.Setenv(variable, fixture.password)
			if err != nil {
				t.Fatal(err)
			}
		})
	}

	if path == fixture.base {
		fake.seedBase(t, fake.repo(path)+":latest", &authn.Basic{Username: fixture.username, Password: fixture.password})
	}

	tag, _ := source["tag"].(string)
	for _, seeded := range fixture.seed {
		if tag == "" || tag == seeded {
			fake.seed(t, fake.repo(path)+":"+seeded, &authn.Basic{Username: fixture.username, Password: fixture.password})
		}
	}
}

// checkDocRegistryPushes asserts, once the block has run, that each tag the fixture expects was pushed and all name one image.
func checkDocRegistryPushes(t *testing.T, fake *fakeRegistry, fixture docRegistryFixture, source map[string]any) {
	t.Helper()

	if len(fixture.pushed) == 0 {
		return
	}

	repository, _ := source["repository"].(string)
	login := &authn.Basic{Username: fixture.username, Password: fixture.password}

	if fixture.base != "" && repository == fake.repo(fixture.base) {
		return
	}

	t.Cleanup(func() {
		if t.Failed() {
			return
		}

		if fixture.base != "" && !fake.hasFile(t, repository+":"+fixture.pushed[0], baseMarker, login) {
			t.Errorf("%s:%s has no %s: the build did not take its FROM from the base get", repository, fixture.pushed[0], baseMarker)
		}

		first := fake.digestOf(t, repository+":"+fixture.pushed[0], login)
		for _, tag := range fixture.pushed[1:] {
			if got := fake.digestOf(t, repository+":"+tag, login); got != first {
				t.Errorf("%s:%s = %s, want the same image as %s, %s", repository, tag, got, fixture.pushed[0], first)
			}
		}
	})
}
