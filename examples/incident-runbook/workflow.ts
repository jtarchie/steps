/// <reference path="../../docs/src/global.d.ts" />
// An incident-response runbook, triggered by a Honeybadger webhook. The
// machine survives its own tooling: the error tracker that opened the
// incident may itself be down (dead-lettered, never fatal), a cheap first
// responder's PROCESS is audited from its transcript (history:), a senior
// model resumes the responder's actual conversation when the diagnosis is
// weak (adopt:), a human multi-selects the remediations to apply, and the
// report always ships (catch: "*").

const REPORT_HEADER = include("prompts/report.md"); // top-level: pinned AND hashed with the machine

// Probe every service's status endpoint. http.get returns non-2xx as DATA
// ({status, body}) — a 404 is evidence, not an error; only a dead host
// raises action_error. Probes never retry: a refused connection is an
// answer. No onItemFailure:"skip": downstream zips items with the services
// list by index, so a dropped item would misalign the report.
const probe: State = {
  forEach: {
    over: ({ services }) => services.split(",").map((s) => s.trim()),
    as: "service",
    concurrency: 4, // live probes run in parallel; mock runs force 1
  },
  action: "http.get",
  retry: "none",
  input: ({ status_base, service }) => ({
    url: `${status_base}/${service}.json`,
  }),
  output: { status: "number", body: "string" }, // per-item shape
};

// Fetch the full fault from the Honeybadger API. The auth header is an
// operator input (hb_auth), passed via http.get's headers arg. When the
// tracker itself is unreachable mid-outage, catch: routes the run to a
// dead-letter note and the diagnosis proceeds on probes alone.
const fetch_fault: State = {
  action: "http.get",
  retry: "none", // an unreachable tracker routes via catch in ONE hop
  input: ({ fault_url, hb_auth }) => ({
    url: fault_url,
    ...(hb_auth ? { headers: { Authorization: hb_auth } } : {}),
  }),
  output: { status: "number", body: "string" },
};

const note_tracker: State = {
  write: "out/tracker-failure.md",
  content: ({ fault_url }) =>
    `# Error tracker unreachable

Honeybadger (${fault_url}) could not be reached when the incident opened.
Diagnosis proceeded from live probes alone.
`,
};

// The first responder: a cheap model, hermetic, evidence-only. NO memo —
// this state is the adopt/history SOURCE, and a memo replay records no
// conversation (it would starve verify's trace and take_over's adoption).
const responder: State = {
  model: "responder",
  system:
    "You are the first responder on the infrastructure on-call rotation. Diagnose only from the evidence given; never invent probe results.",
  structuredOutput: "native", // tool-less + JSON contract: decoder-constrained on live OpenAI-compatible backends
  maxOutputTokens: 1024,
  prompt: ({ incident, services, probe, fetch_fault }) => `
    INCIDENT:
    ${incident}

    LIVE PROBES (one status per service):
    ${
    yaml(
      services.split(",").map((s, i) => ({
        service: s.trim(),
        http_status: probe.items[i].status,
        body: probe.items[i].body,
      })),
    )
  }

    ERROR TRACKER FAULT DETAIL:
    ${
    fetch_fault
      ? fetch_fault.body
      : "(error tracker unreachable — see out/tracker-failure.md)"
  }

    Diagnose the root cause across these services. If you cannot, declare
    the "stuck" event. State your confidence between 0 and 1.`,
  output: { diagnosis: "string", confidence: "number", affected: "string[]" },
  events: ["diagnosed", "stuck"],
};

// The auditor judges the responder's PROCESS, not just its conclusion —
// rung 2: a read-only projection of the responder's transcript, failed
// attempts included, rendered as text into a fresh conversation.
const verify: State = {
  model: "auditor",
  system:
    "You audit incident diagnoses. Judge the PROCESS in the transcript, not just the conclusion.",
  history: {
    from: "responder",
    include: ["messages", "tool_calls"],
    lastTurns: 6,
    as: "trace",
  },
  prompt: ({ trace, responder }) => `
    The first responder's session transcript:
    ${trace}

    Conclusion: ${responder.diagnosis} (confidence ${responder.confidence})

    Did they address every anomalous probe, including non-200 statuses?
    Declare "sound" only if the process holds up.`,
  output: {
    process_ok: "boolean",
    concerns: { type: "array", items: "string", maxItems: 3 },
  },
  events: ["sound", "flawed"],
};

// Tier escalation — rung 3: the senior does not start over; it RESUMES the
// responder's actual conversation. lastTurns: 2 keeps only the responder's
// final exchange — safe because this prompt re-carries the incident.
const take_over: State = {
  model: "senior",
  reasoning: "high",
  maxTurns: 6,
  maxOutputTokens: 4096,
  adopt: { from: "responder", lastTurns: 2 },
  tools: [
    // The senior may re-probe or re-fetch anything — but the credential is
    // machine-pinned: merged over the model's args, never visible to it.
    {
      name: "http.get",
      args: (
        { hb_auth },
      ) => (hb_auth ? { headers: { Authorization: hb_auth } } : {}),
    },
    // The runbook is read-only reference material, only after live evidence,
    // and only inside the pinned root. A rejected call feeds back into the
    // loop rather than failing the state.
    {
      name: "file.read",
      require: "http.get",
      maxCalls: 2,
      onReject: "feedback",
      when: ({ runbook_dir }) => Boolean(runbook_dir),
      args: ({ runbook_dir }) => ({ root: runbook_dir || "" }),
    },
  ],
  prompt: ({ incident, verify }) => `
    You are the senior incident commander taking over mid-session; the
    transcript above is the first responder's attempt${
    verify
      ? `. An auditor flagged it:\n${list(verify.concerns)}`
      : " — they declared themselves stuck"
  }.
    INCIDENT (for reference): ${incident}
    Re-diagnose. Re-probe with http_get first if live evidence would change
    your answer; consult the runbook (file_read) only AFTER re-probing.
    State your confidence between 0 and 1.`,
  output: { diagnosis: "string", confidence: "number", affected: "string[]" },
};

// Turn the winning diagnosis into a bounded remediation list — the gate's
// option source.
const propose: State = {
  model: "responder",
  prompt: ({ responder, take_over, verify }) => `
    DIAGNOSIS (${take_over ? "senior incident commander" : "first responder"}):
    ${(take_over || responder).diagnosis}
    ${
    verify && !verify.process_ok
      ? "AUDIT CONCERNS:\n" + list(verify.concerns)
      : ""
  }
    Propose 2-4 concrete remediations, most urgent first. Each under 15 words.`,
  output: {
    summary: "string",
    remediations: { type: "array", items: "string", minItems: 2, maxItems: 4 },
  },
};

// The human picks WHICH remediations to apply — a multi-select gate. The
// selection lands in pick.selected; a free-form note is always accepted.
const pick: State = {
  human: ({ propose, responder, take_over }) => `
    ${propose.summary}
    Diagnosis (${take_over ? "senior" : "responder"}): ${
    (take_over || responder).diagnosis
  }
    Which remediations should be applied?`,
  choices: {
    multi: ({ propose }) => propose.remediations,
    event: "chosen",
    min: 1,
  },
  timeout: "4h",
};

// One hermetic runbook entry per selected remediation. retry: "none" +
// catch: "*" downstream: a mis-drafted step must never stall the report.
const apply: State = {
  forEach: { over: ({ pick }) => pick.selected, as: "step" },
  memo: true,
  retry: "none",
  model: "responder",
  prompt: ({ step, index, total, responder, take_over }) => `
    Runbook entry ${index + 1} of ${total}.
    STEP: ${step}
    DIAGNOSIS: ${(take_over || responder).diagnosis}
    Write exact copy-pasteable operator instructions (3-6 lines) and rate the risk.`,
  output: { instructions: "string", risk: "enum(low, medium, high)" },
};

// The report ALWAYS ships — every upstream state that might not have run on
// this path is guarded. Written verbatim (write content is not dedented).
const report: State = {
  write: "out/incident-report.md",
  content: (
    {
      incident,
      services,
      probe,
      fetch_fault,
      note_tracker,
      responder,
      verify,
      take_over,
      pick,
      apply,
    },
  ) =>
    `${REPORT_HEADER}
## Incident

${incident}

## Probes

${
      services.split(",").map((s, i) =>
        `- ${s.trim()}: HTTP ${probe.items[i].status}`
      ).join("\n")
    }

## Error tracker

${
      note_tracker
        ? "Honeybadger UNREACHABLE at incident open — page from the tracker UI manually (see out/tracker-failure.md)."
        : `Fault detail fetched (HTTP ${fetch_fault.status}).`
    }

## Diagnosis (${take_over ? "senior incident commander" : "first responder"})

${(take_over || responder).diagnosis}
${
      verify && !verify.process_ok
        ? `
Audit concerns:
${list(verify.concerns)}`
        : ""
    }

## Remediations selected

${list(pick.selected)}
${pick.note ? `Operator note: ${pick.note}` : ""}

## Runbook

${
      apply && apply.items
        ? apply.items.map((it, i) =>
          `### ${i + 1}. ${
            pick.selected[i]
          } (risk: ${it.risk})\n\n${it.instructions}`
        ).join("\n\n")
        : "_(runbook steps were not drafted — see the run journal)_"
    }
`,
};

export default {
  name: "incident-runbook",
  input: {
    incident: { type: "string", required: true }, // what happened, from the webhook payload
    services: { type: "string", required: true }, // comma-separated service names to probe
    status_base: { type: "string", required: true }, // base URL of the status endpoints
    fault_url: { type: "string", required: true }, // Honeybadger fault API URL (composed by the webhook map)
    hb_auth: "string", // optional: FULL Authorization header value ("Basic <b64>" / "Bearer <tok>")
    runbook_dir: "string", // optional: root for the senior's file_read (live runs)
  },
  models: {
    responder: "openrouter/qwen/qwen3-30b-a3b-instruct-2507", // cheap tier: diagnose, propose, scribe
    auditor: "openrouter/qwen/qwen3-30b-a3b-instruct-2507", // aliases name capabilities; swap independently
    senior: "openrouter/anthropic/claude-opus-4.8", // tier escalation (DESIGN.md's own adopt example)
  },
  model: "responder",
  defaults: { reasoning: "low" },
  limits: { maxTransitions: 20, maxTokens: 300000, timeout: "30m" },

  // The trigger: `steps serve --hook workflow.ts` exposes POST /hooks/honeybadger.
  // Operator config (services, status_base, hb_base, hb_auth) arrives via
  // --hook-input and is visible here by name; the payload is `body`.
  //
  // The Honeybadger webhook is SUMMARY-ONLY (klass/message/environment/counts;
  // no backtrace — https://docs.honeybadger.io/guides/integrations/webhook/).
  // So the summary IS the incident here, always available; fetch_fault only
  // enriches it with the backtrace the webhook omits, and its failure is
  // therefore survivable, not fatal.
  webhook: {
    path: "honeybadger",
    map: ({ body, hb_base }) => ({
      incident:
        `Honeybadger fault #${body.fault.id} in ${body.fault.environment}: ${body.fault.klass} — ${body.fault.message} (seen ${body.fault.notices_count}×, last ${body.fault.last_notice_at})`,
      fault_url:
        `${hb_base}/v2/projects/${body.fault.project_id}/faults/${body.fault.id}`,
    }),
  },

  states: {
    probe,
    fetch_fault,
    note_tracker,
    responder,
    verify,
    take_over,
    propose,
    pick,
    apply,
    report,
  },

  flow: pipe(
    probe,
    branch(fetch_fault, {
      catch: { action_error: pipe(note_tracker, responder) }, // tracker down: note, rejoin
    }), // implicit else -> responder (fault fetched: {status, body} in ctx)
    branch(responder, { stuck: take_over }), // "diagnosed" falls through to the auditor
    branch(verify, {
      // Agent proposes "sound"; the guard disposes — soundness only counts
      // with confidence behind it.
      sound: when(({ responder }) => responder.confidence >= 0.75).to(propose),
    }), // "flawed" (or a vetoed "sound") falls through to the senior
    take_over, // adopts the responder's conversation; falls through to propose
    propose,
    branch(pick, {
      chosen: pipe(branch(apply, { catch: { "*": report } }), report), // the report ALWAYS ships
      timeout: fail,
    }),
  ),
};
