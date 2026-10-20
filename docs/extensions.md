# Extensions

An extension is a program that runs beside a tego session. It can:

- block a tool call or change its input, for the session's agent and its subagents
- change a tool's output
- change a prompt before it is sent, or take it
- add tools the model can call
- add slash commands
- show messages, ask the user to confirm, choose or type, and send a prompt

tego itself has no dependencies for this. Each extension is a process that speaks JSON-RPC with tego over stdio. JavaScript and TypeScript extensions run under bun or node through a small host built into tego, so they need no package.

## Where they go

| Folder | Runs |
|---|---|
| `~/.tego/extensions/` | always |
| `.tego/extensions/` in the project and the folders above it | once you trust them with `/extensions trust` |

A project's extension runs code from whoever wrote the repository, so tego asks first. The trust covers the file as it is. When the file changes, it needs trust again.

Each entry is one of these:

- a `.ts`, `.mts`, `.js` or `.mjs` file, run with bun if it is installed, else node (22.6 or newer for TypeScript)
- a `.py` file, run with python3
- an executable file
- a folder with `index.ts`, `index.js`, `index.mjs` or `main.py`

Names starting with `.` or `_` are skipped. An extension's stderr goes to `~/.tego/logs/ext-<name>.log`. `/extensions` lists them with their state, tools, commands and errors.

## JavaScript and TypeScript

Export a function. It gets an API close to pi's:

```ts
// ~/.tego/extensions/guard.ts
export default function (tego) {
  // Block or change tool calls. Subagents' calls come here too (e.agent is their name).
  tego.on("tool_call", async (e, ctx) => {
    if (e.tool === "bash" && /\brm -rf\b/.test(e.input.command)) {
      if (!(await ctx.ui.confirm("Run rm -rf?", e.input.command))) {
        return { block: true, reason: "the user said no" };
      }
    }
  });

  // Change a tool's output.
  tego.on("tool_result", (e) =>
    e.tool === "read" ? { output: e.output.replaceAll(process.env.API_KEY ?? "\0", "[key]") } : undefined);

  // A tool for the model.
  tego.registerTool({
    name: "deploy_status",
    description: "Status of the latest deploy",
    parameters: { type: "object", properties: { env: { type: "string", enum: ["staging", "prod"] } } },
    execute: async (input, ctx) => {
      const r = await fetch(`https://deploy.example.com/${input.env}/status`);
      return await r.text(); // or { content, isError }
    },
  });

  // A slash command.
  tego.registerCommand("review", {
    description: "Review the staged changes",
    handler: async (args, ctx) => ctx.sendMessage("Review the staged changes (git diff --cached). " + args),
  });
}
```

`ctx` has `ui.notify(message, level?)`, `ui.confirm(title, message?)`, `ui.select(title, options)`, `ui.input(title, placeholder?)`, `sendMessage(text)`, `cwd` and `sessionId`. `level` is `info`, `warning` or `error`. Without a terminal (`tego -p`), confirm says no, and select and input return `undefined`.

`console.log` goes to the log. stdout belongs to the protocol.

### Events

| Event | Data | A handler may return |
|---|---|---|
| `session_start` | `{cwd, session_id}` | |
| `input` | `{text}` | `{text}` to change the prompt, `{handled: true}` to send nothing |
| `tool_call` | `{tool, input, agent}` | `{block: true, reason}`, or `{input}` to change it |
| `tool_result` | `{tool, input, output, is_error, agent}` | `{output}` |
| `turn_end` | `{text}`, the final answer | |
| `session_end` | `{}` | |

Handlers run in order: those of one extension, then the next extension's. Each sees what the ones before it changed. A `block` stops the rest. Tool names are tego's: `read`, `write`, `edit`, `bash`, `grep`, `glob`, `web_search`, `task`, `mcp__<server>__<tool>`, and the extensions' own.

## Other languages: the protocol

Messages are JSON-RPC 2.0, one per line, in both directions.

tego sends these requests:

- `initialize` with `{protocol_version: 1, tego_version, cwd, session_id}`. Answer with `{name, tools: [{name, description, parameters}], commands: [{name, description}], events: ["tool_call", ...]}`. Only the events listed are sent.
- `event` with `{event, data}`. Answer as in the table. The events with nothing to return come as notifications, without an `id`.
- `tool/execute` with `{name, input}`. Answer `{content, is_error?}`.
- `command/execute` with `{name, args}`. Answer `{}`.
- `shutdown`, a notification. Exit.

The extension may send these requests:

- `ui/notify` with `{message, level}`
- `ui/confirm` with `{title, message}`, answered `{ok}`
- `ui/select` with `{title, options}`, answered `{index}`, which is -1 when nothing was chosen
- `ui/input` with `{title, placeholder}`, answered `{text, cancelled}`
- `session/sendMessage` with `{text}`: the text is sent as a prompt, after the running turn if there is one

A minimal Python extension:

```python
#!/usr/bin/env python3
import json, sys

def send(msg):
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", **msg}) + "\n")
    sys.stdout.flush()

for line in sys.stdin:
    m = json.loads(line)
    if m.get("method") == "initialize":
        send({"id": m["id"], "result": {"name": "nogit", "events": ["tool_call"]}})
    elif m.get("method") == "event":
        d = m["params"]["data"]
        if d["tool"] == "bash" and "git push" in d["input"].get("command", ""):
            send({"id": m["id"], "result": {"block": True, "reason": "push yourself"}})
        else:
            send({"id": m["id"], "result": {}})
    elif m.get("method") == "shutdown":
        break
```

## Not yet

These are not supported yet: extensions in `tego acp`, status line widgets, custom rendering of tools, and new providers.
