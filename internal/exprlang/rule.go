package exprlang

import (
	"errors"
	"fmt"

	"github.com/expr-lang/expr"
)

// A rule's when: and accept: are expressions over one call to a shared tool.
// They are pure — no env(), http() or file() — so a rule can only decide, never
// reach anything: compiling it once when the pipeline is set is safe, and
// running it on every call costs microseconds. fail("why") is the one
// function, a way to refuse with a reason the caller will read.

// RuleWho is the person calling, as a rule sees them. Zero for a call that is not a person's: an agent step's own tool.
type RuleWho struct {
	Provider      string
	UserID        string
	ID            string
	Email         string
	Organizations []string
	Groups        []string
}

// RuleResult is what a call returned, as the caller is about to see it.
type RuleResult struct {
	// Text is exactly the text the caller will receive.
	Text string `expr:"text"`
	// Content is the same result as an object: stdout, stderr and exit_code for a command, content for an upstream's.
	Content map[string]any `expr:"content"`
	// Error is the call's own error, empty when it succeeded.
	Error string `expr:"error"`
}

// WhenEnv is what a when: rule reads, before the call goes out.
type WhenEnv struct {
	Tool string         `expr:"tool"`
	Args map[string]any `expr:"args"`
	Who  RuleWho        `expr:"who"`
}

// AcceptEnv is what an accept: rule reads, after the call and before the caller sees what came back.
type AcceptEnv struct {
	Tool   string         `expr:"tool"`
	Args   map[string]any `expr:"args"`
	Who    RuleWho        `expr:"who"`
	Result RuleResult     `expr:"result"`
}

// RuleOutcome is a rule's decision.
type RuleOutcome struct {
	Allowed bool
	// Message is the reason a refusal gave through fail(); empty for a plain false.
	Message string
}

type refusal struct{ message string }

func (r *refusal) Error() string { return r.message }

func failRule(params ...any) (any, error) {
	if len(params) != 1 {
		return nil, fmt.Errorf("fail() takes one message, got %d arguments", len(params))
	}

	return nil, &refusal{message: scalarString(params[0])}
}

// CompileWhen compiles a rule's when: expression, which must answer true or false.
func CompileWhen(src string) (func(WhenEnv) (RuleOutcome, error), error) {
	return compileRule(src, WhenEnv{})
}

// CompileAccept compiles a rule's accept: expression, which must answer true or false.
func CompileAccept(src string) (func(AcceptEnv) (RuleOutcome, error), error) {
	return compileRule(src, AcceptEnv{})
}

func compileRule[E any](src string, env E) (func(E) (RuleOutcome, error), error) {
	program, err := expr.Compile(src, expr.Env(env), expr.Function("fail", failRule), expr.AsBool())
	if err != nil {
		return nil, fmt.Errorf("expr: %w", err)
	}

	return func(in E) (RuleOutcome, error) {
		result, err := expr.Run(program, in)

		var refused *refusal
		if errors.As(err, &refused) {
			return RuleOutcome{Message: refused.message}, nil
		}

		if err != nil {
			return RuleOutcome{}, fmt.Errorf("expr: %w", err)
		}

		allowed, ok := result.(bool)
		if !ok {
			return RuleOutcome{}, fmt.Errorf("expr: the rule answered %T, want true or false", result)
		}

		return RuleOutcome{Allowed: allowed}, nil
	}, nil
}
