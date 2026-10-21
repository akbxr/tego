# Discord status for tego

Shows what tego is doing as your Discord status, with the time since the session started:

```
Playing tego
Working on my-app(Opus 5.5)
Editing code
⏱ 12:04 elapsed
```

It shows the model you use, and follows `/model`. The status changes as tego thinks, reads, edits, runs commands, searches and runs subagents, and it clears when tego exits. If Discord isn't running, the extension checks again every 30 seconds.

## Setup

1. Create an application at <https://discord.com/developers/applications>. Its name is what Discord shows after "Playing", so call it `tego`. You can upload an icon under General Information.
2. Copy its Application ID into `~/.tego/discord.json`:

   ```json
   { "client_id": "123456789012345678" }
   ```

3. Copy this folder to `~/.tego/extensions/discord`, or link it there from a clone:

   ```sh
   ln -s "$PWD" ~/.tego/extensions/discord
   ```

tego builds the extension with your Go toolchain the first time it starts. Run `/discord` in tego to see whether it is connected.

## Options

In `~/.tego/discord.json`:

| Key | Default | |
|---|---|---|
| `client_id` | | The application's id. `TEGO_DISCORD_CLIENT_ID` overrides it. |
| `show_project` | `true` | Show the project's folder name. With `false`, the status says "Using Opus 5.5", or "Coding with tego" when the model is hidden too. |
| `show_model` | `true` | Show the model. |
| `show_details` | `false` | Show file names and commands, such as "Editing main.go" or "Running go test ./...", instead of "Editing code" and "Running a command". Anyone who can see your status sees them. |
| `large_image` | the app's icon | An art asset's key from the developer portal, or an `https://` image URL. |
| `large_text` | `tego` | The image's tooltip. |

Discord accepts about five updates per 20 seconds, so the status changes at most every 4 seconds and always shows the latest state.
