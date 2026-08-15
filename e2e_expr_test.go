package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestEndToEndExprResourceType drives an expr-backed resource type through
// the whole CLI stack — config load, validate, merkle plan, check, in, task,
// put — against a local JSON API. Everything below the CLI is exercised by
// unit tests; what only this can prove is that the pieces are wired to each
// other, since `source:` is the sole injection point a pipeline file has.
//
// The API is deliberately shaped like a real one: a listing endpoint, a
// detail endpoint per item, and a publish endpoint that echoes back the
// version it created.
func TestEndToEndExprResourceType(t *testing.T) {
	dir := t.TempDir()

	var posted atomic.Value

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/items":
			// The cursor arrives as a query parameter, which is the entire
			// point of Phase 1 meeting this backend: the type asks for what
			// it has not seen instead of guessing a window.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"since": r.URL.Query().Get("since"),
				"items": []map[string]string{{"id": "1"}, {"id": "2"}},
			})
		case strings.HasPrefix(r.URL.Path, "/items/"):
			id := strings.TrimPrefix(r.URL.Path, "/items/")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "title": "item " + id})
		case r.URL.Path == "/publish":
			body, _ := io.ReadAll(r.Body)
			posted.Store(string(body))
			_, _ = w.Write([]byte(`{"receipt": "r-7"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	// check_file rather than inline: the include has to resolve before
	// validate and before hashing, and this is the only test that proves it
	// does through the real CLI entry point.
	err := os.WriteFile(filepath.Join(dir, "check.expr"), []byte(`
	  let listing = http({
	    url: source.url + "/items",
	    query: {since: version.id ?? "0"},
	  }).json;
	  listing.items | map((
	    {id: #.id, since: listing.since}
	  ))
	`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := `
resource_types:
- name: api
  config:
    expr:
      check_file: check.expr
      in: |
        let item = http({url: source.url + "/items/" + version.id}).json;
        {
          "version.json": toJSON(version),
          "title.txt": item.title,
        }
- name: publisher
  config:
    expr:
      out: |
        let receipt = http({
          url: source.url + "/publish",
          json: {title: file("thing/title.txt"), note: file("notes/note.txt")},
        }).json;
        {receipt: receipt.receipt}

resources:
- name: thing
  type: api
  source:
    url: ` + server.URL + `
- name: published
  type: publisher
  source:
    url: ` + server.URL + `

jobs:
- name: build
  plan:
  - get: thing
  - task: note
    inputs: [thing]
    outputs: [notes]
    run: |
      test "$(cat thing/title.txt)" = "item 2"
      echo shipped > notes/note.txt
  - put: published
    inputs: [thing, notes]
`

	err = os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	// validate first: a syntax error in an expression is a fact about the
	// file, and this is where a pipeline author meets it.
	mustRun(t, "validate", path)
	mustRun(t, "run", path, "--job", "build")

	// The check returned two versions oldest-first, so the get resolved the
	// LAST one — proving the expr backend obeys the same ordering convention
	// as a shell check, which nothing about expressions guarantees for free.
	// The task above already asserted the fetched title; this asserts the put
	// saw the artifacts of both the get and the task.
	body, _ := posted.Load().(string)

	var payload map[string]any

	err = json.Unmarshal([]byte(body), &payload)
	if err != nil {
		t.Fatalf("published body %q: %v", body, err)
	}

	if payload["title"] != "item 2" {
		t.Errorf("published title = %v, want the fetched artifact's contents", payload["title"])
	}

	// file() read across two different input artifacts, and the trailing
	// newline is the task's, not a quirk of the reader.
	if payload["note"] != "shipped\n" {
		t.Errorf("published note = %q, want the task's output verbatim", payload["note"])
	}
}

// TestEndToEndExprSyntaxErrorFailsValidate: an unparsable expression is not a
// load error (internal/config imports no expression engine, deliberately), so
// this is the seam that catches it — before a watch loop polls a type that
// can never run.
func TestEndToEndExprSyntaxErrorFailsValidate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	err := os.WriteFile(path, []byte(`
resource_types:
- name: api
  config:
    expr:
      check: 'source.items | map('

resources:
- name: thing
  type: api
  source: {}

jobs:
- name: build
  plan:
  - get: thing
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = run([]string{"validate", path})
	if err == nil {
		t.Fatal("validate: want an error for an unparsable expression")
	}

	if !strings.Contains(err.Error(), "expr.check") {
		t.Errorf("err = %v, want it to name the slot", err)
	}

	// And --syntax-only must still catch it: nothing about parsing an
	// expression depends on this machine.
	err = run([]string{"validate", path, "--syntax-only"})
	if err == nil {
		t.Fatal("validate --syntax-only: want the same error")
	}
}
