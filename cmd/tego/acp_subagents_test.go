package main

import (
	"context"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/acp"
	"github.com/akbxr/tego/internal/llm"
)

// An editor that shows subagents (clientCapabilities.subagents) gets each
// one as a session of its own: announced on the parent, then its task, its
// tool calls (and their terminal) and its answer on its own session, and
// its end. Others get its calls in the session, labelled with its name.
func TestACPSubagentSessions(t *testing.T) {
	run := func(caps acp.ClientCapabilities) (*editor, string) {
		home, project := t.TempDir(), t.TempDir()
		model := &scripted{replies: []func(ctx context.Context) *llm.Message{
			useTool("c1", "task", map[string]any{"tasks": []map[string]string{{"name": "Digger", "prompt": "dig into parse.go"}}}),
			useTool("c2", "bash", map[string]any{"command": "echo hi"}),
			func(context.Context) *llm.Message { return say("found it in parse.go") },
			func(context.Context) *llm.Message { return say("the subagent found it") },
		}}
		ed := startACPWith(t, home, model, caps)
		sid := newACPSession(t, ed, project)
		if out := ed.prompt(sid, "find the bug"); !strings.Contains(out, "the subagent found it") {
			t.Fatalf("answer %q", out)
		}
		return ed, sid
	}
	caps := acp.ClientCapabilities{FS: acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true}, Terminal: true}

	ed, sid := run(caps)
	if len(ed.seen("subagent_update")) != 0 {
		t.Fatal("subagent sessions for an editor that doesn't show them")
	}
	labelled := false
	for _, c := range ed.seen("tool_call") {
		labelled = labelled || c["title"] == "[Digger] bash echo hi" && c["_session"] == sid
	}
	if !labelled {
		t.Fatalf("subagent call not in the session: %v", ed.seen("tool_call"))
	}

	caps.Subagents = &struct{}{}
	ed, sid = run(caps)
	ups := ed.seen("subagent_update")
	if len(ups) != 2 {
		t.Fatalf("subagent updates %v", ups)
	}
	child, _ := ups[0]["sessionId"].(string)
	state := func(u map[string]any) map[string]any { m, _ := u["state"].(map[string]any); return m }
	if child == "" || child == sid || ups[0]["_session"] != sid || ups[0]["title"] != "Digger" || state(ups[0])["state"] != "running" {
		t.Fatalf("announce %v", ups[0])
	}
	if ups[1]["sessionId"] != child || state(ups[1])["state"] != "idle" || state(ups[1])["stopReason"] != "end_turn" {
		t.Fatalf("end %v", ups[1])
	}
	on := func(kind, session string) string {
		var b strings.Builder
		for _, u := range ed.seen(kind) {
			if u["_session"] == session {
				c, _ := u["content"].(map[string]any)
				s, _ := c["text"].(string)
				b.WriteString(s)
				if s == "" {
					t, _ := u["title"].(string)
					b.WriteString(t + "\n")
				}
			}
		}
		return b.String()
	}
	if got := on("user_message_chunk", child); got != "dig into parse.go" {
		t.Fatalf("task on its session: %q", got)
	}
	if got := on("tool_call", child); got != "bash echo hi\n" {
		t.Fatalf("calls on its session: %q", got)
	}
	if got := on("agent_message_chunk", child); got != "found it in parse.go" {
		t.Fatalf("answer on its session: %q", got)
	}
	if got := on("agent_message_chunk", sid); strings.Contains(got, "found it in parse.go") || !strings.Contains(got, "the subagent found it") {
		t.Fatalf("parent's answer: %q", got)
	}
	if strings.Contains(on("tool_call", sid), "echo hi") {
		t.Fatal("the subagent's call shown on the parent too")
	}
	for _, u := range ed.seen("tool_call_update") {
		if u["toolCallId"] == "c2" && u["_session"] != child {
			t.Fatalf("an update of its call on %v", u["_session"])
		}
	}
	// The terminal of its command belongs to its session.
	ed.mu.Lock()
	cmds, on2 := ed.commands, ed.termOn
	ed.mu.Unlock()
	if len(cmds) != 1 || !strings.Contains(cmds[0], "echo hi") || on2[0] != child {
		t.Fatalf("terminal %q on %q", cmds, on2)
	}
}
