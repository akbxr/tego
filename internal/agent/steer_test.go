package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// recorder answers each request with the next message and records what
// each request carried.
type recorder struct {
	replies []llm.Message
	seen    [][]llm.Message
}

func (p *recorder) Name() string { return "fake" }

func (p *recorder) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	p.seen = append(p.seen, append([]llm.Message(nil), req.Messages...))
	m := p.replies[0]
	p.replies = p.replies[1:]
	return &m, nil
}

func text(s string) llm.Block { return llm.Block{Type: llm.BlockText, Text: s} }

func lastUserText(msgs []llm.Message) string {
	m := msgs[len(msgs)-1]
	if m.Role != llm.RoleUser || len(m.Blocks) == 0 {
		return ""
	}
	return m.Blocks[0].Text
}

func TestSteeringAfterToolRound(t *testing.T) {
	prov := &recorder{replies: []llm.Message{
		{Role: llm.RoleAssistant, StopReason: "tool_use", Blocks: []llm.Block{call("r1", "read", `{"path":"a"}`)}},
		{Role: llm.RoleAssistant, StopReason: "end_turn", Blocks: []llm.Block{text("ok, switching")}},
	}}
	pending := [][]llm.Block{{text("use b instead")}}
	a := &Agent{Provider: prov, UI: nopUI{}, Tools: []tools.Tool{echoTool{}}}
	a.Steer = func() [][]llm.Block { m := pending; pending = nil; return m }
	if err := a.Run(context.Background(), "read a"); err != nil {
		t.Fatal(err)
	}
	if len(prov.seen) != 2 {
		t.Fatalf("%d requests", len(prov.seen))
	}
	// The tool ran, then the message went in before the next request.
	second := prov.seen[1]
	if got := lastUserText(second); got != "use b instead" {
		t.Fatalf("second request ends with %q", got)
	}
	if prev := second[len(second)-2]; prev.Role != llm.RoleUser || prev.Blocks[0].Type != llm.BlockToolResult {
		t.Fatalf("steering not after the tool result: %+v", prev)
	}
}

func TestSteeringWhenModelWouldStop(t *testing.T) {
	prov := &recorder{replies: []llm.Message{
		{Role: llm.RoleAssistant, StopReason: "end_turn", Blocks: []llm.Block{text("first answer")}},
		{Role: llm.RoleAssistant, StopReason: "end_turn", Blocks: []llm.Block{text("second answer")}},
	}}
	calls := 0
	a := &Agent{Provider: prov, UI: nopUI{}}
	a.Steer = func() [][]llm.Block {
		calls++
		if calls == 1 { // typed while the first answer streamed
			return [][]llm.Block{{text("also explain why")}}
		}
		return nil
	}
	if err := a.Run(context.Background(), "question"); err != nil {
		t.Fatal(err)
	}
	if len(prov.seen) != 2 || lastUserText(prov.seen[1]) != "also explain why" {
		t.Fatalf("requests: %d, last %q", len(prov.seen), lastUserText(prov.seen[len(prov.seen)-1]))
	}
	if got := a.LastText(); got != "second answer" {
		t.Fatalf("turn ended on %q", got)
	}
}

// A message sent while task_status waits for background tasks ends the
// wait: the model gets it with the next request instead of after up to
// 10 minutes. Without one, the wait runs on.
func TestSteeringEndsATaskWait(t *testing.T) {
	a := &Agent{}
	bg := &Background{change: make(chan struct{})}
	bg.tasks = []*bgTask{{id: "t1", st: &SubagentStatus{Name: "Slow", State: "running"}, parent: a}}
	tool := &TaskStatusTool{BG: bg}
	run := func() (string, time.Duration) {
		ctx := tools.WithWake(context.Background(), a.wakeChan())
		start := time.Now()
		out, err := tool.Run(ctx, []byte(`{"ids":["t1"],"wait":600}`))
		if err != nil {
			t.Fatal(err)
		}
		return out, time.Since(start)
	}

	time.AfterFunc(50*time.Millisecond, a.SteerArrived)
	out, took := run()
	if took > 5*time.Second || !strings.Contains(out, "Stopped waiting: the user sent a message") || !strings.Contains(out, "t1 Slow") {
		t.Fatalf("after %v:\n%s", took, out)
	}

	// Taken: the next wait is not cut short by the same message.
	a.Steer = func() [][]llm.Block { return [][]llm.Block{{text("go on")}} }
	a.takeSteering()
	ctx, cancel := context.WithTimeout(tools.WithWake(context.Background(), a.wakeChan()), 200*time.Millisecond)
	defer cancel()
	if _, err := tool.Run(ctx, []byte(`{"ids":["t1"],"wait":600}`)); err == nil {
		t.Fatal("the wait ended before its context")
	}
}
