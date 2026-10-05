package web

import (
	"strings"
	"testing"
)

// TestRunlessStateSaysWhyThereIsNoRun: an empty cell is four different facts,
// and each must read as its own word.
func TestRunlessStateSaysWhyThereIsNoRun(t *testing.T) {
	t.Parallel()

	every := consumer{Job: "build", mode: "every"}
	latest := consumer{Job: "build", mode: "latest"}
	pinned := consumer{Job: "build", mode: "pinned", pin: map[string]string{"ref": "v1"}}

	for _, tc := range []struct {
		name                string
		c                   consumer
		order, mark, newest int64
		pinned, queued      bool
		want                string
	}{
		{"every, ahead of the mark, queued", every, 5, 4, 0, false, true, "queued"},
		{"every, ahead of the mark, nothing queued", every, 5, 4, 0, false, false, "waiting"},
		{"every, at the mark", every, 4, 4, 0, false, true, "reaped"},
		{"every, below the mark", every, 3, 4, 0, false, false, "reaped"},
		{"latest, the newest version, queued", latest, 6, 0, 6, false, true, "queued"},
		{"latest, the newest version", latest, 6, 0, 6, false, false, "waiting"},
		{"latest, an older version, queued", latest, 5, 0, 6, false, true, "superseded"},
		{"latest, an older version", latest, 2, 0, 6, false, false, "superseded"},
		{"pinned, the pin", pinned, 2, 0, 0, true, false, "waiting"},
		{"pinned, the pin, queued", pinned, 2, 0, 0, true, true, "queued"},
		{"pinned, not the pin", pinned, 9, 0, 0, false, true, "not pinned"},
	} {
		got := runlessState(tc.c, tc.order, tc.mark, tc.newest, tc.pinned, tc.queued)
		if got != tc.want {
			t.Errorf("%s: runlessState = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestMatchesPin: a pin names a subset of a version's fields, written as
// text, so a numeric field still matches its YAML spelling.
func TestMatchesPin(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		version string
		pin     map[string]string
		want    bool
	}{
		{`{"ref":"v1","n":3}`, map[string]string{"ref": "v1"}, true},
		{`{"ref":"v1","n":3}`, map[string]string{"n": "3"}, true},
		{`{"ref":"v2"}`, map[string]string{"ref": "v1"}, false},
		{`{"ref":"v1"}`, map[string]string{"ref": "v1", "n": "3"}, false},
		{`{"ref":"v1"}`, nil, false},
	} {
		got := matchesPin(tc.version, tc.pin)
		if got != tc.want {
			t.Errorf("matchesPin(%s, %v) = %v, want %v", tc.version, tc.pin, got, tc.want)
		}
	}
}

// TestResourceDetailSaysWhenNoJobGetsIt: with no consumer there are no job
// columns, and the page says so rather than leaving the reader to wonder
// where they went.
func TestResourceDetailSaysWhenNoJobGetsIt(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	cfg := pipeline.Config()
	for i := range cfg.Jobs {
		cfg.Jobs[i].Plan = cfg.Jobs[i].Plan[1:]
	}

	_, body := get(t, server, "/p/demo/resources/repo")

	if !strings.Contains(body, "no job gets this resource") {
		t.Errorf("a resource no job gets does not say so:\n%s", body)
	}
}
