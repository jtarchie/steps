package web

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// signOf is the shared sign that names slug on the wall, from its class to the sign's close.
func signOf(t *testing.T, page, slug string) string {
	t.Helper()

	at := strings.Index(page, `data-slug="`+slug+`"`)
	end := strings.Index(page[max(at, 0):], "</div>")

	if at < 0 || end < 0 {
		t.Fatalf("no shared sign on the wall names %s:\n%s", slug, page)
	}

	return page[strings.LastIndex(page[:at], `class="sign `) : at+end]
}

// TestTheWallSignsEveryPipelineByItsState: a screen across the room answers one question, is anything broken and what. A failing pipeline is its own sign saying which job and how long ago; the rest share one sign per state, each a word, and nothing on it is chrome or a control.
func TestTheWallSignsEveryPipelineByItsState(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", twoJobs, "infra", oneManualJob)
	seedRun(t, pipelines[0], "run-1", "build", "failed")
	seedRun(t, pipelines[1], "run-2", "audit", "succeeded")

	code, page := get(t, server, "/wall")
	if code != http.StatusOK {
		t.Fatalf("GET /wall = %d", code)
	}

	if !strings.Contains(page, `<a class="sign red" href="/p/app" data-slug="app"><span class="word">failing</span><span class="name">app</span><span class="why">build failed · `) {
		t.Errorf("the failing pipeline is not its own sign naming the job:\n%s", page)
	}

	if passing := signOf(t, page, "infra"); !strings.HasPrefix(passing, `class="sign passing"><span class="word">passing</span>`) {
		t.Errorf("the green pipeline is not on the passing sign:\n%s", passing)
	}

	if !strings.Contains(page, `class="wallfoot"`) {
		t.Errorf("the wall has no foot saying when it was updated:\n%s", page)
	}

	wallDrawsNoChrome(t, page)
}

func wallDrawsNoChrome(t *testing.T, page string) {
	t.Helper()

	for _, chrome := range []string{`id="statusline"`, `class="foot"`, `<button`, `<form`} {
		if strings.Contains(page, chrome) {
			t.Errorf("the wall draws %s; it is a screen to look at, not a page to use", chrome)
		}
	}
}

// TestTheWallSaysWhatIsBuilding: on a green wall, what is building is the one thing about to change, so it says which job.
func TestTheWallSaysWhatIsBuilding(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", twoJobs, "infra", oneManualJob)
	seedRun(t, pipelines[0], "run-1", "build", "succeeded")
	seedRun(t, pipelines[0], "run-2", "deploy", "")

	_, page := get(t, server, "/wall")

	if sign := signOf(t, page, "app"); !strings.HasPrefix(sign, `class="sign building"`) || !strings.Contains(sign, "deploy") {
		t.Errorf("a pipeline with a run in flight is not on the building sign naming the job:\n%s", sign)
	}
}

// TestTheWallSaysAPausedPipelineIsPaused: a paused pipeline will not go red on its own, so it must not sit among the live ones.
func TestTheWallSaysAPausedPipelineIsPaused(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", twoJobs, "infra", oneManualJob)

	err := pipelines[0].Store.Pause(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	_, page := get(t, server, "/wall")

	if sign := signOf(t, page, "app"); !strings.HasPrefix(sign, `class="sign paused"`) {
		t.Errorf("a paused pipeline is not on the paused sign:\n%s", sign)
	}
}

// TestANeverRunPipelineIsNotPassing: a pipeline that never ran has proved nothing, so it is neither on the passing sign nor counted in an all-passing wall.
func TestANeverRunPipelineIsNotPassing(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", twoJobs, "infra", oneManualJob)
	seedRun(t, pipelines[1], "run-1", "audit", "succeeded")

	_, page := get(t, server, "/wall")

	if sign := signOf(t, page, "app"); !strings.HasPrefix(sign, `class="sign never"`) {
		t.Errorf("a pipeline that never ran is not on the never-run sign:\n%s", sign)
	}

	if strings.Contains(page, "all 2 passing") || strings.Contains(page, `class="sign passing all"`) {
		t.Errorf("a wall with a never-run pipeline says everything passes:\n%s", page)
	}
}

// TestAnAllGreenWallSaysSoOnce: when nothing needs anybody the wall says that one thing, large, and lists who.
func TestAnAllGreenWallSaysSoOnce(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", twoJobs, "infra", oneManualJob)
	seedRun(t, pipelines[0], "run-1", "build", "succeeded")
	seedRun(t, pipelines[1], "run-2", "audit", "succeeded")

	_, page := get(t, server, "/wall")

	if !strings.Contains(page, `<div class="sign passing all"><span class="word">all 2 passing</span>`) {
		t.Errorf("an all-green wall does not say so once:\n%s", page)
	}
}

// TestARedPipelineBeingRebuiltStaysRed: a rebuild is not a fix until it passes, so the sign stays red and adds the building bar.
func TestARedPipelineBeingRebuiltStaysRed(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", twoJobs, "infra", oneManualJob)
	seedRun(t, pipelines[0], "run-1", "build", "failed")
	seedRun(t, pipelines[0], "run-2", "build", "")

	_, page := get(t, server, "/wall")

	if !strings.Contains(page, `<a class="sign red live" href="/p/app"`) {
		t.Errorf("a red pipeline with a run in flight is not a red sign with the building bar:\n%s", page)
	}
}

// TestTheNewestFailureIsOnTop: the top of the wall is read first, and the failure that just happened is the news.
func TestTheNewestFailureIsOnTop(t *testing.T) {
	t.Parallel()

	now := time.Now()
	rows := []overviewPipeline{
		{Slug: "old", HasRun: true, Mark: mark{Disc: discFailed}},
		{Slug: "new", HasRun: true, Mark: mark{Disc: discFailed}},
	}
	rows[0].Latest.StartedAt = now.Add(-time.Hour)
	rows[1].Latest.StartedAt = now

	signs := sortWall(rows)
	if len(signs.Failing) != 2 || signs.Failing[0].Slug != "new" {
		t.Errorf("failing signs = %v, want the newest failure first", signs.Failing)
	}
}

// TestEveryFailingNameIsOneSize: sized each to its own sign, a long name came out smaller than a short one and read as smaller news. Every failing name is sized for the longest, and one absurd name is cut rather than shrinking the rest.
func TestEveryFailingNameIsOneSize(t *testing.T) {
	t.Parallel()

	red := func(slug string) overviewPipeline {
		return overviewPipeline{Slug: slug, HasRun: true, Mark: mark{Disc: discFailed}}
	}

	if got := sortWall([]overviewPipeline{red("api"), red("billing-service"), {Slug: "a-very-long-green-pipeline-name", HasRun: true}}).Len; got != len("billing-service") {
		t.Errorf("Len = %d, want the longest failing name's %d", got, len("billing-service"))
	}

	if got := sortWall([]overviewPipeline{red(strings.Repeat("x", 60))}).Len; got != wallNameCap {
		t.Errorf("Len with a 60-character name = %d, want the cap %d", got, wallNameCap)
	}
}
