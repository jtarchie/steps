package dockerapi

// Which credentials, for a registry the daemon is about to be sent to.
//
// The daemon has no access to anybody's keychain: a pull's login travels on
// the request. steps sends only a login its caller attached for that exact
// image (WithPullLogin), which is a registry-image resource's own: never one
// read from docker's config.json, a credential helper or a keychain. What a
// pipeline can pull is then what it says, on any machine, and a pull with no
// login attached is anonymous.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"strings"

	"github.com/distribution/reference"
)

// dockerHubAuthKey is the server address a Docker Hub login is sent for. Not a
// hostname: the engine files Hub under this legacy v1 URL.
const dockerHubAuthKey = "https://index.docker.io/v1/"

// Login is a registry login one pull carries.
type Login struct {
	Username string
	Password string
}

// authConfig is the credential the engine API expects, base64'd onto the
// request. Only the fields a pull needs are named.
type authConfig struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	ServerAddress string `json:"serveraddress,omitempty"`
}

type pullLoginsKey struct{}

// WithPullLogin attaches login to every pull of exactly image under ctx. Keyed
// on the whole reference, digest included, so a login reaches the one image it
// was named for and no other image a step happens to pull.
func WithPullLogin(ctx context.Context, image string, login Login) context.Context {
	logins := maps.Clone(pullLogins(ctx))
	if logins == nil {
		logins = map[string]Login{}
	}

	logins[image] = login

	return context.WithValue(ctx, pullLoginsKey{}, logins)
}

func pullLogins(ctx context.Context) map[string]Login {
	logins, _ := ctx.Value(pullLoginsKey{}).(map[string]Login)

	return logins
}

// registryAuth is the value to send with a pull of image, "" for an anonymous
// one.
//
// Empty is a real answer: most pulls are anonymous, and sending a blank
// credential is NOT the same as sending none — a registry that serves
// anonymous reads can reject an empty login outright.
func registryAuth(ctx context.Context, image string) string {
	login, ok := pullLogins(ctx)[image]
	if !ok || login.Username == "" {
		return ""
	}

	//nolint:gosec // a credential IS the payload here; it travels to the daemon on the request, which is the whole mechanism
	encoded, err := json.Marshal(authConfig{Username: login.Username, Password: login.Password, ServerAddress: registryFor(image)})
	if err != nil {
		return ""
	}

	// URL encoding, because the value travels in an HTTP header.
	return base64.URLEncoding.EncodeToString(encoded)
}

// registryFor names the registry an image reference is served by, in the
// spelling the engine expects a login's server address in. The host rule is
// docker's own, through reference.Domain: "myteam/myimage" is Docker Hub,
// "localhost:5000/myimage" is not.
//
// Only the name is parsed: the digest cannot move an image to another
// registry, and a malformed one should reach the daemon, which says so. A
// name that does not parse is not one docker would pull either, so Hub's key
// is as good an answer as any.
func registryFor(ref string) string {
	name, _, _ := strings.Cut(ref, "@")

	named, err := reference.ParseNormalizedNamed(name)
	if err != nil {
		return dockerHubAuthKey
	}

	switch domain := reference.Domain(named); domain {
	case "docker.io", "registry-1.docker.io":
		return dockerHubAuthKey
	default:
		return domain
	}
}
