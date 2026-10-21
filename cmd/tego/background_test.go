package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// bgScript plays a session that starts a background task: the main agent
// asks for it and goes on; the subagent reports after a while; the main
// agent answers the report.
type bgScript struct {
	mu       sync.Mutex
	requests []string // the last user text of each main-agent request
}

func (p *bgScript) Name() string { return "fake" }

func (p *bgScript) Stream(ctx context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	text := func(s string) *llm.Message {
		return &llm.Message{Role: llm.RoleAssistant, StopReason: "end_turn", Blocks: []llm.Block{{Type: llm.BlockText, Text: s}}}
	}
	if first := req.Messages[0].Blocks[0].Text; first == "dig" { // the subagent
		select {
		case <-time.After(150 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return text("found the bug in parse.go"), nil
	}
	last := req.Messages[len(req.Messages)-1]
	var lastText string
	for _, b := range last.Blocks {
		lastText += b.Text + b.Content
	}
	p.mu.Lock()
	p.requests = append(p.requests, lastText)
	p.mu.Unlock()
	switch {
	case strings.Contains(lastText, "Background tasks you started ended"):
		return text("the subagent found the bug"), nil
	case last.Blocks[0].Type == llm.BlockToolResult:
		return text("started it"), nil
	}
	in, _ := json.Marshal(map[string]any{"background": true, "tasks": []map[string]string{{"name": "Digger", "prompt": "dig"}}})
	return &llm.Message{Role: llm.RoleAssistant, StopReason: "tool_use",
		Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "c1", Name: "task", Input: in}}}, nil
}

func newBGApp(t *testing.T, prov llm.Provider) *app {
	dir := t.TempDir()
	a := &app{cwd: dir, ui: &termUI{out: io.Discard, diag: io.Discard, atBOL: true, width: func() int { return 80 }}}
	a.env = &tools.Env{Cwd: dir}
	a.ag = &agent.Agent{Provider: prov, UI: a.ui, Notices: a.env.ShellNotices}
	task := &agent.TaskTool{Parent: a.ag, Env: a.env}
	a.setupBackground(task)
	a.ag.Tools = []tools.Tool{task, &agent.TaskStatusTool{BG: a.bg}}
	t.Cleanup(func() { a.bg.Close(5 * time.Second) })
	return a
}

// tego -p: the turn ends while the task runs; tego waits for it and lets
// the agent answer its report before exiting.
func TestPrintModeWaitsForBackgroundTasks(t *testing.T) {
	prov := &bgScript{}
	a := newBGApp(t, prov)
	ctx := context.Background()
	if err := a.ag.Run(ctx, "find the bug"); err != nil {
		t.Fatal(err)
	}
	if got := a.ag.LastText(); got != "started it" {
		t.Fatalf("first turn: %q", got)
	}
	if err := a.drainBackground(ctx); err != nil {
		t.Fatal(err)
	}
	if got := a.ag.LastText(); got != "the subagent found the bug" {
		t.Fatalf("after the report: %q", got)
	}
	last := prov.requests[len(prov.requests)-1]
	if !strings.Contains(last, "## t1 Digger (done)") || !strings.Contains(last, "found the bug in parse.go") {
		t.Fatalf("report turn: %s", last)
	}
	if a.bg.Pending() || a.bg.Running() != 0 {
		t.Fatal("tasks left after draining")
	}
	select { // OnDone runs just after the task's state says it ended
	case <-a.bgDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the REPL was not woken")
	}
}

// The footer counts and shows background tasks while they run.
func TestFooterShowsBackgroundTasks(t *testing.T) {
	prov := &slowFake{release: make(chan struct{})}
	a := newBGApp(t, prov)
	a.footerAgent.Store(a.ag)
	in, _ := json.Marshal(map[string]any{"background": true, "tasks": []map[string]string{{"name": "Digger", "prompt": "dig"}}})
	out, err := a.ag.Tools[0].Run(context.Background(), in)
	if err != nil || !strings.Contains(out, "t1 Digger") {
		t.Fatalf("%s %v", out, err)
	}
	if n := a.runningAgents(); n != 1 {
		t.Fatalf("running %d", n)
	}
	rows := agentsPanel(palette{}, append(a.ag.Subagents(), a.runningBackground()...), 80, a.cwd)
	if len(rows) < 2 || !strings.Contains(rows[1], "Digger ⇢t1") {
		t.Fatalf("panel %q", rows)
	}
	close(prov.release)
	if err := a.bg.Wait(context.Background(), nil, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if n := a.runningAgents(); n != 0 {
		t.Fatalf("still running %d", n)
	}
	// A new session stops what runs and forgets the rest.
	a.resetBackground()
	if len(a.bg.Statuses()) != 0 || a.bg.Pending() {
		t.Fatal("tasks survived a new session")
	}
}

type slowFake struct{ release chan struct{} }

func (p *slowFake) Name() string { return "fake" }
func (p *slowFake) Stream(ctx context.Context, _ *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &llm.Message{Role: llm.RoleAssistant, StopReason: "end_turn", Blocks: []llm.Block{{Type: llm.BlockText, Text: "ok"}}}, nil
}
