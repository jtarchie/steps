package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestEveryMutatingRouteIsRecordedOrExcused: the log is only as good as the routes it covers, so a mutating route added without an entry, or an entry for a route that is gone, fails here.
func TestEveryMutatingRouteIsRecordedOrExcused(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)
	seen := mutatingRoutes(server)

	for key := range seen {
		route, recorded := activityRoutes[key]
		if !recorded && notRecorded[key] == "" {
			t.Errorf("%s changes something and is neither in activityRoutes nor excused in notRecorded", key)
		}

		if recorded && route.role < RoleOperator {
			t.Errorf("%s changes something and asks for no more than reading", key)
		}
	}

	for key := range activityRoutes {
		if !seen[key] {
			t.Errorf("activityRoutes names %s, which is not a route", key)
		}
	}

	for key := range notRecorded {
		if !seen[key] {
			t.Errorf("notRecorded excuses %s, which is not a route", key)
		}
	}
}

// TestARefusedActionIsNotRecorded: the log says what happened, not what was asked for.
func TestARefusedActionIsNotRecorded(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	if code := post(t, server, "/p/demo/jobs/build/trigger", nil); code < http.StatusBadRequest {
		t.Fatalf("a read-only server accepted a trigger (%d)", code)
	}

	entries, err := pipeline.Store.Reader().Activity(context.Background(), store.ActivityFilter{})
	if err != nil || len(entries) != 0 {
		t.Fatalf("Activity = %+v, %v; want nothing recorded for a refused trigger", entries, err)
	}
}

// TestTheActivityPageLinksAndNarrows: every name narrows the log, a live pipeline's target links to its page, and an entry nobody signed in for says so.
func TestTheActivityPageLinksAndNarrows(t *testing.T) {
	t.Parallel()

	server, pipelines := testPipelines(t, "app")
	pipeline := pipelines[0]
	ctx := context.Background()

	for _, entry := range []store.ActivityEntry{
		{Actor: "local:alex", Action: "trigger", Target: "build"},
		{Action: "pause"},
		{Actor: "local:sam", Action: "reject", Target: "approval 3", Detail: `{"reason":"not today"}`},
	} {
		err := pipeline.Store.RecordActivity(ctx, entry, 0)
		if err != nil {
			t.Fatalf("RecordActivity: %v", err)
		}
	}

	_, body := get(t, server, "/activity")

	for _, want := range []string{
		`<a href="/p/app/jobs/build">build</a>`,
		`href="/activity?actor=local%3Aalex"`,
		`href="/activity?action=reject"`,
		`<span class="dim">“not today”</span>`,
		`<span class="faint">—</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the activity page does not carry %s", want)
		}
	}

	_, narrowed := get(t, server, "/activity?actor=local%3Aalex")
	if strings.Contains(narrowed, "sam (local)") || !strings.Contains(narrowed, `person: <b>alex (local)</b>`) || !strings.Contains(narrowed, `>clear</a>`) {
		t.Errorf("narrowed to alex, the page still shows sam or does not say what it is narrowed by: %s", narrowed)
	}
}

// TestTheActivityPageSaysWhenThereIsNothing: an empty log is a sentence, not an empty table.
func TestTheActivityPageSaysWhenThereIsNothing(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	_, body := get(t, server, "/activity")
	if !strings.Contains(body, "Nothing recorded yet.") || strings.Contains(body, `<table class="activity">`) {
		t.Error("an empty log drew a table instead of saying it is empty")
	}
}

// mutatingRoutes is every route that is not a read, as method and path.
func mutatingRoutes(server *Server) map[string]bool {
	routes := map[string]bool{}

	for _, route := range server.echo.Router().Routes() {
		// echo registers a not-found handler per group under a pseudo-method; it is no route anybody calls.
		if route.Method == http.MethodGet || route.Method == http.MethodHead || route.Method == "echo_route_not_found" {
			continue
		}

		routes[route.Method+" "+route.Path] = true
	}

	return routes
}
