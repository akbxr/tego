package llm

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type wireBody struct {
	Tools []struct {
		Name         string          `json:"name"`
		DeferLoading bool            `json:"defer_loading"`
		CacheControl json.RawMessage `json:"cache_control"`
	} `json:"tools"`
	Messages []struct {
		Content []struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		} `json:"content"`
	} `json:"messages"`
	Thinking          map[string]any `json:"thinking"`
	ContextManagement *struct {
		Edits []map[string]any `json:"edits"`
	} `json:"context_management"`
}

func body(t *testing.T, p *anthropicProvider, req *Request) wireBody {
	t.Helper()
	var turn *ccTurn
	if p.oauth != nil {
		turn = p.turn(context.Background(), "s", "", req.Messages)
	}
	raw, err := p.buildBody(req, "2.1.280", turn)
	if err != nil {
		t.Fatal(err)
	}
	var w wireBody
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

func searchHistory(refs ...string) []Message {
	return []Message{
		{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "weather?"}}},
		{Role: RoleAssistant, Blocks: []Block{{Type: BlockToolUse, ID: "s1", Name: "tool_search", Input: json.RawMessage(`{}`)}}},
		{Role: RoleUser, Blocks: []Block{{Type: BlockToolResult, ToolUseID: "s1", Content: "Loaded weather", ToolRefs: refs}}},
	}
}

func deferredTools() []Tool {
	return []Tool{{Name: "read"}, {Name: "mcp__w__weather", Deferred: true}, {Name: "tool_search"}, {Name: "mcp__w__mail", Deferred: true}}
}

func TestDeferredToolsGoLastWithoutCacheBreakpoint(t *testing.T) {
	p := &anthropicProvider{name: "anthropic", base: "x", oauth: staticToken("tok")}
	w := body(t, p, &Request{Model: "claude-opus-5-5", Tools: deferredTools(), Messages: searchHistory("mcp__w__weather", "mcp__gone__x")})
	var names []string
	for _, tl := range w.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "_read,_tool_search,_mcp__w__weather,_mcp__w__mail" {
		t.Fatalf("tool order %v", names)
	}
	if w.Tools[1].CacheControl == nil || w.Tools[2].CacheControl != nil || !w.Tools[2].DeferLoading || !w.Tools[3].DeferLoading || w.Tools[0].DeferLoading {
		t.Fatalf("breakpoint/defer flags wrong: %+v", w.Tools)
	}
	// The search result becomes references, only to tools in the request.
	res := w.Messages[2].Content[0]
	if res.Type != "tool_result" || string(res.Content) != `[{"type":"tool_reference","tool_name":"_mcp__w__weather"}]` {
		t.Fatalf("tool result %s %s", res.Type, res.Content)
	}
}

func TestDeferredToolsFallBack(t *testing.T) {
	p := &anthropicProvider{name: "anthropic", base: "x", key: "k"}
	// An older model: no defer_loading, the result stays text.
	w := body(t, p, &Request{Model: "claude-sonnet-4-0", Tools: deferredTools(), Messages: searchHistory("mcp__w__weather")})
	for _, tl := range w.Tools {
		if tl.DeferLoading {
			t.Fatalf("old model got defer_loading: %+v", w.Tools)
		}
	}
	if c := string(w.Messages[2].Content[0].Content); c != `"Loaded weather"` {
		t.Fatalf("old model got references: %s", c)
	}
	// Every tool deferred: the API needs one loaded, so none is deferred.
	w = body(t, p, &Request{Model: "claude-opus-5-5", Tools: []Tool{{Name: "a", Deferred: true}}, Messages: searchHistory()})
	if len(w.Tools) != 1 || w.Tools[0].DeferLoading || w.Tools[0].CacheControl == nil {
		t.Fatalf("all-deferred list: %+v", w.Tools)
	}
}

func TestClearToolUsesEdit(t *testing.T) {
	policy := &ClearPolicy{TriggerToolUses: 120, ClearAtLeast: 40_000, Keep: 6, Exclude: []string{"task", "tool_search"}}
	p := &anthropicProvider{name: "anthropic", base: "x", oauth: staticToken("tok")}
	w := body(t, p, &Request{Model: "claude-opus-5-5", Thinking: ThinkingHigh, Tools: []Tool{{Name: "read"}}, Messages: searchHistory(), ClearToolResults: policy})
	if w.ContextManagement == nil || len(w.ContextManagement.Edits) != 2 {
		t.Fatalf("edits: %+v", w.ContextManagement)
	}
	got, _ := json.Marshal(w.ContextManagement.Edits)
	want := `[{"keep":"all","type":"clear_thinking_20251015"},` +
		`{"clear_at_least":{"type":"input_tokens","value":40000},"exclude_tools":["_task","_tool_search"],"keep":{"type":"tool_uses","value":6},"trigger":{"type":"tool_uses","value":120},"type":"clear_tool_uses_20250919"}]`
	if string(got) != want {
		t.Fatalf("edits\n got %s\nwant %s", got, want)
	}
	if bb, _ := w.Thinking["block_binding"].(map[string]any); bb["prefix_mismatch_behavior"] != "drop_block" {
		t.Fatalf("thinking without drop_block: %v", w.Thinking)
	}

	// API key, no thinking: just the clearing edit, unprefixed names.
	p = &anthropicProvider{name: "anthropic", base: "x", key: "k"}
	w = body(t, p, &Request{Model: "claude-sonnet-5-5", Tools: []Tool{{Name: "read"}}, Messages: searchHistory(), ClearToolResults: policy})
	if len(w.ContextManagement.Edits) != 1 || w.ContextManagement.Edits[0]["exclude_tools"].([]any)[0] != "task" || w.Thinking != nil {
		t.Fatalf("api-key edits: %+v thinking %v", w.ContextManagement.Edits, w.Thinking)
	}
	// Models before 4.5: no clearing, and older thinking is not bound.
	w = body(t, p, &Request{Model: "claude-sonnet-4-0", Thinking: ThinkingHigh, Messages: searchHistory(), ClearToolResults: policy})
	if w.ContextManagement != nil || w.Thinking["block_binding"] != nil {
		t.Fatalf("old model: %+v %v", w.ContextManagement, w.Thinking)
	}
}

func TestParseServerEdits(t *testing.T) {
	stream := `event: message_start
data: {"type":"message_start","message":{"id":"m","usage":{"input_tokens":5},"input_transformations":[{"type":"thinking_dropped","path":"messages.1.content.0","reason":"prefix_binding_mismatch"},{"type":"thinking_dropped","reason":"prefix_binding_mismatch"}]}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"ok"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3},"context_management":{"applied_edits":[{"type":"clear_tool_uses_20250919","cleared_tool_uses":16,"cleared_input_tokens":340641}]}}

event: message_stop
data: {"type":"message_stop"}

`
	m, err := parseAnthropicStream(context.Background(), strings.NewReader(stream), false, func(Event) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.droppedPaths, []string{"messages.1.content.0", ""}) {
		t.Fatalf("paths %q", m.droppedPaths)
	}
	if m.ThinkingDropped != 2 || m.ClearedToolUses != 16 || m.ClearedTokens != 340641 {
		t.Fatalf("edits not recorded: dropped=%d cleared=%d/%d", m.ThinkingDropped, m.ClearedToolUses, m.ClearedTokens)
	}
}

func TestDroppedRefsMapWirePaths(t *testing.T) {
	th := func(s string) Block { return Block{Type: BlockThinking, Text: s, Signature: "sig"} }
	msgs := []Message{
		{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "q"}}},
		{Role: RoleAssistant, Blocks: []Block{{Type: BlockText, Text: " "}, th("t1"), {Type: BlockToolUse, ID: "u1", Name: "read"}}},
		{Role: RoleUser, Blocks: []Block{{Type: BlockToolResult, ToolUseID: "u1", Content: "r"}}},
		{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "steer"}}}, // merged into the previous wire message
		{Role: RoleAssistant, Blocks: []Block{{Type: BlockThinking, Text: "foreign"}, th("t2"), {Type: BlockText, Text: "ok"}}},
	}
	got := droppedRefs(msgs, true, []string{"messages.1.content.0", "messages.3.content.0", "messages.2.content.1", "messages.9.content.0", "junk"})
	want := []BlockRef{{1, 1}, {4, 1}, {3, 0}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("refs %v, want %v", got, want)
	}
}

func TestPoolForwardsContextFeatures(t *testing.T) {
	pool := NewPool("anthropic", "", []PoolMember{{Label: "a", Provider: &anthropicProvider{name: "anthropic"}}})
	if !ClearsToolResults(pool, "claude-opus-5-5") || !DefersTools(pool, "claude-opus-5-5") || DefersTools(pool, "claude-sonnet-4-0") {
		t.Fatal("pool does not forward the features")
	}
	if ClearsToolResults(&openaiProvider{name: "o"}, "gpt-5") {
		t.Fatal("openai claims server clearing")
	}
	// Only Anthropic's cache has an exact lifetime; implicit caches are best-effort.
	if !KnownCacheTTL(pool, "claude-opus-5-5") || KnownCacheTTL(&openaiProvider{name: "o"}, "gpt-5.6") ||
		KnownCacheTTL(&codexProvider{}, "gpt-5.6") || KnownCacheTTL(&antigravityProvider{}, "gemini-3.8-flash") {
		t.Fatal("KnownCacheTTL wrong")
	}
}

func TestImplicitCacheProviders(t *testing.T) {
	for p, want := range map[Provider]bool{
		&antigravityProvider{}: true,
		&codexProvider{}:       true,
		&openaiProvider{base: "https://api.openai.com/v1"}:                               true,
		&openaiProvider{base: "https://generativelanguage.googleapis.com/v1beta/openai"}: true,
		&openaiProvider{base: "http://localhost:1234/v1"}:                                false,
		&openaiProvider{base: "https://openrouter.ai/api/v1"}:                            false,
		&anthropicProvider{}: false,
	} {
		if got := HasImplicitCache(p, "m"); got != want {
			t.Errorf("%T %v: %v, want %v", p, p, got, want)
		}
	}
}

// One-off requests (a compaction's transcript, a title) do not put their
// messages in the cache: nothing reads them again.
func TestOneOffRequestsDoNotCacheTheirMessages(t *testing.T) {
	p := &anthropicProvider{name: "anthropic", base: "x", key: "k"}
	msgs := []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: strings.Repeat("transcript ", 500)}}}}
	for class, want := range map[string]bool{ClassMain: true, ClassSubagent: true, ClassCompaction: false, ClassAuxiliary: false} {
		raw, err := p.buildBody(&Request{Model: "claude-opus-5-5", System: "s", Messages: msgs, Class: class}, "2.1.280", nil)
		if err != nil {
			t.Fatal(err)
		}
		var w struct {
			Messages []struct {
				Content []struct {
					CacheControl *struct{} `json:"cache_control"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &w); err != nil {
			t.Fatal(err)
		}
		if got := w.Messages[0].Content[0].CacheControl != nil; got != want {
			t.Errorf("%s: message cached=%v, want %v", class, got, want)
		}
	}
}

// A compaction that reads the conversation's cache repeats the last request
// up to its message breakpoint and puts its own breakpoint there, not on the
// new instruction at the end.
func TestCachedCompactionReadsTheLastRequestsBreakpoint(t *testing.T) {
	p := &anthropicProvider{name: "anthropic", base: "x", key: "k"}
	prev := []Message{
		{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "fix the bug"}}},
		{Role: RoleAssistant, Blocks: []Block{{Type: BlockText, Text: "looking"}, {Type: BlockToolUse, ID: "t1", Name: "read", Input: json.RawMessage(`{"path":"a.go"}`)}}},
		{Role: RoleUser, Blocks: []Block{{Type: BlockToolResult, ToolUseID: "t1", Content: "package a"}, {Type: BlockText, Text: "<system-reminder>x</system-reminder>"}}},
	}
	tools := []Tool{{Name: "read", Description: "r", Schema: json.RawMessage(`{"type":"object"}`)}}
	type body struct {
		System   json.RawMessage   `json:"system"`
		Tools    json.RawMessage   `json:"tools"`
		Messages []json.RawMessage `json:"messages"`
	}
	enc := func(req *Request) body {
		raw, err := p.buildBody(req, "2.1.280", nil)
		if err != nil {
			t.Fatal(err)
		}
		var b body
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	last := enc(&Request{Model: "claude-opus-5-5", System: "s", Tools: tools, Messages: prev, LongCache: true})
	more := append(append([]Message(nil), prev...),
		Message{Role: RoleAssistant, Blocks: []Block{{Type: BlockToolUse, ID: "t2", Name: "read", Input: json.RawMessage(`{"path":"b.go"}`)}}},
		Message{Role: RoleUser, Blocks: []Block{{Type: BlockToolResult, ToolUseID: "t2", Content: "package b"}}},
		Message{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "summarise"}}})
	got := enc(&Request{Model: "claude-opus-5-5", System: "s", Tools: tools, Messages: more, LongCache: true, Class: ClassCompaction, CachePrefix: len(prev)})
	if string(got.System) != string(last.System) || string(got.Tools) != string(last.Tools) {
		t.Fatal("system or tools differ")
	}
	for i := range last.Messages {
		if string(got.Messages[i]) != string(last.Messages[i]) {
			t.Fatalf("message %d differs:\n%s\n%s", i, got.Messages[i], last.Messages[i])
		}
	}
	if len(got.Messages) != len(last.Messages)+2 {
		t.Fatalf("%d messages", len(got.Messages))
	}
	for _, m := range got.Messages[len(last.Messages):] {
		if strings.Contains(string(m), "cache_control") {
			t.Fatalf("a breakpoint after the cached prefix: %s", m)
		}
	}
	if !strings.Contains(string(last.Messages[len(last.Messages)-1]), "cache_control") {
		t.Fatal("the last request had no message breakpoint")
	}
}
