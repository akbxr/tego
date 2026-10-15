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
