package secret

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeOp behaves like `op read`: exit 1 for both a missing item and an expired session, told apart only by stderr.
func fakeOp(t *testing.T, items map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	for path, value := range items {
		file := filepath.Join(dir, filepath.FromSlash(path))

		err := os.MkdirAll(filepath.Dir(file), 0o700)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(file, []byte(value), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func resolverFor(t *testing.T, body string) *Resolver {
	t.Helper()

	resolver, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	return resolver
}

const opScript = `sh -c 'if [ -f "$1" ]; then cat "$1"; elif [ -f "$(dirname "$1")/EXPIRED" ]; then echo "session expired" >&2; exit 1; else echo "isn'"'"'t an item" >&2; exit 1; fi' sh `

func opSource(dir, lookup string) string {
	return `
source:
  ci:
    command: ` + opScript + dir + `/{{ .path }}
    missing: exit_code == 1 && stderr contains "isn't an item"
    lookup:
` + lookup
}

func TestLookupFirstFoundWins(t *testing.T) {
	t.Parallel()

	dir := fakeOp(t, map[string]string{
		"deploy/gh_token":  "deploy-token\n",
		"shared/gh_token":  "shared-token",
		"shared/npm_token": "shared-npm",
	})

	resolver := resolverFor(t, opSource(dir, `
    - path: "{{ .pipeline }}/{{ .name }}"
    - path: release/{{ .name }}
      when: pipeline in ["deploy", "release"]
    - path: shared/{{ .name }}
`))

	for _, tc := range []struct {
		pipeline, name, want string
	}{
		{"deploy", "gh_token", "deploy-token"},
		{"docs", "gh_token", "shared-token"},
		{"deploy", "npm_token", "shared-npm"},
	} {
		value, err := resolver.Lookup(t.Context(), Ref{Pipeline: tc.pipeline, Source: "ci", Name: tc.name})
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.pipeline, tc.name, err)
		}

		if value != tc.want {
			t.Errorf("%s/%s = %q, want %q", tc.pipeline, tc.name, value, tc.want)
		}
	}
}

// The reason missing: is required: an expired login must stop the build, not fall through to the shared credential.
func TestLookupStopsOnAFailureThatIsNotMissing(t *testing.T) {
	t.Parallel()

	dir := fakeOp(t, map[string]string{"deploy/EXPIRED": "", "shared/gh_token": "shared-token"})

	resolver := resolverFor(t, opSource(dir, `
    - path: deploy/{{ .name }}
    - path: shared/{{ .name }}
`))

	value, err := resolver.Lookup(t.Context(), Ref{Pipeline: "deploy", Source: "ci", Name: "gh_token"})
	if err == nil || !strings.Contains(err.Error(), "session expired") {
		t.Fatalf("Lookup = %q, %v; want the expired session reported", value, err)
	}
}

func TestLookupNotFoundNamesWhereItLooked(t *testing.T) {
	t.Parallel()

	resolver := resolverFor(t, opSource(fakeOp(t, nil), `
    - path: deploy/{{ .name }}
    - path: shared/{{ .name }}
      when: pipeline == "nobody"
`))

	_, err := resolver.Lookup(t.Context(), Ref{Pipeline: "deploy", Source: "ci", Name: "gh_token"})
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "deploy/gh_token") || strings.Contains(err.Error(), "shared/") {
		t.Fatalf("Lookup = %v; want ErrNotFound naming only the path tried", err)
	}
}

func TestLookupRefusesAnEmptyValueAndNeverPrintsStdout(t *testing.T) {
	t.Parallel()

	resolver := resolverFor(t, `
source:
  ci:
    command: printf ''
    lookup: [{path: x}]
  loud:
    command: echo the-value-itself; exit 3
    lookup: [{path: x}]
`)

	_, err := resolver.Lookup(t.Context(), Ref{Source: "ci", Name: "a"})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("an empty value = %v, want refused", err)
	}

	_, err = resolver.Lookup(t.Context(), Ref{Source: "loud", Name: "a"})
	if err == nil || strings.Contains(err.Error(), "the-value-itself") {
		t.Errorf("a failed source = %v, want an error without its stdout", err)
	}
}

func TestLookupGivesUpAtTheTimeout(t *testing.T) {
	t.Parallel()

	resolver := resolverFor(t, `
source:
  ci:
    command: sleep 30
    timeout: 200ms
    lookup: [{path: x}]
`)

	start := time.Now()

	_, err := resolver.Lookup(t.Context(), Ref{Source: "ci", Name: "a"})
	if err == nil || !strings.Contains(err.Error(), "gave up after 200ms") {
		t.Fatalf("Lookup = %v, want a timeout", err)
	}

	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Lookup took %s; the source was not killed at its deadline", elapsed)
	}
}

func TestParseRefusals(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, body, want string }{
		{"unknown key", "source: {ci: {command: x, lookup: [{path: x}], mising: y}}", "mising"},
		{"no missing with two paths", "source: {ci: {command: x, lookup: [{path: a}, {path: b}]}}", "missing: is required"},
		{"no lookup", "source: {ci: {command: x}}", "lookup:"},
		{"no command", "source: {ci: {lookup: [{path: a}]}}", "command:"},
		{"bad when", "source: {ci: {command: x, lookup: [{path: a, when: 'env(\"HOME\")'}]}}", "when"},
		{"bad path field", "source: {ci: {command: x, lookup: [{path: '{{ .pipline }}'}]}}", "lookup[0].path"},
		{"bad source name", "source: {CI: {command: x, lookup: [{path: a}]}}", "lower-case"},
		{"no sources", "source: {}", "names no source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse([]byte(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// A poll's cache answers within its window and asks again after it, so a rotated credential reaches the poller within one interval.
func TestForChecksExpiresItsAnswers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	counter := filepath.Join(dir, "count")

	resolver := resolverFor(t, `
source:
  ci:
    command: echo x >> `+counter+`; echo value
    lookup: [{path: x}]
`)

	asks := func() int {
		body, _ := os.ReadFile(counter) //nolint:gosec // the test's own file

		return strings.Count(string(body), "x")
	}

	cache := resolver.ForChecks(100 * time.Millisecond)
	ref := Ref{Pipeline: "p", Source: "ci", Name: "a"}

	for range 3 {
		_, err := cache.Lookup(t.Context(), ref)
		if err != nil {
			t.Fatal(err)
		}
	}

	if asks() != 1 {
		t.Fatalf("the source ran %d times inside one window, want 1", asks())
	}

	time.Sleep(300 * time.Millisecond)

	_, _ = cache.Lookup(t.Context(), ref)

	if asks() != 2 {
		t.Errorf("the source ran %d times after the window, want 2", asks())
	}
}
