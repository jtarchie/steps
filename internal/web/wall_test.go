package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// wallTile is the tile one pipeline draws on the wall.
func tileOf(t *testing.T, page, slug string) string {
	t.Helper()

	return between(t, page, `data-slug="`+slug+`"`, "</a>")
}

// TestTheWallIsEveryPipelineAsATile: a screen across the room has no use for the root's columns. Every pipeline is a tile; a red one says which job and how long ago, a green one only that it is, and nothing on it is chrome or a control.
func TestTheWallIsEveryPipelineAsATile(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", twoJobs, "infra", oneManualJob)
	seedRun(t, pipelines[0], "run-1", "build", "failed")
	seedRun(t, pipelines[1], "run-2", "audit", "succeeded")

	code, page := get(t, server, "/wall")
	if code != http.StatusOK {
		t.Fatalf("GET /wall = %d", code)
	}

	red := tileOf(t, page, "app")
	if !strings.Contains(page, `<a class="tile red" href="/p/app" data-slug="app"`) || !strings.Contains(red, "build failed") {
		t.Errorf("the red pipeline is not a red tile naming its job:\n%s", red)
	}

	green := tileOf(t, page, "infra")
	if !strings.Contains(page, `<a class="tile" href="/p/infra" data-slug="infra"`) || !strings.Contains(green, "audit") || strings.Contains(green, "failed") {
		t.Errorf("the green pipeline is not a quiet tile naming its last job:\n%s", green)
	}

	if !strings.Contains(red, `<span class="jobmap"`) || strings.Count(red, `class="mapjob `) != 2 {
		t.Errorf("a tile does not carry one tick per job:\n%s", red)
	}

	wallDrawsNoChrome(t, page)

	if !strings.Contains(between(t, page, `id="wallhead"`, "</header>"), "1 failing") {
		t.Errorf("the wall's head does not count the failing pipeline:\n%s", page)
	}
}

func wallDrawsNoChrome(t *testing.T, page string) {
	t.Helper()

	for _, chrome := range []string{`id="statusline"`, `class="foot"`, `<button`, `<form`} {
		if strings.Contains(page, chrome) {
			t.Errorf("the wall draws %s; it is a screen to look at, not a page to use", chrome)
		}
	}
}

// TestTheWallSaysWhatIsBuilding: a building pipeline is the one thing on a green wall that is about to change, so it says which job.
func TestTheWallSaysWhatIsBuilding(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", twoJobs, "infra", oneManualJob)
	seedRun(t, pipelines[0], "run-1", "build", "succeeded")
	seedRun(t, pipelines[0], "run-2", "deploy", "")

	_, page := get(t, server, "/wall")

	if !strings.Contains(page, `<a class="tile building" href="/p/app"`) || !strings.Contains(tileOf(t, page, "app"), "deploy") {
		t.Errorf("a pipeline with a run in flight is not a building tile naming the job:\n%s", page)
	}
}

// TestTheWallSaysAPausedPipelineIsPaused: a paused pipeline will not go red on its own, so it must not look alive.
func TestTheWallSaysAPausedPipelineIsPaused(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", twoJobs, "infra", oneManualJob)

	err := pipelines[0].Store.Pause(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	_, page := get(t, server, "/wall")

	if !strings.Contains(page, `<a class="tile paused" href="/p/app"`) {
		t.Errorf("a paused pipeline is not a paused tile:\n%s", page)
	}
}

// TestTheWallFitsOneScreen: a radiator that scrolls hides whatever is below the fold, which is where the red one will be. Columns grow with the square root of the count, shaped for a 16:9 screen, so thirty pipelines are an 8×4 grid and one is not a stripe across the top.
func TestTheWallFitsOneScreen(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ n, cols, rows int }{
		{1, 1, 1}, {2, 2, 1}, {4, 3, 2}, {7, 4, 2}, {12, 5, 3}, {30, 8, 4},
	} {
		cols, rows := wallGrid(tc.n)
		if cols != tc.cols || rows != tc.rows {
			t.Errorf("wallGrid(%d) = %d×%d, want %d×%d", tc.n, cols, rows, tc.cols, tc.rows)
		}
	}

	names := make([]string, 0, 7)
	for i := range 7 {
		names = append(names, fmt.Sprintf("p%d", i))
	}

	server, _ := testPipelines(t, names...)
	_, page := get(t, server, "/wall")

	if !strings.Contains(page, `style="--cols: 4; --rows: 2; --len: 2"`) {
		t.Errorf("seven pipelines are not laid out 4×2:\n%s", page)
	}
}

// TestEveryNameOnTheWallIsOneSize: sized each to its own tile, a long name came out smaller than a short one and read as smaller news. The wall sizes every name for the longest, and one absurd name is cut rather than shrinking every tile.
func TestEveryNameOnTheWallIsOneSize(t *testing.T) {
	t.Parallel()

	tiles := []wallTile{{overviewPipeline{Slug: "api"}}, {overviewPipeline{Slug: "billing-service"}}}
	if got := wallNameLen(tiles); got != len("billing-service") {
		t.Errorf("wallNameLen = %d, want the longest name's %d", got, len("billing-service"))
	}

	long := []wallTile{tiles[0], tiles[1], {overviewPipeline{Slug: strings.Repeat("x", 60)}}}
	if got := wallNameLen(long); got != wallNameCap {
		t.Errorf("wallNameLen with a 60-character name = %d, want the cap %d", got, wallNameCap)
	}
}

// TestANeverRunPipelineIsNotGreen: a pipeline that never ran has proved nothing, so it is neither counted nor drawn green.
func TestANeverRunPipelineIsNotGreen(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", twoJobs, "infra", oneManualJob)
	seedRun(t, pipelines[1], "run-1", "audit", "succeeded")

	_, page := get(t, server, "/wall")

	if !strings.Contains(page, `<a class="tile never" href="/p/app"`) {
		t.Errorf("a pipeline that never ran is not a never tile:\n%s", page)
	}

	if head := between(t, page, `id="wallhead"`, "</header>"); !strings.Contains(head, "1 green") || strings.Contains(head, "all 2 green") {
		t.Errorf("the head counts a never-run pipeline as green:\n%s", head)
	}
}

// TestARedPipelineBeingRebuiltStaysRed: a rebuild is not a fix until it passes, so the tile stays red and adds the building bar.
func TestARedPipelineBeingRebuiltStaysRed(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", twoJobs, "infra", oneManualJob)
	seedRun(t, pipelines[0], "run-1", "build", "failed")
	seedRun(t, pipelines[0], "run-2", "build", "")

	_, page := get(t, server, "/wall")

	if !strings.Contains(page, `<a class="tile red live" href="/p/app"`) {
		t.Errorf("a red pipeline with a run in flight is not a red tile with the building bar:\n%s", page)
	}
}
