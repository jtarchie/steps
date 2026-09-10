package cli

import "testing"

// A daemon's hint omits --db, so the local one may too only when the run's file IS the read commands' default; any other file unnamed is the daemon's opened instead.
func TestAnswerDBNamesEveryStateFileButTheDefault(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		pipeline string
		db       DB
		want     string
	}{
		{"ci/app.yml", "", "ci/.steps/app.yml.db"},
		{"app.yml", "shared.db", "shared.db"},
		{"app.yml", "sqlite://shared.db", "shared.db"},
		{"app.yml", ".steps/steps.db", ""},
		{"app.yml", "sqlite://./.steps/steps.db", ""},
	} {
		got := answerDB(tc.pipeline, tc.db)
		if got != tc.want {
			t.Errorf("answerDB(%q, %q) = %q, want %q", tc.pipeline, tc.db, got, tc.want)
		}
	}
}
