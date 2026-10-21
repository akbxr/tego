// tego's extension host: runs one JavaScript or TypeScript extension under
// bun or node and speaks tego's JSON-RPC protocol (one message per line on
// stdin/stdout) for it. The extension's default export gets the API:
//
//   export default function (tego) {
//     tego.on("tool_call", async (e, ctx) => {
//       if (e.tool === "bash" && /rm -rf/.test(e.input.command)) return { block: true, reason: "no rm -rf" };
//     });
//     tego.registerTool({ name: "now", description: "The time", parameters: { type: "object", properties: {} },
//       execute: async (input, ctx) => new Date().toISOString() });
//     tego.registerCommand("hello", { description: "Say hello", handler: async (args, ctx) => ctx.ui.notify("hello " + args) });
//   }
import { createInterface } from "node:readline";
import { pathToFileURL } from "node:url";
import { basename, extname } from "node:path";

const file = process.argv[2];
const out = process.stdout;
// stdout is the protocol: anything the extension prints goes to the log.
console.log = console.info = console.debug = (...a) => console.error(...a);

let nextId = 1;
const pending = new Map();
const send = (m) => out.write(JSON.stringify({ jsonrpc: "2.0", ...m }) + "\n");
const request = (method, params) =>
  new Promise((resolve, reject) => {
    const id = nextId++;
    pending.set(id, { resolve, reject });
    send({ id, method, params });
  });

const handlers = new Map();
const tools = new Map();
const commands = new Map();
const ui = {
  notify: (message, level = "info") => send({ method: "ui/notify", params: { message: String(message), level } }),
  confirm: (title, message = "") => request("ui/confirm", { title, message }).then((r) => !!r.ok),
  select: (title, options) =>
    request("ui/select", { title, options: options.map(String) }).then((r) => (r.index >= 0 ? options[r.index] : undefined)),
  input: (title, placeholder = "") =>
    request("ui/input", { title, placeholder }).then((r) => (r.cancelled ? undefined : r.text)),
};
const ctx = {
  ui,
  cwd: process.cwd(),
  sessionId: "",
  // The session's model: { provider, model, model_name }, kept current.
  model: { provider: "", model: "", model_name: "" },
  sendMessage: (text) => send({ method: "session/sendMessage", params: { text: String(text) } }),
};
const tego = {
  on(event, fn) {
    if (!handlers.has(event)) handlers.set(event, []);
    handlers.get(event).push(fn);
  },
  registerTool(def) {
    if (!def || !def.name || typeof def.execute !== "function") throw new Error("registerTool needs a name and execute");
    tools.set(def.name, def);
  },
  registerCommand(name, def) {
    const d = typeof def === "function" ? { handler: def } : def;
    if (typeof d?.handler !== "function") throw new Error("registerCommand needs a handler");
    commands.set(String(name).replace(/^\//, ""), d);
  },
  ui,
  sendMessage: ctx.sendMessage,
  get cwd() { return ctx.cwd; },
};

const mod = await import(pathToFileURL(file).href);
const init = mod.default ?? mod.activate;
if (typeof init !== "function") throw new Error(`${file}: export a default function (tego) => {...}`);
await init(tego);

// runEvent runs the handlers of one event in order. Request events merge
// what they return: tool_call stops at a block, and a changed input is
// what the next handler sees; tool_result and input likewise.
async function runEvent(event, data) {
  const result = {};
  for (const fn of handlers.get(event) ?? []) {
    const r = await fn(data, ctx);
    if (!r || typeof r !== "object") continue;
    if (event === "tool_call") {
      if (r.block) return { block: true, reason: r.reason ?? "" };
      if (r.input !== undefined) data.input = result.input = r.input;
    } else if (event === "tool_result") {
      if (r.output !== undefined) data.output = result.output = String(r.output);
    } else if (event === "input") {
      if (r.text !== undefined) data.text = result.text = String(r.text);
      if (r.handled) return { ...result, handled: true };
    }
  }
  return result;
}

async function handle(method, params) {
  switch (method) {
    case "initialize":
      ctx.cwd = params.cwd ?? ctx.cwd;
      ctx.sessionId = params.session_id ?? "";
      if (params.model) ctx.model = params.model;
      return {
        name: mod.name ?? basename(file, extname(file)),
        tools: [...tools.values()].map((t) => ({
          name: t.name,
          description: t.description ?? "",
          parameters: t.parameters ?? { type: "object", properties: {} },
        })),
        commands: [...commands.entries()].map(([name, d]) => ({ name, description: d.description ?? "" })),
        events: [...handlers.keys()],
      };
    case "event": {
      const d = params.data ?? {};
      if (params.event === "session_start" || params.event === "model_change") {
        ctx.model = { provider: d.provider ?? "", model: d.model ?? "", model_name: d.model_name ?? "" };
      }
      return runEvent(params.event, d);
    }
    case "tool/execute": {
      const t = tools.get(params.name);
      if (!t) throw new Error(`no tool ${params.name}`);
      try {
        const r = await t.execute(params.input ?? {}, ctx);
        if (typeof r === "string") return { content: r };
        if (r && typeof r === "object" && "content" in r) return { content: String(r.content), is_error: !!r.isError };
        return { content: JSON.stringify(r ?? null) };
      } catch (err) {
        return { content: String(err?.message ?? err), is_error: true };
      }
    }
    case "command/execute": {
      const c = commands.get(params.name);
      if (!c) throw new Error(`no command /${params.name}`);
      await c.handler(params.args ?? "", ctx);
      return {};
    }
    case "shutdown":
      process.exit(0);
  }
  throw new Error(`method not found: ${method}`);
}

createInterface({ input: process.stdin }).on("line", async (line) => {
  let m;
  try {
    m = JSON.parse(line);
  } catch {
    return;
  }
  if (m.method === undefined) {
    const p = pending.get(m.id);
    if (p) {
      pending.delete(m.id);
      m.error ? p.reject(new Error(m.error.message)) : p.resolve(m.result ?? {});
    }
    return;
  }
  try {
    const result = await handle(m.method, m.params ?? {});
    if (m.id !== undefined) send({ id: m.id, result: result ?? {} });
  } catch (err) {
    if (m.id !== undefined) send({ id: m.id, error: { code: -32000, message: String(err?.message ?? err) } });
    else console.error(err);
  }
}).on("close", () => process.exit(0));
