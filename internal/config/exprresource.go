package config

// The expr: resource backend's config shape and the rules that keep it
// distinguishable from the other two.

import (
	"fmt"
	"strings"
)

// ExprResourceConfig backs a resource type's three lifecycle stages with
// expressions instead of shell commands — the form for a resource that is a
// JSON HTTP API and nothing else.
//
// The rule for choosing, narrow on purpose:
//
//	Reach for expr: when the resource is a JSON HTTP API and nothing else.
//	Reach for check:/in:/out: for everything else.
//
// Shell stays the only option — not merely the traditional one — whenever a
// real tool does the work (git, docker, aws, kubectl), whenever the artifact
// is binary or big enough to want streaming to disk rather than holding as a
// string, and whenever the command should run in a container: image:,
// network:, privileged: and container_limits: are shell-only, because an
// expression evaluates in-process and has no container to configure.
//
// Each slot has a _file sibling, following run_file:/system_file:/prompt_file:
// A twenty-line program has no business inside a YAML scalar, and a real file
// is reviewable: a diff reads as a diff and a comment lands on a line.
// Suggested extension .expr.
type ExprResourceConfig struct {
	Check     string `yaml:"check,omitempty"`
	CheckFile string `yaml:"check_file,omitempty"`
	In        string `yaml:"in,omitempty"`
	InFile    string `yaml:"in_file,omitempty"`
	Out       string `yaml:"out,omitempty"`
	OutFile   string `yaml:"out_file,omitempty"`
}

// validateExprResourceConfig checks one resource type's expr: block: it must
// implement something, and it must not ask for a container it cannot have.
//
// What it deliberately does NOT check is whether the expressions COMPILE.
// This package depends on nothing internal and on no third-party code but the
// YAML parser (enforced by depguard's Config rule), which is what lets every
// other package agree on these types without inheriting an engine. So a
// syntax error is a validate-time fact rather than a load-time one — see
// resource.CompileExprPrograms, wired into `steps validate` and preflight.
func validateExprResourceConfig(rt ResourceType) error {
	expression := rt.Config.Expr

	if expression.Check == "" && expression.In == "" && expression.Out == "" {
		return fmt.Errorf(
			"resource_type %q: expr: sets none of check/in/out, so it can neither detect, fetch, nor publish anything", rt.Name)
	}

	return validateExprNoContainer(rt)
}

// validateExprNoContainer rejects the container settings an expression cannot
// honor.
//
// An expr type runs in this process: there is no container to put on a
// network, no process to run as another user, and nothing to cap. Accepting
// image: here and ignoring it would be the worst outcome — a pipeline that
// reads as isolated and is not. What replaces them is per-call: http() takes
// its own timeout and max_response_bytes.
func validateExprNoContainer(rt ResourceType) error {
	set := []string{}

	if rt.Image != "" {
		set = append(set, "image:")
	}

	if rt.Network != "" {
		set = append(set, "network:")
	}

	if rt.User != "" {
		set = append(set, "user:")
	}

	if rt.Privileged {
		set = append(set, "privileged:")
	}

	if rt.Limits != nil {
		set = append(set, "container_limits:")
	}

	if len(set) == 0 {
		return nil
	}

	return fmt.Errorf(
		"resource_type %q: expr: evaluates in-process, so %s cannot apply; drop them, or write this type with check:/in:/out: so it can run in a container",
		rt.Name, strings.Join(set, "/"))
}
