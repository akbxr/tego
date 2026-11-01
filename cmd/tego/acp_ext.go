package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/akbxr/tego/internal/acp"
)

// Extensions in an editor: they run as in the terminal (extensions.go);
// what they ask of the user goes through the editor. A notice shows in the
// thread, confirm and select become a permission request, and input, which
// ACP has no way to ask, is answered as cancelled.

// watchExtensions tells the editor about the session's extensions once
// they started: their commands, the ones that did not start, and the
// project's that wait to be trusted.
func (sess *acpSession) watchExtensions(untrusted []string) {
	select { // the editor knows the session once it is announced
	case <-sess.announced:
	case <-sess.life.Done():
		return
	}
	if len(untrusted) > 0 {
		sess.OnInfo(fmt.Sprintf("this project has %s not trusted yet (%s): /extensions shows them, /extensions trust runs them",
			plural(len(untrusted), "extension"), strings.Join(untrusted, ", ")))
	}
	failed := sess.app.extensionFailures()
	if sess.life.Err() != nil {
		return // closed meanwhile
	}
	for _, msg := range failed {
		sess.OnInfo(msg)
	}
	if len(sess.app.ext.Commands()) > 0 {
		sess.announceCommands()
	}
}

// announceCommands sends the commands the session offers now.
func (sess *acpSession) announceCommands() {
	sess.notify(acp.CommandsUpdate{SessionUpdate: "available_commands_update", AvailableCommands: sess.commandList()})
}

// acpExtHost answers what extensions ask of tego in an editor.
type acpExtHost struct{ sess *acpSession }

// acpQueueMax caps the prompts extensions may queue, like the terminal's.
const acpQueueMax = 16

func (h acpExtHost) Notify(name, msg, level string) {
	line := fmt.Sprintf("[%s] %s", name, msg)
	if level == "error" {
		line = "error: " + line
	}
	h.sess.OnInfo(line)
}

func (h acpExtHost) Confirm(ctx context.Context, name, title, msg string) (bool, error) {
	id := h.sess.ask(ctx, "["+name+"] "+title, msg, []acp.PermissionOption{
		{OptionID: "yes", Name: "Yes", Kind: "allow_once"},
		{OptionID: "no", Name: "No", Kind: "reject_once"},
	})
	return id == "yes", nil
}

func (h acpExtHost) Select(ctx context.Context, name, title string, options []string) (int, error) {
	opts := make([]acp.PermissionOption, 0, len(options)+1)
	for i, o := range options {
		opts = append(opts, acp.PermissionOption{OptionID: strconv.Itoa(i), Name: o, Kind: "allow_once"})
	}
	opts = append(opts, acp.PermissionOption{OptionID: "none", Name: "None of these", Kind: "reject_once"})
	id := h.sess.ask(ctx, "["+name+"] "+title, "", opts)
	if i, err := strconv.Atoi(id); err == nil && i >= 0 && i < len(options) {
		return i, nil
	}
	return -1, nil
}

// Input: ACP can't ask for text, so the question is cancelled at once.
func (h acpExtHost) Input(_ context.Context, name, title, _ string) (string, bool, error) {
	h.sess.OnInfo(fmt.Sprintf("[%s] asked for text, which an editor can't answer: %s", name, title))
	return "", false, nil
}

// SendMessage queues text as a prompt: it runs right after the running
// prompt, or, when the session is idle, after the next one (ACP has no
// turn the agent starts by itself).
func (h acpExtHost) SendMessage(name, text string) {
	sess := h.sess
	if strings.TrimSpace(text) == "" {
		return
	}
	sess.mu.Lock()
	idle, full := sess.run == nil, len(sess.queued) >= acpQueueMax
	if !full {
		sess.queued = append(sess.queued, text)
	}
	sess.mu.Unlock()
	switch {
	case full:
		sess.OnInfo(fmt.Sprintf("error: [%s] too many messages queued; dropped: %s", name, text))
	case idle:
		sess.OnInfo(fmt.Sprintf("[%s] sent a prompt, which runs after your next one: %s", name, text))
	}
}

// ask puts a question to the user as a permission request, shown as a
// tool call of its own, and returns the option chosen: "" when the
// editor cancelled it or the session closed.
func (sess *acpSession) ask(ctx context.Context, title, detail string, opts []acp.PermissionOption) string {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(sess.life, cancel)()
	id := fmt.Sprintf("ext-ask-%d", sess.asks.Add(1))
	call := acp.ToolCall{SessionUpdate: "tool_call", ToolCallID: id, Title: title, Kind: "other", Status: "pending"}
	if detail != "" {
		block := acp.Text(detail)
		call.Content = []acp.ToolCallContent{{Type: "content", Content: &block}}
	}
	sess.notify(call)
	var resp acp.PermissionResponse
	err := sess.srv.conn.Call(ctx, "session/request_permission", acp.PermissionRequest{
		SessionID: sess.id,
		ToolCall:  acp.ToolCall{ToolCallID: id, Title: title, Kind: "other", Status: "pending"},
		Options:   opts,
	}, &resp)
	chosen := ""
	if err == nil && resp.Outcome.Outcome == "selected" {
		chosen = resp.Outcome.OptionID
	}
	status := "completed"
	if chosen == "" {
		status = "failed"
	}
	sess.notify(acp.ToolCall{SessionUpdate: "tool_call_update", ToolCallID: id, Status: status})
	return chosen
}
