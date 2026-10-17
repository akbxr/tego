package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
)

// countingProvider answers "done" and counts requests, recording the last.
type countingProvider struct {
	calls int
	last  *llm.Request
}

func (p *countingProvider) Name() string { return "fake" }
func (p *countingProvider) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	p.calls++
	p.last = req
	return &llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "done"}}}, nil
}

func TestElideStubsOldToolOutputBeforeSummarising(t *testing.T) {
	prov := &countingProvider{}
	// 100k window: compaction at 85k, elision at ~59.5k.
	a := &Agent{Provider: prov, UI: nopUI{}, ContextWindow: 100_000, AutoCompact: true, KeepRecent: 5000}
	h := toolHistory(30, 2000) // ~60k tokens of read output
	// A subagent report and a big write call in the old part.
	h = append(h[:3], append([]llm.Message{
		{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: "t1", Name: "task", Input: json.RawMessage(`{}`)},
			{Type: llm.BlockToolUse, ID: "w1", Name: "write", Input: json.RawMessage(`{"path":"a.go","content":"` + strings.Repeat("y", 5000) + `"}`)}}},
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ToolUseID: "t1", Content: strings.Repeat("report ", 1000)},
			{Type: llm.BlockToolResult, ToolUseID: "w1", Content: "wrote a.go"}}},
	}, h[3:]...)...)
	shared := h[1].Blocks // the session tree holds the same blocks
	stripUsage(h)         // let the context be estimated from content (~64k)
	a.SetHistory(h)
	a.Messages = append(a.Messages, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "next"}}})

	if err := a.loop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if prov.calls != 1 {
		t.Fatalf("want 1 request (no summarisation), got %d", prov.calls)
	}
	msgs := prov.last.Messages
	var elided, verbatim int
	for _, m := range msgs {
		for _, b := range m.Blocks {
			switch {
			case b.Type == llm.BlockToolResult && strings.HasPrefix(b.Content, elidedMarker):
				elided++
				if !strings.Contains(b.Content, "read output: 1 lines, 7.8 KB") {
					t.Fatalf("stub = %q", b.Content)
				}
			case b.Type == llm.BlockToolResult && len(b.Content) > elideMinChars:
				verbatim++
			}
			if b.ToolUseID == "t1" && strings.HasPrefix(b.Content, elidedMarker) {
				t.Fatal("subagent report was elided")
			}
			if b.ID == "w1" && !strings.Contains(string(b.Input), `"content":"[elided content: 1 lines, 4.9 KB]"`) {
				t.Fatalf("write input = %s", b.Input)
			}
		}
	}
	// Old outputs are stubbed; the recent window (and the task report) is not.
	if elided < 20 || verbatim < 2 {
		t.Fatalf("elided %d, verbatim %d", elided, verbatim)
	}
	if est := a.EstimateContext(); est > a.ElideAt() {
		t.Fatalf("context after elision ~%d tokens, still above %d", est, a.ElideAt())
	}
	if strings.HasPrefix(shared[0].Content, elidedMarker) {
		t.Fatal("elision modified blocks shared with the session tree")
	}
}

func TestElideWaitsWhenContextStaysAboveSoftThreshold(t *testing.T) {
	a := &Agent{Provider: &countingProvider{}, UI: nopUI{}, ContextWindow: 100_000, AutoCompact: true, KeepRecent: 5000}
	// One bulky old output, then ~68k tokens of small outputs nothing can elide.
	h := toolHistory(1, 3000)
	h = append(h, toolHistory(280, 240)[1:]...)
	stripUsage(h)
	a.SetHistory(h)
	if !a.needsElide() {
		t.Fatalf("setup: %d should exceed %d", a.LastCtx, a.ElideAt())
	}
	if n, _ := a.elide(0); n != 1 {
		t.Fatalf("elided %d blocks, want 1", n)
	}
	a.LastCtx = a.EstimateContext()
	if a.LastCtx <= a.ElideAt() {
		t.Fatalf("setup: context %d should stay above %d", a.LastCtx, a.ElideAt())
	}
	// Still above the soft threshold: no pass (and no cache rebuild) every turn…
	if a.needsElide() {
		t.Fatal("elides again right after a pass")
	}
	// …until the context has grown by a tenth of the window.
	a.LastCtx += a.ContextWindow/10 + 1
	if !a.needsElide() {
		t.Fatal("never elides again")
	}
}

func TestElideFallsBackToSummaryWhenNotEnough(t *testing.T) {
	prov := &summaryProvider{}
	a := &Agent{Provider: prov, UI: nopUI{}, ContextWindow: 100_000, AutoCompact: true, KeepRecent: 5000}
	// Many small tool results: nothing is bulky enough to elide, so the
	// loop has to summarise.
	h := toolHistory(400, 240)
	stripUsage(h)
	a.SetHistory(h)
	a.Messages = append(a.Messages, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "next"}}})
	if err := a.loop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.Messages[0].Blocks[0].Text, "SUMMARY") {
		t.Fatalf("history was not summarised: %q", a.Messages[0].Blocks[0].Text)
	}
	for _, m := range a.Messages {
		for _, b := range m.Blocks {
			if strings.HasPrefix(b.Content, elidedMarker) {
				t.Fatal("small outputs were elided")
			}
		}
	}
}

func TestElidedBashOutputPointsAtItsSavedFile(t *testing.T) {
	names := map[string]string{"b1": "bash", "b2": "bash"}
	saved := strings.Repeat("line\n", 500) + "... 9000 lines omitted; all 9800 lines are in /tmp/tego-output/bash-123.log (read it with offset/limit) ...\n" + strings.Repeat("end\n", 300)
	got, ok := elideBlock(llm.Block{Type: llm.BlockToolResult, ToolUseID: "b1", Content: saved}, names)
	if !ok || !strings.Contains(got.Content, "read /tmp/tego-output/bash-123.log with offset/limit") {
		t.Fatalf("stub: %q", got.Content)
	}
	got, ok = elideBlock(llm.Block{Type: llm.BlockToolResult, ToolUseID: "b2", Content: strings.Repeat("x\n", 3000)}, names)
	if !ok || !strings.Contains(got.Content, "run the tool again") {
		t.Fatalf("stub without a file: %q", got.Content)
	}
}
