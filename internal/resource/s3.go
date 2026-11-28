package resource

// The built-in s3 type. A check lists the bucket from this process, which is the only part that needs the keys. A get or put signs a URL for the one object and runs curl against it through the stage's runner, so on a worker the bytes go bucket to worker and the keys stay here.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
)

const (
	s3PathField      = "path"
	s3VersionIDField = "version_id"
	s3PrefixField    = "prefix"
	s3FilesField     = "files"

	// One curl is handed about this much config, which rides in one environment variable: Linux caps a variable at 128 KiB, and an ssh worker takes the whole environment in one request.
	s3BatchBytes = 32 << 10

	// ponytail: a fixed hour; a transfer that needs longer wants this tied to the step's timeout.
	s3TransferTTL = time.Hour

	// A single presigned PUT is S3's limit; past it an upload is multipart, which this does not do yet.
	s3MaxPutBytes = 5 << 30
)

// Everything a transfer is told arrives as environment, never spliced into the script: a key is whatever someone named a file.
const (
	s3EnvURL         = "STEPS_S3_URL"
	s3EnvFile        = "STEPS_S3_FILE"
	s3EnvVersion     = "STEPS_S3_VERSION"
	s3EnvURI         = "STEPS_S3_URI"
	s3EnvPlainURL    = "STEPS_S3_PLAIN_URL"
	s3EnvContentType = "STEPS_S3_CONTENT_TYPE"
	s3EnvConfig      = "STEPS_S3_CONFIG"
)

// The URL reaches curl on its stdin, as a config line: an argument would sit in the worker's process list for the length of the transfer.
const (
	s3NameScript = `set -eu
printf '%s' "$` + s3EnvVersion + `" > version
printf '%s' "$` + s3EnvURI + `" > s3_uri
printf '%s' "$` + s3EnvPlainURL + `" > url
`
	s3GetScript = s3NameScript + `printf 'url = "%s"\n' "$` + s3EnvURL + `" | curl --fail --silent --show-error --location --retry 3 --output "./$` + s3EnvFile + `" --config -
`
	s3MatchScript = `set -eu
IFS=
set -- $` + s3EnvFile + `
if [ "$#" -ne 1 ]; then
  echo "params.file matches more than one file:" >&2
  printf '  %s\n' "$@" >&2
  exit 1
fi
if [ ! -f "$1" ]; then
  echo "params.file matches no file: $1" >&2
  exit 1
fi
printf '%s\n' "$1"
wc -c < "$1"
`
	// find's -type f leaves symlinks out: a link out of the directory would upload a file the put was never given.
	s3ListScript = `set -eu
if [ ! -d "./$` + s3EnvFile + `" ]; then
  echo "params.dir is not a directory: $` + s3EnvFile + `" >&2
  exit 1
fi
cd "./$` + s3EnvFile + `"
find . -type f -exec sh -c 'for f; do printf "%s\t%s\n" "$(wc -c < "$f" | tr -d " ")" "$f"; done' sh {} +
`
	// --globoff: curl expands [1-3] and {a,b} in an uploaded file's name as well as in a URL.
	s3PutDirScript = `set -eu
cd "./$` + s3EnvFile + `"
printf '%s' "$` + s3EnvConfig + `" | curl --fail --fail-early --silent --show-error --retry 3 --globoff --config -
`
	s3PutScript = `set -eu
printf 'url = "%s"\n' "$` + s3EnvURL + `" | curl --fail --silent --show-error --retry 3 --globoff --dump-header - --output /dev/null ${` + s3EnvContentType + `:+--header "Content-Type: $` + s3EnvContentType + `"} --upload-file "./$` + s3EnvFile + `" --config -
`
)

type s3Backend struct{}

func (s3Backend) check(ctx context.Context, c Call) ([]map[string]any, error) {
	source, client, err := s3For(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", c.Resource.Name, err)
	}

	defer client.close()

	var versions []map[string]any

	switch {
	case source.Prefix != "":
		// Nothing to find: a folder is only published into, and the load rule refuses a get of one before this is reached.
	case source.VersionedFile != "":
		versions, err = s3CheckVersioned(ctx, client, source, versionField(c.Version, s3VersionIDField))
	default:
		versions, err = s3CheckRegexp(ctx, client, source, versionField(c.Version, s3PathField))
	}

	if err != nil {
		return nil, fmt.Errorf("check %q: %w", c.Resource.Name, err)
	}

	return versions, nil
}

func s3For(ctx context.Context, c Call) (config.S3Source, *s3Client, error) {
	source, err := config.ParseS3Source(c.Resource.Source)
	if err != nil {
		return source, nil, fmt.Errorf("%w", err)
	}

	client, err := newS3Client(ctx, source)

	return source, client, err
}

// s3Pattern is a source's regexp as Concourse reads it: anchored at both ends, each /-separated piece matching one piece of the key.
type s3Pattern struct {
	anchored *regexp.Regexp
	slashes  int
	group    int
}

func newS3Pattern(raw string) (s3Pattern, error) {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(raw, "^"), "$")

	anchored, err := regexp.Compile("^(?:" + trimmed + ")$")
	if err != nil {
		return s3Pattern{}, fmt.Errorf("source.regexp: %w", err)
	}

	group := 1
	if named := anchored.SubexpIndex("version"); named > 0 {
		group = named
	}

	return s3Pattern{anchored: anchored, slashes: strings.Count(trimmed, "/"), group: group}, nil
}

// version is the version a key names, and false for a key the pattern does not match.
func (p s3Pattern) version(key string) (string, bool) {
	if strings.Count(key, "/") != p.slashes {
		return "", false
	}

	match := p.anchored.FindStringSubmatch(key)
	if len(match) <= p.group {
		return "", false
	}

	return match[p.group], true
}

var s3Metachars = regexp.MustCompile(`[\\*.\[\](){}?|^$+]`) //nolint:gochecknoglobals // compiled once, read-only

// s3ListPrefix is the directories a regexp names outright, which is as far as a listing can be narrowed. ponytail: everything under it is listed; Concourse walks one directory level at a time, which lists less beside a large sibling directory.
func s3ListPrefix(raw string) string {
	pieces := strings.Split(strings.TrimPrefix(raw, "^"), "/")
	literal := 0

	for literal < len(pieces)-1 && !s3Metachars.MatchString(pieces[literal]) {
		literal++
	}

	if literal == 0 {
		return ""
	}

	return strings.Join(pieces[:literal], "/") + "/"
}

type s3Candidate struct {
	key     string
	version semiVersion
}

func s3CheckRegexp(ctx context.Context, client *s3Client, source config.S3Source, cursor string) ([]map[string]any, error) {
	pattern, err := newS3Pattern(source.Regexp)
	if err != nil {
		return nil, err
	}

	keys, err := client.list(ctx, s3ListPrefix(source.Regexp))
	if err != nil {
		return nil, err
	}

	candidates, err := s3Candidates(pattern, keys)
	if err != nil || len(candidates) == 0 {
		return nil, err
	}

	from := len(candidates) - 1

	if raw, matched := pattern.version(cursor); matched {
		at, err := parseSemiVersion(raw)
		if err == nil {
			from = slices.IndexFunc(candidates, func(candidate s3Candidate) bool { return candidate.version.Compare(at) >= 0 })
		}
	}

	if from < 0 {
		return nil, nil
	}

	versions := make([]map[string]any, 0, len(candidates)-from)
	for _, candidate := range candidates[from:] {
		versions = append(versions, map[string]any{s3PathField: candidate.key})
	}

	return versions, nil
}

// s3Candidates is the keys the pattern matches, lowest version first.
func s3Candidates(pattern s3Pattern, keys []string) ([]s3Candidate, error) {
	var candidates []s3Candidate

	for _, key := range keys {
		raw, matched := pattern.version(key)
		if !matched {
			continue
		}

		// Concourse's check panics here; failing by name is the same refusal, readable.
		version, err := parseSemiVersion(raw)
		if err != nil {
			return nil, fmt.Errorf("%q matches source.regexp, but %q is not a version it can order (numbers and words separated by dots, e.g. 1.2.3 or 1.2.3-rc.1)", key, raw)
		}

		candidates = append(candidates, s3Candidate{key: key, version: version})
	}

	slices.SortStableFunc(candidates, func(a, b s3Candidate) int { return a.version.Compare(b.version) })

	return candidates, nil
}

func s3CheckVersioned(ctx context.Context, client *s3Client, source config.S3Source, cursor string) ([]map[string]any, error) {
	ids, err := client.versions(ctx, source.VersionedFile)
	if err != nil || len(ids) == 0 {
		return nil, err
	}

	// Newest first: with no cursor, or one the bucket no longer has, only the newest.
	upTo := slices.Index(ids, cursor)
	if cursor == "" || upTo < 0 {
		upTo = 0
	}

	versions := make([]map[string]any, 0, upTo+1)
	for i := upTo; i >= 0; i-- {
		versions = append(versions, map[string]any{s3VersionIDField: ids[i]})
	}

	return versions, nil
}

// s3Object is the one object a get names.
type s3Object struct {
	key, versionID string
	// version is what the version file holds: the number in the name, or the version id.
	version string
}

func s3ObjectFor(source config.S3Source, version map[string]any) (s3Object, error) {
	if source.VersionedFile != "" {
		id := versionField(version, s3VersionIDField)
		if id == "" {
			return s3Object{}, errors.New("the version names no version_id")
		}

		return s3Object{key: source.VersionedFile, versionID: id, version: id}, nil
	}

	pattern, err := newS3Pattern(source.Regexp)
	if err != nil {
		return s3Object{}, err
	}

	key := versionField(version, s3PathField)

	number, matched := pattern.version(key)
	if !matched {
		return s3Object{}, fmt.Errorf("the version's path %q does not match source.regexp", key)
	}

	return s3Object{key: key, version: number}, nil
}

// s3FileName is the name a key's object is written under, refusing one that is not a plain file name here.
func s3FileName(key string) (string, error) {
	name := path.Base(key)
	if name == "." || name == ".." || name == "/" || strings.ContainsAny(name, "\n\r\x00") {
		return "", fmt.Errorf("key %q has no file name to write it under", key)
	}

	return name, nil
}

func (s3Backend) in(ctx context.Context, c Call) error {
	source, client, err := s3For(ctx, c)
	if err != nil {
		return fmt.Errorf("in %q: %w", c.Resource.Name, err)
	}

	defer client.close()

	params, err := config.ParseS3GetParams(c.Params)
	if err != nil {
		return fmt.Errorf("in %q: %w", c.Resource.Name, err)
	}

	object, err := s3ObjectFor(source, c.Version)
	if err != nil {
		return fmt.Errorf("in %q: %w", c.Resource.Name, err)
	}

	name, err := s3FileName(object.key)
	if err != nil {
		return fmt.Errorf("in %q: %w", c.Resource.Name, err)
	}

	target := client.objectURL(object.key)
	if object.versionID != "" {
		target.RawQuery = url.Values{"versionId": {object.versionID}}.Encode()
	}

	signed, err := client.transferURL(ctx, http.MethodGet, target)
	if err != nil {
		return fmt.Errorf("in %q: %w", c.Resource.Name, err)
	}

	skip := source.SkipDownload
	if params.SkipDownload != nil {
		skip = *params.SkipDownload
	}

	script := s3GetScript
	if skip {
		script = s3NameScript
	}

	spec := s3RunnerSpec(c.Type)
	spec.Cwd, spec.Workdir, spec.FetchAll = c.Dir, shell.GetDir, true

	err = s3Run(ctx, spec, c.Resource.Name+" in", func(run s3Exec) error {
		_, err := run(script, map[string]string{
			s3EnvURL: signed, s3EnvFile: name, s3EnvVersion: object.version,
			s3EnvURI: "s3://" + source.Bucket + "/" + object.key, s3EnvPlainURL: target.String(),
		})

		return err
	})
	if err != nil {
		return fmt.Errorf("in %q: %w", c.Resource.Name, err)
	}

	reportFacts(ctx, []events.Fact{{Name: "filename", Value: name}, {Name: "url", Value: target.String()}})

	return nil
}

// s3RunnerSpec is where a transfer runs: the type's image when it names one, a worker's container from the default image, and otherwise curl on the machine itself.
func s3RunnerSpec(rt config.ResourceType) shell.RunnerSpec {
	spec := runnerSpec(rt, nil)
	spec.ContainerImage = config.DefaultS3Image

	return spec
}

// s3Exec runs one script under env on the transfer's runner and answers what it printed.
type s3Exec func(script string, env map[string]string) (string, error)

// s3Run opens the runner a transfer runs on and hands do a way to run scripts there. Each runs under its env and nothing else: the resource's own secrets are replaced on the context, so the commands get links, never the keys.
func s3Run(ctx context.Context, spec shell.RunnerSpec, label string, do func(run s3Exec) error) error {
	runner, err := newRunner(ctx, spec)
	if err != nil {
		return err
	}

	defer shell.CloseRunner(runner, label)

	return do(func(script string, env map[string]string) (string, error) {
		captured, err := shell.RunCaptureReattachable(shell.WithSecretEnv(ctx, env), runner, script)

		return string(captured), err
	})
}

func (s3Backend) out(ctx context.Context, c Call) (map[string]any, error) {
	source, client, err := s3For(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", c.Resource.Name, err)
	}

	defer client.close()

	params, err := config.ParseS3PutParams(c.Params)
	if err == nil {
		err = params.CheckFor(source)
	}

	if err != nil {
		return nil, fmt.Errorf("out %q: %w", c.Resource.Name, err)
	}

	spec := s3RunnerSpec(c.Type)
	spec.Cwd, spec.Workdir = c.Dir, shell.PutDir

	var version map[string]any

	err = s3Run(ctx, spec, c.Resource.Name+" out", func(run s3Exec) error {
		var err error

		if source.Prefix != "" {
			version, err = s3PutDir(ctx, client, source, params.Dir, run)
		} else {
			version, err = s3PutFile(ctx, client, source, params, run)
		}

		return err
	})
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", c.Resource.Name, err)
	}

	return version, nil
}

// s3PutFile uploads the one file params.file matches, as Concourse's put does.
func s3PutFile(ctx context.Context, client *s3Client, source config.S3Source, params config.S3PutParams, run s3Exec) (map[string]any, error) {
	matched, err := run(s3MatchScript, map[string]string{s3EnvFile: params.File})
	if err != nil {
		return nil, err
	}

	file, err := s3Matched(params.File, matched)
	if err != nil {
		return nil, err
	}

	key := s3PutKey(source, file)

	signed, err := client.transferURL(ctx, http.MethodPut, client.objectURL(key))
	if err != nil {
		return nil, err
	}

	headers, err := run(s3PutScript, map[string]string{s3EnvFile: file, s3EnvURL: signed, s3EnvContentType: params.ContentType})
	if err != nil {
		return nil, err
	}

	reportFacts(ctx, []events.Fact{{Name: "filename", Value: path.Base(key)}, {Name: "url", Value: client.objectURL(key).String()}})

	if source.VersionedFile == "" {
		return map[string]any{s3PathField: key}, nil
	}

	id := s3VersionID(headers)
	if id == "" || id == "null" {
		return nil, fmt.Errorf("uploaded %s, but %w", key, errS3Unversioned)
	}

	return map[string]any{s3VersionIDField: id}, nil
}

// s3PutKey is the key a put of file lands at: the versioned file, else the regexp's directory, read as the check reads it (a leading ^ is an anchor, not part of the key).
func s3PutKey(source config.S3Source, file string) string {
	if source.VersionedFile != "" {
		return source.VersionedFile
	}

	pattern := strings.TrimPrefix(source.Regexp, "^")

	return pattern[:strings.LastIndex(pattern, "/")+1] + path.Base(file)
}

// s3PutDir uploads every file under dir to source.prefix plus its path inside dir, a batch of signed links per curl.
func s3PutDir(ctx context.Context, client *s3Client, source config.S3Source, dir string, run s3Exec) (map[string]any, error) {
	listing, err := run(s3ListScript, map[string]string{s3EnvFile: dir})
	if err != nil {
		return nil, err
	}

	files, err := s3Listed(dir, listing)
	if err != nil {
		return nil, err
	}

	var batch strings.Builder

	for i, file := range files {
		signed, err := client.transferURL(ctx, http.MethodPut, client.objectURL(source.Prefix+file))
		if err != nil {
			return nil, err
		}

		batch.WriteString("url = " + curlQuote(signed) + "\nupload-file = " + curlQuote("./"+file) + "\n")

		if batch.Len() < s3BatchBytes && i < len(files)-1 {
			continue
		}

		_, err = run(s3PutDirScript, map[string]string{s3EnvFile: dir, s3EnvConfig: batch.String()})
		if err != nil {
			return nil, err
		}

		batch.Reset()
	}

	reportFacts(ctx, []events.Fact{{Name: s3FilesField, Value: strconv.Itoa(len(files))}, {Name: "url", Value: client.objectURL(source.Prefix).String()}})

	return map[string]any{s3PrefixField: source.Prefix, s3FilesField: strconv.Itoa(len(files))}, nil
}

// curlQuote is value as a quoted parameter in a curl config file.
func curlQuote(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\t", `\t`).Replace(value) + `"`
}

// s3Listed reads the list script's answer, a size and a path per line, into the paths inside dir, sorted. A line that is neither is a file name holding a line break, which no listing can carry.
func s3Listed(dir, answer string) ([]string, error) {
	var files []string

	for line := range strings.Lines(answer) {
		size, name, found := strings.Cut(strings.TrimSuffix(line, "\n"), "\t")

		bytes, err := strconv.ParseInt(size, 10, 64)
		if err != nil || !found || !strings.HasPrefix(name, "./") {
			return nil, fmt.Errorf("could not read the files under params.dir %q (a file name with a line break in it?): %q", dir, line)
		}

		if bytes > s3MaxPutBytes {
			return nil, fmt.Errorf("%s is %d bytes; a put uploads at most 5 GB a file, because larger objects need a multipart upload this type does not do yet", name, bytes)
		}

		files = append(files, strings.TrimPrefix(name, "./"))
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("params.dir %q holds no files, so there is nothing to publish", dir)
	}

	slices.Sort(files)

	return files, nil
}

// s3Matched reads the match script's answer: the one file the glob named, refused when it is too large to upload in one request.
func s3Matched(pattern, answer string) (string, error) {
	file, size, _ := strings.Cut(strings.TrimSuffix(answer, "\n"), "\n")

	var bytes int64

	_, err := fmt.Sscan(size, &bytes)
	if err != nil || file == "" {
		return "", fmt.Errorf("could not read which file params.file %q matched: %q", pattern, answer)
	}

	if bytes > s3MaxPutBytes {
		return "", fmt.Errorf("%s is %d bytes; a put uploads at most 5 GB, because larger objects need a multipart upload this type does not do yet", file, bytes)
	}

	return file, nil
}

// s3VersionID reads the version S3 gave an upload from the response headers curl printed.
func s3VersionID(headers string) string {
	for line := range strings.Lines(headers) {
		name, value, found := strings.Cut(line, ":")
		if found && strings.EqualFold(strings.TrimSpace(name), "x-amz-version-id") {
			return strings.TrimSpace(value)
		}
	}

	return ""
}
