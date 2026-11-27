package config

// type: s3: objects in an S3 bucket (or anything that speaks S3), versioned by a number in the file name or by the bucket's own versioning. Concourse's s3 resource, with its source and params names. Go rather than YAML because requests are signed here, so the keys never reach the machine that moves the bytes.

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
)

// S3Type is the built-in type's name.
const S3Type = "s3"

// S3Source is an s3 resource's source:.
type S3Source struct {
	Bucket string `yaml:"bucket"`
	// Regexp is a /-separated pattern matched against whole keys; its capture group (the one named version, else the first) is the version.
	Regexp string `yaml:"regexp,omitempty"`
	// VersionedFile is one key in a bucket with versioning on; each upload of it is a version.
	VersionedFile string `yaml:"versioned_file,omitempty"`
	RegionName    string `yaml:"region_name,omitempty"`
	// Endpoint is an S3-compatible server that is not AWS, scheme included.
	Endpoint string `yaml:"endpoint,omitempty"`
	// UsePathStyle addresses the bucket in the path (endpoint/bucket/key) rather than as a host name, which a server with no per-bucket DNS needs.
	UsePathStyle bool `yaml:"use_path_style,omitempty"`
	// SkipDownload has a get write the files that name the object and not the object.
	SkipDownload bool `yaml:"skip_download,omitempty"`
	// AccessKeyIDEnv, SecretAccessKeyEnv and SessionTokenEnv NAME the variables holding the credential, never the values: a literal would be hashed into state.db. With none, requests are anonymous, as Concourse's are.
	AccessKeyIDEnv     string `yaml:"access_key_id_env,omitempty"`
	SecretAccessKeyEnv string `yaml:"secret_access_key_env,omitempty"`
	SessionTokenEnv    string `yaml:"session_token_env,omitempty"`
}

// DefaultS3Image is the image a transfer runs in on a worker that only runs containers: the curl project's own, and all a transfer needs is curl and a shell.
const DefaultS3Image = "curlimages/curl:8.11.1"

// DefaultS3Region is the region a source with none signs for, as Concourse's.
const DefaultS3Region = "us-east-1"

// Region is the region requests are signed for.
func (s S3Source) Region() string {
	if s.RegionName == "" {
		return DefaultS3Region
	}

	return s.RegionName
}

// S3GetParams is a get's params: from an s3 resource.
type S3GetParams struct {
	// SkipDownload overrides the source's for this get.
	SkipDownload *bool `yaml:"skip_download,omitempty"`
}

// S3PutParams is a put's params: to an s3 resource.
type S3PutParams struct {
	// File is a glob, inside the put's inputs, that must match exactly one file.
	File        string `yaml:"file"`
	ContentType string `yaml:"content_type,omitempty"`
}

// ParseS3Source decodes an s3 source: strictly.
func ParseS3Source(raw map[string]any) (S3Source, error) {
	var source S3Source

	err := decodeSource(raw, &source)
	if err != nil {
		return source, err
	}

	switch {
	case strings.TrimSpace(source.Bucket) == "":
		return source, errors.New("an s3 resource needs source.bucket")
	case (source.Regexp == "") == (source.VersionedFile == ""):
		return source, errors.New("an s3 resource needs exactly one of source.regexp (a version in the file name) or source.versioned_file (a bucket with versioning on)")
	}

	err = checkS3Credential(source)
	if err != nil {
		return source, err
	}

	err = checkS3Endpoint(source.Endpoint)
	if err != nil {
		return source, err
	}

	return source, checkS3Regexp(source.Regexp)
}

func checkS3Credential(source S3Source) error {
	switch {
	case (source.AccessKeyIDEnv == "") != (source.SecretAccessKeyEnv == ""):
		return errors.New("source.access_key_id_env and source.secret_access_key_env go together; one alone would be sent as an anonymous request")
	case source.SessionTokenEnv != "" && source.AccessKeyIDEnv == "":
		return errors.New("source.session_token_env needs source.access_key_id_env and source.secret_access_key_env")
	}

	for field, name := range map[string]string{"access_key_id_env": source.AccessKeyIDEnv, "secret_access_key_env": source.SecretAccessKeyEnv, "session_token_env": source.SessionTokenEnv} {
		if name != "" && !envVarPattern.MatchString(name) {
			return fmt.Errorf("source.%s must be the NAME of an environment variable, not %q — a literal would be hashed into state.db in cleartext", field, name)
		}
	}

	return nil
}

func checkS3Endpoint(endpoint string) error {
	if endpoint == "" {
		return nil
	}

	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("source.endpoint %q must be a URL with its scheme, e.g. https://s3.example.com", endpoint)
	}

	return nil
}

func checkS3Regexp(pattern string) error {
	if pattern == "" {
		return nil
	}

	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("source.regexp: %w", err)
	}

	if compiled.NumSubexp() == 0 {
		return fmt.Errorf("source.regexp %q needs a capture group around the version, e.g. builds/app-(.*).tgz", pattern)
	}

	return nil
}

// ParseS3GetParams decodes a get's params: from an s3 resource strictly.
func ParseS3GetParams(raw map[string]any) (S3GetParams, error) {
	var params S3GetParams

	return params, decodeParams(raw, &params)
}

// ParseS3PutParams decodes a put's params: to an s3 resource strictly.
func ParseS3PutParams(raw map[string]any) (S3PutParams, error) {
	var params S3PutParams

	err := decodeParams(raw, &params)
	if err != nil {
		return params, err
	}

	switch {
	case params.File == "":
		return params, errors.New("params.file is required: a glob, inside the put's inputs, matching the one file to upload (out/app-*.tgz)")
	case path.IsAbs(params.File) || strings.HasPrefix(path.Clean(params.File), ".."):
		return params, fmt.Errorf("params.file %q must be a path inside the put's inputs", params.File)
	case strings.ContainsAny(params.File, "\n\r"), strings.ContainsAny(params.ContentType, "\n\r"):
		return params, errors.New("params.file and params.content_type are one line each")
	}

	return params, nil
}

func (c *Config) validateS3Resources() error {
	for _, resource := range c.Resources {
		if c.nativeType(resource.Type) != S3Type {
			continue
		}

		if len(resource.Env) > 0 {
			return fmt.Errorf("resource %q: env: has no effect on an s3 resource — it reads only the variables its source.*_env fields name; drop env: here", resource.Name)
		}

		_, err := ParseS3Source(resource.Source)
		if err != nil {
			return fmt.Errorf("resource %q (s3): %w", resource.Name, err)
		}
	}

	for i := range c.Jobs {
		err := c.Jobs[i].visitSteps(c.checkS3Step)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Config) checkS3Step(label string, step *Step) error {
	if step.Get != "" && c.nativeResourceType(step.GetResourceName()) == S3Type {
		_, err := ParseS3GetParams(step.Params)
		if err != nil {
			return fmt.Errorf("%s: get %q: %w", label, step.Get, err)
		}
	}

	if step.Put == "" || c.nativeResourceType(step.PutResourceName()) != S3Type {
		return nil
	}

	params, err := ParseS3PutParams(step.Params)
	if err != nil {
		return fmt.Errorf("%s: put %q: %w", label, step.Put, err)
	}

	root := firstPathComponent(params.File)
	if !step.InputsAll() && !slices.Contains(step.InputNames(), root) {
		return fmt.Errorf("%s: put %q: params.file %q names artifact %q, which the put does not list in its inputs", label, step.Put, params.File, root)
	}

	return nil
}
