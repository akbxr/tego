package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
)

type summaryProvider struct{ gotTranscript string }

func (p *summaryProvider) Name() string { return "fake" }
func (p *summaryProvider) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	p.gotTranscript = req.Messages[0].Blocks[0].Text
	return &llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "SUMMARY"}}}, nil
}

// history of n tool rounds, each ~size tokens of tool output.
func toolHistory(n, size int) []llm.Message {
	msgs := []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "do the task"}}}}
	for i := range n {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, Usage: &llm.Usage{Input: 1000 * (i + 1)},
				Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: id, Name: "read", Input: json.RawMessage(`{}`)}}},
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: id, Content: strings.Repeat("x", size*4)}}})
	}
	return msgs
}

func TestCompactKeepsRecentToolRoundsIntact(t *testing.T) {
	prov := &summaryProvider{}
	a := &Agent{Provider: prov, UI: nopUI{}, ContextWindow: 200_000, KeepRecent: 5000}
	a.Messages = toolHistory(20, 1000) // ~20k tokens of results
	if err := a.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	got := a.Messages
	if got[0].Role != llm.RoleUser || !strings.Contains(got[0].Blocks[0].Text, "SUMMARY") {
		t.Fatalf("first message should carry the summary: %+v", got[0])
	}
	if got[1].Role != llm.RoleAssistant {
		t.Fatalf("kept part must start at an assistant message, got %s", got[1].Role)
	}
	kept := estimateTokens(got[1:])
	if kept > 5000 || len(got) < 5 {
		t.Fatalf("kept %d messages (~%d tokens), want a recent tail within 5000", len(got)-1, kept)
	}
	for i, m := range got {
		if m.Usage != nil {
			t.Fatalf("message %d kept stale usage", i)
		}
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolUse && (i+1 >= len(got) || got[i+1].Blocks[0].ToolUseID != b.ID) {
				t.Fatalf("tool call %s separated from its result", b.ID)
			}
		}
	}
	if !strings.Contains(prov.gotTranscript, "do the task") {
		t.Fatal("summarised part should include the original prompt")
	}
}

func TestCompactSummarisesEverythingWhenLastResultIsHuge(t *testing.T) {
	a := &Agent{Provider: &summaryProvider{}, UI: nopUI{}, ContextWindow: 200_000}
	a.Messages = toolHistory(2, 50_000)
	if err := a.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if len(a.Messages) != 1 || !strings.Contains(a.Messages[0].Blocks[0].Text, "Continue the task") {
		t.Fatalf("want a single summary+continue message, got %d messages", len(a.Messages))
	}
}

func TestEstimateContextUsesLastUsage(t *testing.T) {
	a := &Agent{UI: nopUI{}}
	a.SetHistory([]llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "hi"}}},
		{Role: llm.RoleAssistant, Usage: &llm.Usage{Input: 10, CacheRead: 40_000, Output: 500}, Blocks: []llm.Block{{Type: llm.BlockText, Text: "ok"}}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: strings.Repeat("y", 4000)}}},
	})
	if a.LastCtx < 40_510+1000 || a.LastCtx > 40_510+1100 {
		t.Fatalf("resumed context %d, want last usage 40510 + ~1000 for the newer message", a.LastCtx)
	}
}

func TestCompactAtMatchesOmpDefault(t *testing.T) {
	for _, tc := range []struct{ window, pct, want int }{
		{200_000, 0, 170_000},   // 15% reserve
		{1_000_000, 0, 850_000}, // 15% reserve
		{64_000, 0, 64_000 - 16_384},
		{32_000, 0, 32_000 - 16_384},
		{16_000, 0, 13_600}, // fixed reserve would leave <15%: fall back to 15%
		{200_000, 50, 100_000},
	} {
		a := &Agent{ContextWindow: tc.window, CompactPercent: tc.pct}
		if got := a.CompactAt(); got != tc.want {
			t.Errorf("window %d pct %d: %d, want %d", tc.window, tc.pct, got, tc.want)
		}
	}
}

func TestOverflowDetection(t *testing.T) {
	if !isContextOverflow(errors.New(`anthropic: HTTP 400: {"error":{"message":"prompt is too long: 210000 tokens > 200000 maximum"}}`)) {
		t.Fatal("anthropic overflow not detected")
	}
	if isContextOverflow(errors.New("HTTP 529 overloaded")) {
		t.Fatal("false positive")
	}
}

// systemProvider records the system prompt of the summary request.
type systemProvider struct{ system string }

func (p *systemProvider) Name() string { return "fake" }
func (p *systemProvider) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	p.system = req.System
	return &llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "## Completed\nsomething"}}}, nil
}

func TestCompactListsChangedFilesAcrossCompactions(t *testing.T) {
	use := func(id, name, path string) llm.Block {
		return llm.Block{Type: llm.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(`{"path":"` + path + `"}`)}
	}
	res := func(id string, isErr bool) llm.Block {
		return llm.Block{Type: llm.BlockToolResult, ToolUseID: id, Content: "ok", IsError: isErr}
	}
	prov := &systemProvider{}
	a := &Agent{Provider: prov, UI: nopUI{}, ContextWindow: 200_000}
	a.Messages = []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: contextSummaryTag + "\nold summary" + filesHeading + "old/a.go, b.go\n</context-summary>"}}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{use("e1", "edit", "b.go"), use("e2", "edit", "c.go"), use("w1", "write", "d.md"), use("r1", "read", "e.go")}},
		{Role: llm.RoleUser, Blocks: []llm.Block{res("e1", false), res("e2", true), res("w1", false), res("r1", false)}},
	}
	if err := a.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	for _, sec := range []string{"## User goals and instructions", "## Code state", "## Tests and checks", "## Next steps", "merge it"} {
		if !strings.Contains(prov.system, sec) {
			t.Errorf("summary prompt lacks %q", sec)
		}
	}
	got := a.Messages[0].Blocks[0].Text
	// The earlier list first, then this round's: the failed edit and the read are not changes.
	if !strings.Contains(got, filesHeading+"old/a.go, b.go, d.md\n") && !strings.HasSuffix(strings.TrimSuffix(got, "\n</context-summary>"), filesHeading+"old/a.go, b.go, d.md") {
		t.Fatalf("summary:\n%s", got)
	}
	if fs := changedFiles(a.Messages); strings.Join(fs, ",") != "old/a.go,b.go,d.md" {
		t.Fatalf("carried over: %q", fs)
	}
}
