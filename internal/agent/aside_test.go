package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/akbxr/tego/internal/llm"
)

// asideProvider answers side questions with "42" and records requests.
type asideProvider struct {
	mu    sync.Mutex
	reqs  []*llm.Request
	tools int // side questions to answer with a tool call only
}

func (p *asideProvider) Name() string { return "fake" }

func (p *asideProvider) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, req)
	if req.Class == llm.ClassAuxiliary && p.tools > 0 {
		p.tools--
		return &llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "x", Name: "read", Input: []byte(`{}`)}}}, nil
	}
	text := "done"
	if req.Class == llm.ClassAuxiliary {
		text = "42"
	}
	return &llm.Message{Role: llm.RoleAssistant, StopReason: "end_turn", Usage: &llm.Usage{Output: 1}, Blocks: []llm.Block{{Type: llm.BlockText, Text: text}}}, nil
}

func TestAside(t *testing.T) {
	prov := &asideProvider{}
	a := &Agent{Provider: prov, UI: nopUI{}, Model: "m", System: "sys", Thinking: "high", Asides: true}
	ctx := context.Background()
	if _, err := a.AsideRequest(ctx, "q", true); err == nil {
		t.Fatal("a busy aside before anything was sent")
	}
	if err := a.Run(ctx, "build it"); err != nil {
		t.Fatal(err)
	}
	turn := prov.reqs[0]

	// While a turn runs: the request it sent with a cache breakpoint where
	// it ended, the answer so far, then the question.
	req, err := a.AsideRequest(ctx, "what is the answer?", true)
	if err != nil {
		t.Fatal(err)
	}
	if req.Class != llm.ClassAuxiliary || req.CachePrefix != len(turn.Messages) || len(req.Messages) != len(turn.Messages)+2 ||
		textOf(&req.Messages[len(turn.Messages)]) != "done" ||
		req.System != "sys" || req.Thinking != "high" || req.Model != "m" {
		t.Fatalf("aside request: class %s prefix %d of %d messages", req.Class, req.CachePrefix, len(req.Messages))
	}
	q := req.Messages[len(req.Messages)-1].Blocks[0].Text
	if !strings.Contains(q, "while you work on the task above") || !strings.HasSuffix(q, "Question: what is the answer?\n</btw>") {
		t.Fatalf("question %q", q)
	}
	answer, err := a.AskAside(ctx, "what is the answer?", req)
	if err != nil || answer != "42" {
		t.Fatalf("answer %q, %v", answer, err)
	}
	if n := len(a.Messages); n != 2 || textOf(&a.Messages[1]) != "done" {
		t.Fatalf("the conversation changed: %d messages", n)
	}

	// Idle: the conversation as the next prompt sends it, the side thread so far.
	req, err = a.AsideRequest(ctx, "and why?", false)
	if err != nil {
		t.Fatal(err)
	}
	if req.CachePrefix != 2 || textOf(&req.Messages[1]) != "done" {
		t.Fatalf("idle aside: prefix %d", req.CachePrefix)
	}
	if q := req.Messages[2].Blocks[0].Text; !strings.Contains(q, "Q: what is the answer?\nA: 42") || strings.Contains(q, "while you work") {
		t.Fatalf("thread %q", q)
	}

	// A tool call is not run: told so once, the model answers in text.
	prov.tools = 1
	if answer, err := a.AskAside(ctx, "and why?", req); err != nil || answer != "42" {
		t.Fatalf("after a tool call: %q, %v", answer, err)
	}
	last := prov.reqs[len(prov.reqs)-1].Messages
	if res := last[len(last)-1].Blocks[0]; res.Type != llm.BlockToolResult || res.Content != asideNotRun || last[len(last)-2].Blocks[0].Type != llm.BlockToolUse {
		t.Fatalf("retry: %+v", res)
	}
	prov.tools = 2
	if _, err := a.AskAside(ctx, "and why?", req); err == nil || !strings.Contains(err.Error(), "wanted to call read") {
		t.Fatalf("tool answers: %v", err)
	}

	// While the turn's tool calls run, the side question sees them.
	call := llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "running it"},
		{Type: llm.BlockToolUse, ID: "b1", Name: "bash", Input: []byte(`{"command":"sleep 25"}`)}}}
	a.noteAside(&llm.Request{Model: "m", Messages: a.Messages})
	a.noteAsideReply(&call)
	req, err = a.AsideRequest(ctx, "what are you running?", true)
	if err != nil {
		t.Fatal(err)
	}
	n := len(req.Messages)
	if req.CachePrefix != 2 || n != 4 || textOf(&req.Messages[2]) != "running it" {
		t.Fatalf("busy with calls: prefix %d of %d", req.CachePrefix, n)
	}
	if res := req.Messages[3].Blocks; len(res) != 2 || res[0].ToolUseID != "b1" || res[0].Content != asideRunning || !strings.Contains(res[1].Text, "what are you running?") {
		t.Fatalf("busy with calls: %+v", res)
	}

	// A new history ends the side thread.
	a.SetHistory(nil)
	if _, err := a.AsideRequest(ctx, "x", true); err == nil {
		t.Error("the last turn's request outlived the history")
	}
	if _, err := a.AsideRequest(ctx, "x", false); err == nil {
		t.Error("an aside about an empty conversation")
	}
}

// Only agents that keep asides copy each request.
func TestAsideOff(t *testing.T) {
	a := &Agent{Provider: &asideProvider{}, UI: nopUI{}, Model: "m"}
	if err := a.Run(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if a.aside.sent != nil {
		t.Error("kept without Asides")
	}
}
