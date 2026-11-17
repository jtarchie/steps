package config

// type: registry-image: an image in an OCI registry, found by digest, fetched as the reference a step's image: runs, and published from an OCI tarball. Go rather than YAML because the registry protocol (token handshakes, blob uploads) is a client library's job, not a shell's.

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

// RegistryImageSource is a registry-image resource's source:.
//
// The credential is the resource's and nothing else's: the operator's docker config is never read, so the login a pipeline runs with is the one written in it.
type RegistryImageSource struct {
	// Repository is the image name a pull would use, registry host included unless it is Docker Hub.
	Repository string `yaml:"repository"`
	// Tag is the one tag the check follows, latest when unset.
	Tag string `yaml:"tag,omitempty"`
	// Username goes with password_env; a registry that takes a token alone (ghcr) still wants a name, any name.
	Username string `yaml:"username,omitempty"`
	// PasswordEnv NAMES the variable holding the password or token, never the value: a literal would be hashed into state.db.
	PasswordEnv string `yaml:"password_env,omitempty"`
}

// DefaultRegistryImageTag is the tag a source with none follows, as docker's own default.
const DefaultRegistryImageTag = "latest"

// TagOrDefault is the tag the check follows.
func (s RegistryImageSource) TagOrDefault() string {
	if s.Tag == "" {
		return DefaultRegistryImageTag
	}

	return s.Tag
}

// RegistryImagePutParams is a put's params: to a registry-image resource.
type RegistryImagePutParams struct {
	// Image is the path, inside the put's inputs, of the OCI image to push: an OCI layout directory, or a tarball of one (BuildKit's type=oci output), or a docker save tarball.
	Image string `yaml:"image"`
	// AdditionalTags is the path of a file of whitespace-separated tags the pushed image also gets, beside source.tag.
	AdditionalTags string `yaml:"additional_tags,omitempty"`
}

// ParseRegistryImageSource decodes a registry-image source: strictly.
func ParseRegistryImageSource(raw map[string]any) (RegistryImageSource, error) {
	var source RegistryImageSource

	err := decodeSource(raw, &source)
	if err != nil {
		return source, err
	}

	switch {
	case strings.TrimSpace(source.Repository) == "":
		return source, errors.New("a registry-image resource needs source.repository, the image name, e.g. ghcr.io/owner/name")
	// ImageReference refuses the same, later and with less to say; a step would only find out when it ran.
	case !isBareRepository(source.Repository):
		return source, fmt.Errorf("source.repository %q must be a bare image name: a tag goes in source.tag, and a digest is what the check finds", source.Repository)
	case strings.ContainsAny(source.Tag, "@:/ \t\n"):
		return source, fmt.Errorf("source.tag %q is not a tag", source.Tag)
	case source.PasswordEnv != "" && !envVarPattern.MatchString(source.PasswordEnv):
		return source, fmt.Errorf("source.password_env must be the NAME of an environment variable holding the password, not %q — a literal would be hashed into state.db in cleartext", source.PasswordEnv)
	case (source.Username == "") != (source.PasswordEnv == ""):
		return source, errors.New("source.username and source.password_env go together: a registry login is both, and one alone would be sent as an anonymous request")
	}

	return source, nil
}

// isBareRepository is a name with no tag or digest in it: the one ':' it may hold is a registry host's port (localhost:5050/tools).
func isBareRepository(repository string) bool {
	if strings.HasPrefix(repository, "-") || strings.ContainsAny(repository, "@ \t\n") {
		return false
	}

	host, rest, found := strings.Cut(repository, "/")
	if !found {
		return !strings.Contains(repository, ":")
	}

	return !strings.Contains(rest, ":") && strings.Count(host, ":") <= 1
}

// validateRegistryImageResources holds the built-in's resources and puts to its closed shape. A pipeline that declares its own registry-image type replaces the built-in, as one declaring git does, and nativeType then answers "" for its resources: their source: and params: are that type's business.
func (c *Config) validateRegistryImageResources() error {
	for _, resource := range c.Resources {
		if c.nativeType(resource.Type) != RegistryImageType {
			continue
		}

		if len(resource.Env) > 0 {
			return fmt.Errorf("resource %q: env: has no effect on a registry-image resource — it reads exactly one variable, the one source.password_env names; drop env: here", resource.Name)
		}

		_, err := ParseRegistryImageSource(resource.Source)
		if err != nil {
			return fmt.Errorf("resource %q (registry-image): %w", resource.Name, err)
		}
	}

	return c.validateRegistryImagePuts()
}

func (c *Config) validateRegistryImagePuts() error {
	for i := range c.Jobs {
		err := c.Jobs[i].visitSteps(func(label string, step *Step) error {
			if step.Put == "" || c.nativeResourceType(step.PutResourceName()) != RegistryImageType {
				return nil
			}

			err := checkRegistryImagePut(step)
			if err != nil {
				return fmt.Errorf("%s: put %q: %w", label, step.Put, err)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func checkRegistryImagePut(step *Step) error {
	var params RegistryImagePutParams

	err := decodeParams(step.Params, &params)
	if err != nil {
		return err
	}

	if params.Image == "" {
		return errors.New("params.image is required: the OCI image to push, as an input's path (image/image.tar)")
	}

	for field, value := range map[string]string{"image": params.Image, "additional_tags": params.AdditionalTags} {
		if value == "" {
			continue
		}

		if path.IsAbs(value) || strings.HasPrefix(path.Clean(value), "..") {
			return fmt.Errorf("params.%s %q must be a path inside the put's inputs", field, value)
		}

		root := firstPathComponent(value)
		if !step.InputsAll() && !slices.Contains(step.InputNames(), root) {
			return fmt.Errorf("params.%s %q names artifact %q, which the put does not list in its inputs", field, value, root)
		}
	}

	return nil
}

// ParseRegistryImagePutParams decodes a put's params: to a registry-image resource; the load rules have checked them already.
func ParseRegistryImagePutParams(raw map[string]any) (RegistryImagePutParams, error) {
	var params RegistryImagePutParams

	err := decodeParams(raw, &params)

	return params, err
}
