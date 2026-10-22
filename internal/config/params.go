package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// ParamSpec limits what one argument of a tool may be. The limits are
// published in the tool's input schema, so a model sees them and corrects
// itself before calling, and are enforced before the call is made, so a
// caller that ignores the schema is refused all the same.
type ParamSpec struct {
	// MaxLength and MinLength count characters, not bytes. String parameters only.
	MaxLength *int `yaml:"max_length,omitempty"`
	MinLength *int `yaml:"min_length,omitempty"`
	// Pattern is a Go (RE2) regular expression the value must match somewhere; anchor it with ^ and $ to match all of it. String parameters only.
	Pattern string `yaml:"pattern,omitempty"`
	// Enum lists every value the parameter may take.
	Enum []string `yaml:"enum,omitempty"`
	// Min and Max bound a number. Number parameters only.
	Min *float64 `yaml:"min,omitempty"`
	Max *float64 `yaml:"max,omitempty"`
	// Default fills the argument when it is missing, which also makes it optional.
	Default *string `yaml:"default,omitempty"`
}

// StringLimits reports whether p sets a limit only a string has.
func (p ParamSpec) StringLimits() bool {
	return p.MaxLength != nil || p.MinLength != nil || p.Pattern != ""
}

// NumberLimits reports whether p sets a limit only a number has.
func (p ParamSpec) NumberLimits() bool {
	return p.Min != nil || p.Max != nil
}

// patternCache holds compiled ParamSpec patterns; a package-level cache because ParamSpec is passed by value and an unexported field would trip the reflection tests over config structs. Bounded by the patterns a loaded pipeline declares.
var patternCache sync.Map //nolint:gochecknoglobals // process-wide memo of a pure function

func compiledPattern(pattern string) (*regexp.Regexp, error) {
	if cached, ok := patternCache.Load(pattern); ok {
		return cached.(*regexp.Regexp), nil //nolint:forcetypeassert // only *regexp.Regexp is ever stored
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile pattern: %w", err)
	}

	patternCache.Store(pattern, re)

	return re, nil
}

// CheckString is why value is not an acceptable string for p, or "".
func (p ParamSpec) CheckString(value string) string {
	length := utf8.RuneCountInString(value)

	if p.MaxLength != nil && length > *p.MaxLength {
		return fmt.Sprintf("must be at most %d characters (it was %d)", *p.MaxLength, length)
	}

	if p.MinLength != nil && length < *p.MinLength {
		return fmt.Sprintf("must be at least %d characters (it was %d)", *p.MinLength, length)
	}

	if p.Pattern != "" {
		re, err := compiledPattern(p.Pattern)
		if err != nil || !re.MatchString(value) {
			return "must match the pattern " + p.Pattern
		}
	}

	if len(p.Enum) > 0 && !slices.Contains(p.Enum, value) {
		return "must be one of: " + joinQuoted(p.Enum)
	}

	return ""
}

// CheckNumber is why value is not an acceptable number for p, or "".
func (p ParamSpec) CheckNumber(value float64) string {
	if p.Min != nil && value < *p.Min {
		return "must be at least " + formatFloat(*p.Min)
	}

	if p.Max != nil && value > *p.Max {
		return "must be at most " + formatFloat(*p.Max)
	}

	if len(p.Enum) > 0 && !slices.ContainsFunc(p.Enum, func(entry string) bool {
		n, err := strconv.ParseFloat(entry, 64)

		return err == nil && n == value
	}) {
		return "must be one of: " + joinQuoted(p.Enum)
	}

	return ""
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func joinQuoted(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = strconv.Quote(v)
	}

	return strings.Join(quoted, ", ")
}

// validateToolParamsShape enforces where params: may appear and that each
// limit is coherent. What it cannot know — whether a parameter is a number,
// which only an MCP server's schema says — is checked where the schema is
// read (internal/agent).
//
// Like timeout:, it binds where the tool is granted: a step's tools: list
// selects a granted tool by name, and the selection is dropped for the
// agent's own spec, so a limit written there would silently not apply.
func validateToolParamsShape(context string, pos toolPosition, spec ToolSpec) error {
	if len(spec.Params) == 0 {
		return nil
	}

	name := ToolSpecName(spec)

	err := validateParamsForm(context, name, pos, spec)
	if err != nil {
		return err
	}

	names := make([]string, 0, len(spec.Params))
	for param := range spec.Params {
		names = append(names, param)
	}

	sort.Strings(names)

	for _, param := range names {
		err = validateParam(spec, param, spec.Params[param])
		if err != nil {
			return fmt.Errorf("%s: tool %q: params %q: %w", context, name, param, err)
		}
	}

	return nil
}

func validateParamsForm(context, name string, pos toolPosition, spec ToolSpec) error {
	inlineCustom := spec.Name != "" && spec.Run != ""

	switch {
	case spec.Builtin != "":
		return fmt.Errorf("%s: builtin tool %q: params: is not valid on a built-in", context, name)
	case spec.MCP != "" && spec.MCPTool == "":
		return fmt.Errorf("%s: tool %q: params: limits one tool's arguments, so it needs the single-tool form {mcp, tool}", context, name)
	case pos != grantPosition && !inlineCustom:
		return fmt.Errorf("%s: tool %q: params: binds only where the tool is granted — move it to the agents: entry's tools:, and select it here by bare name", context, name)
	}

	return nil
}

func validateParam(spec ToolSpec, param string, limits ParamSpec) error {
	err := validateParamTarget(spec, param)
	if err != nil {
		return err
	}

	err = validateParamLimits(limits)
	if err != nil {
		return err
	}

	// A custom tool's arguments all reach a shell as text, and a sub-agent's one argument is a request: neither is a number.
	if (spec.Run != "" || spec.Agent != "") && limits.NumberLimits() {
		return fmt.Errorf("min/max bound a number, and %q is a string", param)
	}

	if limits.Default != nil {
		return validateStringDefault(spec, limits)
	}

	return nil
}

func validateParamTarget(spec ToolSpec, param string) error {
	if _, pinned := spec.Args[param]; pinned {
		return errors.New("it is pinned by args:, so the caller cannot vary it")
	}

	switch {
	case spec.Agent != "":
		if param != SubAgentRequestParam {
			return fmt.Errorf("a sub-agent tool takes one argument, %q", SubAgentRequestParam)
		}
	case spec.Run != "":
		if !slices.Contains(InferToolParams(spec.Run), param) {
			return fmt.Errorf("the run: template never reads {{ .args.%s }}", param)
		}
	}

	return nil
}

func validateParamLimits(limits ParamSpec) error {
	if limits.StringLimits() && limits.NumberLimits() {
		return errors.New("a parameter is a string or a number: max_length/min_length/pattern and min/max cannot both be set")
	}

	err := validateParamBounds(limits)
	if err != nil {
		return err
	}

	if limits.Pattern != "" {
		_, err = regexp.Compile(limits.Pattern)
		if err != nil {
			return fmt.Errorf("pattern does not compile: %w", err)
		}
	}

	if limits.Enum != nil && len(limits.Enum) == 0 {
		return errors.New("enum is empty, so nothing could pass")
	}

	return nil
}

func validateParamBounds(limits ParamSpec) error {
	for _, n := range []*int{limits.MaxLength, limits.MinLength} {
		if n != nil && *n < 0 {
			return errors.New("max_length and min_length must be >= 0")
		}
	}

	if limits.MaxLength != nil && limits.MinLength != nil && *limits.MinLength > *limits.MaxLength {
		return fmt.Errorf("min_length %d is above max_length %d, so nothing could pass", *limits.MinLength, *limits.MaxLength)
	}

	if limits.Min != nil && limits.Max != nil && *limits.Min > *limits.Max {
		return fmt.Errorf("min %s is above max %s, so nothing could pass", formatFloat(*limits.Min), formatFloat(*limits.Max))
	}

	return nil
}

func validateStringDefault(spec ToolSpec, limits ParamSpec) error {
	// An MCP parameter may be a number; its default is parsed against the server's schema where that is read.
	if spec.MCP != "" {
		return nil
	}

	if why := limits.CheckString(*limits.Default); why != "" {
		return fmt.Errorf("the default %s", why)
	}

	return nil
}

// SubAgentRequestParam is the one argument a sub-agent tool takes.
const SubAgentRequestParam = "request"

// agentToolArgPattern matches a {{ .args.NAME }} reference and everything up to the closing "}}", so it also matches the documented safe idiom {{ .args.NAME | shellquote }}.
//
//nolint:gochecknoglobals // compiled once, read-only
var agentToolArgPattern = regexp.MustCompile(`\{\{-?\s*\.args\.([A-Za-z_]\w*)[^}]*\}\}`)

// InferToolParams is each distinct NAME a custom tool's run template reads as {{ .args.NAME }}, in first-seen order.
func InferToolParams(run string) []string {
	matches := agentToolArgPattern.FindAllStringSubmatch(run, -1)

	seen := make(map[string]bool, len(matches))
	params := make([]string, 0, len(matches))

	for _, m := range matches {
		if seen[m[1]] {
			continue
		}

		seen[m[1]] = true

		params = append(params, m[1])
	}

	return params
}
