package config

import (
	"strings"
	"testing"
	"time"
)

func TestCheckTimeoutValidationAndDefault(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, value, wantErr string
		wantLimit            time.Duration
	}{
		{"unset gets the 1h default", "", "", time.Hour},
		{"a duration is kept", "90s", "", 90 * time.Second},
		{"garbage is refused", "soon", `resource "r": check_timeout: invalid timeout "soon"`, 0},
		{"negative is refused", "-5s", "must not be negative", 0},
		{"zero is refused", "0s", "check_timeout must be a positive duration", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := &Config{Resources: []Resource{{Name: "r", CheckTimeout: tc.value}}}

			err := cfg.validateTimeouts()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got := cfg.Resources[0].CheckLimit(); got != tc.wantLimit {
				t.Errorf("CheckLimit = %v, want %v", got, tc.wantLimit)
			}
		})
	}
}
