package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

type releaseFixture struct {
	id               int64
	tag              string
	draft, pre       bool
	published, body  string
	name, objectType string
}

// releasesServer lists releases newest filed first, a page at a time, and answers one by id or tag; every tag is a commit unless objectType says otherwise.
func releasesServer(t *testing.T, releases []releaseFixture) map[string]any {
	t.Helper()

	encode := func(release releaseFixture) map[string]any {
		var published any
		if release.published != "" {
			published = release.published
		}

		return map[string]any{
			"id": release.id, "tag_name": release.tag, "name": release.name, "body": release.body, "draft": release.draft, "prerelease": release.pre,
			"created_at": "2026-01-01T00:00:00Z", "published_at": published, "html_url": "https://github.example/acme/app/releases/tag/" + release.tag,
		}
	}

	connection := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		answer := releaseAnswer(strings.TrimPrefix(r.URL.Path, "/repos/acme/app"), r.URL.Query().Get("page"), releases, encode)
		if answer == nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))

			return
		}

		_ = json.NewEncoder(w).Encode(answer)
	})

	return map[string]any{"repo": "acme/app", "token_env": connection.TokenEnv, "endpoint": connection.Endpoint}
}

// releaseAnswer is what the fake answers path with, nil for a 404.
func releaseAnswer(path, page string, releases []releaseFixture, encode func(releaseFixture) map[string]any) any {
	switch {
	case path == "/releases":
		return releasePage(page, releases, encode)
	case strings.HasPrefix(path, "/releases/"):
		if release, ok := findRelease(releases, func(r releaseFixture) bool {
			return path == "/releases/"+strconv.FormatInt(r.id, 10) || path == "/releases/tags/"+r.tag
		}); ok {
			return encode(release)
		}
	case strings.HasPrefix(path, "/git/ref/tags/"):
		if release, ok := findRelease(releases, func(r releaseFixture) bool { return "/git/ref/tags/"+r.tag == path }); ok {
			return map[string]any{"object": map[string]any{"type": cmpOr(release.objectType, "commit"), "sha": fmt.Sprintf("%040d", release.id)}}
		}
	case strings.HasPrefix(path, "/git/tags/"):
		// Every annotated tag points at another: a chain with no commit at the end.
		return map[string]any{"object": map[string]any{"type": "tag", "sha": strings.TrimPrefix(path, "/git/tags/")}}
	}

	return nil
}

// releasePage is one page of a hundred, newest filed first.
func releasePage(page string, releases []releaseFixture, encode func(releaseFixture) map[string]any) []map[string]any {
	number, _ := strconv.Atoi(page)
	listed := []map[string]any{}

	for i := len(releases) - 1 - (number-1)*100; i >= 0 && len(listed) < 100; i-- {
		listed = append(listed, encode(releases[i]))
	}

	return listed
}

func findRelease(releases []releaseFixture, match func(releaseFixture) bool) (releaseFixture, bool) {
	for _, release := range releases {
		if match(release) {
			return release, true
		}
	}

	return releaseFixture{}, false
}

func checkedTags(t *testing.T, source, cursor map[string]any) string {
	t.Helper()

	versions, err := githubReleaseCheck(context.Background(), source, cursor)
	if err != nil {
		t.Fatal(err)
	}

	tags := make([]string, 0, len(versions))
	for _, version := range versions {
		tags = append(tags, versionField(version, "tag"))
	}

	return strings.Join(tags, " ")
}

func published(day int) string {
	return time.Date(2026, 11, day, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
}

// TestGitHubReleaseCheckIsConcoursesCursor: no cursor answers the newest release alone; a cursor answers everything from its place on, oldest first, in version order (v2 before v10, and a tie by the id GitHub minted first); a cursor past every release, or one with no tag to place, answers the newest again.
func TestGitHubReleaseCheckIsConcoursesCursor(t *testing.T) {
	source := releasesServer(t, []releaseFixture{
		{id: 1, tag: "v10.0.0", published: published(1)},
		{id: 2, tag: "v2.0.0", published: published(2)},
		{id: 3, tag: "v9.1.0", published: published(3)},
		{id: 4, tag: "9.1.0", published: published(4)},
		{id: 5, tag: "not-a-version!", published: published(5)},
	})

	for name, tc := range map[string]struct {
		cursor map[string]any
		want   string
	}{
		"no cursor":           {nil, "v10.0.0"},
		"a cursor":            {map[string]any{"tag": "v2.0.0"}, "v2.0.0 v9.1.0 9.1.0 v10.0.0"},
		"between two":         {map[string]any{"tag": "v9.0.0"}, "v9.1.0 9.1.0 v10.0.0"},
		"past every release":  {map[string]any{"tag": "v11.0.0"}, "v10.0.0"},
		"a tag with no place": {map[string]any{"id": "2"}, "v10.0.0"},
		"an unreadable tag":   {map[string]any{"tag": "x y"}, "v10.0.0"},
	} {
		if got := checkedTags(t, source, tc.cursor); got != tc.want {
			t.Errorf("%s: check = %q, want %q", name, got, tc.want)
		}
	}
}

// TestGitHubReleaseCheckByTime: order_by: time sorts by publication and places the cursor by its timestamp; the tag must still match the filter, but need not be a version.
func TestGitHubReleaseCheckByTime(t *testing.T) {
	source := releasesServer(t, []releaseFixture{
		{id: 1, tag: "v10.0.0", published: published(3)},
		{id: 2, tag: "v2.0.0", published: published(1)},
		{id: 3, tag: "nightly-1", published: published(2)},
		{id: 4, tag: "vv", published: published(4)},
	})
	source["order_by"] = "time"

	if got, want := checkedTags(t, source, map[string]any{"timestamp": published(1) + ".5"}), "v10.0.0"; got != want {
		t.Errorf("an unreadable timestamp: check = %q, want %q", got, want)
	}

	if got, want := checkedTags(t, source, map[string]any{"tag": "v2.0.0", "timestamp": published(1)}), "v2.0.0 nightly-1 v10.0.0"; got != want {
		t.Errorf("check = %q, want %q: by publication, the filter-refused vv left out", got, want)
	}
}

// TestGitHubReleaseCheckFilters: Concourse's draft, release and pre-release switches, each alone.
func TestGitHubReleaseCheckFilters(t *testing.T) {
	source := releasesServer(t, []releaseFixture{
		{id: 1, tag: "v1.0.0", published: published(1)},
		{id: 2, tag: "v1.1.0-rc.1", pre: true, published: published(2)},
		{id: 3, tag: "v1.2.0", draft: true},
		{id: 4, tag: "v1.2.0-rc.1", draft: true, pre: true},
	})
	tags := func(keys map[string]any) string {
		for key, value := range keys {
			source[key] = value
		}

		defer func() {
			for key := range keys {
				delete(source, key)
			}
		}()

		return checkedTags(t, source, map[string]any{"tag": "v0"})
	}

	for name, tc := range map[string]struct {
		keys map[string]any
		want string
	}{
		"the defaults":          {map[string]any{}, "v1.0.0"},
		"pre-releases too":      {map[string]any{"pre_release": true}, "v1.0.0 v1.1.0-rc.1"},
		"pre-releases alone":    {map[string]any{"pre_release": true, "release": false}, "v1.1.0-rc.1"},
		"drafts":                {map[string]any{"drafts": true}, "v1.2.0"},
		"drafts, pre-releases":  {map[string]any{"drafts": true, "pre_release": true}, "v1.2.0-rc.1 v1.2.0"},
		"a constraint":          {map[string]any{"semver_constraint": ">= 1.1.0-0", "pre_release": true}, "v1.1.0-rc.1"},
		"a filter of no tag":    {map[string]any{"tag_filter": "^release-(.*)"}, ""},
		"a template":            {map[string]any{"tag_filter": `^v(\d+)\.(\d+)`, "version_template": "$2"}, "v1.0.0"},
		"one release, a cursor": {map[string]any{"semver_constraint": "1.0.0"}, "v1.0.0"},
	} {
		if got := tags(tc.keys); got != tc.want {
			t.Errorf("%s: check = %q, want %q", name, got, tc.want)
		}
	}
}

// TestGitHubReleaseCheckReadsEveryPage: a repository with more releases than one page holds is read to the end, or its newest could be on the page never asked for.
func TestGitHubReleaseCheckReadsEveryPage(t *testing.T) {
	releases := make([]releaseFixture, 0, 150)
	for i := range 150 {
		// Filed newest version first, so the newest version is on the last page.
		releases = append(releases, releaseFixture{id: int64(i + 1), tag: fmt.Sprintf("v1.%d.0", 150-i), published: published(1)})
	}

	if got := checkedTags(t, releasesServer(t, releases), nil); got != "v1.150.0" {
		t.Errorf("check = %q, want v1.150.0 from the last page", got)
	}
}

func readFiles(t *testing.T, dir string) map[string]string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = root.Close() })

	files := map[string]string{}

	for _, entry := range entries {
		body, err := root.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}

		files[entry.Name()] = string(body)
	}

	return files
}

// TestGitHubReleaseInFindsAReleaseAgainByTag: a version whose id is gone is fetched by its tag, and reports what it fetched as facts.
func TestGitHubReleaseInFindsAReleaseAgainByTag(t *testing.T) {
	source := releasesServer(t, []releaseFixture{{id: 7, tag: "v3_4_13", name: "Ruby 3.4.13", body: "Notes.", published: published(18)}})
	source["tag_filter"], source["version_template"] = `^v(\d+)[._](\d+)[._](\d+)$`, "$1.$2.$3"
	dir := t.TempDir()
	ctx, facts := CollectFacts(context.Background())

	err := githubReleaseIn(ctx, source, map[string]any{"id": "6", "tag": "v3_4_13"}, dir)
	if err != nil {
		t.Fatal(err)
	}

	url := "https://github.example/acme/app/releases/tag/v3_4_13"
	sha := fmt.Sprintf("%040d", 7)
	assertFacts(t, facts(), "[{url "+url+"} {tag v3_4_13} {name Ruby 3.4.13} {commit_sha "+sha+"}]")

	files := readFiles(t, dir)
	for name, want := range map[string]string{"tag": "v3_4_13", "version": "3.4.13", "url": url, "body": "Notes.", "timestamp": "2026-11-18T09:00:00Z", "commit_sha": sha} {
		if files[name] != want {
			t.Errorf("%s = %q, want %q", name, files[name], want)
		}
	}

	if !strings.Contains(files["release.json"], `"id":7`) {
		t.Errorf("release.json = %q, want GitHub's object", files["release.json"])
	}
}

// TestGitHubReleaseInDraft: a draft has no commit to resolve and no body to write, and is stamped with when it was created.
func TestGitHubReleaseInDraft(t *testing.T) {
	source := releasesServer(t, []releaseFixture{{id: 8, tag: "v2.0.0", draft: true}})
	dir := t.TempDir()

	err := githubReleaseIn(context.Background(), source, map[string]any{"id": "8"}, dir)
	if err != nil {
		t.Fatal(err)
	}

	files := readFiles(t, dir)
	if _, ok := files["commit_sha"]; ok {
		t.Error("a draft wrote commit_sha, want none: its tag need not exist")
	}

	if _, ok := files["body"]; ok {
		t.Error("an empty body was written, want no file, as Concourse writes none")
	}

	if files["timestamp"] != "2026-01-01T00:00:00Z" {
		t.Errorf("timestamp = %q, want the creation time", files["timestamp"])
	}
}

// TestGitHubReleaseInRefusals: a version naming nothing, an id and tag both gone, a tag at no commit, and a chain of tags with no end each fail, naming why.
func TestGitHubReleaseInRefusals(t *testing.T) {
	source := releasesServer(t, []releaseFixture{
		{id: 1, tag: "v1.0.0", published: published(1), objectType: "tree"},
		{id: 2, tag: "v2.0.0", published: published(1), objectType: "tag"},
	})

	for name, tc := range map[string]struct {
		version map[string]any
		want    string
	}{
		"nothing named":    {map[string]any{"id": "../1"}, "names no release id or tag"},
		"gone":             {map[string]any{"id": "9", "tag": "v9"}, "/releases/tags/v9: 404"},
		"a tree":           {map[string]any{"id": "1"}, `tag "v1.0.0" points at a tree`},
		"an endless chain": {map[string]any{"id": "2"}, "more than 10 annotated tags away"},
	} {
		err := githubReleaseIn(context.Background(), source, tc.version, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestEscapeRefPathKeepsSlashes(t *testing.T) {
	t.Parallel()

	if got := escapeRefPath("release/1.0 rc#2"); got != "release/1.0%20rc%232" {
		t.Errorf("escapeRefPath = %q", got)
	}
}
