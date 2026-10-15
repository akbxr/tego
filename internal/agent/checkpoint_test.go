package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

type nopUI struct{}

func (nopUI) OnText(string)                                      {}
func (nopUI) OnThinking(string)                                  {}
func (nopUI) OnToolStart(string)                                 {}
func (nopUI) OnToolCall(string, json.RawMessage)                 {}
func (nopUI) OnToolResult(string, json.RawMessage, string, bool) {}
func (nopUI) OnTurnEnd(*llm.Message)                             {}
func (nopUI) OnInfo(string)                                      {}

func call(id, name, input string) llm.Block {
	return llm.Block{Type: llm.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}
}

func result(id, content string) llm.Block {
	return llm.Block{Type: llm.BlockToolResult, ToolUseID: id, Content: content}
}

// step mimics one loop iteration: append the assistant message, run its
// calls through the agent's tools, append results, apply a pending rewind.
func step(t *testing.T, a *Agent, blocks ...llm.Block) {
	t.Helper()
	a.Messages = append(a.Messages, llm.Message{Role: llm.RoleAssistant, Blocks: blocks})
	var res []llm.Block
	for _, b := range blocks {
		if b.Type == llm.BlockToolUse {
			res = append(res, a.execute(context.Background(), b))
		}
	}
	a.Messages = append(a.Messages, llm.Message{Role: llm.RoleUser, Blocks: res})
	a.applyRewind()
}

type echoTool struct{}

func (echoTool) Def() llm.Tool { return llm.Tool{Name: "read"} }
func (echoTool) Run(context.Context, json.RawMessage) (string, error) {
	return strings.Repeat("file contents ", 50), nil
}

func TestRewindKeepsCheckpointAndReportDropsExploration(t *testing.T) {
	a := &Agent{UI: nopUI{}}
	a.Tools = []tools.Tool{&TodoTool{Parent: a}, &CheckpointTool{Parent: a}, &RewindTool{Parent: a}, echoTool{}}
	a.Messages = []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "task"}}}}

	step(t, a, call("t1", "todo", `{"todos":[{"content":"explore","status":"in_progress"}]}`), call("c1", "checkpoint", `{"goal":"find x"}`))
	step(t, a, call("r1", "read", `{"path":"a"}`))
	step(t, a, call("r2", "read", `{"path":"b"}`))
	step(t, a, llm.Block{Type: llm.BlockThinking, Text: "done", Signature: "sig"},
		call("r3", "read", `{"path":"c"}`), call("w1", "rewind", `{"report":"x is in b:12"}`))

	got := a.Messages
	// prompt, checkpoint call, its results, rewind call, rewind result
	if len(got) != 5 {
		t.Fatalf("history has %d messages, want 5: %+v", len(got), got)
	}
	if got[1].Blocks[1].Name != "checkpoint" || got[2].Blocks[1].ToolUseID != "c1" {
		t.Fatalf("checkpoint call/result not kept: %+v %+v", got[1], got[2])
	}
	rw := got[3]
	if len(rw.Blocks) != 2 || rw.Blocks[0].Type != llm.BlockThinking || rw.Blocks[1].ID != "w1" {
		t.Fatalf("rewind message should keep thinking + rewind call only, got %+v", rw.Blocks)
	}
	if len(got[4].Blocks) != 1 || got[4].Blocks[0].ToolUseID != "w1" ||
		!strings.Contains(got[4].Blocks[0].Content, "(4 messages of tool calls") ||
		!strings.Contains(got[4].Blocks[0].Content, "▸ explore") {
		t.Fatalf("rewind result: %+v", got[4].Blocks)
	}
	// Every tool_use must be answered by the next message (provider invariant).
	for i, m := range got {
		for _, b := range m.Blocks {
			if b.Type != llm.BlockToolUse {
				continue
			}
			found := false
			for _, r := range got[i+1].Blocks {
				found = found || r.ToolUseID == b.ID
			}
			if !found {
				t.Fatalf("tool_use %s has no result", b.ID)
			}
		}
	}
	if a.cp.active || a.pendingRewind != nil {
		t.Fatal("checkpoint state not cleared")
	}
	if _, err := (&RewindTool{Parent: a}).Run(context.Background(), json.RawMessage(`{"report":"again"}`)); err == nil {
		t.Fatal("rewind without an active checkpoint must fail")
	}
}

func TestRewindNudgeOnce(t *testing.T) {
	a := &Agent{UI: nopUI{}}
	a.cp = checkpointState{active: true, goal: "g"}
	if !a.needsRewindNudge() {
		t.Fatal("first finish with open checkpoint should nudge")
	}
	if a.needsRewindNudge() || a.cp.active {
		t.Fatal("second finish should give up and clear the checkpoint")
	}
}
