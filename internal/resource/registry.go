package resource

// The built-in registry-image type: check finds the digest a tag points at, in names that digest for a step's image:, and out pushes an OCI image a build made. Concourse's registry-image resource is the model, and go-containerregistry the client it uses too.
//
// The credential is the resource's, never the operator's: no keychain, no docker config.json, no credential helper is consulted, so what a pipeline can reach is what its source: says, on any machine.

import (
	"archive/tar"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	ocitarball "github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// registryDigestField is the one key of a registry-image version, the field config.ImageReference reads.
const registryDigestField = "digest"

// RegistryImageResource is one registry-image resource, parsed.
type RegistryImageResource struct {
	source     config.RegistryImageSource
	repository name.Repository
}

// RegistryImage parses one registry-image resource. Preflight calls it too, so a repository the client cannot name is refused where the pipeline is set.
func RegistryImage(res config.Resource) (*RegistryImageResource, error) {
	source, err := config.ParseRegistryImageSource(res.Source)
	if err != nil {
		return nil, fmt.Errorf("resource %q: %w", res.Name, err)
	}

	var nameOptions []name.Option
	if source.Insecure {
		nameOptions = append(nameOptions, name.Insecure)
	}

	repository, err := name.NewRepository(source.Repository, nameOptions...)
	if err != nil {
		return nil, fmt.Errorf("resource %q: source.repository %q: %w", res.Name, source.Repository, err)
	}

	return &RegistryImageResource{source: source, repository: repository}, nil
}

// options is how every request of this resource authenticates: the resource's login, or none.
func (r *RegistryImageResource) options(ctx context.Context) ([]remote.Option, error) {
	transport, err := r.transport()
	if err != nil {
		return nil, err
	}

	options := []remote.Option{remote.WithContext(ctx)}

	if r.source.PasswordEnv == "" {
		return append(options, remote.WithAuth(authn.Anonymous), remote.WithTransport(transport)), nil
	}

	password, err := r.password(shell.SecretEnv(ctx))
	if err != nil {
		return nil, err
	}

	// insecure: is the author saying plain http is fine for this registry, login and all.
	var guarded http.RoundTripper = cleartextGuard{next: transport}
	if r.source.Insecure {
		guarded = transport
	}

	return append(options,
		remote.WithAuth(authn.FromConfig(authn.AuthConfig{Username: r.source.Username, Password: password})),
		remote.WithTransport(guarded),
	), nil
}

// transport is go-containerregistry's own, trusting source.ca_certs beside the system's roots, or nothing at all under insecure:.
func (r *RegistryImageResource) transport() (http.RoundTripper, error) {
	if !r.source.Insecure && len(r.source.CACerts) == 0 {
		return remote.DefaultTransport, nil
	}

	base, ok := remote.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("go-containerregistry's default transport is not an *http.Transport")
	}

	transport := base.Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}

	if r.source.Insecure {
		transport.TLSClientConfig.InsecureSkipVerify = true

		return transport, nil
	}

	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}

	for i, cert := range r.source.CACerts {
		if !pool.AppendCertsFromPEM([]byte(cert)) {
			return nil, fmt.Errorf("source.ca_certs[%d] holds no certificate", i)
		}
	}

	transport.TLSClientConfig.RootCAs = pool

	return transport, nil
}

// password reads password_env from the resource's resolved secrets first, so it may name a secrets: key, then the daemon's environment: github-*'s token_env rule.
func (r *RegistryImageResource) password(secrets map[string]string) (string, error) {
	password, ok := secrets[r.source.PasswordEnv]
	if !ok {
		password = os.Getenv(r.source.PasswordEnv)
	}

	if password == "" {
		return "", fmt.Errorf("$%s is not set (source.password_env)", r.source.PasswordEnv)
	}

	return password, nil
}

// WithTypePullLogin attaches a type: registry-image entry's login to every pull of its image under ctx, so the type's check, in and out, wherever they run, pull with it; a type with none is left anonymous.
func WithTypePullLogin(ctx context.Context, rt config.ResourceType) (context.Context, error) {
	username, variable := rt.PullLogin()
	if username == "" || rt.Image == "" {
		return ctx, nil
	}

	password := os.Getenv(variable)
	if password == "" {
		return ctx, fmt.Errorf("resource_type %q: $%s is not set (source.password_env)", rt.Name, variable)
	}

	return shell.WithPullLogin(ctx, rt.Image, username, password), nil
}

// WithTypePullLogins is WithTypePullLogin for every type in cfg, for the pull of every image a run names before its first step.
func WithTypePullLogins(ctx context.Context, cfg *config.Config) (context.Context, error) {
	for _, rt := range cfg.ResourceTypes {
		var err error

		ctx, err = WithTypePullLogin(ctx, rt)
		if err != nil {
			return ctx, err
		}
	}

	return ctx, nil
}

// RegistryLogin is the login a step's docker daemon pulls res's image with: its username and password_env, or none (an anonymous pull) for a resource that names none or is not the built-in registry-image's. The same login the resource's check and get used, so what the check could see the step can run.
func RegistryLogin(ctx context.Context, cfg *config.Config, res config.Resource) (username, password string, err error) {
	resourceType, err := cfg.FindResourceType(res.Type)
	if err != nil || resourceType == nil || !resourceType.Config.Registry {
		return "", "", nil //nolint:nilerr // an unresolvable type is a load error reported elsewhere; it has no login to give
	}

	image, err := RegistryImage(res)
	if err != nil || image.source.PasswordEnv == "" {
		return "", "", err
	}

	var secrets map[string]string

	if _, named := res.Secrets[image.source.PasswordEnv]; named {
		secrets, err = LookupSecrets(ctx, res.Name, res.Secrets)
		if err != nil {
			return "", "", err
		}
	}

	password, err = image.password(secrets)
	if err != nil {
		return "", "", fmt.Errorf("resource %q: %w", res.Name, err)
	}

	return image.source.Username, password, nil
}

// cleartextGuard refuses plain http to anything but this machine once a login is in play. go-containerregistry falls back to http on its own for a private-network address, which would send the password in the clear.
type cleartextGuard struct {
	next http.RoundTripper
}

func (g cleartextGuard) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme == "http" && !config.IsLoopbackHost(request.URL.Hostname()) {
		return nil, fmt.Errorf("refusing to send source.password_env's login to %s over plain http; only a registry on this machine may be reached without TLS", request.URL.Host)
	}

	return g.next.RoundTrip(request) //nolint:wrapcheck // the transport's own error, which go-containerregistry wraps
}

// describe names a registry's refusal by what it means here rather than by status code alone.
func describe(ref name.Reference, err error) error {
	var terr *transport.Error
	if errors.As(err, &terr) {
		switch terr.StatusCode {
		case http.StatusNotFound:
			return fmt.Errorf("%s does not exist in the registry", ref)
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("%s: the registry refused the login (%d); check source.username and source.password_env", ref, terr.StatusCode)
		}
	}

	return fmt.Errorf("%s: %w", ref, err)
}

type registryBackend struct{}

// check is the tag's digest, one version: a tag that is not there fails rather than reading as "no versions yet", so a typo cannot hold every consumer back in silence.
func (registryBackend) check(ctx context.Context, c Call) ([]map[string]any, error) {
	image, err := RegistryImage(c.Resource)
	if err != nil {
		return nil, err
	}

	options, err := image.options(ctx)
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", c.Resource.Name, err)
	}

	ref := image.repository.Tag(image.source.TagOrDefault())

	// HEAD, which the registry answers with the digest alone, and which Docker Hub does not count against its pull limit.
	descriptor, err := remote.Head(ref, options...)
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", c.Resource.Name, describe(ref, err))
	}

	slog.InfoContext(ctx, "resource.checked", "resource_type", c.Type.Name, "digest", descriptor.Digest.String())

	return []map[string]any{{registryDigestField: descriptor.Digest.String()}}, nil
}

// in writes what the version names and fetches no layers: a step's image: is pulled by its docker daemon, by digest. It asks the registry once that the digest is still there, so a deleted image fails here, by name, rather than as a step that cannot start.
func (registryBackend) in(ctx context.Context, c Call) error {
	image, err := RegistryImage(c.Resource)
	if err != nil {
		return err
	}

	raw := versionField(c.Version, registryDigestField)

	digest, err := v1.NewHash(raw)
	if err != nil {
		return fmt.Errorf("in %q: version digest %q: %w", c.Resource.Name, raw, err)
	}

	options, err := image.options(ctx)
	if err != nil {
		return fmt.Errorf("in %q: %w", c.Resource.Name, err)
	}

	ref := image.repository.Digest(digest.String())

	_, err = remote.Head(ref, options...)
	if err != nil {
		return fmt.Errorf("in %q: %w", c.Resource.Name, describe(ref, err))
	}

	files := map[string]string{
		"repository": image.source.Repository,
		"tag":        image.source.TagOrDefault(),
		"digest":     digest.String(),
	}

	for file, content := range files {
		err := os.WriteFile(filepath.Join(c.Dir, file), []byte(content), 0o644) //nolint:gosec // metadata a step reads, not a secret
		if err != nil {
			return fmt.Errorf("in %q: %w", c.Resource.Name, err)
		}
	}

	return nil
}

// out pushes the image params.image names to source.tag and each additional tag; its version is the pushed digest, the same shape the check finds.
func (registryBackend) out(ctx context.Context, c Call) (map[string]any, error) {
	image, err := RegistryImage(c.Resource)
	if err != nil {
		return nil, err
	}

	params, err := config.ParseRegistryImagePutParams(c.Params)
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", c.Resource.Name, err)
	}

	tags, err := additionalTags(c.Dir, params.AdditionalTags)
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", c.Resource.Name, err)
	}

	options, err := image.options(ctx)
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", c.Resource.Name, err)
	}

	path, err := insideDir(c.Dir, params.Image)
	if err != nil {
		return nil, fmt.Errorf("out %q: params.image: %w", c.Resource.Name, err)
	}

	pushable, cleanup, err := loadImage(path)
	if err != nil {
		return nil, fmt.Errorf("out %q: params.image %q: %w", c.Resource.Name, params.Image, err)
	}

	defer cleanup()

	digest, err := pushable.Digest()
	if err != nil {
		return nil, fmt.Errorf("out %q: params.image %q: %w", c.Resource.Name, params.Image, err)
	}

	for _, tag := range append([]string{image.source.TagOrDefault()}, tags...) {
		ref := image.repository.Tag(tag)

		err := pushable.push(ref, options)
		if err != nil {
			return nil, fmt.Errorf("out %q: pushing %s: %w", c.Resource.Name, ref, describe(ref, err))
		}

		slog.InfoContext(ctx, "resource.put", "resource_type", c.Type.Name, "ref", ref.String(), "digest", digest.String())
	}

	return map[string]any{registryDigestField: digest.String()}, nil
}

// insideDir resolves a params: path under the put's tree, refusing one a symlink carries out of it.
func insideDir(dir, rel string) (string, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	resolved, err := filepath.EvalSymlinks(filepath.Join(dir, rel))
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	within, err := filepath.Rel(root, resolved)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q leads outside the put's inputs", rel)
	}

	return resolved, nil
}

func additionalTags(dir, rel string) ([]string, error) {
	if rel == "" {
		return nil, nil
	}

	path, err := insideDir(dir, rel)
	if err != nil {
		return nil, fmt.Errorf("params.additional_tags: %w", err)
	}

	content, err := os.ReadFile(path) //nolint:gosec // insideDir kept it within the put's inputs
	if err != nil {
		return nil, fmt.Errorf("params.additional_tags: %w", err)
	}

	tags := strings.Fields(string(content))
	for _, tag := range tags {
		_, err := name.NewTag("example.invalid/x:" + tag)
		if err != nil {
			return nil, fmt.Errorf("params.additional_tags: %q is not a tag", tag)
		}
	}

	return tags, nil
}

// pushable is the one image or index an OCI layout or tarball holds.
type pushable struct {
	image v1.Image
	index v1.ImageIndex
}

func (p pushable) Digest() (v1.Hash, error) {
	if p.index != nil {
		return p.index.Digest() //nolint:wrapcheck // the caller names the image
	}

	return p.image.Digest() //nolint:wrapcheck // as above
}

func (p pushable) push(ref name.Tag, options []remote.Option) error {
	if p.index != nil {
		return remote.WriteIndex(ref, p.index, options...) //nolint:wrapcheck // the caller names the ref
	}

	return remote.Write(ref, p.image, options...) //nolint:wrapcheck // as above
}

// loadImage reads an OCI layout directory, a tarball of one (BuildKit's type=oci output), or a docker save tarball. The cleanup removes what unpacking a tarball left behind.
func loadImage(path string) (pushable, func(), error) {
	noop := func() {}

	info, err := os.Stat(path)
	if err != nil {
		return pushable{}, noop, fmt.Errorf("%w", err)
	}

	if info.IsDir() {
		loaded, err := fromLayout(path)

		return loaded, noop, err
	}

	isLayout, err := tarHasEntry(path, "oci-layout")
	if err != nil {
		return pushable{}, noop, err
	}

	if !isLayout {
		image, err := ocitarball.ImageFromPath(path, nil)
		if err != nil {
			return pushable{}, noop, fmt.Errorf("neither an OCI layout nor a docker save tarball: %w", err)
		}

		return pushable{image: image}, noop, nil
	}

	unpacked, err := os.MkdirTemp("", "steps-registry-image-")
	if err != nil {
		return pushable{}, noop, fmt.Errorf("%w", err)
	}

	cleanup := func() { _ = os.RemoveAll(unpacked) }

	err = untarLayout(path, unpacked)
	if err != nil {
		cleanup()

		return pushable{}, noop, err
	}

	loaded, err := fromLayout(unpacked)
	if err != nil {
		cleanup()

		return pushable{}, noop, err
	}

	return loaded, cleanup, nil
}

// fromLayout is the layout's one top-level manifest: an image, or an index (a multi-platform build). A layout holding several is ambiguous about which is meant, so it is refused rather than guessed at.
func fromLayout(dir string) (pushable, error) {
	index, err := layout.ImageIndexFromPath(dir)
	if err != nil {
		return pushable{}, fmt.Errorf("not an OCI layout: %w", err)
	}

	manifest, err := index.IndexManifest()
	if err != nil {
		return pushable{}, fmt.Errorf("%w", err)
	}

	if len(manifest.Manifests) != 1 {
		return pushable{}, fmt.Errorf("the OCI layout holds %d images; push one", len(manifest.Manifests))
	}

	descriptor := manifest.Manifests[0]
	if descriptor.MediaType.IsIndex() {
		child, err := index.ImageIndex(descriptor.Digest)

		return pushable{index: child}, err //nolint:wrapcheck // the caller names the image
	}

	image, err := index.Image(descriptor.Digest)

	return pushable{image: image}, err //nolint:wrapcheck // as above
}

func tarHasEntry(path, entry string) (bool, error) {
	file, err := os.Open(path) //nolint:gosec // insideDir kept it within the put's inputs
	if err != nil {
		return false, fmt.Errorf("%w", err)
	}

	defer func() { _ = file.Close() }()

	reader := tar.NewReader(file)

	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return false, nil
		}

		if err != nil {
			return false, fmt.Errorf("not a tarball: %w", err)
		}

		if filepath.Clean(header.Name) == entry {
			return true, nil
		}
	}
}

// untarLayout unpacks regular files and directories only: an OCI layout has nothing else, and a link or a path climbing out of dst is a tarball that is not one.
func untarLayout(path, dst string) error {
	file, err := os.Open(path) //nolint:gosec // insideDir kept it within the put's inputs
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	defer func() { _ = file.Close() }()

	reader := tar.NewReader(file)

	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("unpacking: %w", err)
		}

		err = unpackLayoutEntry(dst, header, reader)
		if err != nil {
			return fmt.Errorf("unpacking %q: %w", header.Name, err)
		}
	}
}

func unpackLayoutEntry(dst string, header *tar.Header, content io.Reader) error {
	clean := filepath.Clean(header.Name)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return errors.New("the entry leaves the layout")
	}

	target := filepath.Join(dst, clean)

	switch header.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, 0o700) //nolint:wrapcheck // the caller names the entry
	case tar.TypeReg:
		return writeEntry(target, content)
	default:
		return errors.New("the entry is not a file or directory")
	}
}

func writeEntry(target string, content io.Reader) error {
	err := os.MkdirAll(filepath.Dir(target), 0o700)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // target is under dst, checked by the caller
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	_, err = io.Copy(out, content)
	closeErr := out.Close()

	return errors.Join(err, closeErr)
}
