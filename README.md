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

`tego update` installs the latest release over the running binary after checking its checksum, and `tego update --check` only tells you whether there is one. tego also looks for a new release once a day in the background and shows `⬆ Update available` under the banner when it finds one. Set `"update_check": "off"` in the config or `TEGO_NO_UPDATE_CHECK=1` to turn that off.

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

**Less memory, fewer tokens.** On a benchmark in the VS Code repository, tego's process used 18 to 26 MB while it worked, where oh-my-pi used 250 to 400 MB. Its fixed prompt (system prompt plus tool definitions) is about 6k tokens against 15k for oh-my-pi, and every request carries it. MCP tools load only when the model searches for them, and old tool output is trimmed when that saves money. See [Token use](#token-use).

**Your subscriptions.** Log in with Claude Pro/Max, ChatGPT Plus/Pro or a Google account (Antigravity), or use an API key for any OpenAI-compatible provider. Ollama and LM Studio work without a key. Add several accounts per provider and tego moves to the next one when an account hits its limit. `tego usage` shows the 5-hour and weekly limits of all of them.

**An inline UI.** No full-screen takeover, so scrollback, search and copy work as usual. Markdown, code and diffs render in place, and a small footer shows the model, git branch, context use and token totals. You can keep typing while the agent works. Enter steers the running turn and Alt+Enter queues a follow-up.

**Tools.** `read`, `write`, `edit`, `bash`, `grep`, `glob`, `web_search`, parallel subagents and a live todo list. One edit call can change several places in a file. Bash shows its output live, and `background: true` keeps a dev server running between turns. Read-only calls run in parallel.

**Guard rails.**
- After every edit tego runs a syntax check for the language and shows the model what broke.
- An edit fails if the file changed since the agent read it, and the error carries the current content so the retry works.
- `/undo` puts the files back as they were before any prompt of the session. Snapshots live in a separate git repository under `~/.tego/snapshots/`, so your own `.git` stays untouched.
- Plan mode (Shift+Tab or `/plan`) lets the agent read and run read-only commands, then propose a plan. Nothing changes until you approve it.
- The bash tool refuses `rm -rf ~`, force pushes to `main`, writes to raw disks and a few other disasters. This is a guard rail, not a sandbox.

**Sessions you can branch.** Sessions are append-only trees in `~/.tego/sessions/`. Press Esc twice to rewind to an earlier prompt and edit it, `/tree` to switch branches, `/fork` to start a new session from any point.

**Skills, commands and MCP.** Skills use the same `SKILL.md` format as Claude Code and are found in the usual `.agents`, `.claude` and `.tego` folders. MCP servers come from `~/.tego/mcp.json` or the project's `.mcp.json` (stdio, HTTP and SSE). `AGENTS.md` and `CLAUDE.md` load automatically.

**Usage stats.** Every request is logged to `~/.tego/stats.jsonl` without prompt text. `tego stats` prints a summary, and `tego stats serve` opens a local dashboard with cost, cache use, speed and failures per model, project and tool. Costs are estimates at API list prices from [models.dev](https://github.com/anomalyco/models.dev).

## Token use

tego tries hard not to resend what the model already has.

- MCP tools stay out of the prompt until the model searches for them. With 63 tools connected, that took tool definitions from about 18.9k to 3.4k tokens per request.
- When the Anthropic prompt cache has expired anyway, tego trims old tool output before the next request instead of paying to cache it again. On Claude 4.5 and newer, the API also clears old tool results once a prompt passes 200k tokens. Replaying four long Opus sessions, the two together cut prompt cost by about 23%. That is an estimate, and it already counts the files the model reads again afterwards.
- `grep` returns 100 lines per page, clipped at 200 columns, and a cut result ends with match counts per file and an `offset` for the next page.
- Through OpenRouter, Claude, Gemini and Qwen only cache with a marker, so tego sends one, plus a session id that keeps the conversation on the same provider and its cache.
- Only when the context still nears the window does tego ask the model for a summary, and the last 20k tokens always stay verbatim.
- The system prompt holds only what stays fixed for a project. The date and git branch go after your prompt, so a resume on another day keeps the cache.

In my own benchmarks, 3 runs per task, the results depend a lot on the repository. On small Python and Go tasks tego cost 44% less than oh-my-pi with Sonnet 5.5. In every run tego's process used 18 to 26 MB of memory, against 250 to 400 MB for oh-my-pi.

## Configuration

`~/.tego/config.json`. Every key is optional.

```json
{
  "model": "anthropic/claude-sonnet-5-5",
  "thinking": "medium",
  "subagents": 8,
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

The model and thinking level you pick with `/model` are saved here. Thinking goes from `off` through `low`, `medium` and `high` to `xhigh` and `max`, which the newest Claude and GPT models support. On a model without them, tego runs the highest level that model has and says so. API keys can also come from the environment (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `OPENROUTER_API_KEY`, `GEMINI_API_KEY` and others). Credentials are stored in `~/.tego/auth.json` with mode 0600.

## Commands

| Command | Does |
| --- | --- |
| `/model`, `/thinking` | Switch model or thinking level, e.g. `/model opus:high` |
| `/resume`, `/new` | Pick, delete or rename sessions from any project |
| `/tree`, `/fork`, `/clone` | Branch a session |
| `/undo` | Restore the files from before a prompt |
| `/plan [prompt]` | Plan mode on or off (also Shift+Tab) |
| `/compact`, `/retry` | Summarise the context, retry a failed turn |
| `/login`, `/accounts`, `/usage` | Accounts and subscription limits |
| `/stats`, `/mcp`, `/skills` | Usage, MCP servers, skills |

Type `/` for the full list. `@` attaches a file, `!cmd` runs a shell command yourself, and Ctrl+V pastes an image.

## What tego leaves out

A full-screen UI, LSP integration, browser control, MCP OAuth and git worktrees. Some of these may come later. If you need one, open an issue with your use case.

## Contributing

Bug reports and pull requests are welcome. Run `go vet ./...` and `go test ./...` before you send one. CI runs the same on Linux and macOS. The binary should stay small, so a new dependency needs a good reason.

## License

MIT, see [LICENSE](LICENSE).
