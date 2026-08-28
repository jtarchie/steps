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

	// Spelled out rather than compared against the constant. The literal IS
	// the contract — it is where `docker login` writes a Hub credential, so a
	// test that reads the constant on both sides would go green for any value
	// at all, including a plausible-looking "docker.io" that finds nothing on
	// a machine that is logged in.
	const hub = "https://index.docker.io/v1/"

	if dockerHubAuthKey != hub {
		t.Errorf("dockerHubAuthKey = %q, want the key docker login actually writes, %q", dockerHubAuthKey, hub)
	}

	for _, testCase := range []struct {
		reference string
		want      string
	}{
		// An unqualified name is Docker Hub, whose credentials are filed
		// under a legacy v1 URL rather than under a hostname. Nothing about
		// that is guessable; it is simply where `docker login` puts them.
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

// writeAuthConfig writes a config.json holding the given fields and points
// DOCKER_CONFIG at its directory.
func writeAuthConfig(t *testing.T, config map[string]any) {
	t.Helper()

	root := t.TempDir()
	writeJSON(t, filepath.Join(root, "config.json"), config)
	t.Setenv("DOCKER_CONFIG", root)
}

func TestRegistryAuthReadsAStoredLogin(t *testing.T) {
	writeAuthConfig(t, map[string]any{
		"auths": map[string]any{
			"ghcr.io": map[string]any{
				"auth": base64.StdEncoding.EncodeToString([]byte("someone:a-token")),
			},
		},
	})

	got := decodeAuth(t, RegistryAuth(t.Context(), "ghcr.io/o/r:v1"))

	if got.Username != "someone" || got.Password != "a-token" {
		t.Errorf("RegistryAuth = %+v, want the stored username and token", got)
	}

	if got.ServerAddress != "ghcr.io" {
		t.Errorf("ServerAddress = %q, want ghcr.io", got.ServerAddress)
	}
}

// TestRegistryAuthIsEmptyForAnUnknownRegistry pins that an anonymous pull
// stays anonymous. Sending an empty credential is not the same as sending
// none, and a registry that accepts anonymous reads can reject a blank login.
func TestRegistryAuthIsEmptyForAnUnknownRegistry(t *testing.T) {
	writeAuthConfig(t, map[string]any{
		"auths": map[string]any{"ghcr.io": map[string]any{"auth": "eA=="}},
	})

	if got := RegistryAuth(t.Context(), "quay.io/o/r"); got != "" {
		t.Errorf("RegistryAuth = %q, want nothing for a registry with no stored login", got)
	}
}

func TestRegistryAuthNoConfigIsEmpty(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())

	if got := RegistryAuth(t.Context(), "ghcr.io/o/r"); got != "" {
		t.Errorf("RegistryAuth = %q, want nothing when there is no configuration at all", got)
	}
}

// writeCredentialHelper installs a `docker-credential-<name>` on PATH that
// answers with the given username and secret, and records the registry it was
// asked about.
func writeCredentialHelper(t *testing.T, name, username, secret string) (asked string) {
	t.Helper()

	dir := t.TempDir()
	askedPath := filepath.Join(dir, "asked")

	script := "#!/bin/sh\ncat > " + askedPath + "\n" +
		"printf '%s' '{\"Username\":\"" + username + "\",\"Secret\":\"" + secret + "\"}'\n"

	err := os.WriteFile(filepath.Join(dir, "docker-credential-"+name), []byte(script), 0o700) //nolint:gosec // a test fixture that has to be executable
	if err != nil {
		t.Fatalf("writing the credential helper: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return askedPath
}

func TestRegistryAuthUsesACredentialHelper(t *testing.T) {
	askedPath := writeCredentialHelper(t, "faux", "helper-user", "helper-secret")

	writeAuthConfig(t, map[string]any{
		"credHelpers": map[string]any{"ghcr.io": "faux"},
	})

	got := decodeAuth(t, RegistryAuth(t.Context(), "ghcr.io/o/r"))

	if got.Username != "helper-user" || got.Password != "helper-secret" {
		t.Errorf("RegistryAuth = %+v, want the helper's answer", got)
	}

	asked, err := os.ReadFile(askedPath) //nolint:gosec // a path this test just created
	if err != nil {
		t.Fatalf("reading what the helper was asked: %v", err)
	}

	// The helper protocol is "the registry on stdin", and a helper asked
	// about the wrong one answers confidently with the wrong credential.
	if string(asked) != "ghcr.io" {
		t.Errorf("the helper was asked about %q, want ghcr.io", asked)
	}
}

// TestRegistryAuthPrefersAHelperOverAStoredLogin pins the precedence docker
// uses: a per-registry helper is the more specific answer, and a stale `auths`
// entry left behind by an older login must not win over it.
func TestRegistryAuthPrefersAHelperOverAStoredLogin(t *testing.T) {
	writeCredentialHelper(t, "faux", "helper-user", "helper-secret")

	writeAuthConfig(t, map[string]any{
		"credHelpers": map[string]any{"ghcr.io": "faux"},
		"auths": map[string]any{
			"ghcr.io": map[string]any{"auth": base64.StdEncoding.EncodeToString([]byte("stale:stale"))},
		},
	})

	got := decodeAuth(t, RegistryAuth(t.Context(), "ghcr.io/o/r"))

	if got.Username != "helper-user" {
		t.Errorf("Username = %q, want the helper to outrank a stored login", got.Username)
	}
}

// TestRegistryAuthUsesTheGlobalCredsStore pins the arrangement Docker Desktop
// produces: no auths at all, one helper for everything.
func TestRegistryAuthUsesTheGlobalCredsStore(t *testing.T) {
	writeCredentialHelper(t, "faux", "store-user", "store-secret")

	writeAuthConfig(t, map[string]any{"credsStore": "faux"})

	got := decodeAuth(t, RegistryAuth(t.Context(), "ghcr.io/o/r"))

	if got.Username != "store-user" || got.Password != "store-secret" {
		t.Errorf("RegistryAuth = %+v, want the credential store's answer", got)
	}
}

// TestRegistryAuthSurvivesABrokenHelper pins that a helper which fails leaves
// the pull anonymous rather than failing the run.
//
// An absent or erroring helper is the normal answer for a registry the
// operator never logged into — `docker-credential-osxkeychain` exits nonzero
// for a URL it has nothing for — so treating it as fatal would break every
// anonymous pull on a machine that has a helper configured.
func TestRegistryAuthSurvivesABrokenHelper(t *testing.T) {
	writeAuthConfig(t, map[string]any{
		"credHelpers": map[string]any{"ghcr.io": "definitely-not-installed"},
	})

	if got := RegistryAuth(t.Context(), "ghcr.io/o/r"); got != "" {
		t.Errorf("RegistryAuth = %q, want an anonymous pull when the helper cannot answer", got)
	}
}

// decodeAuth unpacks the header value RegistryAuth produces, which is the
// base64 of a JSON credential — the shape the engine API expects.
func decodeAuth(t *testing.T, encoded string) authConfig {
	t.Helper()

	if encoded == "" {
		t.Fatal("RegistryAuth returned nothing, want a credential")
	}

	raw, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decoding the auth header: %v", err)
	}

	var config authConfig

	err = json.Unmarshal(raw, &config)
	if err != nil {
		t.Fatalf("decoding the auth payload: %v", err)
	}

	return config
}
