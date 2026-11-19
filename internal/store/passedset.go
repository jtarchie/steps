package store

import (
	"context"
	"maps"
	"slices"
)

// PassedBuildsQuery asks a driver for one upstream job's green builds that hold a version of every one of Resources, newest first, Limit at a time from Offset.
type PassedBuildsQuery struct {
	Job       string
	Resources []string
	// Fixed narrows to builds that hold these versions: what a job resolved earlier already chose for a resource it shares with this one.
	Fixed  map[string]VersionKey
	Offset int
	Limit  int
}

// PassedBuildsPage is how many builds a resolution reads of one upstream job at a time.
const PassedBuildsPage = 32

// PassedBuilds is a driver's half of ResolvePassedSet: each build is its version of every queried resource.
type PassedBuilds func(ctx context.Context, query PassedBuildsQuery) ([]map[string]VersionKey, error)

// ResolvePassedSet is Concourse's group resolver (atc/scheduler/algorithm/group_resolver.go) over a driver's builds: upstream jobs in name order, each one's builds newest first, every job agreeing with the jobs before it on any resource they share, backtracking when one cannot.
func ResolvePassedSet(ctx context.Context, constraints map[string][]string, builds PassedBuilds) (map[string]VersionKey, bool, error) {
	byJob := map[string][]string{}

	for resource, jobs := range constraints {
		for _, job := range jobs {
			if !slices.Contains(byJob[job], resource) {
				byJob[job] = append(byJob[job], resource)
			}
		}
	}

	for _, resources := range byJob {
		slices.Sort(resources)
	}

	chosen := map[string]VersionKey{}

	found, err := resolvePassedFrom(ctx, slices.Sorted(maps.Keys(byJob)), byJob, chosen, builds)
	if err != nil || !found {
		return nil, false, err
	}

	return chosen, true, nil
}

func resolvePassedFrom(ctx context.Context, jobs []string, byJob map[string][]string, chosen map[string]VersionKey, builds PassedBuilds) (bool, error) {
	if len(jobs) == 0 {
		return true, nil
	}

	resources := byJob[jobs[0]]
	fixed, open := splitChosen(resources, chosen)

	for offset := 0; ; offset += PassedBuildsPage {
		page, err := builds(ctx, PassedBuildsQuery{Job: jobs[0], Resources: resources, Fixed: fixed, Offset: offset, Limit: PassedBuildsPage})
		if err != nil {
			return false, err
		}

		for _, build := range page {
			found, err := resolvePassedWith(ctx, build, open, jobs[1:], byJob, chosen, builds)
			// With nothing open every build agrees with what is chosen, so the first answers for them all.
			if err != nil || found || len(open) == 0 {
				return found, err
			}
		}

		if len(page) < PassedBuildsPage {
			return false, nil
		}
	}
}

// splitChosen is a job's resources an earlier job already chose, with their versions, and those still open.
func splitChosen(resources []string, chosen map[string]VersionKey) (map[string]VersionKey, []string) {
	fixed := map[string]VersionKey{}

	var open []string

	for _, resource := range resources {
		if version, ok := chosen[resource]; ok {
			fixed[resource] = version
		} else {
			open = append(open, resource)
		}
	}

	return fixed, open
}

// resolvePassedWith chooses build's versions of the open resources and resolves the jobs left, unchoosing them when that fails.
func resolvePassedWith(
	ctx context.Context, build map[string]VersionKey, open, jobs []string, byJob map[string][]string, chosen map[string]VersionKey, builds PassedBuilds,
) (bool, error) {
	for _, resource := range open {
		chosen[resource] = build[resource]
	}

	found, err := resolvePassedFrom(ctx, jobs, byJob, chosen, builds)
	if !found {
		for _, resource := range open {
			delete(chosen, resource)
		}
	}

	return found, err
}

// GroupPassedBuilds folds a driver's (build, resource, version) rows, each build's together and in the order the builds come, into PassedBuilds' answer.
func GroupPassedBuilds(rows [][3]string) []map[string]VersionKey {
	var (
		builds []map[string]VersionKey
		last   string
	)

	for _, row := range rows {
		if len(builds) == 0 || row[0] != last {
			builds = append(builds, map[string]VersionKey{})
			last = row[0]
		}

		builds[len(builds)-1][row[1]] = VersionKey(row[2])
	}

	return builds
}
