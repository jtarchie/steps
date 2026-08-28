package dockerapi

// Which credentials, for a registry the daemon is about to be sent to.
//
// `docker pull` reads this out of config.json and the credential helpers; the
// engine API does not, because the DAEMON has no access to the user's
// keychain — the client resolves the credential and sends it on the request.
// Skipping that step does not degrade to "public images still work": it turns
// every pull from a private registry into a 401 on a machine where `docker
// pull` succeeds, which is the least debuggable shape a regression can take.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// dockerHubAuthKey is where a Docker Hub login is filed. Not a hostname:
// `docker login` writes this legacy v1 URL, and a lookup by "docker.io" finds
// nothing on a machine that is logged in.
const dockerHubAuthKey = "https://index.docker.io/v1/"

// credentialHelperTimeout bounds one helper. They are small programs that
// usually answer instantly, but one of them talks to a system keychain that
// can prompt — and a prompt nobody can see is indistinguishable from a hang.
const credentialHelperTimeout = 10 * time.Second

// authConfig is the credential the engine API expects, base64'd onto the
// request. Only the fields a pull needs are named.
type authConfig struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
	ServerAddress string `json:"serveraddress,omitempty"`
}

// dockerConfig is the part of config.json that decides credentials.
type dockerConfig struct {
	Auths map[string]struct {
		Auth     string `json:"auth"`
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"auths"`
	CredsStore  string            `json:"credsStore"`
	CredHelpers map[string]string `json:"credHelpers"`
}

// RegistryAuth returns the value to send with a pull of reference, or "" for
// an anonymous one.
//
// Empty is a real answer, not a failure: most pulls are anonymous, and sending
// a blank credential is NOT the same as sending none — a registry that serves
// anonymous reads can reject an empty login outright.
func RegistryAuth(ctx context.Context, reference string) string {
	registry := registryFor(reference)

	config, ok := readDockerConfig()
	if !ok {
		return ""
	}

	credential, ok := lookupCredential(ctx, config, registry)
	if !ok {
		return ""
	}

	credential.ServerAddress = registry

	//nolint:gosec // a credential IS the payload here; it travels to the daemon on the request, which is the whole mechanism
	encoded, err := json.Marshal(credential)
	if err != nil {
		return ""
	}

	// URL encoding, because the value travels in an HTTP header.
	return base64.URLEncoding.EncodeToString(encoded)
}

// lookupCredential asks the three places docker asks, in docker's order: a
// helper named for this registry, the global credential store, then a login
// written straight into config.json.
func lookupCredential(ctx context.Context, config dockerConfig, registry string) (authConfig, bool) {
	if helper, ok := config.CredHelpers[registry]; ok {
		return runCredentialHelper(ctx, helper, registry)
	}

	if config.CredsStore != "" {
		return runCredentialHelper(ctx, config.CredsStore, registry)
	}

	entry, ok := config.Auths[registry]
	if !ok {
		return authConfig{}, false
	}

	if entry.Auth == "" {
		return authConfig{Username: entry.Username, Password: entry.Password}, entry.Username != ""
	}

	decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
	if err != nil {
		return authConfig{}, false
	}

	username, password, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return authConfig{}, false
	}

	return authConfig{Username: username, Password: password}, true
}

// runCredentialHelper asks `docker-credential-<name> get` about a registry,
// which is the whole protocol: the registry on stdin, a JSON credential on
// stdout.
//
// The caller's context bounds it on top of the timeout, so a run that is being
// cancelled is not held up by a helper waiting on a keychain.
//
// A helper that is missing or answers nonzero leaves the pull anonymous rather
// than failing the run. That is the NORMAL answer for a registry the operator
// never logged into — docker-credential-osxkeychain exits nonzero for a URL it
// has nothing for — so treating it as fatal would break every anonymous pull
// on a machine that has a credential store configured, which on macOS is most
// of them.
func runCredentialHelper(ctx context.Context, name, registry string) (authConfig, bool) {
	ctx, cancel := context.WithTimeout(ctx, credentialHelperTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker-credential-"+name, "get") //nolint:gosec // the helper name comes from the operator's own docker configuration, exactly as the docker CLI reads it
	cmd.Stdin = strings.NewReader(registry)

	out, err := cmd.Output()
	if err != nil {
		slog.Debug("dockerapi.credential_helper_declined", "helper", name, "registry", registry, "error", err)

		return authConfig{}, false
	}

	var answer struct {
		Username string `json:"Username"`
		Secret   string `json:"Secret"`
	}

	err = json.Unmarshal(out, &answer)
	if err != nil {
		slog.Debug("dockerapi.credential_helper_unreadable", "helper", name, "registry", registry, "error", err)

		return authConfig{}, false
	}

	// A helper answers an identity-token login by naming this sentinel user,
	// which the API expects in IdentityToken rather than as a password.
	if answer.Username == "<token>" {
		return authConfig{IdentityToken: answer.Secret}, true
	}

	if answer.Username == "" {
		return authConfig{}, false
	}

	return authConfig{Username: answer.Username, Password: answer.Secret}, true
}

// readDockerConfig loads config.json, reporting whether there was one.
func readDockerConfig() (dockerConfig, bool) {
	raw, err := os.ReadFile(filepath.Join(configDir(), "config.json"))
	if err != nil {
		return dockerConfig{}, false
	}

	var config dockerConfig

	err = json.Unmarshal(raw, &config)
	if err != nil {
		return dockerConfig{}, false
	}

	return config, true
}

// registryFor names the registry an image reference is served by, in the
// spelling credentials are filed under.
//
// The rule is docker's: the first path component is a registry only if it
// LOOKS like a host — it contains a dot or a colon, or it is localhost.
// Everything else is Docker Hub, which is why "myteam/myimage" is a Hub image
// and "localhost:5000/myimage" is not.
func registryFor(reference string) string {
	first, _, hasSlash := strings.Cut(reference, "/")
	if !hasSlash {
		return dockerHubAuthKey
	}

	if !strings.Contains(first, ".") && !strings.Contains(first, ":") && first != "localhost" {
		return dockerHubAuthKey
	}

	if first == "docker.io" || first == "index.docker.io" || first == "registry-1.docker.io" {
		return dockerHubAuthKey
	}

	return first
}
