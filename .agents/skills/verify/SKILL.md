---
name: verify
description: How to check a change to tego before reporting it done or committing it - gofmt, vet, tests, race tests, then a real run of the changed path in tmux, and cleanup.
---

# Verify a change to tego

Unit tests use fake providers and a fake terminal. They miss what the API refuses (headers, betas, request fields), what the screen shows, and timing. So a change is checked twice: the suite, then a real run of the path it changed.

## 1. The suite

```sh
gofmt -l cmd internal extension    # prints nothing
go vet ./...
go test ./... -count=1
```

When the change touches goroutines, the agent loop, background tasks or the terminal, also:

```sh
go test -race ./internal/agent ./cmd/tego -count=1
```

A test that fails now and then is a bug to find, not to run again.

## 2. A real run

Build a binary of its own, and run it in a tmux server of its own, outside herdr, in a scratch folder:

```sh
go build -o /tmp/tego-verify ./cmd/tego
mkdir -p /tmp/tego-verify-run && cd /tmp/tego-verify-run && git init -q
env -u HERDR_ENV -u HERDR_PANE_ID -u HERDR_BIN_PATH -u HERDR_TAB_ID -u HERDR_SOCKET_PATH -u HERDR_WORKSPACE_ID -u TMUX \
  tmux -L verify new-session -d -s v -x 160 -y 45 -c /tmp/tego-verify-run \
  "/tmp/tego-verify -m anthropic/claude-haiku-5-5 -t off"
sleep 4
tmux -L verify send-keys -t v "<a prompt or /command that runs the changed path>" Enter
sleep 15
tmux -L verify capture-pane -p -t v | grep -v '^\s*$' | tail -30
```

- Haiku with thinking off is cheap and quick. Use the model the change is about when it depends on one: thinking, betas, a provider.
- `tego -p "…"` paths: run `/tmp/tego-verify -p "…"` directly.
- Editor (ACP) paths: `cmd/tego/acp_*_test.go` drive a real ACP server with a scripted model; add a test there.
- Speed or cost changes: run the installed `tego` and `/tmp/tego-verify` on the same input, twice each, and compare. `~/.tego/stats.jsonl` has each request's tokens and timings.
- Requests that failed show in the screen, and in `~/.tego/stats.jsonl` with `"stop":"error"`.

## 3. Clean up

The user's disk is not for test leftovers:

```sh
tmux -L verify kill-server
rm -rf /tmp/tego-verify /tmp/tego-verify-run ~/.tego/sessions/private-tmp-tego-verify-run* ~/.tego/snapshots/tego-verify-run-*
```

## 4. Report

Say what you ran and what you saw. Say what you could not check, and why.
