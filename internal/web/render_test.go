package web

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

func TestFormatUSDKeepsFractionsOfACent(t *testing.T) {
	t.Parallel()

	for amount, want := range map[float64]string{
		0.5:  "$0.5000",
		1:    "$1.00",
		12.5: "$12.50",
	} {
		if got := FormatUSD(amount); got != want {
			t.Errorf("FormatUSD(%v) = %q, want %q", amount, got, want)
		}
	}
}

func TestFormatDurationTiersChangeAtTheirUnit(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{0, "—"},
		{time.Millisecond, "1ms"},
		{time.Second, "1.0s"},
		{time.Minute, "1m 00s"},
		{time.Hour, "1h 00m"},
	} {
		if got := formatDuration(tc.in); got != tc.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSparklineScalesOldestFirstToTheSlowest(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	run := func(took time.Duration, status string) store.RunRow {
		return store.RunRow{StartedAt: start, FinishedAt: start.Add(took), Status: status}
	}

	bars := sparkline([]store.RunRow{
		run(2*time.Second, "failed"),
		run(time.Second, "succeeded"),
		run(10*time.Millisecond, "succeeded"),
	})

	want := []struct {
		height int
		failed bool
	}{{1, false}, {7, false}, {14, true}}

	if len(bars) != len(want) {
		t.Fatalf("got %d bars, want %d: %+v", len(bars), len(want), bars)
	}

	for i, bar := range bars {
		if bar.Height != want[i].height || bar.Failed != want[i].failed {
			t.Errorf("bar %d = height %d failed %v, want height %d failed %v",
				i, bar.Height, bar.Failed, want[i].height, want[i].failed)
		}
	}
}

// TestJobPageSparkBarsStandOnTheBaseline: the bars are placed by template arithmetic, so only the drawn SVG shows them landing on the 16-high baseline at an 8-unit pitch.
func TestJobPageSparkBarsStandOnTheBaseline(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	startFinishedRun(t, pipeline, "run-1", "build", "succeeded")
	startFinishedRun(t, pipeline, "run-2", "build", "failed")

	_, body := get(t, server, "/p/demo/jobs/build/detail")
	svg := between(t, body, `<svg class="spark"`, "</svg>")

	if !strings.HasPrefix(svg, `<svg class="spark" width="16"`) {
		t.Errorf("two bars should make a 16-wide chart: %s", svg)
	}

	rects := regexp.MustCompile(`<rect x="(-?\d+)" y="(-?\d+)" width="6" height="(\d+)"`).FindAllStringSubmatch(svg, -1)
	if len(rects) != 2 {
		t.Fatalf("want 2 bars, got %d: %s", len(rects), svg)
	}

	for i, rect := range rects {
		x, _ := strconv.Atoi(rect[1])
		y, _ := strconv.Atoi(rect[2])
		height, _ := strconv.Atoi(rect[3])

		if x != 8*i || y+height != 16 {
			t.Errorf("bar %d at x=%d y=%d height=%d, want x=%d and its top at 16-height", i, x, y, height, 8*i)
		}
	}
}
