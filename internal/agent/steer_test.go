package agent

import (
	"context"
	"testing"

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
