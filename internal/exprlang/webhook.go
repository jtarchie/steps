package exprlang

import (
	"fmt"

	"github.com/expr-lang/expr"
)

// WebhookEnv is what a webhook expression reads. A struct rather than a map so payload is typed any: a JSON body can be an object, an array, or absent.
type WebhookEnv struct {
	Provider string            `expr:"provider"`
	Event    string            `expr:"event"`
	Method   string            `expr:"method"`
	Headers  map[string]string `expr:"headers"`
	Query    map[string]string `expr:"query"`
	Body     string            `expr:"body"`
	Payload  any               `expr:"payload"`
}

// Webhook compiles an expression over one webhook delivery into a plain func; it is pure (no env(), http(), file(), fail() or clock), so compiling once when the pipeline is set is safe, and wantBool makes a non-boolean filter: a compile error.
func Webhook(src string, wantBool bool) (func(env WebhookEnv) (any, error), error) {
	options := []expr.Option{
		expr.Env(WebhookEnv{}),
		expr.DisableBuiltin("now"),
		expr.DisableBuiltin("date"),
		expr.DisableBuiltin("duration"),
		expr.DisableBuiltin("timezone"),
	}

	if wantBool {
		options = append(options, expr.AsBool())
	}

	program, err := expr.Compile(src, options...)
	if err != nil {
		return nil, fmt.Errorf("expr: %w", err)
	}

	return func(env WebhookEnv) (any, error) {
		result, err := expr.Run(program, env)
		if err != nil {
			return nil, fmt.Errorf("expr: %w", err)
		}

		return result, nil
	}, nil
}
