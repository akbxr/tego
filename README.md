# tego

A coding agent for your terminal, written in Go. One static binary, about 10 MB.

I wanted an agent that starts instantly, leaves my terminal alone, and doesn't need half a gigabyte of RAM to show a diff. tego idles at 5 to 7 MB and sits at 9 to 30 MB after a few turns with MCP servers loaded.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/akbxr/tego/main/install.sh | sh
```

With Go 1.23 or newer you can install or build from source instead:

```sh
go install github.com/akbxr/tego/cmd/tego@latest
# or
git clone https://github.com/akbxr/tego && cd tego
go build -ldflags="-s -w" -o ~/.local/bin/tego ./cmd/tego
```

## Quick start

```sh
tego                                 # start a session
tego login anthropic                 # or openai-codex, google-antigravity, openrouter
tego "fix the failing test"          # start with a prompt
tego -p "summarise this repo"        # answer once and exit
git diff | tego -p "review this"     # stdin works too
tego -c                              # continue the last session here
tego -r                              # pick a session, from any project
tego -m openai-codex/gpt-5.6 -t high # model and thinking level
```

## What you get

**Less memory, fewer tokens.** In a benchmark on the VS Code repository, tego used 18 to 26 MB while it worked. oh-my-pi used 250 to 400 MB. The fixed prompt that goes with every request is about 6k tokens, against 15k for oh-my-pi. See [Token use](#token-use).

**Your subscriptions.** Log in with Claude Pro/Max, ChatGPT Plus/Pro or a Google account through Antigravity, or use an API key for any OpenAI-compatible provider. Ollama and LM Studio need no key. With several accounts per provider, tego moves to the next one when an account hits its limit. `tego usage` shows the 5-hour and weekly limits of all of them.

**An inline UI.** No full-screen takeover, so scrollback, search and copy work as usual. Markdown, code and diffs render in place. A footer shows the model, git branch, context use and tokens. You can type while the agent works. Enter steers the running turn and Alt+Enter queues a follow-up.

**Tools.** `read`, `write`, `edit`, `bash`, `grep`, `glob`, `web_search`, parallel subagents and a live todo list. One edit call can change several places in a file. Bash streams its output, and `background: true` keeps a dev server running between turns. Read-only calls run in parallel. `web_search` uses the provider's own search on Anthropic, Codex and Antigravity, and Exa, Brave, Tavily or DuckDuckGo elsewhere.

**Guard rails.**
- Permission modes decide which calls wait for you. `yolo`, the default, runs everything. `edits` asks before shell commands and MCP tools that may change things. `ask` asks before file edits too. Reads and read-only commands such as `go test` never ask. "Don't ask again" covers that kind of call for the rest of the session, but never `sudo`, inline scripts, command substitution or a redirect into a file. No stops the turn, subagents included. No with a note lets the agent go on with what you wrote. Set the mode with `/permissions`, `--permissions` or `"permissions"`. With `-p` nobody can answer, so those calls are refused.
- After every edit tego runs a syntax check and shows the model what broke.
- An edit fails if the file changed since the agent read it, and the error carries the current content.
- `/undo` restores the files from before any prompt of the session. Snapshots live in their own git repository under `~/.tego/snapshots/`, so your `.git` stays untouched.
- Plan mode, Shift+Tab or `/plan`, lets the agent read and propose a plan. Nothing changes until you approve it.
- Bash refuses `rm -rf ~`, force pushes to `main`, writes to raw disks and a few other disasters. It is a guard rail, not a sandbox.

**Sessions you can branch.** Sessions are append-only trees in `~/.tego/sessions/`. Press Esc twice to rewind to an earlier prompt and edit it, `/tree` to switch branches, `/fork` to start a new session from any point.

**Worktrees.** `tego -w [name]` works in a git worktree of its own, on branch `tego/<name>`, so several sessions can change one repository at once. Worktrees go in `~/.tego/worktrees/`, outside the repository, so its build and test tools don't pick them up. Ignored files listed in `.worktreeinclude`, such as `.env`, are copied in. Dependencies are not, so run your install step there. On exit, a worktree with nothing in it is removed. One with work in it stays unless you remove it. `tego -w <name>` and `tego -r` go back to it, and `tego worktree` lists, removes (`rm`) or prunes them. Subagents can have one each too (`isolation: worktree` on a task or in an agent file), so several can edit the same files in parallel. Each commits on its own branch and its report says what to merge. A worktree a subagent left nothing in is removed.

**Skills, commands and MCP.** Skills use Claude Code's `SKILL.md` format and load from the usual `.agents`, `.claude` and `.tego` folders. MCP servers come from `~/.tego/mcp.json` or the project's `.mcp.json`, over stdio, HTTP or SSE. A local server runs only while it is used: tego keeps its tool list, starts it at the first call and stops it after 10 minutes unused (`"mcp_idle"`, or `"off"`). Give a server `"keepAlive": true` to keep it running all session. `AGENTS.md` and `CLAUDE.md` load automatically.

**Agents.** Subagents can be defined in Claude Code's format, as `agents/<name>.md` in the same folders: `name`, `description`, `tools`, `disallowedTools`, `model`, `effort`, `maxTurns` and `isolation` in the frontmatter, and the instructions as the body. The session's model picks one per task from its description. `model` takes Claude's aliases (`opus`, `sonnet`, `haiku`), `inherit`, `small`, a `subagent_models` name or any `provider/model`. Tools tego has no equivalent for, such as `WebFetch`, are ignored. `/agents defined` lists them.

**Extensions.** An extension can block or change tool calls, change their output and your prompts, add tools and slash commands, and ask you things. It is a TypeScript, JavaScript or Python file, or any executable, in `~/.tego/extensions/` or the project's `.tego/extensions/`. A project's extensions only run once you trust them. TypeScript runs under bun or node with an API like pi's, and tego adds no dependency for it. See [docs/extensions.md](docs/extensions.md).

**In your editor.** `tego acp` speaks the [Agent Client Protocol](https://agentclientprotocol.com), so Zed, JetBrains, Neovim, Emacs and other ACP editors can run tego with your logins and settings. Edits show as diffs, tools read the editor's open buffers including unsaved changes, and commands run in its terminals. The editor's mode, model and thinking pickers work, and `/` offers tego's commands, your custom commands and every skill. Sessions are shared, so `tego -r` opens them in the terminal too. In Zed, add to `settings.json`:

```json
"agent_servers": {
  "tego": { "type": "custom", "command": "tego", "args": ["acp"] }
}
```

**Usage stats.** Every request is logged to `~/.tego/stats.jsonl`, without prompt text. `tego stats` prints a summary and `tego stats serve` opens a local dashboard with cost, cache use, speed and failures per model, project and tool. Costs are estimates at API list prices from [models.dev](https://github.com/anomalyco/models.dev).

## Token use

tego tries hard not to resend what the model already has.

- MCP tools stay out of the prompt until the model searches for them. With 63 tools connected, that cut tool definitions from 18.9k to 3.4k tokens per request.
- When the Anthropic cache has expired anyway, tego trims old tool output before the next request rather than cache it again. On Claude 4.5 and newer the API also clears old tool results past 200k tokens, in batches, so the requests in between still hit the cache. Replaying four long Opus sessions, the two cut prompt cost by about 23%, counting the files the model reads again.
- After a break of over an hour on Anthropic, a context still above 200k tokens is summarised before your next prompt instead of being cached again. On a 395k-token session, with the limit at 150k, that took the next cache write from 222k tokens to 32k. `/tree` brings back the full history. `"idle_compact": false` turns this off and `"idle_compact_tokens"` sets the limit.
- Near the end of the window the model summarises the conversation. The last 20k tokens stay as they were, and your own messages, the files changed, the skills in use and the todo list follow the summary word for word.
- Titles, web search and simple subagent jobs go to a cheaper model on the same account, Haiku 5.5 on Anthropic and Gemini 3.5 Flash-Lite on Antigravity. Set `"small_model"` to another model or to `"off"`. Compaction stays on the session's model.
- Bash keeps the first 200 and last 300 lines of a long output and saves all of it to a file the model can page through, so it doesn't run the command again.
- On Codex and Antigravity the cache is automatic, so tego leaves the history alone until it nears the window. There it drops old tool output first, and only summarises if that isn't enough.
- `grep` returns 100 lines per page, cut at 200 columns, with match counts per file and an `offset` for the next page.
- Through OpenRouter, Claude, Gemini and Qwen cache only with a marker. tego sends one, plus a session id that keeps you on the same provider and its cache.
- The system prompt holds only what stays fixed for a project. The date and git branch go after your prompt, so resuming on another day keeps the cache.
- Requests over 32 KB go gzipped to Anthropic, Codex and Antigravity. The tokens are the same, but the upload, repeated on every tool round, is about half the size. If a server refuses it, tego sends that request again uncompressed and stops compressing for it. `"gzip_requests": false` turns this off.

In my benchmarks, 3 runs per task, tego cost 44% less than oh-my-pi with Sonnet 5.5 on small Python and Go tasks. The gap depends a lot on the repository.

## Configuration

`~/.tego/config.json`. Every key is optional.

```json
{
  "model": "anthropic/claude-sonnet-5-5",
  "thinking": "medium",
  "subagents": 8,
  "small_model": "anthropic/claude-haiku-5-5",
  "subagent_models": {
    "gpt": { "model": "openai-codex/gpt-5.6-terra", "use": "code review and debugging" },
    "gemini": "google-antigravity/gemini-3.8-pro"
  },
  "permissions": "yolo",
  "accounts": "fallback",
  "icons": "unicode",
  "providers": {
    "lmstudio": { "kind": "openai", "base_url": "http://localhost:1234/v1" }
  },
  "prices": {
    "lmstudio/qwen3-coder": { "input": 0, "output": 0 }
  }
}
```

`subagent_models` names more models for subagents, from other subscriptions too. The session's model picks one per task from its name and `use`, or you can ask for one ("review it with gpt"). `thinking` sets one's thinking level. Only models you are logged in for are offered, and a subagent whose model fails to start runs on the session's model and says so.

`/model` saves the model and thinking level here. Thinking goes from `off` to `max`, and on a model without the level you picked, tego uses the highest it has. `/fast` or `--fast` turns on Claude's fast mode for Opus 4.8 and newer. It gives up to 2.5 times the output speed at twice the price, and a subscription pays for it from usage credits only. If fast mode is rate limited, tego carries on at standard speed. API keys can also come from the environment, such as `ANTHROPIC_API_KEY` or `OPENROUTER_API_KEY`. Logins are stored in `~/.tego/auth.json` with mode 0600.

## Commands

| Command | Does |
| --- | --- |
| `/model`, `/thinking` | Switch model or thinking level, e.g. `/model opus:high` |
| `/resume`, `/new` | Pick, delete or rename sessions from any project |
| `/tree`, `/fork`, `/clone` | Branch a session |
| `/undo` | Restore the files from before a prompt |
| `/plan [prompt]` | Plan mode on or off, also Shift+Tab |
| `/fast [on\|off]` | Fast mode on or off, Claude Opus 4.8+ |
| `/permissions [yolo\|edits\|ask]` | Which tool calls wait for your approval |
| `/compact`, `/retry` | Summarise the context, retry a failed turn |
| `/login`, `/accounts`, `/usage` | Accounts and subscription limits |
| `/stats`, `/mcp`, `/skills` | Usage, MCP servers, skills |

Type `/` for the full list. `@` attaches a file, `!cmd` runs a shell command yourself, and Ctrl+V pastes an image.

## What tego leaves out

A full-screen UI, LSP integration, browser control and MCP OAuth. Some may come later. If you need one, open an issue with your use case.

## Contributing

Bug reports and pull requests are welcome. Run `go vet ./...` and `go test ./...` first. CI runs the same on Linux and macOS. The binary should stay small, so a new dependency needs a good reason.

## License

MIT, see [LICENSE](LICENSE).
