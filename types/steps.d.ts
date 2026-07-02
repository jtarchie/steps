// Type declarations for steps machine files.
//
// Enable editor autocomplete and checking by adding a jsconfig.json next to
// your machine:
//
//   { "compilerOptions": { "checkJs": true } }
//
// and referencing this file from your machine:
//
//   // @ts-check
//   /// <reference path="./types/steps.d.ts" />
//
// Structure is data; any computed value is a plain function of one Scope.

/** The single argument every machine function receives. */
interface Scope {
  /** Run input at the root, plus ctx.<state> = that state's output. */
  ctx: Record<string, any>;
  /** The current state's validated output (transition guards only). */
  output?: any;
  /** The agent-declared event (transition guards only). */
  event?: string;
  /** Entry counts per state — bound loops with visits.draft < 3. */
  visits: Record<string, number>;
  /** Cumulative run accounting. */
  run: { transitions: number; tokens: number; cost: number };
  /** Attempt number within the current state. */
  attempt: number;
  /** foreach: item position. The item itself appears under its `as` name. */
  index?: number;
  total?: number;
  /** Tool guards: the model-authored arguments being judged. */
  args?: Record<string, any>;
  /** Tool guards: invocation counts per tool this state. */
  calls?: Record<string, number>;
  /** Tool guards: model calls this conversation turn. */
  turn?: number;
  /** foreach item under its `as` name (file, lead, item, ...). */
  [as: string]: any;
}

type Fn<T> = (scope: Scope) => T;

/** A schema fragment: "string", "enum(a, b)", "string[]", [{...shape}], or JSON schema. */
type SchemaFragment = string | SchemaFragment[] | { [key: string]: any };

interface OutputSpec {
  /** Property shapes; every declared property is required. */
  schema?: Record<string, SchemaFragment>;
  /** Allowed events — injected into the schema as a required enum. */
  events?: string[];
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

interface AgentSpec {
  /** Alias/provider ref, or a routing function returning one. */
  model?: string | Fn<string>;
  system?: string | Fn<string>;
  prompt?: string | Fn<string>;
  tools?: (string | ToolRef)[];
  /** Model calls per conversation turn (resets across semantic retries). */
  maxTurns?: number;
  /** Cap per model call — no state may generate unboundedly. */
  maxOutputTokens?: number;
  temperature?: number;
  /** prompt (default, portable) | native (decoder-constrained JSON). */
  structuredOutput?: "prompt" | "native";
  /** Thinking budget: low | medium | high. */
  reasoning?: "low" | "medium" | "high";
  /** Continue a prior state's conversation (rung 3). "self" for revisits. */
  adopt?: string | { from: string; lastTurns?: number };
  /** Inject a journal projection of a prior state (rung 2). */
  history?: { from: string; include?: ("messages" | "tool_calls" | "thoughts")[]; lastTurns?: number; as?: string };
}

interface RetryPolicy {
  match: string[];
  maxAttempts: number;
  backoff?: { initial?: string; factor?: number; jitter?: boolean; cap?: string };
}

interface Transition {
  /** Match the agent-declared event. */
  on?: string;
  /** Guard: agent proposes, guards dispose. */
  when?: Fn<boolean>;
  to: string;
}

interface State {
  /** Exactly one handler: agent, action, or human (or terminal: true). */
  agent?: string | Fn<string> | AgentSpec;
  action?: string;
  human?: { prompt: string | Fn<string>; timeout?: string; onTimeout?: string };
  /** Fan the handler out over a list — one hermetic context per item. */
  forEach?: {
    over: Fn<any[]>;
    as?: string;
    concurrency?: number;
    onItemFailure?: "fail" | "skip";
  };
  /** Replay cached output when the rendered input is byte-identical. */
  memo?: boolean;
  /** Action args / agent user message: object (values may be functions) or one function. */
  input?: Record<string, any> | Fn<Record<string, any>>;
  output?: OutputSpec;
  retry?: RetryPolicy[] | "none";
  catch?: { match: string[]; to: string }[];
  /** Omitted: linear default to the next declared state. */
  transitions?: string | Transition[];
  terminal?: boolean;
  status?: "failed";
}

interface Machine {
  name: string;
  version?: number;
  description?: string;
  /** Declaring inputs buys strict ctx checking at validate time. */
  input?: Record<string, { type?: string; required?: boolean }>;
  /** Human-named aliases (scout, senior) for provider refs. */
  models?: Record<string, string>;
  defaults?: {
    agent?: Pick<AgentSpec, "model" | "maxTurns" | "maxOutputTokens" | "temperature" | "structuredOutput" | "reasoning"> & { model?: string };
    retry?: RetryPolicy[];
  };
  limits?: { maxTransitions?: number; maxTokens?: number; maxCost?: number; timeout?: string };
  initial?: string;
  states: Record<string, State>;
}

/** Read a text asset (prompt file) relative to the machine; pinned with the run. */
declare function include(path: string): string;

declare var module: { exports: Machine };
