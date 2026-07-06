// Ambient type declarations for steps machine files (workflow.ts).
//
// Machines are TypeScript: `steps` transpiles them with esbuild before
// running them in goja, so the types are stripped at load — they exist
// purely for editor autocomplete and checking. Reference this file from the
// top of a machine to pull the flow combinators and helpers into scope:
//
//   /// <reference path="../../docs/src/global.d.ts" />
//   export default { ... } satisfies Machine;   // `satisfies` optional
//
// A machine is: state consts + one flow expression. Structure is data; any
// computed value is a function of ONE flat scope — destructure what you
// need: ({ article, critique }) => `...`. The parameter list doubles as the
// state's declared input contract, checked at load.

// The single flat argument every machine function receives. Every member is
// optional: destructuring `({ article, critique }) => ...` in an untyped
// state const must type-check, so `Scope` is an open bag (its index
// signature covers run inputs and state outputs by name). The engine-supplied
// keys below exist for autocomplete and hover; the runtime dry-run is what
// actually verifies a destructured name against the state's real scope.
interface Scope {
  /** Entry counts per state — bound loops with visits.draft < 3. */
  visits?: Record<string, number>;
  /** Cumulative run accounting. */
  run?: { transitions: number; tokens: number; cost: number };
  /** Attempt number within the current state. */
  attempt?: number;
  /** This state's validated output (flow guards only). */
  output?: any;
  /** The agent-declared event (flow guards only). */
  event?: string;
  /** forEach: item position. The item itself appears under its `as` name. */
  index?: number;
  total?: number;
  /** Tool guards: the model-authored arguments being judged. */
  args?: Record<string, any>;
  /** Tool guards: invocation counts per tool this state. */
  calls?: Record<string, number>;
  /** Tool guards: model calls this conversation turn. */
  turn?: number;
  /** Run inputs and state outputs, by name (and the forEach item). */
  [name: string]: any;
}

type Fn<T> = (scope: Scope) => T;

/** A schema fragment: "string", "enum(a, b)", "string[]", [{...shape}], or JSON schema. */
type SchemaFragment = string | SchemaFragment[] | { [key: string]: any };

/** One model-extracted slice of a scope value (rung 1.5 — see docs/distill.md). */
interface Distill {
  /** What this state needs from the source. A string or a function of the
   *  state's (pre-distill) scope — per-item for forEach consumers. Required. */
  for: string | Fn<string>;
  /** Source scope key: a run input or a predecessor state's output.
   *  Default: this entry's key — shadowing it (inside the state, the key IS
   *  the slice). */
  from?: string;
  /** Output budget of the slice (the implicit state's maxOutputTokens). Default 512.
   *  Doubles as the pass-through threshold: a source that already fits this
   *  budget crosses verbatim with no model call — distill never loses. */
  maxTokens?: number;
  /** Alias/ref. Default: models.distiller, then the machine default model. */
  model?: string;
  /** Replay by hash(model + source + need). Default true — distillation is pure. */
  memo?: boolean;
}

interface ToolRef {
  name: string;
  /** Per-state call budget (0 = unlimited). */
  maxCalls?: number;
  /** Guard judged per call; scope includes the model-authored args. */
  when?: Fn<boolean>;
  /** feedback (default): rejection becomes the tool result. fail: semantic failure. */
  onReject?: "feedback" | "fail";
  /** Another tool that must have been called first. */
  require?: string;
  /** Machine-authored args merged over the model's — never overridable. */
  args?: Record<string, any> | Fn<Record<string, any>>;
}

/** A state: one handler (inferred from keys) + contracts + spend controls. */
interface State {
  // agent (default handler)
  prompt?: string | Fn<string>;
  /** Labeled blocks appended to the prompt, in declaration order — the
   *  mechanical plumbing (ARTICLE:\n${article}) as data. `true` injects the
   *  scope key named by the entry; a function computes the block and a falsy
   *  return omits it (conditional revision feedback with no ternary); a
   *  string is a constant block. Keys render as headers: reviewer_feedback →
   *  REVIEWER FEEDBACK:. */
  evidence?: Record<string, true | string | Fn<any>>;
  system?: string | Fn<string>;
  tools?: (string | ToolRef)[];
  /** Alias/provider ref, or a routing function returning one. */
  model?: string | Fn<string>;
  /** Model calls per conversation turn (resets across semantic retries). */
  maxTurns?: number;
  /** Cap per model call — no state may generate unboundedly. */
  maxOutputTokens?: number;
  /** Opt-in cap on the rendered input (system + prompt, ~chars/4). Over
   *  budget classifies budget_exceeded — distill: is the fix at the callsite.
   *  Never cascades onto implicit distill states. */
  maxInputTokens?: number;
  temperature?: number;
  /** prompt (default, portable) | native (decoder-constrained JSON). */
  structuredOutput?: "prompt" | "native";
  /** Thinking budget: low | medium | high. */
  reasoning?: "low" | "medium" | "high";
  /** Continue a prior state's conversation (rung 3). "self" for revisits. */
  adopt?: string | { from: string; lastTurns?: number };
  /** Inject a journal projection of a prior state (rung 2). */
  history?: {
    from: string;
    include?: ("messages" | "tool_calls" | "thoughts")[];
    lastTurns?: number;
    as?: string;
  };

  // action handler
  action?: string;

  // write sugar (action file.write)
  write?: string | Fn<string>;
  content?: string | Fn<string>;

  // human gate — routes live in the flow (branch timeout:/event keys)
  human?: string | Fn<string>;
  timeout?: string;
  /** How the gate's answer is collected. Two forms:
   *  - `{resumeEvent: label}` — single choice (confirm is two options);
   *    each key must be one of the gate's branch keys.
   *  - `{multi: [...]|fn, event?, min?, max?}` — multi-select; emits ONE
   *    event (defaulted when the branch has exactly one) and puts the
   *    selection in the gate's output as `selected`.
   *  Every gate answer may also carry a free-form `note` string. */
  choices?:
    | Record<string, string>
    | {
      multi: string[] | Fn<string[]>;
      event?: string;
      min?: number;
      max?: number;
    };

  // shared
  /** Fan the handler out over a list — one hermetic context per item.
   *  carry: pair each output with its source item — aggregate items entries
   *  become {item, output, index} (index into the original over list), so a
   *  downstream state stays aligned even when onItemFailure: "skip" drops one. */
  forEach?: {
    over: Fn<any[]>;
    as?: string;
    concurrency?: number;
    onItemFailure?: "fail" | "skip";
    carry?: boolean;
  };
  /** This state's own acceptance test, declared once: ({ output }) => boolean.
   *  loop() adopts it as the accept edge when accept: is omitted, so the
   *  criterion is not restated across the schema, events, and a guard. */
  verdict?: Fn<boolean>;
  /** Replay cached output when the rendered input is byte-identical. */
  memo?: boolean;
  /** Replace (or derive from) large scope values with model-extracted slices
   *  before this state runs. Each entry lowers to a real implicit agent state
   *  (`name#key`) — journaled, memoized, budgeted like any state. */
  distill?: Record<string, Distill>;
  /** Action args / agent user message: object (values may be functions) or one function. */
  input?: Record<string, any> | Fn<Record<string, any>>;
  /** The output schema — every property required; shorthand welcome. */
  output?: Record<string, SchemaFragment>;
  /** Allowed events — injected into the schema as a required enum. */
  events?: string[];
  retry?: {
    match: string[];
    maxAttempts: number;
    backoff?: {
      initial?: string;
      factor?: number;
      jitter?: boolean;
      cap?: string;
    };
  }[] | "none";

  terminal?: boolean;
  status?: "failed";
}

/** A model tier: a provider ref plus the per-role knobs that cascade into any
 *  agent state selecting it (where the state leaves the field unset). */
interface ModelTier {
  model: string;
  reasoning?: "low" | "medium" | "high";
  maxOutputTokens?: number;
  memo?: boolean;
}

/** Opaque flow nodes built by pipe/branch/when. */
interface FlowNode {
  readonly __steps: string;
}
type FlowTarget = State | FlowNode;
type FlowEdge = FlowNode; // when(...).to(...)

interface Machine {
  name: string;
  version?: number;
  description?: string;
  /** Declaring inputs buys strict contract checking at load. */
  input?: Record<string, string | { type?: string; required?: boolean }>;
  /** Human-named tiers (scout, senior) for provider refs. A value is either a
   *  provider-ref string or a tier object bundling the ref with per-role knobs
   *  (reasoning, maxOutputTokens, memo) — so "cheap scout vs expensive senior"
   *  is declared once and states just say model: "scout". Precedence:
   *  state-explicit > tier > machine defaults > engine default. */
  models?: Record<string, string | ModelTier>;
  /** Default agent model (sugar for defaults.model). */
  model?: string;
  /** Flat agent defaults + retry policies. */
  defaults?: {
    model?: string;
    maxTurns?: number;
    maxOutputTokens?: number;
    maxInputTokens?: number;
    temperature?: number;
    reasoning?: "low" | "medium" | "high";
    structuredOutput?: "prompt" | "native";
    retry?: { match: string[]; maxAttempts: number; backoff?: object }[];
  };
  limits?: {
    maxTransitions?: number;
    maxTokens?: number;
    maxCost?: number;
    timeout?: string;
  };
  initial?: string;
  /** Name registration: the shorthand keys name your state consts. */
  states: Record<string, State | string | Fn<string>>;
  /**
   * Inbound trigger: `steps serve --hook` maps a webhook payload to run inputs.
   * maxInFlight bounds concurrent runs of this hook (default 1); maxQueued
   * bounds durably-queued runs awaiting a slot (default 100) — overflow → 429.
   */
  webhook?: {
    path?: string;
    map: (scope: any) => Record<string, any>;
    maxInFlight?: number;
    maxQueued?: number;
  };
  /** The whole topology in one expression. Omit for linear declaration order. */
  flow?: FlowNode;
}

/** Sequence: each step falls through to the next. */
declare function pipe(...steps: FlowTarget[]): FlowNode;
/** All outgoing edges of a state: event keys, else, catch classes, gate timeout. */
declare function branch(
  state: State,
  edges: {
    [eventOrElse: string]: FlowTarget | FlowEdge | Record<string, FlowTarget>;
  } | (FlowEdge | FlowTarget)[],
): FlowNode;
/** Guard an edge: when(s => ...).to(target). */
declare function when(guard: Fn<boolean>): { to(target: FlowTarget): FlowEdge };
/** Bounded judge/revise cycle — the body falls through to the judge; accept
 *  exits, rejection revises while the visits budget lasts, exhaustion routes
 *  out. Pure sugar over branch: the judge gets exactly [accept -> then,
 *  visits.<judge> < maxVisits -> revise, fallback -> exhausted].
 *  A gate that never loops is just a `branch`. */
declare function loop(body: FlowTarget, opts: LoopOptions): FlowNode;
interface LoopOptions {
  /** The state whose out-edges the loop owns. May be an action state — a
   *  build command's exit code judges as well as a model's score. */
  judge: State;
  /** Exit test on the judge's result: ({ output, event, ... }) => boolean.
   *  Optional when the judge declares verdict: (declaring both is an error). */
  accept?: Fn<boolean>;
  /** The judge runs at most this many times (visits.<judge> < maxVisits). */
  maxVisits: number;
  /** Accept route. Defaults to the pipe successor (exactly one must exist). */
  then?: FlowTarget;
  /** Reject route while budget lasts. Defaults to the body's entry —
   *  explicit for loops that re-enter upstream of the body. */
  revise?: FlowTarget;
  /** Budget spent without acceptance. Defaults to fail. */
  exhausted?: FlowTarget;
  /** Sugar for the commonest exhausted tail: synthesize a human gate
   *  (`gate#<judge>_escalate`) whose approve rejoins the loop's then route
   *  and whose reject/timeout fail. A prompt, or {prompt, timeout}.
   *  Mutually exclusive with exhausted:. */
  escalate?: string | Fn<string> | {
    prompt: string | Fn<string>;
    timeout?: string;
  };
  /** The judge's catch edges, same as branch: {errorClass: target}. */
  catch?: Record<string, FlowTarget>;
}
/** Synthesize a human escalation state (`gate#<name>`) and its branch tail from
 *  a prompt + a choice→target map — no hand-written escalate state. Usable
 *  anywhere a flow target is (loop exhausted:, branch edges, mid-pipe).
 *  - approve: shorthand → approved -> target, synthesized rejected -> fail.
 *  - choices: full map {event: target | {to, label}}; targets may be subtrees.
 *  - timeout: routes to fail unless onTimeout: names a target.
 *  A state literally named `gate` shadows this — only calls reach the combinator. */
declare function gate(name: string, opts: GateOptions): FlowNode;
interface GateOptions {
  prompt: string | Fn<string>;
  /** Shorthand: approved -> target, plus a synthesized rejected -> fail. */
  approve?: FlowTarget;
  /** Full form: each resume event -> a target (or {to, label}). */
  choices?: Record<string, FlowTarget | { to: FlowTarget; label?: string }>;
  timeout?: string;
  /** Timeout route (requires timeout:). Defaults to fail. */
  onTimeout?: FlowTarget;
}

/** The js-dsl closure form of a state: build it by calling setters instead of
 *  writing an object literal — so fields can be added conditionally or in a loop
 *  (for (const t of tools) s.tool(t)). The recorder records plain data; the
 *  result is the same validated State the literal produces, dry-run at load.
 *  Setters return the builder for chaining; an unknown method throws at load.
 *  An optional name must match the states: key it is registered under.
 *  A state literally named `state` shadows this — only calls reach the builder. */
declare function state(build: (s: StateBuilder) => void): State;
declare function state(name: string, build: (s: StateBuilder) => void): State;
interface StateBuilder {
  // agent (default handler)
  prompt(v: State["prompt"]): this;
  system(v: State["system"]): this;
  model(v: State["model"]): this;
  maxTurns(n: number): this;
  maxOutputTokens(n: number): this;
  maxInputTokens(n: number): this;
  temperature(n: number): this;
  reasoning(v: "low" | "medium" | "high"): this;
  structuredOutput(v: "prompt" | "native"): this;
  toolChoice(v: string): this;
  adopt(v: State["adopt"]): this;
  history(v: State["history"]): this;
  evidence(v: State["evidence"]): this;
  /** Set the tool list (spread args or a single array). */
  tools(...t: (string | ToolRef)[]): this;
  tools(t: (string | ToolRef)[]): this;
  /** Append one tool — the loop-friendly form. */
  tool(t: string | ToolRef): this;
  // action / write / human
  action(name: string): this;
  write(v: State["write"]): this;
  content(v: State["content"]): this;
  human(v: State["human"]): this;
  timeout(v: string): this;
  choices(v: State["choices"]): this;
  // parallel / terminal
  parallel(v: Record<string, FlowTarget>): this;
  concurrency(n: number): this;
  onBranchFailure(v: string): this;
  terminal(v?: boolean): this;
  status(v: "failed"): this;
  // shared
  memo(v?: boolean): this;
  forEach(v: State["forEach"]): this;
  distill(v: State["distill"]): this;
  retry(v: State["retry"]): this;
  output(v: State["output"]): this;
  /** Allowed events (spread args or a single array). */
  events(...names: string[]): this;
  events(names: string[]): this;
  input(v: State["input"]): this;
  verdict(v: Fn<boolean>): this;
}

/** The aasm-style whole-machine block: author the machine by CALLING verbs on a
 *  builder top-to-bottom, instead of writing an object literal + flow expression.
 *  Topology is event-centric — named events own their from->to(+guard) edges and
 *  fan in from many states. It lowers to the same validated Machine; the object
 *  form still works unchanged. A machine literally named `machine` shadows this. */
declare function machine(
  name: string,
  build: (m: MachineBuilder) => void,
): Machine;

/** A state ref returned by m.state — referenced by identity in events. */
type MachineState = State;
/** One or more source states (aasm's `from: [a, b]` fan-in). */
type From = MachineState | MachineState[];

interface MachineBuilder {
  /** Declare a state (delegates to the state() builder, or takes a config
   *  object). Returns the ref so `const draft = m.state(...)` reads naturally. */
  state(
    name: string,
    build: (s: StateBuilder) => void,
    opts?: { initial?: boolean },
  ): MachineState;
  state(
    name: string,
    config: State,
    opts?: { initial?: boolean },
  ): MachineState;
  /** Mark the initial state (else the first declared state). */
  start(s: MachineState): this;

  /** A named agent event owning its transition; `from` may fan in. Declaring an
   *  event here auto-adds it to each non-human from-state's output.events. */
  event(
    name: string,
    spec: { from: From; to: FlowTarget; when?: Fn<boolean> },
  ): this;
  /** Unconditional edge (linear advance / trailing fallback). Two spellings. */
  step(from: From, to: FlowTarget): this;
  always(from: From, to: FlowTarget): this;
  /** Guard-only edge (no event). */
  guard(from: From, to: FlowTarget, when: Fn<boolean>): this;
  /** Error-class routing: { errorClass: target }. */
  catch(from: From, map: Record<string, FlowTarget>): this;
  /** Route an expired human gate. */
  timeout(gate: MachineState, to: FlowTarget): this;

  /** Config verbs — aasm spellings plus object-form aliases. */
  uses(models: Machine["models"]): this;
  models(models: Machine["models"]): this;
  needs(input: Machine["input"]): this;
  input(input: Machine["input"]): this;
  limit(limits: Machine["limits"]): this;
  limits(limits: Machine["limits"]): this;
  model(ref: string): this;
  defaults(d: Machine["defaults"]): this;
  describe(text: string): this;
  version(n: number): this;
  webhook(w: Machine["webhook"]): this;
}

/** Terminal states. */
declare const done: FlowNode;
declare const fail: FlowNode;

/** Render a list as bulleted lines for prompts. */
declare function list(items: any[]): string;
/** Render any value as compact YAML for prompts. */
declare function yaml(value: any): string;
/** Read a text asset (prompt file) relative to the machine; pinned with the run. */
declare function include(path: string): string;
