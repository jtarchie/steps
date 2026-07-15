package resource

import (
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
