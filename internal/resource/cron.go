package resource

// The built-in cron type: a version is a moment its expression named, minted
// by the check that first ran after it. A port of govuk-pay/cron-resource,
// whose rules are kept exactly: one version per check at most, no catch-up of
// slots a stopped daemon missed, and a first check that fires only when a
// slot fell in the last hour — or at once, when fire_immediately says so.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
)

// cronVersionField is the one key of a cron version: the moment it was minted, RFC3339 in UTC.
const cronVersionField = "time"

// cronCold is how far back a first check looks for a slot when nothing has been recorded — the upstream resource's rule, kept as-is.
const cronCold = time.Hour

// cronParser reads the five-field crontab form, the same with seconds in front, and descriptors like @hourly.
var cronParser = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor) //nolint:gochecknoglobals // built once, read-only

// Schedule is one cron resource compiled: when it fires, in which zone, and what its first check does.
type Schedule struct {
	schedule        cron.Schedule
	location        *time.Location
	fireImmediately bool
}

// Cron compiles one cron resource. Every door a pipeline comes in by calls it, so a bad expression is refused where the pipeline is set rather than at the first poll.
func Cron(res config.Resource) (*Schedule, error) {
	source, err := res.CronSource()
	if err != nil {
		return nil, err //nolint:wrapcheck // CronSource names the resource
	}

	schedule, err := compileCron(source)
	if err != nil {
		return nil, fmt.Errorf("resource %q: %w", res.Name, err)
	}

	return schedule, nil
}

func compileCron(source config.CronSource) (*Schedule, error) {
	location, err := source.TimeLocation()
	if err != nil {
		return nil, err //nolint:wrapcheck // names the field
	}

	schedule, err := cronParser.Parse(source.Expression)
	if err != nil {
		return nil, fmt.Errorf("source.expression %q: %w", source.Expression, err)
	}

	return &Schedule{schedule: schedule, location: location, fireImmediately: source.FireImmediately}, nil
}

// Check is the versions a poll at now reports, given the last version recorded: one, when a slot has passed since it, or none.
//
// A slot that passed is answered with now rather than with the slot itself, as upstream does: the version says when the job was actually released, and `epoch` in the artifact means that. It is whole seconds because RFC3339 is, and a slot is never finer.
func (s *Schedule) Check(previous map[string]any, now time.Time) ([]map[string]any, error) {
	now = now.In(s.location)

	last, found, err := cronPrevious(previous)
	if err != nil {
		return nil, err
	}

	if !found {
		if s.fireImmediately {
			return []map[string]any{cronVersion(now)}, nil
		}

		last = now.Add(-cronCold)
	}

	// robfig answers a schedule with no slot in the next five years (`0 0 30 2 *`)
	// with the zero time, which every now is after — read as "due" it would
	// mint on every poll, the opposite of never.
	next := s.schedule.Next(last.In(s.location))
	if next.IsZero() || now.Before(next) {
		return nil, nil
	}

	return []map[string]any{cronVersion(now)}, nil
}

func cronVersion(at time.Time) map[string]any {
	return map[string]any{cronVersionField: at.UTC().Format(time.RFC3339)}
}

// cronPrevious reads the moment a version names; found is false for the empty cursor a first check is handed.
func cronPrevious(version map[string]any) (time.Time, bool, error) {
	raw, ok := version[cronVersionField]
	if !ok {
		return time.Time{}, false, nil
	}

	text, ok := raw.(string)
	if !ok {
		return time.Time{}, false, fmt.Errorf("version.%s is %T, want an RFC3339 string", cronVersionField, raw)
	}

	at, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("version.%s: %w", cronVersionField, err)
	}

	return at, true, nil
}

// CompileCrons is Cron over every cron resource, for a caller that wants only the refusal.
func CompileCrons(cfg *config.Config) error {
	for _, res := range cfg.Resources {
		if res.Type != config.CronType {
			continue
		}

		_, err := Cron(res)
		if err != nil {
			return err
		}
	}

	return nil
}

func cronCheckVersions(ctx context.Context, rt config.ResourceType, source, version map[string]any) ([]map[string]any, error) {
	schedule, err := cronSchedule(source)
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	versions, err := schedule.Check(version, nowFrom(ctx))
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	events.Logger(ctx).Info("resource.checked", "resource_type", rt.Name, "versions", len(versions))

	return versions, nil
}

func cronSchedule(source map[string]any) (*Schedule, error) {
	parsed, err := config.ParseCronSource(source)
	if err != nil {
		return nil, err //nolint:wrapcheck // names the field
	}

	return compileCron(parsed)
}

// cronRunIn writes the version out three ways: as itself, as a timestamp in the resource's zone, and as seconds since the epoch — the two files scripts read from Concourse's time resource. Only the zone is read from source: — a get has a version in hand and nothing to compute from the expression.
func cronRunIn(rt config.ResourceType, source, version map[string]any, destDir string) error {
	parsed, err := config.ParseCronSource(source)
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	location, err := parsed.TimeLocation()
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	at, found, err := cronPrevious(version)
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	if !found {
		return fmt.Errorf("in %q: version has no %s", rt.Name, cronVersionField)
	}

	encoded, err := json.Marshal(version)
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	files := map[string][]byte{
		"version.json": encoded,
		"timestamp":    []byte(at.In(location).Format(time.RFC3339)),
		"epoch":        []byte(strconv.FormatInt(at.Unix(), 10)),
	}

	for name, contents := range files {
		err = os.WriteFile(filepath.Join(destDir, name), contents, 0o600)
		if err != nil {
			return fmt.Errorf("in %q: %w", rt.Name, err)
		}
	}

	return nil
}

// errCronOut is a put of a cron resource that reached RunOut; the load rule (validateResourcePut) refuses it first.
var errCronOut = errors.New("a cron resource cannot be put to: it only tells time")

type nowKey struct{}

// WithNow fixes what a cron check reads as the current time. Tests drive a schedule through a poll with it; production reads the wall clock.
func WithNow(ctx context.Context, now func() time.Time) context.Context {
	return context.WithValue(ctx, nowKey{}, now)
}

func nowFrom(ctx context.Context) time.Time {
	if now, ok := ctx.Value(nowKey{}).(func() time.Time); ok {
		return now()
	}

	return time.Now()
}
