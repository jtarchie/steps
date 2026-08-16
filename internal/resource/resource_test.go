package resource

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func TestVersionMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		version    any
		wantMode   string
		wantPinned map[string]string
	}{
		{"unset", nil, "latest", nil},
		{"latest string", "latest", "latest", nil},
		{"every string", "every", "every", nil},
		{"pinned any", map[string]any{"number": 87}, "pinned", map[string]string{"number": "87"}},
		{"pinned string", map[string]string{"ref": "abc"}, "pinned", map[string]string{"ref": "abc"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mode, pinned := VersionMode(config.Step{Version: tt.version})
			if mode != tt.wantMode {
				t.Errorf("mode = %q, want %q", mode, tt.wantMode)
			}

			if !reflect.DeepEqual(pinned, tt.wantPinned) {
				t.Errorf("pinned = %v, want %v", pinned, tt.wantPinned)
			}
		})
	}
}

func TestSelectVersion(t *testing.T) {
	t.Parallel()

	versions := []map[string]any{
		{"number": 1},
		{"number": 2},
		{"number": 3},
	}

	// TestConformance note: "latest when unpinned" verifies steps's claim
	// (CheckVersions's doc comment, "Concourse convention — no sorting
	// happens here") that a check script must return versions oldest-first
	// with the latest last, and steps trusts that order rather than
	// re-sorting. Concourse doc: concourse-ci.org/docs/resource-types/
	// implementing/ ("check" section) — versions are returned "in
	// chronological order (oldest first)".
	t.Run("latest when unpinned", func(t *testing.T) {
		t.Parallel()

		got, err := SelectVersion(versions, nil)
		if err != nil {
			t.Fatal(err)
		}

		if got["number"] != 3 {
			t.Errorf("got %v, want the last version", got)
		}
	})

	t.Run("matches pin", func(t *testing.T) {
		t.Parallel()

		got, err := SelectVersion(versions, map[string]string{"number": "2"})
		if err != nil {
			t.Fatal(err)
		}

		if got["number"] != 2 {
			t.Errorf("got %v, want number=2", got)
		}
	})

	t.Run("error on no versions", func(t *testing.T) {
		t.Parallel()

		_, err := SelectVersion(nil, nil)
		if err == nil {
			t.Error("expected error for empty versions")
		}
	})

	t.Run("error on unmatched pin", func(t *testing.T) {
		t.Parallel()

		_, err := SelectVersion(versions, map[string]string{"number": "99"})
		if err == nil {
			t.Error("expected error for unmatched pin")
		}
	})
}

// TestConformanceRunOutUnparsableStdoutIsNilNotError verifies RunOut's shell
// backend against the same claim internal/resource/mcp_test.go's
// TestRunOutMCPUnparsableResultIsNilNotError already verifies for the MCP
// backend (its own comment says "mirrors the shell backend's own
// convention" — this was previously asserted only in that comment, with no
// test of the shell path itself).
//
// Concourse doc: concourse-ci.org/docs/resource-types/implementing/ ("out"
// section) — an out script's stdout is a JSON object with version/metadata;
// nothing in the documented contract requires a script to emit one.
//
// steps claim under test: internal/resource/resource.go's RunOut doc
// comment ("unparsable or empty stdout is not an error").
func TestConformanceRunOutUnparsableStdoutIsNilNotError(t *testing.T) {
	t.Parallel()

	rt := config.ResourceType{
		Name:   "dummy",
		Config: config.ResourceTypeConfig{Out: "echo not-json"},
	}

	result, err := RunOut(context.Background(), nil, rt, nil, map[string]any{}, map[string]any{}, t.TempDir())
	if err != nil {
		t.Fatalf("RunOut: %v, want nil error for unparsable stdout", err)
	}

	if result != nil {
		t.Errorf("RunOut result = %v, want nil", result)
	}
}

// TestConformanceCheckReceivesCurrentVersion pins the half of the check
// contract steps used to omit. Concourse doc:
// concourse-ci.org/docs/resource-types/implementing/ ("check" section) — a
// check "is given the configured source and current version on stdin".
// steps passed the source alone, so every resource type talking to a web API
// had to guess a window (`limit: 20`) instead of asking for what it had not
// seen.
//
// The empty-map case is the load-bearing one: on the first-ever check there
// is no cursor, and templates render with missingkey=error, so a nil version
// would make `{{ index .version "ref" }}` a hard failure on the first poll of
// every pipeline. CheckVersions normalizes nil to an empty map so the
// documented `index ... | default` idiom works from the very first run.
func TestConformanceCheckReceivesCurrentVersion(t *testing.T) {
	t.Parallel()

	rt := config.ResourceType{
		Name: "dummy",
		Config: config.ResourceTypeConfig{
			Check: `printf '[{"ref": "%s"}]' '{{ index .version "ref" | default "none" }}'`,
		},
	}

	tests := map[string]struct {
		version map[string]any
		want    string
	}{
		"cursor recorded":     {version: map[string]any{"ref": "abc"}, want: "abc"},
		"first check, nil":    {version: nil, want: "none"},
		"first check, empty":  {version: map[string]any{}, want: "none"},
		"cursor without hits": {version: map[string]any{"other": "x"}, want: "none"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			versions, err := CheckVersions(context.Background(), nil, rt, nil, map[string]any{}, test.version)
			if err != nil {
				t.Fatalf("CheckVersions: %v", err)
			}

			if len(versions) != 1 || versions[0]["ref"] != test.want {
				t.Errorf("versions = %+v, want one version with ref %q", versions, test.want)
			}
		})
	}
}

// TestShellExtraEnvUnionsWithTypeEnv is the shell-backend counterpart of
// expr_test.go's TestExtraEnvUnionsWithTypeEnv — UnionEnv (and its wiring
// into shell.RunnerSpec.Env at every CheckVersions/RunIn/RunOut shell branch)
// was previously exercised only through the expr backend and the built-in
// Slack e2e tests, leaving the far more common shell-backed path unverified.
// Not t.Parallel: t.Setenv forbids it.
func TestShellExtraEnvUnionsWithTypeEnv(t *testing.T) {
	t.Setenv("STEPS_TEST_SHELL_TYPE_TOKEN", "type-value")
	t.Setenv("STEPS_TEST_SHELL_EXTRA_TOKEN", "extra-value")

	rt := config.ResourceType{
		Name: "dummy",
		Env:  []string{"STEPS_TEST_SHELL_TYPE_TOKEN"},
		Config: config.ResourceTypeConfig{
			Check: `printf '[{"a": "%s", "b": "%s"}]' "$STEPS_TEST_SHELL_TYPE_TOKEN" "$STEPS_TEST_SHELL_EXTRA_TOKEN"`,
		},
	}

	versions, err := CheckVersions(context.Background(), nil, rt, nil, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("CheckVersions: %v", err)
	}

	if versions[0]["b"] != "" {
		t.Fatalf("versions = %+v, want STEPS_TEST_SHELL_EXTRA_TOKEN invisible without extraEnv", versions)
	}

	versions, err = CheckVersions(context.Background(), nil, rt, []string{"STEPS_TEST_SHELL_EXTRA_TOKEN"}, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("CheckVersions with extraEnv: %v", err)
	}

	if versions[0]["a"] != "type-value" || versions[0]["b"] != "extra-value" {
		t.Fatalf("versions = %+v, want both the type's own env: and extraEnv readable", versions)
	}
}

// TestParseVersionJSON covers the reason this decodes with UseNumber: a
// recorded version goes straight back into a check template and out over the
// wire. Slack's ts through encoding/json's default float64 renders as
// 1.6998876540012e+09, which is not a timestamp any API has heard of.
func TestParseVersionJSON(t *testing.T) {
	t.Parallel()

	version, err := ParseVersionJSON(`{"ts": 1699887654.001200, "id": 12345678901234567890}`)
	if err != nil {
		t.Fatalf("ParseVersionJSON: %v", err)
	}

	if got := fmt.Sprint(version["ts"]); got != "1699887654.001200" {
		t.Errorf("ts = %s, want the digits as written", got)
	}

	if got := fmt.Sprint(version["id"]); got != "12345678901234567890" {
		t.Errorf("id = %s, want an id wider than float64 to survive", got)
	}

	_, err = ParseVersionJSON("not json")
	if err == nil {
		t.Error("ParseVersionJSON(not json): want an error")
	}
}
