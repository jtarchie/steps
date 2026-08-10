package config

// serial: / serial_groups: — stop jobs racing each other.

import "fmt"

// SerialGroupsByJob maps each job to the serial groups it belongs to.
//
// `serial: true` contributes an implicit group named after the job itself, so
// the cross-job machinery covers the single-job case too.
//
// It also turns something on now. Until job-level max_in_flight existed this
// runner serialized every job unconditionally, which made serial: true a
// statement of intent with nothing behind it; a job's effective concurrency is
// decided by MaxInFlightByJob below, and serial: forces it to 1.
func (c *Config) SerialGroupsByJob() map[string][]string {
	groups := map[string][]string{}

	for _, job := range c.Jobs {
		var names []string

		if job.Serial {
			names = append(names, "job:"+job.Name)
		}

		names = append(names, job.SerialGroups...)

		if len(names) > 0 {
			groups[job.Name] = names
		}
	}

	return groups
}

// validateSerial rejects the two spellings that would promise something this
// runner does not do.
func (c *Config) validateSerial() error {
	for _, job := range c.Jobs {
		seen := map[string]bool{}

		for _, group := range job.SerialGroups {
			if group == "" {
				return fmt.Errorf("job %q: serial_groups must not contain an empty name", job.Name)
			}

			if seen[group] {
				return fmt.Errorf("job %q: serial_groups names %q twice", job.Name, group)
			}

			seen[group] = true
		}

		err := checkJobMaxInFlight(job)
		if err != nil {
			return err
		}
	}

	return nil
}

// checkJobMaxInFlight rejects a max_in_flight: that cannot mean what it says.
//
// Concourse documents serial:/serial_groups: as taking precedence and forcing
// the value to 1, which means `max_in_flight: 5` beside `serial: true` is a
// line that does nothing. This runner rejects that rather than silently
// winning, the same way get_params: beside no_get: true is rejected — a
// deliberate narrowing of a config Concourse would accept, on the grounds that
// a number quietly overridden is worse than a load error naming the conflict.
func checkJobMaxInFlight(job Job) error {
	if job.MaxInFlight < 0 {
		return fmt.Errorf("job %q: max_in_flight must be a positive number of concurrent builds (omit it for unlimited)", job.Name)
	}

	if job.MaxInFlight == 0 {
		return nil
	}

	if job.Serial {
		return fmt.Errorf("job %q: max_in_flight is set alongside serial: true, which forces one build at a time — remove one", job.Name)
	}

	if len(job.SerialGroups) > 0 {
		return fmt.Errorf("job %q: max_in_flight is set alongside serial_groups, which forces one build at a time — remove one", job.Name)
	}

	return nil
}

// UnlimitedInFlight is what an unset max_in_flight: is stored as.
//
// A large number rather than NULL or 0, because Store.ClaimNextJob has to read
// this in one atomic statement and needs a value it can compare against. That
// leaves a missing row — a job removed from the pipeline between enqueue and
// claim — free to default to 1, which is the conservative answer, instead of
// being indistinguishable from "unlimited".
//
// Unlimited is bounded in practice anyway: `steps watch --max-concurrent`
// caps how many builds run at once across the whole pipeline.
const UnlimitedInFlight = 1_000_000

// MaxInFlightByJob maps each job to how many of its builds may run at once.
//
// serial:/serial_groups: force 1 and take precedence, matching Concourse. A
// job that sets neither and names no max_in_flight: is unlimited.
func (c *Config) MaxInFlightByJob() map[string]int {
	limits := map[string]int{}

	for _, job := range c.Jobs {
		limits[job.Name] = job.EffectiveMaxInFlight()
	}

	return limits
}

// EffectiveMaxInFlight is how many builds of this job may run at once, after
// serial:/serial_groups: have had their say.
func (j Job) EffectiveMaxInFlight() int {
	if j.Serial || len(j.SerialGroups) > 0 {
		return 1
	}

	if j.MaxInFlight > 0 {
		return j.MaxInFlight
	}

	return UnlimitedInFlight
}
