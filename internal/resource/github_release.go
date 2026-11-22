package resource

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
)

// The files a github-releases get writes: Concourse's names, so a task ported from a Concourse pipeline reads them unchanged, and release.json, as every github-* finder writes GitHub's own object.
const (
	releaseJSONFile      = "release.json"
	releaseTagFile       = "tag"
	releaseVersionFile   = "version"
	releaseBodyFile      = "body"
	releaseTimestampFile = "timestamp"
	releaseCommitFile    = "commit_sha"
	releaseURLFile       = "url"
)

// releaseTagDepth bounds the chain of annotated tags a tag is followed through to its commit, as Concourse bounds it.
const releaseTagDepth = 10

type githubRelease struct {
	ID          int64   `json:"id"`
	TagName     string  `json:"tag_name"`
	Name        string  `json:"name"`
	Body        string  `json:"body"`
	Draft       bool    `json:"draft"`
	Prerelease  bool    `json:"prerelease"`
	CreatedAt   string  `json:"created_at"`
	PublishedAt *string `json:"published_at"`
	HTMLURL     string  `json:"html_url"`
}

// timestamp is when it was published, or created when it has not been (a draft): Concourse's order for order_by: time and its version's timestamp.
func (r githubRelease) timestamp() time.Time {
	raw := r.CreatedAt
	if r.PublishedAt != nil && *r.PublishedAt != "" {
		raw = *r.PublishedAt
	}

	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}

	return at
}

// version is the release's version as Concourse's check reports it: the id fetches it, the tag finds it again if the id is gone, and the timestamp is the cursor order_by: time reads.
func (r githubRelease) version() map[string]any {
	return map[string]any{"id": strconv.FormatInt(r.ID, 10), "tag": r.TagName, "timestamp": r.timestamp().UTC().Format(time.RFC3339Nano)}
}

// rankedRelease is a release that passed the filters, with what it sorts by.
type rankedRelease struct {
	release githubRelease
	version semiVersion
	at      time.Time
}

func githubReleaseCheck(ctx context.Context, raw, cursor map[string]any) ([]map[string]any, error) {
	source, versioning, err := config.ParseGitHubReleaseSource(raw)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	client, err := newGitHubClient(ctx, source.Connection())
	if err != nil {
		return nil, err
	}

	listing := "/repos/" + source.Repo + "/releases"

	all, err := listAll(ctx, client, listing, url.Values{})
	if err != nil {
		return nil, err
	}

	ranked := make([]rankedRelease, 0, len(all))

	for _, entry := range all {
		var release githubRelease

		err := json.Unmarshal(entry, &release)
		if err != nil {
			return nil, fmt.Errorf("github: GET %s: %w", listing, err)
		}

		if candidate, ok := rankRelease(source, versioning, release); ok {
			ranked = append(ranked, candidate)
		}
	}

	byTime := source.OrderBy == config.GitHubReleaseByTime
	sortReleases(ranked, byTime)

	return releasesFrom(ranked, cursor, versioning, byTime), nil
}

// rankRelease is Concourse's filter, in its order: draft or not, final or pre-release, the constraint, then a tag the chosen order can read.
func rankRelease(source config.GitHubReleaseSource, versioning config.GitHubReleaseVersioning, release githubRelease) (rankedRelease, bool) {
	if release.Draft != source.Drafts {
		return rankedRelease{}, false
	}

	if (release.Prerelease && !source.PreRelease) || (!release.Prerelease && !source.FindsReleases()) {
		return rankedRelease{}, false
	}

	parsed := versioning.Version(release.TagName)
	if !versioning.Admits(parsed) {
		return rankedRelease{}, false
	}

	candidate := rankedRelease{release: release, at: release.timestamp()}

	if source.OrderBy == config.GitHubReleaseByTime {
		return candidate, versioning.Matches(release.TagName) && !candidate.at.IsZero()
	}

	semi, err := parseSemiVersion(parsed)
	candidate.version = semi

	return candidate, err == nil
}

// sortReleases puts them oldest first, as a check reports. Equal versions or moments fall back to the id, which GitHub mints in creation order, so the order never depends on how GitHub happened to page them.
func sortReleases(ranked []rankedRelease, byTime bool) {
	slices.SortFunc(ranked, func(a, b rankedRelease) int {
		var order int
		if byTime {
			order = a.at.Compare(b.at)
		} else {
			order = a.version.Compare(b.version)
		}

		return cmp.Or(order, cmp.Compare(a.release.ID, b.release.ID))
	})
}

// releasesFrom is what Concourse's check answers: with no cursor the newest release alone; with one, every release from the cursor's place on, or the newest alone when none is at or past it. A release ordered below the cursor is never reported, however recently it was published: a patch to an older line is not news to a resource following a newer one.
func releasesFrom(ranked []rankedRelease, cursor map[string]any, versioning config.GitHubReleaseVersioning, byTime bool) []map[string]any {
	if len(ranked) == 0 {
		return []map[string]any{}
	}

	first := cursorIndex(ranked, cursor, versioning, byTime)
	if first < 0 || first >= len(ranked) {
		first = len(ranked) - 1
	}

	versions := make([]map[string]any, 0, len(ranked)-first)
	for _, candidate := range ranked[first:] {
		versions = append(versions, candidate.release.version())
	}

	return versions
}

// cursorIndex is the first release at or past the cursor, or -1 when the cursor names no place: there is none, or its tag or timestamp cannot be read.
func cursorIndex(ranked []rankedRelease, cursor map[string]any, versioning config.GitHubReleaseVersioning, byTime bool) int {
	if byTime {
		at, err := time.Parse(time.RFC3339Nano, versionField(cursor, "timestamp"))
		if err != nil {
			return -1
		}

		index, _ := slices.BinarySearchFunc(ranked, at, func(candidate rankedRelease, at time.Time) int { return candidate.at.Compare(at) })

		return index
	}

	tag := versionField(cursor, "tag")
	if tag == "" {
		return -1
	}

	at, err := parseSemiVersion(versioning.Version(tag))
	if err != nil {
		return -1
	}

	index, _ := slices.BinarySearchFunc(ranked, at, func(candidate rankedRelease, at semiVersion) int { return candidate.version.Compare(at) })

	return index
}

func githubReleaseIn(ctx context.Context, raw, version map[string]any, destDir string) error {
	source, versioning, err := config.ParseGitHubReleaseSource(raw)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	client, err := newGitHubClient(ctx, source.Connection())
	if err != nil {
		return err
	}

	release, releaseJSON, err := fetchRelease(ctx, client, source.Repo, version)
	if err != nil {
		return err
	}

	files := map[string][]byte{releaseJSONFile: releaseJSON, releaseURLFile: []byte(release.HTMLURL)}
	facts := []events.Fact{{Name: "url", Value: release.HTMLURL}, {Name: "tag", Value: release.TagName}}

	if release.Name != "" {
		facts = append(facts, events.Fact{Name: "name", Value: release.Name})
	}

	if release.TagName != "" {
		files[releaseTagFile] = []byte(release.TagName)
		files[releaseVersionFile] = []byte(versioning.Version(release.TagName))
		files[releaseTimestampFile] = []byte(release.timestamp().UTC().Format(time.RFC3339Nano))

		if release.Body != "" {
			files[releaseBodyFile] = []byte(release.Body)
		}

		// A draft's tag need not exist yet, so it has no commit to name.
		if !release.Draft {
			sha, err := resolveTagCommit(ctx, client, source.Repo, release.TagName)
			if err != nil {
				return err
			}

			files[releaseCommitFile] = []byte(sha)
			facts = append(facts, events.Fact{Name: "commit_sha", Value: sha})
		}
	}

	reportFacts(ctx, facts)

	return writeMetadata(destDir, files)
}

// fetchRelease is the version's release by id, or by its tag when the id is gone, as Concourse finds it: a release deleted and published again under the same tag is the same release to a pipeline.
func fetchRelease(ctx context.Context, client *githubClient, repo string, version map[string]any) (githubRelease, []byte, error) {
	var release githubRelease

	id := versionNumber(version, "id")
	tag := versionField(version, "tag")

	if id == "" && tag == "" {
		return release, nil, fmt.Errorf("version %v names no release id or tag", version)
	}

	var byID error

	if id != "" {
		body, err := client.getJSON(ctx, "/repos/"+repo+"/releases/"+id, &release)
		if err == nil {
			return release, body, nil
		}

		byID = err
	}

	if tag == "" {
		return release, nil, byID
	}

	body, err := client.getJSON(ctx, "/repos/"+repo+"/releases/tags/"+url.PathEscape(tag), &release)
	if err != nil {
		return release, nil, errors.Join(byID, err)
	}

	return release, body, nil
}

type gitObject struct {
	Object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
}

// resolveTagCommit is the commit a tag names, through any annotated tag objects in between.
func resolveTagCommit(ctx context.Context, client *githubClient, repo, tag string) (string, error) {
	var ref gitObject

	_, err := client.getJSON(ctx, "/repos/"+repo+"/git/ref/tags/"+escapeRefPath(tag), &ref)
	if err != nil {
		return "", err
	}

	for range releaseTagDepth {
		switch ref.Object.Type {
		case "commit":
			return ref.Object.SHA, nil
		case "tag":
			if strings.Trim(ref.Object.SHA, "0123456789abcdefABCDEF") != "" {
				return "", fmt.Errorf("github: tag %q points at %q, which is not an object id", tag, ref.Object.SHA)
			}

			_, err := client.getJSON(ctx, "/repos/"+repo+"/git/tags/"+ref.Object.SHA, &ref)
			if err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("github: tag %q points at a %s, not a commit or an annotated tag", tag, cmp.Or(ref.Object.Type, "nothing"))
		}
	}

	return "", fmt.Errorf("github: tag %q is more than %d annotated tags away from a commit", tag, releaseTagDepth)
}

// escapeRefPath escapes each segment of a tag name and keeps its slashes, which the ref route reads as part of the name: release/1.0 is refs/tags/release/1.0.
func escapeRefPath(tag string) string {
	segments := strings.Split(tag, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}

	return strings.Join(segments, "/")
}
