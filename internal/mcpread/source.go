// Package mcpread is the daemon's read-only MCP server: the tools an agent uses to read what its person could read in the browser — pipelines, runs, a step's output, what is waiting on somebody — and nothing that acts.
//
// It owns the shape of the answers (paging, cursors, the size cap, scrubbing, framing free text as data) and none of the access: every question goes to a Source, which internal/web implements as the person the request carries. A pipeline the Source will not show is ErrNotFound, the same answer as a name nobody set, so a tool cannot tell an agent that a hidden pipeline exists.
package mcpread

import (
	"context"
	"errors"
)

// ErrNotFound is anything the caller may not see, or that is not there: a pipeline, a run, a step, a revision, a resource. The two are deliberately one answer.
var ErrNotFound = errors.New("not found")

// Source answers the tools' questions as one person. Every method sees only what that person may, so the tools need no access logic of their own.
type Source interface {
	// Pipelines is every pipeline the person may see.
	Pipelines(ctx context.Context) ([]Pipeline, error)
	Pipeline(ctx context.Context, name string) (PipelineDetail, error)
	// Config is the revision named by sha, or the one run ran under, or the one served now when both are empty.
	Config(ctx context.Context, pipeline, sha, run string) (Config, error)
	// Runs is a page of runs newest first, older than q.Before when it names one.
	Runs(ctx context.Context, pipeline string, q RunQuery) ([]Run, error)
	Run(ctx context.Context, pipeline, id string) (RunDetail, error)
	// StepOutput is everything one step printed, whole, in the order it printed it.
	StepOutput(ctx context.Context, pipeline, run string, step int64) (string, error)
	Search(ctx context.Context, pipeline, text string) ([]Hit, error)
	// Waiting is the approvals and questions still waiting on a person.
	Waiting(ctx context.Context, pipeline string) ([]Waiting, error)
	// Cost is one run's spend by step, or with run empty the newest runs' totals.
	Cost(ctx context.Context, pipeline, run string) (Cost, error)
	Resources(ctx context.Context, pipeline string) ([]Resource, error)
	// Versions is a resource's recorded versions, newest first.
	Versions(ctx context.Context, pipeline, resource string) ([]string, error)
	// Record notes that tool read these pipelines. It names the tool and the pipelines and nothing the agent sent: arguments are the agent's, and an activity log is read by people who were not asked.
	Record(ctx context.Context, tool string, pipelines []string)
}

// RunQuery is a page of runs.
type RunQuery struct {
	Job    string
	Before string
	Limit  int
}

type sourceKey struct{}

// With is ctx carrying the Source the tools answer from. internal/web puts one on every request after deciding who it is from.
func With(ctx context.Context, source Source) context.Context {
	return context.WithValue(ctx, sourceKey{}, source)
}

func sourceFrom(ctx context.Context) (Source, bool) {
	source, ok := ctx.Value(sourceKey{}).(Source)

	return source, ok
}

// Pipeline is one line of list_pipelines.
type Pipeline struct {
	Name   string `json:"name"`
	Paused bool   `json:"paused"`
	Jobs   int    `json:"jobs"`
}

// PipelineDetail is get_pipeline's answer.
type PipelineDetail struct {
	Name   string `json:"name"`
	Paused bool   `json:"paused"`
	// CurrentSHA is the revision served now, which get_config reads with no sha.
	CurrentSHA string `json:"current_sha,omitempty"`
	Jobs       []Job  `json:"jobs"`
}

// Job is a job and how its newest run went.
type Job struct {
	Name string `json:"name"`
	// Held is a job its breaker stopped after failing in a row.
	Held   bool `json:"held"`
	Latest *Run `json:"latest,omitempty"`
}

// Run is one run, without its steps.
type Run struct {
	ID         string `json:"id"`
	Job        string `json:"job"`
	Status     string `json:"status"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	CreatedBy  string `json:"created_by,omitempty"`
	RerunOf    string `json:"rerun_of,omitempty"`
	ConfigSHA  string `json:"config_sha,omitempty"`
}

// RunDetail is a run and its steps.
type RunDetail struct {
	Run
	// FailedStep is the innermost step that failed first, the one to read the output of.
	FailedStep *Step  `json:"failed_step,omitempty"`
	Steps      []Step `json:"steps"`
}

// Step is one step of a run.
type Step struct {
	ID         int64  `json:"id"`
	ParentID   int64  `json:"parent_id,omitempty"`
	Name       string `json:"name"`
	Kind       string `json:"kind,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	Reason     string `json:"reason,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	// Printed is whether the step printed anything get_step_output can read.
	Printed bool `json:"printed"`
}

// Config is a pipeline revision: its YAML and the files it included.
type Config struct {
	SHA      string
	Source   string
	Includes map[string]string
}

// Hit is one search result.
type Hit struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	RunID  string `json:"run_id,omitempty"`
	Status string `json:"status,omitempty"`
}

// Waiting is an approval or a question nobody has answered.
type Waiting struct {
	Pipeline string   `json:"pipeline"`
	Kind     string   `json:"kind"`
	ID       int64    `json:"id"`
	RunID    string   `json:"run_id,omitempty"`
	Job      string   `json:"job"`
	Agent    string   `json:"agent,omitempty"`
	Message  string   `json:"message"`
	Options  []string `json:"options,omitempty"`
	Since    string   `json:"since"`
}

// Cost is what agent steps spent: per step for one run, per run otherwise.
type Cost struct {
	Run   string     `json:"run,omitempty"`
	Steps []StepCost `json:"steps,omitempty"`
	Runs  []RunCost  `json:"runs,omitempty"`
}

// StepCost is one agent request's spend.
type StepCost struct {
	Step       string   `json:"step"`
	Model      string   `json:"model,omitempty"`
	Tokens     int      `json:"tokens"`
	Cached     int      `json:"cached"`
	CostUSD    *float64 `json:"cost_usd,omitempty"`
	DurationMS int64    `json:"duration_ms,omitempty"`
}

// RunCost is one run's total spend.
type RunCost struct {
	Run     string   `json:"run"`
	Tokens  int      `json:"tokens"`
	Cached  int      `json:"cached"`
	CostUSD *float64 `json:"cost_usd,omitempty"`
	// Unpriced counts requests whose model had no price, so CostUSD is a floor.
	Unpriced int `json:"unpriced,omitempty"`
}

// Resource is a resource and what its checks last found.
type Resource struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	LastChecked string `json:"last_checked,omitempty"`
	Version     string `json:"version,omitempty"`
	CheckError  string `json:"check_error,omitempty"`
}
