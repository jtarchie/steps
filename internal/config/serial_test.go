package config

import "testing"

// TestEffectiveMaxInFlight covers the precedence Concourse documents, which is
// where the value a job actually gets is decided — Store.ClaimNextJob reads
// only this answer, never the three fields it came from.
func TestEffectiveMaxInFlight(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		job  Job
		want int
	}{
		{"unset is unlimited", Job{Name: "j"}, UnlimitedInFlight},
		{"an explicit cap is used", Job{Name: "j", MaxInFlight: 3}, 3},
		{"serial forces one", Job{Name: "j", Serial: true}, 1},
		{"serial_groups forces one", Job{Name: "j", SerialGroups: []string{"deploy"}}, 1},
		{
			// Rejected at load (checkJobMaxInFlight), but the precedence still
			// has to be right: this resolver is what any future caller reaches
			// for, and "serial wins" is Concourse's documented rule.
			"serial still wins over a cap",
			Job{Name: "j", Serial: true, MaxInFlight: 9},
			1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := test.job.EffectiveMaxInFlight()
			if got != test.want {
				t.Errorf("EffectiveMaxInFlight() = %d, want %d", got, test.want)
			}
		})
	}
}

// TestMaxInFlightByJobCoversEveryJob guards the wiring rather than the maths:
// Watch syncs this map wholesale, and a job missing from it falls back to
// ClaimNextJob's conservative default of 1 — which would silently serialize a
// job whose pipeline said otherwise.
func TestMaxInFlightByJobCoversEveryJob(t *testing.T) {
	t.Parallel()

	cfg := &Config{Jobs: []Job{
		{Name: "unbounded"},
		{Name: "capped", MaxInFlight: 4},
		{Name: "serialized", Serial: true},
	}}

	limits := cfg.MaxInFlightByJob()

	if len(limits) != len(cfg.Jobs) {
		t.Fatalf("MaxInFlightByJob() covered %d jobs, want %d — an uncovered job silently falls back to 1", len(limits), len(cfg.Jobs))
	}

	for name, want := range map[string]int{
		"unbounded":  UnlimitedInFlight,
		"capped":     4,
		"serialized": 1,
	} {
		if limits[name] != want {
			t.Errorf("limit for %q = %d, want %d", name, limits[name], want)
		}
	}
}
