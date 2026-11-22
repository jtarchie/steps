package e2e

// The fake GitHub's releases, for the built-in github-releases type: the release listing, one release by id or by tag, and the git refs a tag resolves to its commit through.

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

type fakeRelease struct {
	ID         int64
	Tag        string
	Name       string
	Body       string
	Draft      bool
	Prerelease bool
	// Published is when it was published; GitHub answers null for a draft, which has never been.
	Published time.Time
	// Annotated makes its tag an annotated tag object, which the commit is one more request behind.
	Annotated bool
}

// releaseSHA is the commit release id's tag points at.
func releaseSHA(id int64) string { return fmt.Sprintf("c0ffee%034d", id) }

// releaseTagObject is the annotated tag object of release id, when it has one.
func releaseTagObject(id int64) string { return fmt.Sprintf("7a9000%034d", id) }

func (f *fakeGitHub) addRelease(release fakeRelease) { f.addReleases(release) }

func (f *fakeGitHub) addReleases(releases ...fakeRelease) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.releases = append(f.releases, releases...)
}

func (f *fakeGitHub) setRelease(id int64, edit func(*fakeRelease)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for i := range f.releases {
		if f.releases[i].ID == id {
			edit(&f.releases[i])

			return
		}
	}

	f.t.Errorf("no release %d to edit", id)
}

func (f *fakeGitHub) releaseRoutes(handle func(string, func(http.ResponseWriter, *http.Request)), repo string) {
	handle("GET "+repo+"/releases", f.listReleases)
	handle("GET "+repo+"/releases/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.serveRelease(w, func(release fakeRelease) bool { return strconv.FormatInt(release.ID, 10) == r.PathValue("id") })
	})
	handle("GET "+repo+"/releases/tags/{tag}", func(w http.ResponseWriter, r *http.Request) {
		// A draft has no tag yet as far as this route is concerned: GitHub finds only published releases by tag.
		f.serveRelease(w, func(release fakeRelease) bool { return release.Tag == r.PathValue("tag") && !release.Draft })
	})
	handle("GET "+repo+"/git/ref/tags/{tag...}", f.serveTagRef)
	handle("GET "+repo+"/git/tags/{sha}", f.serveTagObject)
}

// listReleases answers newest filed first, a page at a time, as GitHub lists by creation.
func (f *fakeGitHub) listReleases(w http.ResponseWriter, r *http.Request) {
	listed := slices.Clone(f.releases)
	slices.Reverse(listed)

	perPage := max(atoi(r.URL.Query().Get("per_page")), 1)
	page := max(atoi(r.URL.Query().Get("page")), 1)
	start := min((page-1)*perPage, len(listed))
	end := min(start+perPage, len(listed))

	out := make([]map[string]any, 0, end-start)
	for _, release := range listed[start:end] {
		out = append(out, f.releaseJSON(release))
	}

	respondJSON(w, http.StatusOK, out)
}

func (f *fakeGitHub) serveRelease(w http.ResponseWriter, match func(fakeRelease) bool) {
	for _, release := range f.releases {
		if match(release) {
			respondJSON(w, http.StatusOK, f.releaseJSON(release))

			return
		}
	}

	respondJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
}

func (f *fakeGitHub) releaseJSON(release fakeRelease) map[string]any {
	created := release.Published.Add(-time.Hour)
	if release.Draft {
		created = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	}

	var published any
	if !release.Draft {
		published = release.Published.Format(time.RFC3339)
	}

	return map[string]any{
		"id": release.ID, "tag_name": release.Tag, "name": release.Name, "body": release.Body,
		"draft": release.Draft, "prerelease": release.Prerelease,
		"created_at": created.Format(time.RFC3339), "published_at": published,
		"html_url": "https://github.example/" + f.repo + "/releases/tag/" + release.Tag,
	}
}

func (f *fakeGitHub) serveTagRef(w http.ResponseWriter, r *http.Request) {
	for _, release := range f.releases {
		if release.Tag != r.PathValue("tag") || release.Draft {
			continue
		}

		object := map[string]any{"type": "commit", "sha": releaseSHA(release.ID)}
		if release.Annotated {
			object = map[string]any{"type": "tag", "sha": releaseTagObject(release.ID)}
		}

		respondJSON(w, http.StatusOK, map[string]any{"ref": "refs/tags/" + release.Tag, "object": object})

		return
	}

	respondJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
}

func (f *fakeGitHub) serveTagObject(w http.ResponseWriter, r *http.Request) {
	for _, release := range f.releases {
		if release.Annotated && strings.EqualFold(releaseTagObject(release.ID), r.PathValue("sha")) {
			respondJSON(w, http.StatusOK, map[string]any{"tag": release.Tag, "object": map[string]any{"type": "commit", "sha": releaseSHA(release.ID)}})

			return
		}
	}

	respondJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
}
