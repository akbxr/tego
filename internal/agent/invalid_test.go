package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
)

// scripted returns its messages in order and records every request.
type scripted struct {
	replies []*llm.Message
	reqs    []*llm.Request
}

func (p *scripted) Name() string { return "fake" }
func (p *scripted) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	cp := *req
	cp.Messages = append([]llm.Message(nil), req.Messages...)
	p.reqs = append(p.reqs, &cp)
	m := p.replies[0]
	p.replies = p.replies[1:]
	return m, nil
}

type countingTool struct{ runs int }

func (t *countingTool) Def() llm.Tool { return llm.Tool{Name: "multi_grep"} }
func (t *countingTool) Run(context.Context, json.RawMessage) (string, error) {
	t.runs++
	return "ran", nil
}

func TestMalformedToolArgumentsAreReportedNotRun(t *testing.T) {
	// What a model streamed once with fine-grained tool streaming.
	raw := `{"patterns": read before,read first}`
	tool := &countingTool{}
	prov := &scripted{replies: []*llm.Message{
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "t1", Name: "multi_grep", Input: llm.ToolInput([]byte(raw))}}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "ok"}}},
	}}
	a := &Agent{Provider: prov, UI: nopUI{}}
	a.Tools = append(a.Tools, tool)
	if err := a.Run(context.Background(), "search"); err != nil {
		t.Fatal(err)
	}
	if tool.runs != 0 {
		t.Fatal("tool ran with made-up arguments")
	}
	// The second request carries a valid history and an error quoting the input.
	last := prov.reqs[1].Messages
	call := last[len(last)-2].Blocks[0]
	if !json.Valid(call.Input) {
		t.Fatalf("history holds invalid JSON: %s", call.Input)
	}
	res := last[len(last)-1].Blocks[0]
	if !res.IsError || !strings.Contains(res.Content, "not valid JSON") || !strings.Contains(res.Content, raw) {
		t.Fatalf("tool result = %+v", res)
	}
}
