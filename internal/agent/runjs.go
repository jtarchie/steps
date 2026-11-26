package agent

// ponytail: a run_js script runs in this process with no memory bound (goja has none), held only by its timeout and call-stack cap; fence: lists it for that reason. The upgrade is a child process with an rlimit and the tools proxied over a pipe.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/dop251/goja"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
)

// runJSDefaultTimeout bounds a script whose grant set no timeout:. Unlike every other tool, a script can spin without calling anything, so "no deadline unless asked" is not a default it can have.
const runJSDefaultTimeout = "60s"

// runJSMaxCallStack ends a runaway recursion as a RangeError the script can read, long before it is a memory problem.
const runJSMaxCallStack = 1024

const runJSCodeArg = "code"

const runJSDescription = "Run a JavaScript script that calls your other tools as functions, and get back only what the script returns." +
	" Use it when a job takes many tool calls whose intermediate results you do not need to read yourself: loop, filter and combine them in one script instead of one call per turn." +
	" Each tool this description lists as callable is a global function of exactly that name, taking ONE object of its arguments and returning its result object," +
	` e.g. const r = read_file({path: "go.mod"}); a tool whose name is not a JavaScript identifier is tools["its-name"]({...}).` +
	" Calls are synchronous: do not await them. There is no require/import, no timers, and no filesystem or network except through those functions." +
	" A tool that fails does not throw: its result carries an error field or a nonzero exit_code, exactly as you would see it, so check for it." +
	" Each call counts against that tool's own call budget as if you had made it directly." +
	" The value of the script's LAST expression comes back as result (it must be JSON-serializable), and anything passed to console.log as logs." +
	" The script is stopped when its time limit runs out."

// describeScriptTools finishes run_js's description with what this agent's script can call and how long it has. The names are spelled out because a CLI-backed model sees its tools under a prefix (mcp__steps__read_file) that no script global carries.
func describeScriptTools(decls []*genai.FunctionDeclaration, registry map[string]toolImpl, specs []config.ToolSpec) {
	for _, decl := range decls {
		if decl.Name != config.RunJSBuiltinName {
			continue
		}

		callable := "No other tool is granted here, so a script can call nothing."
		if names := scriptableTools(registry); len(names) > 0 {
			callable = "Callable from a script here: " + strings.Join(names, ", ") + "." +
				" Use those names exactly, without any prefix such as mcp__steps__ your own tool list may show; nothing else is callable, not run_js itself, ask_user or verdict."
		}

		limit := runJSDefaultTimeout

		for _, spec := range specs {
			if spec.Builtin == config.RunJSBuiltinName {
				limit = declaredTimeout(spec)
			}
		}

		decl.Description += " " + callable + " Time limit: " + limit + "."
	}
}

// scriptTools is how a script reaches the step's other tools. Whichever loop owns the step's budgets and its record sets it (the hosted conversation, or the CLI bridge), because a script's call must be admitted, counted and recorded where a direct call is. Zero outside a conversation, where a script can call nothing.
type scriptTools struct {
	names []string
	call  func(ctx context.Context, env toolEnv, name string, args map[string]any) map[string]any
}

// scriptableTools is every tool in registry a script may call: all of them but the ones that end or park the conversation, and run_js itself.
func scriptableTools(registry map[string]toolImpl) []string {
	names := make([]string, 0, len(registry))

	for name := range registry {
		switch name {
		case config.RunJSBuiltinName, config.AskUserBuiltinName, verdictToolName:
		default:
			names = append(names, name)
		}
	}

	slices.Sort(names)

	return names
}

func runJSTool() builtinTool {
	return builtinTool{
		decl: &genai.FunctionDeclaration{
			Name:        config.RunJSBuiltinName,
			Description: toolDescription(config.RunJSBuiltinName, runJSDescription),
			Parameters: objectSchema(map[string]*genai.Schema{
				runJSCodeArg: {Type: genai.TypeString, Description: "The script. Its last expression is the result."},
			}, runJSCodeArg),
		},
		impl: execRunJS,
	}
}

func execRunJS(ctx context.Context, args map[string]any, env toolEnv) map[string]any {
	code := stringArg(args, runJSCodeArg)
	if strings.TrimSpace(code) == "" {
		return errorResult("run_js: code is required")
	}

	vm := goja.New()
	vm.SetMaxCallStackSize(runJSMaxCallStack)

	// A script that never calls a tool never reads ctx, so the deadline and an abort both reach it as an interrupt; one blocked inside a tool is ended by that tool's own ctx.
	stop := context.AfterFunc(ctx, func() { vm.Interrupt(context.Cause(ctx)) })
	defer stop()

	script := jsScript{vm: vm, limit: maxToolOutputBytes}

	err := script.install(ctx, env)
	if err != nil {
		return errorResult("run_js: " + err.Error())
	}

	value, err := vm.RunString(code)
	if err != nil {
		return script.failed(err)
	}

	return script.finished(ctx, env, value)
}

// jsScript is one script's runtime and what it has printed.
type jsScript struct {
	vm        *goja.Runtime
	parse     goja.Callable
	stringify goja.Callable
	logs      strings.Builder
	limit     int
}

// install defines the script's globals: one function per callable tool, the tools object naming them all, and console.
func (s *jsScript) install(ctx context.Context, env toolEnv) error {
	jsonObject := s.vm.Get("JSON").ToObject(s.vm)

	var ok bool

	s.parse, ok = goja.AssertFunction(jsonObject.Get("parse"))
	if !ok {
		return errors.New("JSON.parse is missing from the runtime")
	}

	s.stringify, ok = goja.AssertFunction(jsonObject.Get("stringify"))
	if !ok {
		return errors.New("JSON.stringify is missing from the runtime")
	}

	tools := s.vm.NewObject()
	global := s.vm.GlobalObject()

	for _, name := range env.script.names {
		fn := s.vm.ToValue(s.toolFunc(ctx, env, name))

		err := tools.Set(name, fn)
		if err != nil {
			return fmt.Errorf("defining %s: %w", name, err)
		}

		// A tool never shadows what the language already defines under its name.
		if global.Get(name) != nil {
			continue
		}

		err = global.Set(name, fn)
		if err != nil {
			return fmt.Errorf("defining %s: %w", name, err)
		}
	}

	console := s.vm.NewObject()
	for _, level := range []string{"log", "info", "warn", "error", "debug"} {
		err := console.Set(level, s.log)
		if err != nil {
			return fmt.Errorf("defining console.%s: %w", level, err)
		}
	}

	return errors.Join(global.Set("tools", tools), global.Set("console", console))
}

// toolFunc is the function a script calls name through. Arguments and result cross as JSON, so the tool sees what a model's own call would have sent (numbers as float64) and the script gets real arrays and objects rather than wrapped Go values.
func (s *jsScript) toolFunc(ctx context.Context, env toolEnv, name string) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		args := map[string]any{}

		if arg := call.Argument(0); !goja.IsUndefined(arg) && !goja.IsNull(arg) {
			text, err := s.stringify(goja.Undefined(), arg)
			if err != nil {
				panic(err)
			}

			// A nil map is what "null" decodes to, which is how NaN and a toJSON returning null serialize.
			if json.Unmarshal([]byte(text.String()), &args) != nil || args == nil {
				panic(s.vm.NewTypeError(name + "(args): args must be one object naming the tool's arguments"))
			}
		}

		// A script already told to stop gets no further side effects.
		if ctx.Err() != nil {
			panic(s.vm.NewGoError(context.Cause(ctx)))
		}

		encoded, err := json.Marshal(env.script.call(ctx, env, name, args))
		if err != nil {
			panic(s.vm.NewGoError(fmt.Errorf("%s: result is not JSON: %w", name, err)))
		}

		result, err := s.parse(goja.Undefined(), s.vm.ToValue(string(encoded)))
		if err != nil {
			panic(err)
		}

		return result
	}
}

// log appends one console line, up to the output limit; what a script prints past it is dropped rather than held.
func (s *jsScript) log(call goja.FunctionCall) goja.Value {
	if s.logs.Len() >= s.limit {
		return goja.Undefined()
	}

	for i, arg := range call.Arguments {
		if i > 0 {
			s.logs.WriteByte(' ')
		}

		s.logs.WriteString(s.render(arg))
	}

	s.logs.WriteByte('\n')

	return goja.Undefined()
}

// render is a value as console.log shows it: a string as itself, anything else as JSON.
func (s *jsScript) render(value goja.Value) string {
	if _, isString := value.Export().(string); isString {
		return value.String()
	}

	text, err := s.stringify(goja.Undefined(), value)
	if err != nil || goja.IsUndefined(text) {
		return value.String()
	}

	return text.String()
}

func (s *jsScript) failed(err error) map[string]any {
	result := errorResult("run_js: " + truncateToolOutputLimit(shown(err.Error), s.limit))
	s.addLogs(result)

	return result
}

// shown is render's text. What a script threw is shown by calling its toString, which is the script's own code running again with no frame of the script's left to catch a throw: one that escapes, or runs into the deadline, is reported in the value's place rather than as this process's panic.
func shown(render func() string) (text string) {
	defer func() {
		if recover() != nil {
			text = "a value whose toString failed"
		}
	}()

	return render()
}

// settled is the value a script ended on, with an async function's promise read for what it settled to: every tool call is synchronous, so a script written with await has already finished by the time it returns.
func settled(value goja.Value) (goja.Value, error) {
	// Asked by type first: exporting any other object reads every property, and a getter that throws out here is a panic.
	object, isObject := value.(*goja.Object)
	if !isObject || object.ExportType() != reflect.TypeFor[*goja.Promise]() {
		return value, nil
	}

	promise, isPromise := object.Export().(*goja.Promise)
	if !isPromise {
		return value, nil
	}

	switch promise.State() {
	case goja.PromiseStateRejected:
		return nil, fmt.Errorf("the script's promise was rejected: %s", shown(promise.Result().String))
	case goja.PromiseStatePending:
		return nil, errors.New("the script returned a promise that never settles; tool functions are synchronous, so return the value itself")
	case goja.PromiseStateFulfilled:
	}

	return promise.Result(), nil
}

// finished turns the script's last value into the tool result.
func (s *jsScript) finished(ctx context.Context, env toolEnv, value goja.Value) map[string]any {
	value, err := settled(value)
	if err != nil {
		return s.failed(err)
	}

	result := map[string]any{}
	s.addLogs(result)

	text, err := s.stringify(goja.Undefined(), value)
	if err != nil {
		return s.failed(fmt.Errorf("the result is not JSON-serializable: %w", err))
	}

	// undefined, or a function: the script ended on a statement with no value.
	if goja.IsUndefined(text) {
		result["result"] = nil

		return result
	}

	encoded := text.String()
	if len(encoded) > s.limit {
		result["result"] = spillOrTruncateLimit(ctx, encoded, s.limit, env.spillDir)
		result["truncated"] = true

		return result
	}

	var decoded any

	err = json.Unmarshal([]byte(encoded), &decoded)
	if err != nil {
		return s.failed(fmt.Errorf("the result is not JSON: %w", err))
	}

	result["result"] = decoded

	return result
}

func (s *jsScript) addLogs(result map[string]any) {
	if s.logs.Len() > 0 {
		result["logs"] = truncateToolOutputLimit(s.logs.String(), s.limit)
	}
}
