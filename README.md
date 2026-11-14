<h1 align="center"><img src="assets/tego.svg" alt="tego" width="420"></h1>

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

**Your subscriptions.** Log in with Claude Pro/Max, ChatGPT Plus/Pro or a Google account through Antigravity, or use an API key for any OpenAI-compatible provider. Ollama and LM Studio need no key. With several accounts per provider, tego moves to the next one when an account hits its limit. `tego usage` shows the 5-hour and weekly limits of all of them. Weekly quota left at an account's reset is lost, so `/accounts expiring` (`"accounts": "expiring"`) sends a new prompt to the account that would lose the most per hour if unused: the share of its week left divided by the hours until it resets, skipping accounts whose 5-hour window is 90% used. A prompt whose session's cache is still warm stays on its account, since moving writes the whole cache again. Claude's answers carry their account's limits, and all tego processes share what they report in `~/.tego/limits.json`; Anthropic's usage endpoint, which refuses all but a few calls, is asked only for an account no answer reported on in the last hour, and not for an hour after it refused.

**An inline UI.** No full-screen takeover, so scrollback, search and copy work as usual. Markdown, code and diffs render in place. A footer shows the model, git branch, context use and tokens. You can type while the agent works. Enter steers the running turn and Alt+Enter queues a follow-up. `/btw <question>` is answered at once from the conversation so far, the tool calls running included, without tools and without the turn or the next prompt seeing it; it reads the conversation's prompt cache, so on a 13k-token conversation a side question cost 150 new input tokens. Questions in a row form a side thread.

**Tools.** `read`, `write`, `edit`, `bash`, `grep`, `glob`, `web_search`, parallel subagents and a live todo list. One edit call can change several places in a file. Bash streams its output, and `background: true` keeps a dev server running between turns. Read-only calls run in parallel.

**Background shells.** A background shell counts as running while any process it started is alive. A plain command that leaves something running, such as `npm run dev &`, has it kept as a background shell too, so nothing outlives tego unseen. When a turn ends with shells it started still running, tego reminds the model once to stop them or tell you where they run. The header shows the port of a lone server (`▶ bg :3000`). `/tasks` shows each shell's output and lets you stop it. A subagent's shells stop when it finishes, and all of them stop when tego exits. If tego crashes, the next tego offers to stop what it left running. `web_search` uses the provider's own search on Anthropic, Codex and Antigravity, and Exa, Brave, Tavily or DuckDuckGo elsewhere.

**Guard rails.**
- Permission modes decide which calls wait for you. `yolo`, the default, runs everything. `edits` asks before shell commands and MCP tools that may change things. `ask` asks before file edits too. Reads and read-only commands such as `go test` never ask. "Don't ask again" covers that kind of call for the rest of the session, but never `sudo`, inline scripts, command substitution or a redirect into a file. No stops the turn, subagents included. No with a note lets the agent go on with what you wrote. Set the mode with `/permissions`, `--permissions` or `"permissions"`. With `-p` nobody can answer, so those calls are refused.
- After every edit, the file's language server reports the errors the edit left, wrong types and missing imports included. Servers that check the whole project, such as gopls, also report the callers it broke in other files. tego uses gopls, TypeScript 7's `tsc --lsp`, typescript-language-server, basedpyright or pyright, rust-analyzer, clangd, sourcekit-lsp and zls. It installs none of them: it looks in the project's `node_modules/.bin` or `.venv/bin`, then on your PATH. A server starts at the first edit of a file it checks and stops after 10 minutes unused. Without one, tego runs a syntax check instead.
- An edit fails if the file changed since the agent read it, and the error carries the current content.
- `/undo` restores the files from before any prompt of the session. Snapshots live in their own git repository under `~/.tego/snapshots/`, so your commits, branches and index stay untouched. Contents already committed in your `.git` are read from there instead of being stored twice. Snapshots are kept for 30 days, and those of folders that no longer exist are deleted. A folder that takes over 15 seconds to snapshot is not snapshotted for a week.
- Plan mode, Shift+Tab or `/plan`, lets the agent read and propose a plan. Nothing changes until you approve it.
- Bash refuses `rm -rf ~`, force pushes to `main`, writes to raw disks and a few other disasters. It is a guard rail, not a sandbox.

**Sessions you can branch.** Sessions are append-only trees in `~/.tego/sessions/`. Each image, video or audio clip is stored once per session file, however many compactions and branches repeat it; rewriting three image-heavy sessions this way took them from 217, 75 and 61 MB to 97, 50 and 34 MB. Press Esc twice to rewind to an earlier prompt and edit it, `/tree` to switch branches, `/fork` to start a new session from any point.

**Sessions that read and talk to each other.** The `sessions` tool lists your sessions, past and open now, searches what was said in them, and reads one from its start, compactions included, with media left out. So you can say "continue what the Blender session did" or "what did we decide about the cache yesterday" in any session. With `"session_messages": true`, open sessions also take messages from each other through `session_send`. A session whose turn runs reads a message before its next request. An idle session in a terminal starts a turn to answer it, and its answer comes back the same way. A message says which session sent it, and the model is told it comes from another session, not from you, so it can't grant permissions. Six messages a minute per session at most, and after four messages in a row between sessions, the next one waits for your prompt instead of starting a turn. An editor (`tego acp`) can't start a turn by itself, so there a message to an idle session goes with your next prompt.

**Worktrees.** `tego -w [name]` works in a git worktree of its own, on branch `tego/<name>`, so several sessions can change one repository at once. Worktrees go in `~/.tego/worktrees/`, outside the repository, so its build and test tools don't pick them up. Ignored files listed in `.worktreeinclude`, such as `.env`, are copied in. Dependencies are not, so run your install step there. On exit, a worktree with nothing in it is removed. One with work in it stays unless you remove it. `tego -w <name>` and `tego -r` go back to it, and `tego worktree` lists, removes (`rm`) or prunes them. Subagents can have one each too (`isolation: worktree` on a task or in an agent file), so several can edit the same files in parallel. Each commits on its own branch and its report says what to merge. A worktree a subagent left nothing in is removed. With `"subagent_merge": true`, the commits of a subagent that ended without an error are merged into the session's branch and its worktree removed. A merge that would conflict, or a subagent that left uncommitted changes, is left for you, as without the option.

**Background subagents.** For long work whose result the next step doesn't need, tego can start subagents in the background (`background: true` on a task call, or ask it to). The turn ends at once, so you keep chatting while they work, and the footer shows them. A report that comes while tego is working goes with its next request. One that comes while it is idle starts a turn that tells you the result. `task_status` lets tego check on them, wait for or stop them. `/tasks` shows what each did and lets you stop one. Exiting while some run asks first. `tego -p` waits for them before it exits.

**Program status (OSC 7501).** tego tells the terminal what it is doing through the [Program Status Protocol](https://www.superlogical.com/rex/docs/build/program-status), so [herdr-core](https://github.com/herdrdev/herdr-core) (`hc ls`, `hc get`, `hc wait`), herdr, Rex, kitty, tuios and cmux show its state without scraping the screen. The root record is `working` (with the prompt) while a turn or a background task runs, `blocked` (`kind=permission` or `question`, with what it asks) while an approval, a plan or an extension waits for you, `done` (with the first line of the answer) when a turn finishes, until you act again, `error` when it fails, and `idle` otherwise. Each running subagent gets a child record `agents/<name>` with its latest tool call. `tego -p` reports only when stdout is a terminal and leaves `done` or `error` behind when it exits. Inside a herdr pane the same state, `blocked` included, also goes to herdr's `report-agent`. `TEGO_PROGRAM_STATUS=0` turns the escape sequences off.

**Skills, commands and MCP.** Skills use Claude Code's `SKILL.md` format and load from the usual `.agents`, `.claude` and `.tego` folders. MCP servers come from `~/.tego/mcp.json` or the project's `.mcp.json`, over stdio, HTTP or SSE. A local server runs only while it is used: tego keeps its tool list, starts it at the first call and stops it after 10 minutes unused (`"mcp_idle"`, or `"off"`). Give a server `"keepAlive": true` to keep it running all session. `AGENTS.md` and `CLAUDE.md` load automatically.

**Agents.** Subagents can be defined in Claude Code's format, as `agents/<name>.md` in the same folders: `name`, `description`, `tools`, `disallowedTools`, `model`, `effort`, `maxTurns` and `isolation` in the frontmatter, and the instructions as the body. The session's model picks one per task from its description. `model` takes Claude's aliases (`opus`, `sonnet`, `haiku`), `inherit`, `small`, a `subagent_models` name or any `provider/model`. Tools tego has no equivalent for, such as `WebFetch`, are ignored. `/agents defined` lists them.

**Extensions.** An extension can block or change tool calls, change their output and your prompts, add tools and slash commands, and ask you things. It is a TypeScript, JavaScript, Go or Python file, or any executable, in `~/.tego/extensions/` or the project's `.tego/extensions/`. A project's extensions only run once you trust them. TypeScript runs under bun or node with an API like pi's. Go uses the `github.com/akbxr/tego/extension` package, and tego builds it for you. tego adds no dependency for either. See [docs/extensions.md](docs/extensions.md).

**In your editor.** `tego acp` speaks the [Agent Client Protocol](https://agentclientprotocol.com), so Zed, JetBrains, Neovim, Emacs and other ACP editors can run tego with your logins and settings. Edits show as diffs, tools read the editor's open buffers including unsaved changes, and commands run in its terminals. The editor's mode, model and thinking pickers work, and `/` offers tego's commands, your custom commands, your extensions' commands and every skill. Extensions, `/undo`, background subagents (`/tasks`) and the prompt cache keep-alive work as in the terminal; ACP has no turns the agent starts itself, so a background report or a prompt an extension sends runs after the current prompt, or with your next one. A prompt the editor sends while a turn runs steers it as Enter does in the terminal, and both prompts are answered when the turn ends; so does claude-code-acp's `_session/steering` request. A `/` command, or a prompt sent after a stop, runs after the turn. Zed and T3 Code queue a message typed during a turn, or stop the turn and send it, so they don't use this yet. In an editor that shows subagents (`clientCapabilities.subagents`, the ACP subagent sessions draft, as Tern does), each subagent gets a session of its own with its task, answer, thinking, tool calls and terminals. Other editors show its tool calls in the session, labelled with its name. Sessions are shared, so `tego -r` opens them in the terminal too. In Zed, add to `settings.json`:

```json
"agent_servers": {
  "tego": { "type": "custom", "command": "tego", "args": ["acp"] }
}
```

**Usage stats.** Every request is logged to `~/.tego/stats.jsonl`, without prompt text. `tego stats` prints a summary and `tego stats serve` opens a local dashboard with cost, cache use, speed and failures per model, project and tool. `tego stats cache` (`/stats cache` in a session) lists prompt-cache writes that could have been reads: a request that wrote again what its session's previous request had cached, by cause (the API's reason, a keep-alive that missed, a model switch), with the costliest ones. Writes after the cache expired while idle, and after compaction or elision shrank the history, are expected and counted apart. Costs are estimates at API list prices from [models.dev](https://github.com/anomalyco/models.dev).

## Token use

tego tries hard not to resend what the model already has.

- MCP tools stay out of the prompt until the model searches for them. With 63 tools connected, that cut tool definitions from 18.9k to 3.4k tokens per request. On Claude this holds for every MCP tool, as Claude loads a found tool without the prompt cache changing: the nine tools of one Blender server were 6k tokens in every request of every session. Other providers get MCP tools inline while they take at most about 2k tokens, since loading one later rewrites their cache. `"mcp_tools": "eager"` or `"lazy"` sends them always or never.
- When the Anthropic cache has expired anyway, tego trims old tool output before the next request rather than cache it again. Images, videos and audio you attached before your last two prompts go too, outside the recent messages: the model gets a line with the file's path and can read it again. On Claude 4.5 and newer the API also clears old tool results past 200k tokens, in batches, so the requests in between still hit the cache. Replaying four long Opus sessions, the two cut prompt cost by about 23%, counting the files the model reads again.
- While tego stays open and you are away, it keeps an Anthropic prompt cache warm: shortly before the 1-hour cache would expire, it reads the conversation again with `max_tokens: 0`, at the cache read price (a twentieth of input on Opus 5.5), so your next prompt does not write it all again. It stops after 12 hours, or sooner once the refreshes would cost more than a cold cache. `"cache_keepalive"` takes a fixed time such as `"8h"`, or `"off"`. Contexts under 30k tokens are left to expire.
- When an Anthropic request should have read the cache and didn't, because something before the new messages changed, tego says what changed, using the API's cache diagnostics. For example, `prompt cache missed: the tool definitions changed since the last request (42k tokens not read from the cache)`. Changes tego makes itself, such as compaction, another model, or a thinking or speed switch, are not reported. The stats log records every reported miss.
- After a break of over an hour on Anthropic, a context still above 200k tokens is summarised before your next prompt instead of being cached again. On a 395k-token session, with the limit at 150k, that took the next cache write from 222k tokens to 32k. `/tree` brings back the full history. `"idle_compact": false` turns this off and `"idle_compact_tokens"` sets the limit.
- Near the end of the window the model summarises the conversation. The last 20k tokens stay as they were, and your own messages, the files changed, the skills in use and the todo list follow the summary word for word.
- The summary is asked for as the conversation's next turn, the last request repeated with one instruction after it, so it reads the prompt cache and sees every tool output in full. On a 61k-token Haiku session, 98% of the summary request came from the cache. When the cache may have expired, the model changed, or the history changed since the last request, tego summarises a transcript instead, with each tool output cut to 2,000 characters. It does the same when the transcript is much cheaper, as in a short session.
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
  "subagent_max_turns": 150,
  "subagent_model": "anthropic/claude-opus-5-5:xhigh",
  "subagent_watchdog": 40,
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

`subagent_max_turns` is how many requests a subagent may make, 150 by default. `0` means no limit. The last request has no tool calls: the subagent writes its report (what is done, what is left), so the session's model can go on from there. An agent file's `maxTurns` overrides it, up or down. With a limit of 20 or more, a subagent is told to start wrapping up a tenth of the limit before it (at least 5 requests before), so it finishes the change in progress instead of being cut off in the middle of it.

A background subagent can be given a message while it runs: the session's model calls `task_status` with `ids` and `message`, and the subagent reads it before its next request. This corrects or narrows a task without stopping it and losing what it has done.

A subagent that ended, whether stopped at its limit, by the watchdog, interrupted or done, can be resumed: the session's model calls `task` with `"resume": "<name or t3>"` and says what to do next, and the subagent goes on with its context instead of reading everything again. One that ran on the session's model runs on the model the session has now, so after `/model` it changes with the session. One with a model of its own keeps it. The last 8 are kept per session. Their histories are saved in the session file, gzipped, with each resume adding only what is new, so they can still be resumed after tego restarts (`tego -c`). A rewind to before a subagent ran forgets it, and going back to that branch brings it back. Worktree subagents can't be resumed, since their worktree is committed or removed when they end.

`subagent_watchdog` checks on a subagent every 40 requests by default (`0` turns it off). The small model reads its task and what it did since the last check, and says whether it is getting somewhere. The first time it is stuck (the same failure again and again, going in circles, or working on something else), the subagent is told and changes course. The second time in a row, it writes its report and stops. The check runs beside the subagent, so it never waits for it, and costs about one small request per 40.

`subagent_models` names more models for subagents, from other subscriptions too. The session's model picks one per task from its name and `use`, or you can ask for one ("review it with gpt"). `thinking` sets one's thinking level. Only models you are logged in for are offered, and a subagent whose model fails to start runs on the session's model and says so.

`subagent_model` is the model of a task that names none: a Claude alias (`opus`, `sonnet`, `haiku`, `fable`), any `provider/model`, or a `subagent_models` name, whose `thinking` then applies. A `:level` at the end sets the subagents' thinking level, as in `/model opus:high`; without one they think as much as the session. Without it, subagents run on the session's model. With it, the session's model can still ask for itself with `"model": "main"` when a task needs it most. So Fable can lead while the subagents run on Opus, at half the price per token and outside Fable's own weekly limit. An agent file's `model` wins over it (a `:level` works there too), and `inherit` means the session's model.

`"lsp": false` turns off the language servers. `lsp_servers` adds one, changes a built-in one by name or turns it off with `false`:

```json
"lsp_servers": {
  "luau-lsp": { "command": ["luau-lsp", "lsp"], "languages": { ".luau": "luau" }, "roots": [".luaurc"] },
  "clangd": false
}
```

A server also takes `env`, `settings` (its `workspace/configuration`) and `init_options`.

`/model` saves the model and thinking level here. Thinking goes from `off` to `max`, and on a model without the level you picked, tego uses the highest it has. `/fast` or `--fast` turns on Claude's fast mode for Opus 4.8 and newer. It gives up to 2.5 times the output speed at twice the price, and a subscription pays for it from usage credits only. If fast mode is rate limited, tego carries on at standard speed.

A response's output limit covers its thinking and its answer. On Claude it is 64k tokens, and the whole 128k at `xhigh` and `max` on models that can write that much. `max_output_tokens` sets your own. A response is billed for what it writes, not for its limit. When a response hits the limit without a tool call, tego goes on by itself up to twice a prompt. One that spent it all thinking is told to start acting. API keys can also come from the environment, such as `ANTHROPIC_API_KEY` or `OPENROUTER_API_KEY`. Logins are stored in `~/.tego/auth.json` with mode 0600.

## Commands

| Command | Does |
| --- | --- |
| `/model`, `/thinking` | Switch model or thinking level, e.g. `/model opus:high` |
| `/resume`, `/new` | Pick, delete or rename sessions from any project |
| `/tree`, `/fork`, `/clone` | Branch a session |
| `/btw <question>` | Ask about the session on the side, even while it works; the answer stays out of the conversation |
| `/undo` | Restore the files from before a prompt |
| `/plan [prompt]` | Plan mode on or off, also Shift+Tab |
| `/fast [on\|off]` | Fast mode on or off, Claude Opus 4.8+ |
| `/permissions [yolo\|edits\|ask]` | Which tool calls wait for your approval |
| `/compact`, `/retry` | Summarise the context, retry a failed turn |
| `/login`, `/accounts`, `/usage` | Accounts and subscription limits |
| `/stats`, `/mcp`, `/skills` | Usage, MCP servers, skills |
| `/restart` | Start tego again on this session, on the binary and config installed now |

A session can stay open for days. When the tego binary is replaced (an install or `tego update`) or `config.json` changes, tego says so once while it waits for a prompt. `/restart` then starts it again on the same session, keeping its model, thinking level, permission mode, fast mode and worktree. Background tasks and shells stop, after asking first, and a subagent stopped this way can be resumed after the restart.

Type `/` for the full list. `@` attaches a file, `!cmd` runs a shell command yourself, and Ctrl+V pastes an image. Drag in an image or a video, or type its path. Gemini models get the video itself with its sound, up to 20 minutes per read. A file up to 10 MB in mp4, mov, webm or 3gp (H.264, HEVC, VP8/9 or AV1) is sent as is. Other files, and parts of a video, are re-encoded to 1280px at 2 fps. Other models get frames with their timestamps: 6 to 16 spread over the video's length, without the sound. If you switch to such a model, earlier videos in the session are sent to it as frames. The model can look at part of a video again with `start` and `end` (and `frames`). Video needs `ffmpeg` on PATH. Claude takes at most 600 images and 32 MB a request: past that, requests leave out the oldest images a quarter of the limit at a time, each replaced by a line naming its file. The session keeps them.

Audio files work the same way: mp3, wav, m4a, aac, ogg, opus, flac, aiff and others. Gemini models hear the sound itself, up to an hour per read. A file up to 4 MB in mp3, wav, aiff, aac, ogg or flac is sent as is. Other files, and parts of a file, are re-encoded to mono 16 kHz mp3 at 24 kbps. Other models are told they can't hear the file. With `"transcribe": true` in the config they get a transcript with timestamps from a local whisper instead: `whisper-cli` (whisper.cpp, with a ggml model in `~/.cache/whisper.cpp` or `$WHISPER_CPP_MODEL`), `mlx_whisper` or `whisper` (openai-whisper), whichever is found first on PATH. Transcription is off by default because each whisper run loads a model of a GB or more. When it is on, one whisper runs at a time across all tego processes. A file up to 30 minutes is transcribed whole, once, and transcripts are kept in `~/.tego/cache/transcripts`. These models also get a transcript of a video's sound along with its frames. The model can hear or transcribe part of a file with `start` and `end`.

## What tego leaves out

A full-screen UI, LSP navigation (go to definition, references), browser control and MCP OAuth. Some may come later. If you need one, open an issue with your use case.

## Contributing

Bug reports and pull requests are welcome. Run `go vet ./...` and `go test ./...` first. CI runs the same on Linux and macOS. The binary should stay small, so a new dependency needs a good reason.

## License

MIT, see [LICENSE](LICENSE).
