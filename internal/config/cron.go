package config

// type: cron — the shape of a cron resource's source:, and the load-time rules that need no cron parser.

import (
	"errors"
	"fmt"
	"time"

	// A location is checked at load, and a daemon in a scratch container has no zoneinfo to check it against.
	_ "time/tzdata"
)

// CronType is the built-in type whose versions are moments a crontab expression names; it is Go rather than YAML because there is nothing to run, only a clock to read.
const CronType = "cron"

// CronSource is a cron resource's source:, typed. Config knows its shape; the expression's meaning lives in internal/resource, which is where resource.Cron finishes the job.
type CronSource struct {
	// Expression is a crontab: five fields, or six with seconds in front, or a descriptor like @hourly.
	Expression string `yaml:"expression"`
	// Location is the zone the expression is read in, UTC when unset.
	Location string `yaml:"location,omitempty"`
	// FireImmediately makes the first-ever check mint a version instead of waiting for a slot.
	FireImmediately bool `yaml:"fire_immediately,omitempty"`
}

// CronSource decodes this resource's source: strictly, so a misspelled key is a load error rather than a schedule nobody reads.
func (r Resource) CronSource() (CronSource, error) {
	source, err := ParseCronSource(r.Source)
	if err != nil {
		return source, fmt.Errorf("resource %q: %w", r.Name, err)
	}

	return source, nil
}

// ParseCronSource is CronSource for a caller handed the map and not the resource — the check is. The zone is only named here; whoever needs it resolves it once through TimeLocation.
func ParseCronSource(raw map[string]any) (CronSource, error) {
	var source CronSource

	err := decodeSource(raw, &source)
	if err != nil {
		return source, err
	}

	if source.Expression == "" {
		return source, errors.New("a cron resource needs source.expression, the crontab that says when a version is minted")
	}

	return source, nil
}

// TimeLocation is the zone the expression is read in.
func (s CronSource) TimeLocation() (*time.Location, error) {
	if s.Location == "" {
		return time.UTC, nil
	}

	location, err := time.LoadLocation(s.Location)
	if err != nil {
		return nil, fmt.Errorf("source.location %q: %w", s.Location, err)
	}

	return location, nil
}

func (c *Config) validateCronResources() error {
	for _, rt := range c.ResourceTypes {
		if rt.Name == CronType && !rt.Config.Cron {
			return fmt.Errorf("resource_type %q: the name is built in — a cron resource needs no resource_types: entry, so name this type something else", rt.Name)
		}
	}

	for _, resource := range c.Resources {
		if resource.Type != CronType {
			continue
		}

		source, err := resource.CronSource()
		if err != nil {
			return err
		}

		_, err = source.TimeLocation()
		if err != nil {
			return fmt.Errorf("resource %q: %w", resource.Name, err)
		}
	}

	return nil
}
