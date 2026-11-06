package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// slowProvider answers every request after a short delay and records the
// peak number of concurrent requests.
type slowProvider struct {
	mu        sync.Mutex
	cur, peak int
}

func (p *slowProvider) Name() string { return "fake" }

func (p *slowProvider) Stream(ctx context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	p.mu.Lock()
	p.cur++
	p.peak = max(p.peak, p.cur)
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.cur--; p.mu.Unlock() }()
	select {
	case <-time.After(20 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	prompt := req.Messages[0].Blocks[0].Text
	return &llm.Message{Role: llm.RoleAssistant, StopReason: "end_turn",
		Blocks: []llm.Block{{Type: llm.BlockText, Text: "report for " + prompt}}}, nil
}

func TestTaskRunsMoreThanLimitInQueue(t *testing.T) {
	prov := &slowProvider{}
	parent := &Agent{Provider: prov, UI: nopUI{}}
	tool := &TaskTool{Parent: parent, Env: &tools.Env{Cwd: t.TempDir()}, Concurrency: 3}

	var tasks []map[string]string
	for i := range 12 {
		tasks = append(tasks, map[string]string{"name": fmt.Sprintf("T%d", i), "prompt": fmt.Sprintf("p%d", i)})
	}
	in, _ := json.Marshal(map[string]any{"tasks": tasks})
	out, err := tool.Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		if !strings.Contains(out, fmt.Sprintf("## T%d (done)\nreport for p%d", i, i)) {
			t.Fatalf("missing/wrong report %d in:\n%s", i, out)
		}
	}
	if prov.peak != 3 {
		t.Fatalf("peak concurrency %d, want 3", prov.peak)
	}
	// The UI's view: every subagent finished, in call order, with its
	// request counted and its answer kept.
	subs := parent.Subagents()
	if len(subs) != 12 {
		t.Fatalf("%d subagent states", len(subs))
	}
	for i, s := range subs {
		if s.Name != fmt.Sprintf("T%d", i) || s.State != SubDone || s.Requests != 1 ||
			s.Text != fmt.Sprintf("report for p%d", i) || s.Started.IsZero() || s.Ended.Before(s.Started) {
			t.Fatalf("subagent %d = %+v", i, s)
		}
	}
}

// busyProvider calls a tool in every response until stopAt requests, then
// answers; it notes what the requests ended with.
type busyProvider struct {
	mu       sync.Mutex
	requests int
	stopAt   int // 0: never stops by itself
	lastSeen []string
}

func (p *busyProvider) Name() string { return "fake" }

func (p *busyProvider) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests++
	last := req.Messages[len(req.Messages)-1]
	p.lastSeen = append(p.lastSeen, last.Blocks[len(last.Blocks)-1].Text)
	if p.stopAt > 0 && p.requests >= p.stopAt {
		return &llm.Message{Role: llm.RoleAssistant, StopReason: "end_turn", Blocks: []llm.Block{{Type: llm.BlockText, Text: "all done"}}}, nil
	}
	in, _ := json.Marshal(map[string]string{"pattern": fmt.Sprintf("*.x%d", p.requests)}) // varied: not a stuck loop
	return &llm.Message{Role: llm.RoleAssistant, StopReason: "tool_use", Blocks: []llm.Block{
		{Type: llm.BlockText, Text: fmt.Sprintf("REPORT after %d", p.requests)},
		{Type: llm.BlockToolUse, ID: fmt.Sprintf("c%d", p.requests), Name: "glob", Input: in},
	}}, nil
}

// A subagent's last allowed request is told to report, its calls don't run,
// and the parent gets that report with the reason it stopped.
func TestSubagentTurnLimitReports(t *testing.T) {
	prov := &busyProvider{}
	parent := &Agent{Provider: prov, UI: nopUI{}}
	tool := &TaskTool{Parent: parent, Env: &tools.Env{Cwd: t.TempDir()}, MaxTurns: 3}
	in, _ := json.Marshal(map[string]any{"tasks": []map[string]string{{"name": "Worker", "prompt": "work"}}})
	out, err := tool.Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if prov.requests != 3 {
		t.Fatalf("%d requests, want 3", prov.requests)
	}
	if !strings.Contains(prov.lastSeen[2], "This is your last response") || strings.Contains(prov.lastSeen[1], "last response") {
		t.Fatalf("last-turn note: %q", prov.lastSeen)
	}
	if !strings.Contains(out, "failed: stopped at its limit of 3 turns") || !strings.Contains(out, "REPORT after 3") {
		t.Fatalf("report:\n%s", out)
	}
}

// subagent_max_turns 0: no limit.
func TestSubagentNoTurnLimit(t *testing.T) {
	prov := &busyProvider{stopAt: 200}
	parent := &Agent{Provider: prov, UI: nopUI{}}
	tool := &TaskTool{Parent: parent, Env: &tools.Env{Cwd: t.TempDir()}}
	in, _ := json.Marshal(map[string]any{"tasks": []map[string]string{{"name": "Worker", "prompt": "work"}}})
	out, err := tool.Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if prov.requests != 200 || !strings.Contains(out, "(done)\nall done") {
		t.Fatalf("%d requests:\n%s", prov.requests, out)
	}
}

// An agent definition's maxTurns overrides subagent_max_turns, up or down.
func TestAgentDefMaxTurns(t *testing.T) {
	for _, c := range []struct{ base, def, want int }{{150, 400, 400}, {150, 20, 20}, {0, 20, 20}, {0, 0, 0}, {150, 0, 150}} {
		sub := &Agent{MaxTurns: c.base}
		(&AgentDef{MaxTurns: c.def}).apply(sub)
		if sub.MaxTurns != c.want {
			t.Fatalf("base %d, def %d: %d, want %d", c.base, c.def, sub.MaxTurns, c.want)
		}
	}
}

func TestTodoPhases(t *testing.T) {
	list, err := ParseTodos(json.RawMessage(`{"todos":[
		{"content":"read","status":"completed","phase":" Review "},
		{"content":"fix","status":"in_progress","phase":"Fix"},
		{"content":"loose","status":"pending"},
		{"content":"review more","phase":"Review"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	// Phases keep the order they first appear in; later items join them.
	got := RenderTodos(list)
	want := "Todo 1/4\n1. Review 1/2\n✓ read\n○ review more\n2. Fix 0/1\n▸ fix\n3. Other 0/1\n○ loose\n"
	if got != want {
		t.Fatalf("RenderTodos:\n%s\nwant:\n%s", got, want)
	}
}

// A subagent near its limit is told to wrap up, once, a tenth of the
// limit before it (at least 5); low limits and no limit get no warning.
func TestSubagentWrapUp(t *testing.T) {
	for limit, want := range map[int]int{0: 0, 10: 0, 19: 0, 20: 15, 60: 54, 150: 135} {
		if got := wrapUpTurn(limit); got != want {
			t.Fatalf("wrapUpTurn(%d) = %d, want %d", limit, got, want)
		}
	}
	prov := &busyProvider{}
	parent := &Agent{Provider: prov, UI: nopUI{}}
	tool := &TaskTool{Parent: parent, Env: &tools.Env{Cwd: t.TempDir()}, MaxTurns: 20}
	in, _ := json.Marshal(map[string]any{"tasks": []map[string]string{{"name": "Worker", "prompt": "work"}}})
	if _, err := tool.Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	for i, seen := range prov.lastSeen {
		if got := strings.Contains(seen, "Start wrapping up"); got != (i == 14) {
			t.Fatalf("request %d: wrap-up note %v: %q", i+1, got, seen)
		}
	}
	if !strings.Contains(prov.lastSeen[14], "You have made 14 of your 20 requests") {
		t.Fatalf("note: %q", prov.lastSeen[14])
	}
}
