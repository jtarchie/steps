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

// The hint is printed to be pasted: a url's ? and & would glob and background the command unquoted, and a password in it would be printed to a terminal and a log.
func TestAnswerDBQuotesAURLAndDropsItsPassword(t *testing.T) {
	t.Parallel()

	got := answerDB("app.yml", "postgres://ci:hunter22@db:5432/steps?sslmode=verify-full&application_name=x")

	if want := "'postgres://ci@db:5432/steps?sslmode=verify-full&application_name=x'"; got != want {
		t.Errorf("answerDB = %s, want %s", got, want)
	}

	if got := answerDB("app.yml", "my state.db"); got != "'my state.db'" {
		t.Errorf("answerDB = %s, want the path quoted", got)
	}
}
