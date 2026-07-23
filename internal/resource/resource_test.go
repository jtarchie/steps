package resource

import (
	"context"
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

	result, err := RunOut(context.Background(), nil, rt, map[string]any{}, map[string]any{}, t.TempDir())
	if err != nil {
		t.Fatalf("RunOut: %v, want nil error for unparsable stdout", err)
	}

	if result != nil {
		t.Errorf("RunOut result = %v, want nil", result)
	}
}
