// A scripted OpenAI-compatible endpoint, so a short re-renders byte-identical after every UI change: node fakellm.mjs <port> <turns.json>
// turns.json is an ordered list of tool calls; the reply is the first one the conversation history has not made yet, then the final text.
import { createServer } from "node:http";
import { readFileSync } from "node:fs";

const [port, file] = process.argv.slice(2);
const script = JSON.parse(readFileSync(file, "utf8"));

const completion = (message, finish) =>
  JSON.stringify({ id: "fake", object: "chat.completion", created: 0, model: "fake-model",
    choices: [{ index: 0, finish_reason: finish, message }],
    usage: { prompt_tokens: 1200, completion_tokens: 80, total_tokens: 1280 } });

createServer((req, res) => {
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    const { messages = [] } = JSON.parse(body || "{}");
    const made = messages.flatMap((m) => (m.tool_calls ?? []).map((c) => c.function.name));
    const next = script.calls.find((c) => !made.includes(c.name));
    res.setHeader("Content-Type", "application/json");
    if (!next) {
      res.end(completion({ role: "assistant", content: script.final }, "stop"));
      return;
    }
    res.end(completion({ role: "assistant", content: null, tool_calls: [
      { id: `call_${made.length + 1}`, type: "function", function: { name: next.name, arguments: JSON.stringify(next.args) } },
    ] }, "tool_calls"));
  });
}).listen(Number(port), "127.0.0.1");
