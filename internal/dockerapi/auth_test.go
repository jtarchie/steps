package dockerapi

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryForReference(t *testing.T) {
	t.Parallel()

	// Spelled out rather than compared against the constant: the literal IS
	// the contract, the server address the engine files a Hub login under.
	const hub = "https://index.docker.io/v1/"

	if dockerHubAuthKey != hub {
		t.Errorf("dockerHubAuthKey = %q, want the engine's key for Docker Hub, %q", dockerHubAuthKey, hub)
	}

	for _, testCase := range []struct {
		reference string
		want      string
	}{
		// An unqualified name is Docker Hub, filed under a legacy v1 URL
		// rather than under a hostname.
		{"alpine:3", hub},
		{"library/alpine", hub},
		{"docker.io/library/alpine:3", hub},
		{"index.docker.io/library/alpine", hub},

		{"ghcr.io/jtarchie/steps:latest", "ghcr.io"},
		{"registry.digitalocean.com/team/img", "registry.digitalocean.com"},

		// A first component is only a registry if it looks like a host. This
		// is the whole rule, and it is why "myteam/myimage" is Docker Hub
		// while "localhost:5000/myimage" is not.
		{"localhost:5000/img", "localhost:5000"},
		{"myteam/myimage:v1", hub},
		{"registry:5000/a/b/c", "registry:5000"},

		// A digest reference has no tag to confuse the split.
		{"ghcr.io/o/r@sha256:abc123", "ghcr.io"},
	} {
		if got := registryFor(testCase.reference); got != testCase.want {
			t.Errorf("registryFor(%q) = %q, want %q", testCase.reference, got, testCase.want)
		}
	}
}

// TestAPullLoginReachesOnlyItsImage: a login attached for one reference is sent with that pull and no other, and a login in docker's own config.json is never read.
func TestAPullLoginReachesOnlyItsImage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)

	err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auths":{"ghcr.io":{"username":"from-config","password":"x"}}}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	const image = "ghcr.io/o/app@sha256:0000000000000000000000000000000000000000000000000000000000000000"

	ctx := WithPullLogin(t.Context(), image, Login{Username: "robot", Password: "pw"})

	got := decodeAuth(t, registryAuth(ctx, image))
	if got.Username != "robot" || got.Password != "pw" || got.ServerAddress != "ghcr.io" {
		t.Errorf("the attached image's login = %+v, want robot/pw for ghcr.io", got)
	}

	for _, other := range []string{"ghcr.io/o/other:latest", "ghcr.io/o/app:latest", "alpine:3"} {
		if auth := registryAuth(ctx, other); auth != "" {
			t.Errorf("%s carried %+v, want an anonymous pull", other, decodeAuth(t, auth))
		}
	}

	if auth := registryAuth(t.Context(), image); auth != "" {
		t.Errorf("with nothing attached, %s carried %+v from docker's config, want an anonymous pull", image, decodeAuth(t, auth))
	}
}

func decodeAuth(t *testing.T, encoded string) authConfig {
	t.Helper()

	raw, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decoding %q: %v", encoded, err)
	}

	var config authConfig

	err = json.Unmarshal(raw, &config)
	if err != nil {
		t.Fatal(err)
	}

	return config
}
