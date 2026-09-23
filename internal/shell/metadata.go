package shell

import (
	"context"
	"slices"
	"strings"
)

// BuildMetadata is which build a command is part of, handed to every command
// as STEPS_RUN_ID, STEPS_JOB_NAME, STEPS_PIPELINE_NAME, STEPS_PIPELINE_REVISION
// and STEPS_URL — what Concourse calls build metadata.
//
// Never hashed: a run id changes every run, so a step keyed on it would never
// hit. It travels on the context, never in a config field, so no merkle builder
// can see it. config refuses these names in env: (see its reservedEnvNames).
type BuildMetadata struct {
	RunID, JobName, PipelineName, PipelineRevision, URL string
}

type buildMetadataKey struct{}

// WithBuildMetadata carries m to every runner command made under the context. A zero m strips any inherited metadata.
func WithBuildMetadata(ctx context.Context, m BuildMetadata) context.Context {
	return context.WithValue(ctx, buildMetadataKey{}, m)
}

// buildEnvNames are the variables BuildMetadata sets, in the order of its fields.
//
//nolint:gochecknoglobals // static, read-only
var buildEnvNames = []string{"STEPS_RUN_ID", "STEPS_JOB_NAME", "STEPS_PIPELINE_NAME", "STEPS_PIPELINE_REVISION", "STEPS_URL"}

// BuildEnvNames lists the variables build metadata sets.
func BuildEnvNames() []string {
	return slices.Clone(buildEnvNames)
}

// BuildEnv is the metadata the context carries as name→value, or nil when it
// carries none. An empty field is unset rather than empty, the STEPS_WORKER
// convention, and a fresh map is returned because callers merge into it.
func BuildEnv(ctx context.Context) map[string]string {
	m, _ := ctx.Value(buildMetadataKey{}).(BuildMetadata)

	values := []string{m.RunID, m.JobName, m.PipelineName, m.PipelineRevision, m.URL}

	var env map[string]string

	for i, value := range values {
		// ponytail: a NUL makes exec fail with EINVAL for every command in the job, so drop the one variable; a load-time job-name rule is the real fix.
		if value == "" || strings.ContainsRune(value, 0) {
			continue
		}

		if env == nil {
			env = map[string]string{}
		}

		env[buildEnvNames[i]] = value
	}

	return env
}

// buildEnvPairs is BuildEnv as sorted NAME=value pairs.
func buildEnvPairs(ctx context.Context) []string {
	env := BuildEnv(ctx)
	pairs := make([]string, 0, len(env))

	for name, value := range env {
		pairs = append(pairs, name+"="+value)
	}

	slices.Sort(pairs)

	return pairs
}
