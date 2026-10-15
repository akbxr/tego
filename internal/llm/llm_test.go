package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestXXHash64(t *testing.T) {
	cases := []struct {
		in   string
		seed uint64
		want uint64
	}{
		{"", 0, 0xef46db3751d8e999},
		{"a", 0, 0xd24ec4f1a98c6e5b},
		{"abc", 0, 0x44bc2cf5ad770999},
		{"Nobody inspects the spammish repetition", 0, 0xfbcea83c8a378bf1},
		{"xxhash", 20141025, 0xb559b98d844e0635},
		// Computed with Bun.hash.xxHash64.
		{"hello world, this is a longer input of more than 32 bytes!", billingSeed, 0xab9285e8efdb2ddb},
	}
	for _, c := range cases {
		if got := xxHash64([]byte(c.in), c.seed); got != c.want {
			t.Errorf("xxHash64(%q, %#x) = %#016x, want %#016x", c.in, c.seed, got, c.want)
		}
	}
}

func TestSSEReader(t *testing.T) {
	in := ": comment\r\n" +
		"event: one\n" +
		"data: {\"a\":1}\n" +
		"\n" +
		"data: line1\n" +
		"data:line2\n" +
		"id: 7\n" +
		"\n" +
		"\n" +
		"event: ignored-no-data\n" +
		"\n" +
		"data: " + strings.Repeat("x", 100000) + "\n" +
		"\n" +
		"data: tail-without-blank-line"
	r := newSSEReader(strings.NewReader(in))
	type ev struct{ event, data string }
	want := []ev{
		{"one", `{"a":1}`},
		{"", "line1\nline2"},
		{"", strings.Repeat("x", 100000)},
		{"", "tail-without-blank-line"},
	}
	for i, w := range want {
		e, err := r.Next()
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if e.Event != w.event || string(e.Data) != w.data {
			t.Fatalf("event %d = (%q, %.40q), want (%q, %.40q)", i, e.Event, e.Data, w.event, w.data)
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestBillingHeader(t *testing.T) {
	const prefix = "x-anthropic-billing-header: cc_version=2.1.280."
	// chars at 4,7,20 = "o", "t", "i"; reference: Bun.SHA256.hash("59cf53e54c78oti2.1.280").
	aux := billingHeader("Hello, this is the first message", "2.1.280", &ccTurn{class: ClassAuxiliary})
	if want := prefix + "e51; cc_entrypoint=cli; cch=00000;"; aux != want {
		t.Errorf("auxiliary header %q, want %q", aux, want)
	}
	main := billingHeader("Hello, this is the first message", "2.1.280", &ccTurn{class: ClassMain, promptID: "p-1", promptIndex: 2, prevReq: "req_9"})
	if want := prefix + "e51; cc_entrypoint=cli; cch=00000; cc_prev_req=req_9; cc_prompt_id=p-1; cc_turn_origin=human; cc_prompt_index=2; cc_turn_index=2;"; main != want {
		t.Errorf("main header %q, want %q", main, want)
	}
	sub := billingHeader("Hello, this is the first message", "2.1.280", &ccTurn{class: ClassSubagent})
	if !strings.HasSuffix(sub, "cch=00000; cc_is_subagent=true;") {
		t.Errorf("subagent header %q", sub)
	}
	if jsCharAt("ab", 5) != "0" || jsCharAt("a😀b", 1) != "\uFFFD" || jsCharAt("a😀b", 3) != "b" {
		t.Error("jsCharAt UTF-16 semantics broken")
	}
}

func TestPatchBilling(t *testing.T) {
	body := func(model, maxTokens, text string) []byte {
		return []byte(`{"model":"` + model + `","messages":[{"role":"user","content":[{"type":"text","text":"` + text + `"}]}],` +
			`"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.296.abc; cc_entrypoint=cli; cch=00000;"}],` +
			`"max_tokens":` + maxTokens + `,"stream":true}`)
	}
	cch := func(b []byte) string {
		if !patchBilling(b) {
			t.Fatalf("patch not applied: %s", b)
		}
		i := bytes.Index(b, []byte("cch=")) + 4
		return string(b[i : i+5])
	}
	// Claude Code leaves the model and max_tokens out of the hash, so
	// switching either keeps cch; any other change moves it.
	base := cch(body("claude-opus-5-5", "128000", "say hi"))
	if got := cch(body("claude-haiku-4-5", "64", "say hi")); got != base {
		t.Errorf("model/max_tokens changed cch: %s vs %s", got, base)
	}
	if got := cch(body("claude-opus-5-5", "128000", "say ho")); got == base {
		t.Errorf("content change kept cch %s", got)
	}
	if got, want := string(billingHashInput(body("m", "5", "x"))), `{"model":"","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.296.abc; cc_entrypoint=cli; cch=00000;"}],"stream":true}`; got != want {
		t.Errorf("hash input = %s", got)
	}
	// Missing anchor / placeholder too far away are left alone.
	far := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header:` + strings.Repeat(" ", 200) + `cch=00000"}]}`)
	if patchBilling(far) || patchBilling([]byte(`{"system":"cch=00000"}`)) {
		t.Fatal("patched without a valid anchor")
	}
}

func TestAnthropicOAuthBody(t *testing.T) {
	p := &anthropicProvider{name: "anthropic", base: "x", oauth: staticToken("tok")}
	req := &Request{
		Model:     "claude-opus-4-6",
		System:    "SYS",
		SessionID: "sess",
		Thinking:  ThinkingHigh,
		Tools:     []Tool{{Name: "read", Description: "d"}},
		Messages: []Message{
			{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "<b>hi</b> there"}}},
			{Role: RoleAssistant, Blocks: []Block{
				{Type: BlockThinking, Text: "foreign"},
				{Type: BlockThinking, Text: "mine", Signature: "sig"},
				{Type: BlockToolUse, ID: "t1", Name: "read"},
			}},
			{Role: RoleUser, Blocks: []Block{{Type: BlockToolResult, ToolUseID: "t1", Content: "ok"}}},
			{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "next"}}},
		},
	}
	body, err := p.buildBody(req, "2.1.280", p.turn(context.Background(), req.SessionID, "", req.Messages))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.280.`)) {
		t.Fatalf("billing anchor missing: %s", body)
	}
	if bytes.Contains(body, []byte("cch=00000")) || !bytes.Contains(body, []byte("<b>hi</b>")) {
		t.Fatalf("placeholder unpatched or HTML escaped: %s", body)
	}
	// Two prompts so far: the billing header names the second one.
	if !bytes.Contains(body, []byte("cc_turn_origin=human; cc_prompt_index=2; cc_turn_index=2;")) {
		t.Fatalf("prompt position missing: %s", body)
	}
	// Re-zeroing the placeholder must reproduce the embedded hash.
	i := bytes.Index(body, []byte("cch=")) + 4
	got := string(body[i : i+5])
	copy(body[i:i+5], "00000")
	if want := fmt.Sprintf("%05x", xxHash64(billingHashInput(body), billingSeed)&0xfffff); got != want {
		t.Fatalf("cch=%s, want %s", got, want)
	}
	var r struct {
		System []struct {
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control"`
		} `json:"system"`
		Messages []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
		Tools        []map[string]any `json:"tools"`
		Thinking     map[string]any   `json:"thinking"`
		OutputConfig map[string]any   `json:"output_config"`
		MaxTokens    int              `json:"max_tokens"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.System) != 3 || r.System[1].Text != claudeCodeInstruction || r.System[2].Text != "SYS" || r.System[2].CacheControl == nil {
		t.Fatalf("system blocks wrong: %+v", r.System)
	}
	if len(r.Messages) != 3 {
		t.Fatalf("want 3 merged messages, got %d", len(r.Messages))
	}
	asst := r.Messages[1].Content
	if len(asst) != 2 || asst[0]["type"] != "thinking" || asst[1]["name"] != "_read" {
		t.Fatalf("assistant content wrong: %v", asst)
	}
	last := r.Messages[2].Content
	if len(last) != 2 || last[0]["type"] != "tool_result" || last[1]["cache_control"] == nil {
		t.Fatalf("final user content wrong: %v", last)
	}
	if r.Tools[0]["name"] != "_read" || r.Tools[0]["cache_control"] == nil {
		t.Fatalf("tools wrong: %v", r.Tools)
	}
	if r.Thinking["type"] != "adaptive" || r.OutputConfig["effort"] != "high" || r.MaxTokens != 32000 {
		t.Fatalf("thinking wrong: %v %v %d", r.Thinking, r.OutputConfig, r.MaxTokens)
	}
}

func TestAnthropicLongCacheTTL(t *testing.T) {
	p := &anthropicProvider{name: "anthropic", base: "x", oauth: staticToken("tok")}
	req := &Request{Model: "claude-sonnet-4-5", System: "SYS", Tools: []Tool{{Name: "read"}},
		Messages: []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "hi"}}}}}
	body, err := p.buildBody(req, "2.1.280", p.turn(context.Background(), "s", "", req.Messages))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(`"ttl"`)) {
		t.Fatalf("short-lived request asked for a 1h cache: %s", body)
	}
	req.LongCache = true
	body, _ = p.buildBody(req, "2.1.280", p.turn(context.Background(), "s", "", req.Messages))
	// Every breakpoint (system, tools, last user message) uses the same 1h
	// TTL: the API rejects a 1h entry after a 5m one.
	if n, m := bytes.Count(body, []byte(`"cache_control"`)), bytes.Count(body, []byte(`"cache_control":{"type":"ephemeral","ttl":"1h"}`)); n == 0 || n != m {
		t.Fatalf("%d breakpoints, %d with 1h ttl: %s", n, m, body)
	}
}

func TestAnthropicThinkingSelection(t *testing.T) {
	th, oc := anthropicThinking("claude-sonnet-4-5-20250929", ThinkingMedium)
	if th.Type != "enabled" || th.BudgetTokens != 12000 || oc != nil {
		t.Errorf("legacy: %+v %+v", th, oc)
	}
	th, oc = anthropicThinking("claude-opus-5-5", ThinkingLow)
	if th.Type != "adaptive" || th.Display != "summarized" || oc.Effort != "low" {
		t.Errorf("adaptive: %+v %+v", th, oc)
	}
	if th, _ := anthropicThinking("claude-opus-4-6", ThinkingLow); th.Display != "" {
		t.Errorf("4.6 should not request display: %+v", th)
	}
	if th, oc := anthropicThinking("claude-opus-4-6", ThinkingOff); th != nil || oc != nil {
		t.Error("off should omit thinking")
	}
	if n := anthropicMaxTokens("claude-haiku-4-5", 0, nil); n != 16000 {
		t.Errorf("haiku max tokens = %d", n)
	}
}

func TestParseAnthropicStream(t *testing.T) {
	stream := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"cache_read_input_tokens":5,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"SIG"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hi"}}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tu","name":"_read","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"a\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: content_block_start
data: {"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"tu2","name":"_ls","input":{}}}

event: content_block_stop
data: {"type":"content_block_stop","index":3}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`
	var evs []Event
	var msgID string
	msg, err := parseAnthropicStream(context.Background(), strings.NewReader(stream), true, func(e Event) { evs = append(evs, e) }, &msgID)
	if err != nil {
		t.Fatal(err)
	}
	if msg.StopReason != "tool_use" || msg.Usage.Input != 10 || msg.Usage.CacheRead != 5 || msg.Usage.Output != 42 || msgID != "msg_1" {
		t.Fatalf("meta wrong: %+v %+v", msg, msg.Usage)
	}
	if len(msg.Blocks) != 4 || msg.Blocks[0].Text != "hmm" || msg.Blocks[0].Signature != "SIG" || msg.Blocks[1].Text != "Hi" {
		t.Fatalf("blocks wrong: %+v", msg.Blocks)
	}
	if b := msg.Blocks[2]; b.Name != "read" || string(b.Input) != `{"path":"a"}` {
		t.Fatalf("tool wrong: %+v", b)
	}
	if string(msg.Blocks[3].Input) != "{}" {
		t.Fatalf("empty tool input = %s", msg.Blocks[3].Input)
	}
	if len(evs) != 5 || evs[0].Type != EvStarted || evs[3].Type != EvToolCall || evs[3].ToolName != "read" {
		t.Fatalf("events wrong: %+v", evs)
	}

	// Truncated stream: partial tool call is dropped, text kept.
	cut := stream[:strings.Index(stream, `"partial_json":"\"a\"}"`)]
	cut = cut[:strings.LastIndex(cut, "event:")]
	msg, err = parseAnthropicStream(context.Background(), strings.NewReader(cut), true, func(Event) {}, nil)
	if err == nil || len(msg.Blocks) != 2 || msg.Blocks[1].Text != "Hi" {
		t.Fatalf("truncated: err=%v blocks=%+v", err, msg.Blocks)
	}
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }
