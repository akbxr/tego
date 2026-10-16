package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// featured is a provider with server-side context features.
type featured struct {
	recorder
	clears, defers bool
}

func (f *featured) ClearsToolResults(string) bool { return f.clears }
func (f *featured) DefersTools(string) bool       { return f.defers }

type mcpTool struct{ name, desc string }

func (t mcpTool) Def() llm.Tool {
	return llm.Tool{Name: t.name, Description: t.desc, Schema: json.RawMessage(`{"type":"object"}`)}
}
func (mcpTool) Run(context.Context, json.RawMessage) (string, error) { return "ok", nil }

func mcpCatalog(n int) []tools.Tool {
	out := []tools.Tool{mcpTool{"mcp__studio__screenshot", "Take a screenshot of the viewport."}}
	for i := 1; i < n; i++ {
		out = append(out, mcpTool{fmt.Sprintf("mcp__studio__tool%d", i), "Does thing."})
	}
	return out
}

func lazyAgent(p llm.Provider) *Agent {
	cat := mcpCatalog(12)
	return &Agent{Provider: p, UI: nopUI{}, Tools: []tools.Tool{echoTool{}}, LazyThreshold: 10,
		Dynamic: func() []tools.Tool { return cat }}
}

func defNames(defs []llm.Tool) (loaded, deferred []string) {
	for _, d := range defs {
		if d.Deferred {
			deferred = append(deferred, d.Name)
		} else {
			loaded = append(loaded, d.Name)
		}
	}
	return
}

func TestDeferringProviderGetsEveryMCPToolDeferred(t *testing.T) {
	a := lazyAgent(&featured{defers: true})
	loaded, deferred := defNames(a.toolDefs())
	if !reflect.DeepEqual(loaded, []string{"read", "tool_search"}) || len(deferred) != 12 {
		t.Fatalf("loaded %v, deferred %d", loaded, len(deferred))
	}
	// The search result references the matches; the tool list does not change.
	before := a.toolDefs()
	res := a.execute(context.Background(), call("s1", "tool_search", `{"query":"screenshot"}`))
	if !reflect.DeepEqual(res.ToolRefs, []string{"mcp__studio__screenshot"}) || res.IsError {
		t.Fatalf("result refs %v (%s)", res.ToolRefs, res.Content)
	}
	if !reflect.DeepEqual(before, a.toolDefs()) {
		t.Fatal("the tool list changed after a search")
	}
	// Deferred schemas are not part of the context estimate.
	plain := lazyAgent(&featured{})
	if a.overheadTokens() != plain.overheadTokens() {
		t.Fatalf("overhead %d vs %d", a.overheadTokens(), plain.overheadTokens())
	}
	// Claude Code's exact-name syntax loads exactly the named tools.
	res = a.execute(context.Background(), call("s3", "tool_search", `{"query":"select:mcp__studio__tool3, mcp__studio__screenshot"}`))
	if !reflect.DeepEqual(res.ToolRefs, []string{"mcp__studio__screenshot", "mcp__studio__tool3"}) {
		t.Fatalf("select refs %v (%s)", res.ToolRefs, res.Content)
	}
	// A server listing must not promise loading by calling.
	res = a.execute(context.Background(), call("s2", "tool_search", `{"query":"studio"}`))
	if strings.Contains(res.Content, "call one by exact name") {
		t.Fatalf("listing: %s", res.Content)
	}
}

func TestOtherProvidersActivateSearchedTools(t *testing.T) {
	a := lazyAgent(&featured{})
	loaded, deferred := defNames(a.toolDefs())
	if len(loaded) != 2 || deferred != nil {
		t.Fatalf("loaded %v deferred %v", loaded, deferred)
	}
	a.execute(context.Background(), call("s1", "tool_search", `{"query":"screenshot"}`))
	if loaded, _ = defNames(a.toolDefs()); !reflect.DeepEqual(loaded, []string{"read", "tool_search", "mcp__studio__screenshot"}) {
		t.Fatalf("after search: %v", loaded)
	}
}

// toolTurns is a history of n turns that each call tool name.
func toolTurns(n int, name string) []llm.Message {
	msgs := []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{text("go")}}}
	for i := range n {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: id, Name: name, Input: []byte(`{}`)}}},
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: id, Content: "x"}}})
	}
	return msgs
}

func TestClearPolicy(t *testing.T) {
	a := &Agent{AutoCompact: true, ContextWindow: 1_000_000, Messages: toolTurns(40, "bash")}
	a.Messages = append(a.Messages, toolTurns(5, "todo")[1:]...) // excluded: not counted
	a.LastCtx = 150_000
	if p := a.clearPolicy(); p != nil {
		t.Fatalf("policy below the token threshold: %+v", p)
	}
	a.LastCtx = 210_000
	p := a.clearPolicy()
	want := &llm.ClearPolicy{TriggerToolUses: 37, ClearAtLeast: 80_000, Keep: 6,
		Exclude: []string{"checkpoint", "rewind", "task", "todo", "tool_search"}}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("policy %+v", p)
	}
	// Once set, the trigger stays while the cache is warm.
	a.lastReply = time.Now()
	a.Messages = append(a.Messages, toolTurns(30, "bash")[1:]...)
	a.LastCtx = 400_000
	if p = a.clearPolicy(); p.TriggerToolUses != 37 {
		t.Fatalf("warm cache: trigger moved to %d", p.TriggerToolUses)
	}
	// The cache has expired: set afresh.
	a.lastReply = time.Now().Add(-2 * time.Hour)
	if p = a.clearPolicy(); p.TriggerToolUses != 67 {
		t.Fatalf("cold cache: trigger %d", p.TriggerToolUses)
	}
	// A small window: the token threshold is the elision one, clearing half as much.
	b := &Agent{AutoCompact: true, ContextWindow: 64_000, Messages: toolTurns(40, "bash")}
	b.LastCtx = b.CompactAt()*elideSoftRatio/100 + 1
	if p = b.clearPolicy(); p == nil || p.ClearAtLeast != b.CompactAt()*elideSoftRatio/100/2 {
		t.Fatalf("small window policy %+v (compact at %d)", p, b.CompactAt())
	}
	// Too few tool uses to be worth a pass.
	c := &Agent{AutoCompact: true, ContextWindow: 1_000_000, Messages: toolTurns(clearMinUses+clearBelow-1, "bash"), LastCtx: 300_000}
	if p = c.clearPolicy(); p != nil {
		t.Fatalf("policy with few tool uses %+v", p)
	}
	a.AutoCompact = false
	if a.clearPolicy() != nil {
		t.Fatal("policy without auto-compaction")
	}
}

func TestElideAtCappedUnlessProviderClears(t *testing.T) {
	a := &Agent{AutoCompact: true, ContextWindow: 1_000_000, Provider: &featured{}}
	if got := a.ElideAt(); got != elideMaxAt {
		t.Fatalf("1M window, no server clearing: elide at %d", got)
	}
	a.Provider = &featured{clears: true}
	if got := a.ElideAt(); got != a.CompactAt()*elideSoftRatio/100 {
		t.Fatalf("server clearing: elide at %d", got)
	}
	a.Provider, a.ContextWindow = &featured{}, 128_000
	if got := a.ElideAt(); got != a.CompactAt()*elideSoftRatio/100 || got > elideMaxAt {
		t.Fatalf("128k window: elide at %d", got)
	}
}

type infoUI struct {
	nopUI
	infos []string
}

func (u *infoUI) OnInfo(s string) { u.infos = append(u.infos, s) }

func TestServerEditsReportedOnce(t *testing.T) {
	ui := &infoUI{}
	a := &Agent{UI: ui}
	for _, n := range []int{0, 16, 16, 18, 18} {
		a.noteServerEdits(&llm.Message{ClearedToolUses: n, ClearedTokens: n * 20_000})
	}
	if len(ui.infos) != 2 || !strings.Contains(ui.infos[0], "16 tool result(s)") || !strings.Contains(ui.infos[1], "18") {
		t.Fatalf("infos %q", ui.infos)
	}
}

func TestDroppedThinkingIsRemovedOnce(t *testing.T) {
	think := func(s string) llm.Block { return llm.Block{Type: llm.BlockThinking, Text: s, Signature: "sig"} }
	shared := []llm.Block{think("a"), text("x"), think("b")}
	ui := &infoUI{}
	a := &Agent{UI: ui, Messages: []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{text("q")}},
		{Role: llm.RoleAssistant, Blocks: shared},
	}}
	// Refs that don't point at thinking (stale indices) are ignored.
	a.noteServerEdits(&llm.Message{ThinkingDropped: 2, DroppedThinking: []llm.BlockRef{{Msg: 1, Block: 0}, {Msg: 1, Block: 2}, {Msg: 1, Block: 1}, {Msg: 0, Block: 0}, {Msg: 9, Block: 9}}})
	if got := a.Messages[1].Blocks; len(got) != 1 || got[0].Text != "x" {
		t.Fatalf("blocks %+v", got)
	}
	if shared[0].Text != "a" || len(shared) != 3 {
		t.Fatal("the session's blocks were modified in place")
	}
	if len(ui.infos) != 1 || !strings.Contains(ui.infos[0], "removed 2 thinking") {
		t.Fatalf("infos %q", ui.infos)
	}
	a.noteServerEdits(&llm.Message{}) // nothing dropped any more: no line
	if len(ui.infos) != 1 {
		t.Fatalf("infos %q", ui.infos)
	}
}

func TestRequestCarriesClearPolicy(t *testing.T) {
	prov := &policyRecorder{}
	a := &Agent{Provider: prov, UI: nopUI{}, AutoCompact: true, ContextWindow: 1_000_000, Messages: toolTurns(40, "bash")}
	a.Messages[len(a.Messages)-2].Usage = &llm.Usage{CacheRead: 300_000}
	if err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if prov.got == nil || prov.got.TriggerToolUses != 37 {
		t.Fatalf("policy %+v", prov.got)
	}
	// The reply records the trigger, for a resumed session.
	if last := a.Messages[len(a.Messages)-1]; last.ClearTrigger != 37 {
		t.Fatalf("reply carries trigger %d", last.ClearTrigger)
	}
}

type policyRecorder struct{ got *llm.ClearPolicy }

func (p *policyRecorder) Name() string { return "fake" }
func (p *policyRecorder) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	p.got = req.ClearToolResults
	return &llm.Message{Role: llm.RoleAssistant, StopReason: "end_turn", Blocks: []llm.Block{text("hello")}}, nil
}

func TestWarmElisionFollowsCacheWritePrice(t *testing.T) {
	premium := func(_, model string) bool { return model == "gpt-5.6" }
	a := &Agent{AutoCompact: true, ContextWindow: 1_000_000, Provider: &featured{}, CacheWritePremium: premium}
	a.Model = "gemini-3.8-flash"
	if a.ElideAt() != elideMaxAt || a.elideGap() != elideRegrow {
		t.Fatalf("no write premium: elide at %d, gap %d", a.ElideAt(), a.elideGap())
	}
	a.Model = "gpt-5.6" // each pass rewrites an expensive cache: elide later, in bigger steps
	if a.ElideAt() != clearTrigger || a.elideGap() != clearAtLeast {
		t.Fatalf("write premium: elide at %d, gap %d", a.ElideAt(), a.elideGap())
	}
}
