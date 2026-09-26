package resource

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

func cronType() config.ResourceType {
	return config.ResourceType{Name: config.CronType, Config: config.ResourceTypeConfig{Cron: true}}
}

// cronCheck runs the built-in check at a fixed moment, through the same door a poll uses.
func cronCheck(t *testing.T, source map[string]any, version map[string]any, now time.Time) []map[string]any {
	t.Helper()

	ctx := WithNow(context.Background(), func() time.Time { return now })

	versions, err := CheckVersions(ctx, nil, cronType(), nil, source, version)
	if err != nil {
		t.Fatalf("check at %s: %v", now, err)
	}

	return versions
}

func at(t *testing.T, text string) time.Time {
	t.Helper()

	moment, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatal(err)
	}

	return moment
}

// TestCronFiresWhenASlotHasPassedSinceTheLastVersion is the steady state: the cursor is the last version minted, and a slot between it and now is the only thing that mints another.
func TestCronFiresWhenASlotHasPassedSinceTheLastVersion(t *testing.T) {
	t.Parallel()

	source := map[string]any{"expression": "0 2 * * *"}
	previous := map[string]any{"time": "2026-09-24T02:00:10Z"}

	if got := cronCheck(t, source, previous, at(t, "2026-09-25T01:59:59Z")); len(got) != 0 {
		t.Errorf("a minute before the slot minted %v, want nothing", got)
	}

	got := cronCheck(t, source, previous, at(t, "2026-09-25T02:00:00Z"))
	if len(got) != 1 || got[0]["time"] != "2026-09-25T02:00:00Z" {
		t.Errorf("on the slot minted %v, want the moment itself", got)
	}

	// The version is when the check ran, in whole seconds — not the slot it answered.
	got = cronCheck(t, source, previous, at(t, "2026-09-25T02:00:45Z").Add(500*time.Millisecond))
	if len(got) != 1 || got[0]["time"] != "2026-09-25T02:00:45Z" {
		t.Errorf("a late poll minted %v, want its own moment, whole seconds", got)
	}

	// Once minted, the same slot does not fire again on the next poll.
	if got := cronCheck(t, source, got[0], at(t, "2026-09-25T02:01:15Z")); len(got) != 0 {
		t.Errorf("the poll after minting minted %v again, want nothing until the next slot", got)
	}
}

// TestCronFirstCheckFiresOnlyForASlotInTheLastHour: with nothing recorded there is no "since", and upstream's rule is an hour's worth of one.
func TestCronFirstCheckFiresOnlyForASlotInTheLastHour(t *testing.T) {
	t.Parallel()

	source := map[string]any{"expression": "0 2 * * *"}

	if got := cronCheck(t, source, nil, at(t, "2026-09-25T02:30:00Z")); len(got) != 1 {
		t.Errorf("half an hour after the slot, a first check minted %v, want one version", got)
	}

	if got := cronCheck(t, source, nil, at(t, "2026-09-25T03:30:00Z")); len(got) != 0 {
		t.Errorf("ninety minutes after the slot, a first check minted %v, want nothing", got)
	}

	immediate := map[string]any{"expression": "0 2 * * *", "fire_immediately": true}
	if got := cronCheck(t, immediate, nil, at(t, "2026-09-25T15:00:00Z")); len(got) != 1 || got[0]["time"] != "2026-09-25T15:00:00Z" {
		t.Errorf("fire_immediately minted %v on a first check, want the moment itself", got)
	}

	// And only on the first: once a version exists the schedule governs.
	if got := cronCheck(t, immediate, map[string]any{"time": "2026-09-25T15:00:00Z"}, at(t, "2026-09-25T15:05:00Z")); len(got) != 0 {
		t.Errorf("fire_immediately minted %v with a version recorded, want nothing", got)
	}
}

// TestCronReadsTheExpressionInItsLocation: two in the morning in New York is six in the morning UTC, in September.
func TestCronReadsTheExpressionInItsLocation(t *testing.T) {
	t.Parallel()

	source := map[string]any{"expression": "0 2 * * *", "location": "America/New_York"}
	previous := map[string]any{"time": "2026-09-24T06:00:00Z"}

	if got := cronCheck(t, source, previous, at(t, "2026-09-25T02:00:00Z")); len(got) != 0 {
		t.Errorf("two in the morning UTC minted %v, want nothing — the expression is in New York", got)
	}

	if got := cronCheck(t, source, previous, at(t, "2026-09-25T06:00:00Z")); len(got) != 1 || got[0]["time"] != "2026-09-25T06:00:00Z" {
		t.Errorf("two in the morning New York minted %v, want one version, recorded in UTC", got)
	}
}

// TestCronHonorsASecondsField: six fields, seconds first.
func TestCronHonorsASecondsField(t *testing.T) {
	t.Parallel()

	source := map[string]any{"expression": "*/2 * * * * *"}
	previous := map[string]any{"time": "2026-09-25T12:00:00Z"}

	if got := cronCheck(t, source, previous, at(t, "2026-09-25T12:00:01Z")); len(got) != 0 {
		t.Errorf("one second on minted %v, want nothing", got)
	}

	if got := cronCheck(t, source, previous, at(t, "2026-09-25T12:00:02Z")); len(got) != 1 {
		t.Errorf("two seconds on minted %v, want one version", got)
	}
}

// TestCronWithNoSlotInReachNeverFires: the parser answers a schedule it cannot
// satisfy within five years with the zero time, and a zero read as "already
// passed" would mint on every poll.
func TestCronWithNoSlotInReachNeverFires(t *testing.T) {
	t.Parallel()

	source := map[string]any{"expression": "0 0 30 2 *"}

	if got := cronCheck(t, source, map[string]any{"time": "2026-09-24T02:00:10Z"}, at(t, "2026-09-25T02:00:00Z")); len(got) != 0 {
		t.Errorf("a slot that never comes minted %v, want nothing", got)
	}

	if got := cronCheck(t, source, nil, at(t, "2026-09-25T02:00:00Z")); len(got) != 0 {
		t.Errorf("a first check of a slot that never comes minted %v, want nothing", got)
	}
}

func TestCronRefusesABadExpressionOrLocationBeforeAnythingPolls(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		source map[string]any
		want   string
	}{
		"expression": {map[string]any{"expression": "0 25 * * *"}, "source.expression"},
		"location":   {map[string]any{"expression": "@hourly", "location": "Mars/Olympus"}, "source.location"},
		"shape":      {map[string]any{"expresion": "@hourly"}, "expresion"},
	} {
		cfg := &config.Config{Resources: []config.Resource{{Name: "tick", Type: config.CronType, Source: tc.source}}}

		err := CompileCrons(cfg)
		if err == nil || !strings.Contains(err.Error(), `resource "tick"`) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want the resource and %q named", name, err, tc.want)
		}
	}

	good := &config.Config{Resources: []config.Resource{{Name: "tick", Type: config.CronType, Source: map[string]any{"expression": "30 6 * * 1-5", "location": "Europe/London"}}}}
	err := CompileCrons(good)
	if err != nil {
		t.Errorf("a well-formed resource was refused: %v", err)
	}
}

func TestCronRefusesACursorItCannotRead(t *testing.T) {
	t.Parallel()

	for name, cursor := range map[string]map[string]any{
		"not a moment": {"time": "yesterday"},
		"not a string": {"time": 1758780000},
	} {
		_, err := CheckVersions(context.Background(), nil, cronType(), nil, map[string]any{"expression": "@hourly"}, cursor)
		if err == nil || !strings.Contains(err.Error(), "version.time") {
			t.Errorf("%s: err = %v, want the unreadable cursor named", name, err)
		}
	}
}

func TestCronInRefusesWhatItCannotWrite(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		source, version map[string]any
		want            string
	}{
		"no source":  {map[string]any{}, map[string]any{"time": "2026-09-25T06:00:00Z"}, "source.expression"},
		"no version": {map[string]any{"expression": "@hourly"}, map[string]any{}, "version has no time"},
		"bad moment": {map[string]any{"expression": "@hourly"}, map[string]any{"time": "noon"}, "version.time"},
	} {
		err := RunIn(context.Background(), nil, cronType(), nil, tc.source, tc.version, nil, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

// TestCronInWritesTheMomentThreeWays: the version as itself, in the resource's zone, and as an epoch — what a script reads.
func TestCronInWritesTheMomentThreeWays(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := map[string]any{"expression": "0 2 * * *", "location": "America/New_York"}

	err := RunIn(context.Background(), nil, cronType(), nil, source, map[string]any{"time": "2026-09-25T06:00:00Z"}, nil, dir)
	if err != nil {
		t.Fatalf("RunIn: %v", err)
	}

	want := map[string]string{
		"version.json": `{"time":"2026-09-25T06:00:00Z"}`,
		"timestamp":    "2026-09-25T02:00:00-04:00",
		"epoch":        "1790316000",
	}

	for name, contents := range want {
		data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a t.TempDir()-scoped file this test asked for
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		if string(data) != contents {
			t.Errorf("%s = %q, want %q", name, data, contents)
		}
	}
}

func TestCronCannotBePutTo(t *testing.T) {
	t.Parallel()

	_, err := RunOut(context.Background(), nil, cronType(), nil, map[string]any{"expression": "@hourly"}, nil, PutInputs{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "only tells time") {
		t.Errorf("err = %v, want the put refused", err)
	}
}

// TestPreflightCompilesACronResource: preflight has one thing to prove for a
// type that runs in this process — that its source parses — and says so as a
// problem that is not transient, since no amount of waiting fixes a crontab.
func TestPreflightCompilesACronResource(t *testing.T) {
	t.Parallel()

	pipeline := func(expression string) *config.Config {
		return &config.Config{
			ResourceTypes: []config.ResourceType{cronType()},
			Resources:     []config.Resource{{Name: "tick", Type: config.CronType, Source: map[string]any{"expression": expression}}},
		}
	}

	if problems := Preflight(context.Background(), pipeline("@hourly"), nil, []string{"tick"}, nil); len(problems) != 0 {
		t.Errorf("a well-formed resource raised %v", problems)
	}

	problems := Preflight(context.Background(), pipeline("0 25 * * *"), nil, []string{"tick"}, nil)
	if len(problems) != 1 || problems[0].Target != `resource "tick"` || problems[0].Transient || !strings.Contains(problems[0].Detail, "source.expression") {
		t.Errorf("a bad expression raised %v, want one problem on the resource, not transient, naming the field", problems)
	}
}
