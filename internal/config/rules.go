package config

import (
	"errors"
	"fmt"
	"regexp"
)

// Rule is a named check on a call to a tool, written once and attached by name
// to an export, a tool of an export, or a tool an agent is granted. It can
// refuse a call before it goes out (When), refuse what comes back before the
// caller sees it (Accept), and hand the decision to one of the pipeline's own
// agents (JudgedBy). A refused call tells the caller why and is recorded under
// the rule's name.
type Rule struct {
	// Name is how an export or a tool refers to the rule, and what a refusal and the activity log call it.
	Name string `yaml:"name"`
	// When is an expression over the call — tool, args (after pins and defaults) and who — that must hold for the call to go out. fail("why") refuses with a reason; a plain false says "refused by rule <name>".
	When string `yaml:"when,omitempty"`
	// Accept is the same over what came back, with result (text, content, error) in scope as well, and must hold for the caller to see it.
	Accept string `yaml:"accept,omitempty"`
	// JudgedBy names an agents: entry that is shown the call and answers allow or deny. A judge that fails or answers neither refuses the call.
	JudgedBy string `yaml:"judged_by,omitempty"`
}

//nolint:gochecknoglobals // compiled once, read-only
var ruleName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$`)

// FindRule is the rule a tool or an export names.
func (c *Config) FindRule(name string) (*Rule, error) {
	return findNamed("rule", name, c.Rules, func(r Rule) string { return r.Name })
}

func (c *Config) validateRules() error {
	var errs []error

	seen := map[string]bool{}

	for _, rule := range c.Rules {
		err := c.validateRule(rule)
		if err != nil {
			errs = append(errs, fmt.Errorf("rules %q: %w", rule.Name, err))
		}

		if seen[rule.Name] {
			errs = append(errs, fmt.Errorf("rules %q is declared twice", rule.Name))
		}

		seen[rule.Name] = true
	}

	for _, export := range c.MCPExports {
		for _, name := range export.Rules {
			_, err := c.FindRule(name)
			if err != nil {
				errs = append(errs, fmt.Errorf("mcp_exports %q: rules: %w", export.Name, err))
			}
		}
	}

	return errors.Join(errs...)
}

func (c *Config) validateRule(rule Rule) error {
	if !ruleName.MatchString(rule.Name) {
		return errors.New("name: letters, digits, _ and -, starting with a letter or digit, at most 63")
	}

	if rule.When == "" && rule.Accept == "" && rule.JudgedBy == "" {
		return errors.New("it sets none of when:, accept: or judged_by:, so it checks nothing")
	}

	if rule.JudgedBy == "" {
		return nil
	}

	judge, err := c.FindAgent(rule.JudgedBy)
	if err != nil {
		return fmt.Errorf("judged_by: %w", err)
	}

	if agentUsesCLI(*judge) {
		return fmt.Errorf("judged_by: %q is a cli source (%s...), which runs its own tool loop and cannot answer inside a call; name a hosted agent", judge.Name, CLISourcePrefix)
	}

	err = c.checkJudgeIsNotJudged(judge)
	if err != nil {
		return err
	}

	return c.checkNoJudgeCycle(rule)
}

// checkNoJudgeCycle refuses a judge that could end up waiting on itself. An agent reaches the agents it grants as sub-agents and the judges of the rules on its tools; a judge that reaches itself would build a judge that builds a judge, without end.
func (c *Config) checkNoJudgeCycle(rule Rule) error {
	if c.judgeReaches(rule.JudgedBy, rule.JudgedBy, map[string]bool{}, true) {
		return fmt.Errorf("judged_by: %q can reach itself through the agents it grants or the judges of its tools' rules, so building it would never finish", rule.JudgedBy)
	}

	return nil
}

// judgeReaches is whether target is reachable from the agent name, not counting the start itself.
func (c *Config) judgeReaches(name, target string, visited map[string]bool, start bool) bool {
	if !start && name == target {
		return true
	}

	if visited[name] {
		return false
	}

	visited[name] = true

	agent, err := c.FindAgent(name)
	if err != nil {
		return false
	}

	for _, spec := range agent.Tools {
		for _, next := range c.reachedBy(spec) {
			if c.judgeReaches(next, target, visited, false) {
				return true
			}
		}
	}

	return false
}

// reachedBy is the agents one tool grant leads to: its sub-agent, and the judges of its rules.
func (c *Config) reachedBy(spec ToolSpec) []string {
	var next []string
	if spec.Agent != "" {
		next = append(next, spec.Agent)
	}

	for _, name := range spec.Rules {
		rule, err := c.FindRule(name)
		if err == nil && rule.JudgedBy != "" {
			next = append(next, rule.JudgedBy)
		}
	}

	return next
}

// checkJudgeIsNotJudged refuses a judge whose own tools are held to a judge. Every call the judge made would wait on another judge, and a cycle of them would wait forever.
func (c *Config) checkJudgeIsNotJudged(judge *Agent) error {
	for _, spec := range judge.Tools {
		for _, name := range spec.Rules {
			attached, err := c.FindRule(name)
			if err == nil && attached.JudgedBy != "" {
				return fmt.Errorf("judged_by: %q is itself held to rule %q, which is judged by %q; a judge answers, it is not judged", judge.Name, name, attached.JudgedBy)
			}
		}
	}

	return nil
}

// validateToolRulesShape checks a tool's rules: name rules that exist, where the tool is granted (a step's selection is replaced by the granted tool's own spec, so a rule written there would silently not bind).
func (c *Config) validateToolRulesShape(context string, pos toolPosition, spec ToolSpec) error {
	if len(spec.Rules) == 0 {
		return nil
	}

	for _, name := range spec.Rules {
		_, err := c.FindRule(name)
		if err != nil {
			return fmt.Errorf("%s: tool %q: rules: %w", context, ToolSpecName(spec), err)
		}
	}

	inlineCustom := spec.Name != "" && spec.Run != ""
	if pos != grantPosition && !inlineCustom {
		return fmt.Errorf("%s: tool %q: rules: binds only where the tool is granted — move it to the agents: entry's tools:, and select it here by bare name", context, ToolSpecName(spec))
	}

	return nil
}
