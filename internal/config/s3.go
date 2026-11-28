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
	// Prefix is a folder to publish into, ending in a slash: a put uploads a directory under it, and there are no versions to get. Not Concourse's; its resource uploads one file per put.
	Prefix     string `yaml:"prefix,omitempty"`
	RegionName string `yaml:"region_name,omitempty"`
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
	File string `yaml:"file,omitempty"`
	// Dir is a directory, inside the put's inputs, every file under which is uploaded; source.prefix's put.
	Dir         string `yaml:"dir,omitempty"`
	ContentType string `yaml:"content_type,omitempty"`
}

// CheckFor is whether these params fit source's mode: a folder takes dir, a versioned object takes file.
func (p S3PutParams) CheckFor(source S3Source) error {
	switch {
	case source.Prefix != "" && p.Dir == "":
		return errors.New("params.dir is required: source.prefix publishes a folder, so name the directory, inside the put's inputs, whose files to upload")
	case source.Prefix == "" && p.Dir != "":
		return errors.New("params.dir needs source.prefix; under source.regexp or source.versioned_file a put uploads the one file params.file matches")
	case source.Prefix == "" && p.File == "":
		return errors.New("params.file is required: a glob, inside the put's inputs, matching the one file to upload (out/app-*.tgz)")
	}

	return nil
}

// ParseS3Source decodes an s3 source: strictly.
func ParseS3Source(raw map[string]any) (S3Source, error) {
	var source S3Source

	err := decodeSource(raw, &source)
	if err != nil {
		return source, err
	}

	modes := 0

	for _, mode := range []string{source.Regexp, source.VersionedFile, source.Prefix} {
		if mode != "" {
			modes++
		}
	}

	switch {
	case strings.TrimSpace(source.Bucket) == "":
		return source, errors.New("an s3 resource needs source.bucket")
	case modes != 1:
		return source, errors.New("an s3 resource needs exactly one of source.regexp (a version in the file name), source.versioned_file (a bucket with versioning on) or source.prefix (a folder to publish into)")
	}

	err = checkS3Prefix(source)
	if err != nil {
		return source, err
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

func checkS3Prefix(source S3Source) error {
	switch {
	case source.Prefix == "":
		return nil
	case strings.HasPrefix(source.Prefix, "/"):
		return fmt.Errorf("source.prefix %q must not start with /: a key has no leading slash", source.Prefix)
	case !strings.HasSuffix(source.Prefix, "/"):
		// ponytail: so the bucket's root cannot be published into; an explicit spelling for it when someone needs one.
		return fmt.Errorf("source.prefix %q must end in /: it is the folder the files land in, e.g. %s/", source.Prefix, source.Prefix)
	case strings.ContainsAny(source.Prefix, "\n\r"):
		return errors.New("source.prefix is one line")
	case source.SkipDownload:
		return errors.New("source.skip_download has no effect with source.prefix, which only publishes; drop it")
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

	// The bucket is addressed from the host's root, so a path here would be silently dropped.
	if strings.Trim(parsed.Path, "/") != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return fmt.Errorf("source.endpoint %q must be a scheme and a host only, e.g. https://s3.example.com:9000", endpoint)
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

	local := params.Path()

	switch {
	case params.File != "" && params.Dir != "":
		return params, errors.New("params.file (one file) or params.dir (a folder, under source.prefix), not both")
	case path.IsAbs(local) || path.Clean(local) == ".." || strings.HasPrefix(path.Clean(local), "../"):
		return params, fmt.Errorf("params %q must be a path inside the put's inputs", local)
	case strings.ContainsAny(local, "\n\r"), strings.ContainsAny(params.ContentType, "\n\r"):
		return params, errors.New("params.file, params.dir and params.content_type are one line each")
	case params.Dir != "" && params.ContentType != "":
		// ponytail: a folder's files go up with no Content-Type; per-extension types when a served bucket needs them.
		return params, errors.New("params.content_type applies to one file, not to params.dir's folder; drop it")
	}

	return params, nil
}

// Path is the file glob or the directory, whichever the put names.
func (p S3PutParams) Path() string {
	if p.Dir != "" {
		return p.Dir
	}

	return p.File
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

// s3SourceOf is the parsed source of the s3 resource name names, and false for any other; validateS3Resources has already refused one that does not parse.
func (c *Config) s3SourceOf(name string) (S3Source, bool) {
	resource, err := c.FindResource(name)
	if err != nil || c.nativeType(resource.Type) != S3Type {
		return S3Source{}, false
	}

	source, err := ParseS3Source(resource.Source)

	return source, err == nil
}

func (c *Config) checkS3Get(label string, step *Step) error {
	source, ok := c.s3SourceOf(step.GetResourceName())
	if !ok {
		return nil
	}

	if source.Prefix != "" {
		return fmt.Errorf("%s: get %q targets an s3 resource with source.prefix, which only publishes; fetch from one with source.regexp or source.versioned_file instead", label, step.Get)
	}

	_, err := ParseS3GetParams(step.Params)
	if err != nil {
		return fmt.Errorf("%s: get %q: %w", label, step.Get, err)
	}

	return nil
}

func (c *Config) checkS3Step(label string, step *Step) error {
	if step.Get != "" {
		return c.checkS3Get(label, step)
	}

	source, ok := c.s3SourceOf(step.PutResourceName())
	if step.Put == "" || !ok {
		return nil
	}

	params, err := ParseS3PutParams(step.Params)
	if err == nil {
		err = params.CheckFor(source)
	}

	if err != nil {
		return fmt.Errorf("%s: put %q: %w", label, step.Put, err)
	}

	root := firstPathComponent(params.Path())
	if !step.InputsAll() && !slices.Contains(step.InputNames(), root) {
		return fmt.Errorf("%s: put %q: params %q names artifact %q, which the put does not list in its inputs", label, step.Put, params.Path(), root)
	}

	return nil
}
