package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func agBuild(t *testing.T, req *Request) map[string]any {
	t.Helper()
	tg := resolveAntigravityModel(req.Model, req.Thinking, req.MaxTokens)
	p := newAntigravityProvider(AntigravityKind, "", "proj-1", nil, nil)
	body, err := antigravityBody(req, AntigravityKind, "proj-1", tg, p.turn(req, tg))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func agHistory(model string) []Message {
	img := Block{Type: BlockImage, MediaType: "image/png", Data: "iVBORw0K"}
	return []Message{
		{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "look"}, img}},
		{Role: RoleAssistant, Provider: AntigravityKind, Model: model, Blocks: []Block{
			{Type: BlockThinking, Text: "plan", Signature: "c2lnMQ=="},
			{Type: BlockText, Text: "Reading.", Signature: "c2lnMg=="},
			{Type: BlockToolUse, ID: "call:1", Name: "read", Input: json.RawMessage(`{"path":"go.mod"}`), Signature: "c2lnMw=="},
			{Type: BlockToolUse, ID: "call:2", Name: "glob", Input: json.RawMessage(`{"pattern":"*.go"}`)},
		}},
		{Role: RoleUser, Blocks: []Block{
			{Type: BlockToolResult, ToolUseID: "call:1", Content: "module tego", Images: []Block{img}},
			{Type: BlockToolResult, ToolUseID: "call:2", Content: "boom", IsError: true},
		}},
		// A turn from another provider: its thinking and signatures are not replayed.
		{Role: RoleAssistant, Provider: "anthropic", Model: "claude-sonnet-5-5", Blocks: []Block{
			{Type: BlockThinking, Text: "foreign", Signature: "QUJD"},
			{Type: BlockToolUse, ID: "t3", Name: "read", Input: json.RawMessage(`{"path":"a"}`)},
		}},
		{Role: RoleUser, Blocks: []Block{{Type: BlockToolResult, ToolUseID: "t3", Content: "ok"}}},
	}
}

func TestAntigravityBodyGemini(t *testing.T) {
	req := &Request{
		Model: "gemini-3.8-flash", Thinking: ThinkingMedium, System: "be brief", SessionID: "sess",
		Messages: agHistory("gemini-3.8-flash"),
		Tools:    []Tool{{Name: "read", Description: "Read a file", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}},
	}
	v := agBuild(t, req)
	if v["project"] != "proj-1" || v["model"] != "gemini-3.8-flash-medium" || v["userAgent"] != "antigravity" || v["requestType"] != "agent" {
		t.Fatalf("envelope = %s", jsonOf(v))
	}
	if id, _ := v["requestId"].(string); !strings.HasPrefix(id, "agent/") || !strings.HasSuffix(id, "/2") {
		t.Errorf("requestId = %q", id)
	}
	r := v["request"].(map[string]any)
	if got := jsonOf(r["systemInstruction"]); got != `{"parts":[{"text":"be brief"}],"role":"user"}` {
		t.Errorf("systemInstruction = %s", got)
	}
	if got := jsonOf(r["generationConfig"]); got != `{"maxOutputTokens":65536,"thinkingConfig":{"includeThoughts":true,"thinkingBudget":4000}}` {
		t.Errorf("generationConfig = %s", got)
	}
	if got := jsonOf(r["toolConfig"]); got != `{"functionCallingConfig":{"mode":"VALIDATED"}}` {
		t.Errorf("toolConfig = %s", got)
	}
	if got := jsonOf(r["tools"]); got != `[{"functionDeclarations":[{"description":"Read a file","name":"read","parameters":{"properties":{"path":{"type":"string"}},"required":["path"],"type":"object"}}]}]` {
		t.Errorf("tools = %s", got)
	}
	labels := r["labels"].(map[string]any)
	if labels["used_claude"] != "false" || labels["last_step_index"] != "1" || labels["trajectory_id"] == "" {
		t.Errorf("labels = %v", labels)
	}
	if s, _ := r["sessionId"].(string); !strings.HasPrefix(s, "-") {
		t.Errorf("sessionId = %q", s)
	}
	want := `[` +
		`{"parts":[{"text":"look"},{"inlineData":{"data":"iVBORw0K","mimeType":"image/png"}}],"role":"user"},` +
		`{"parts":[{"text":"plan","thought":true,"thoughtSignature":"c2lnMQ=="},{"text":"Reading.","thoughtSignature":"c2lnMg=="},` +
		`{"functionCall":{"args":{"path":"go.mod"},"name":"read"},"thoughtSignature":"c2lnMw=="},` +
		`{"functionCall":{"args":{"pattern":"*.go"},"name":"glob"}}],"role":"model"},` +
		`{"parts":[{"functionResponse":{"name":"read","parts":[{"inlineData":{"data":"iVBORw0K","mimeType":"image/png"}}],"response":{"output":"module tego"}}},` +
		`{"functionResponse":{"name":"glob","response":{"error":"boom"}}}],"role":"user"},` +
		`{"parts":[{"functionCall":{"args":{"path":"a"},"name":"read"},"thoughtSignature":"skip_thought_signature_validator"}],"role":"model"},` +
		`{"parts":[{"functionResponse":{"name":"read","response":{"output":"ok"}}}],"role":"user"}]`
	if got := jsonOf(r["contents"]); got != want {
		t.Errorf("contents:\n got %s\nwant %s", got, want)
	}

	// Another model of the same provider must not receive these signatures.
	req.Model = "gemini-3.7-flash"
	r = agBuild(t, req)["request"].(map[string]any)
	if got := jsonOf(r["contents"]); strings.Contains(got, "c2ln") || strings.Contains(got, `"thought":true`) {
		t.Errorf("signatures replayed to a different model: %s", got)
	}
}

func TestAntigravityBodyClaude(t *testing.T) {
	req := &Request{Model: "claude-opus-4-6", Thinking: ThinkingMedium, Messages: agHistory("claude-opus-4-6"),
		Tools: []Tool{{Name: "read", Schema: json.RawMessage(`{"type":"object","properties":{}}`)}}}
	v := agBuild(t, req)
	if v["model"] != "claude-opus-4-6-thinking" {
		t.Fatalf("model = %v", v["model"])
	}
	r := v["request"].(map[string]any)
	if got := jsonOf(r["generationConfig"]); got != `{"maxOutputTokens":64000,"thinkingConfig":{"includeThoughts":true,"thinkingBudget":8192}}` {
		t.Errorf("generationConfig = %s", got)
	}
	if r["labels"].(map[string]any)["used_claude"] != "true" {
		t.Errorf("labels = %v", r["labels"])
	}
	contents := r["contents"].([]any)
	want := `[` +
		`{"parts":[{"text":"look"},{"inlineData":{"data":"iVBORw0K","mimeType":"image/png"}}],"role":"user"},` +
		`{"parts":[{"text":"plan","thought":true,"thoughtSignature":"c2lnMQ=="},{"text":"Reading.","thoughtSignature":"c2lnMg=="},` +
		`{"functionCall":{"args":{"path":"go.mod"},"id":"call_1","name":"read"},"thoughtSignature":"c2lnMw=="},` +
		`{"functionCall":{"args":{"pattern":"*.go"},"id":"call_2","name":"glob"}}],"role":"model"},` +
		`{"parts":[{"functionResponse":{"id":"call_1","name":"read","response":{"output":"module tego"}}},` +
		`{"functionResponse":{"id":"call_2","name":"glob","response":{"error":"boom"}}}],"role":"user"},` +
		`{"parts":[{"text":"Tool result image:"},{"inlineData":{"data":"iVBORw0K","mimeType":"image/png"}}],"role":"user"},` +
		`{"parts":[{"functionCall":{"args":{"path":"a"},"id":"t3","name":"read"}}],"role":"model"},` +
		`{"parts":[{"functionResponse":{"id":"t3","name":"read","response":{"output":"ok"}}}],"role":"user"}]`
	if got := jsonOf(contents); got != want {
		t.Errorf("contents:\n got %s\nwant %s", got, want)
	}
	p := &antigravityProvider{}
	t.Setenv("TEGO_ANTIGRAVITY_VERSION", "9.9.9")
	h := p.header(context.Background(), "tok", resolveAntigravityModel("claude-opus-4-6", ThinkingMedium, 0))
	if h.Get("anthropic-beta") != antigravityClaudeBeta || h.Get("Authorization") != "Bearer tok" ||
		h.Get("User-Agent") != "antigravity/hub/9.9.9 (aidev_client; os_type=darwin; arch=arm64; cl=963137146)" {
		t.Errorf("headers = %v", h)
	}
}

func TestAntigravityTextOnlyModelImages(t *testing.T) {
	req := &Request{Model: "gpt-oss-120b", Messages: []Message{
		{Role: RoleUser, Blocks: []Block{{Type: BlockImage, MediaType: "image/png", Data: "x"}}},
	}}
	r := agBuild(t, req)["request"].(map[string]any)
	if got := jsonOf(r["contents"]); got != `[{"parts":[{"text":"[image omitted: model does not support vision]"}],"role":"user"}]` {
		t.Errorf("contents = %s", got)
	}
}

func TestResolveAntigravityModel(t *testing.T) {
	cases := []struct {
		model, thinking, wire, thinkingCfg string
		maxOut                             int
	}{
		{"gemini-3.8-flash", ThinkingOff, "gemini-3.8-flash-low", `{"includeThoughts":true,"thinkingBudget":1000}`, 65536},
		{"gemini-3.8-flash", ThinkingMedium, "gemini-3.8-flash-medium", `{"includeThoughts":true,"thinkingBudget":4000}`, 65536},
		{"gemini-3.8-flash", ThinkingHigh, "gemini-3.8-flash-high", `{"includeThoughts":true,"thinkingLevel":"HIGH"}`, 65536},
		{"gemini-3.5-flash", ThinkingOff, "gemini-3.5-flash-extra-low", `{"includeThoughts":false,"thinkingBudget":0}`, 65536},
		{"gemini-3.5-flash", ThinkingMedium, "gemini-3.5-flash-low", `{"includeThoughts":true,"thinkingBudget":4000}`, 65536},
		{"gemini-3.5-flash", ThinkingHigh, "gemini-3-flash-agent", `{"includeThoughts":true,"thinkingBudget":10000}`, 65536},
		{"gemini-3.1-pro", ThinkingMedium, "gemini-3.1-pro-low", `{"includeThoughts":true,"thinkingBudget":1001}`, 65535},
		{"gemini-3.1-pro", ThinkingHigh, "gemini-pro-agent", `{"includeThoughts":true,"thinkingBudget":10001}`, 65535},
		{"gemini-3.5-flash-lite", ThinkingOff, "gemini-3.5-flash-lite", `{"includeThoughts":true,"thinkingLevel":"MINIMAL"}`, 65535},
		{"gemini-2.5-flash", ThinkingOff, "gemini-2.5-flash", `null`, 65535},
		{"gemini-2.5-flash", ThinkingLow, "gemini-2.5-flash-thinking", `{"includeThoughts":true,"thinkingBudget":4096}`, 65535},
		{"claude-sonnet-4-6", ThinkingOff, "claude-sonnet-4-6", `null`, 64000},
		{"claude-sonnet-4-6", ThinkingHigh, "claude-sonnet-4-6", `{"includeThoughts":true,"thinkingBudget":16384}`, 64000},
		{"gpt-oss-120b", ThinkingLow, "gpt-oss-120b-medium", `{"includeThoughts":true,"thinkingBudget":4096}`, 32768},
	}
	for _, c := range cases {
		got := resolveAntigravityModel(c.model, c.thinking, 0)
		if got.wire != c.wire || jsonOf(got.thinking) != c.thinkingCfg || got.maxOutput != c.maxOut {
			t.Errorf("%s/%s = %s %s %d, want %s %s %d", c.model, c.thinking, got.wire, jsonOf(got.thinking), got.maxOutput, c.wire, c.thinkingCfg, c.maxOut)
		}
	}
	// An explicit output limit is shared with the thinking budget.
	got := resolveAntigravityModel("claude-sonnet-4-6", ThinkingHigh, 8000)
	if got.maxOutput != 24384 || *got.thinking.ThinkingBudget != 16384 {
		t.Errorf("limit = %d budget = %d", got.maxOutput, *got.thinking.ThinkingBudget)
	}
}

func TestCleanSchemaForCCA(t *testing.T) {
	in := `{
	  "$schema": "http://json-schema.org/draft-07/schema#",
	  "type": "object",
	  "additionalProperties": false,
	  "$defs": {"mode": {"type": "string", "enum": ["fast", "slow"], "description": "speed"}},
	  "properties": {
	    "path": {"type": "string", "minLength": 1, "format": "uri", "description": "file"},
	    "mode": {"$ref": "#/$defs/mode"},
	    "limit": {"type": ["integer", "null"], "minimum": 1},
	    "kind": {"const": "x"},
	    "level": {"type": "integer", "enum": [1, 2]},
	    "opt": {"anyOf": [{"type": "string"}, {"type": "null"}], "description": "optional"},
	    "mixed": {"oneOf": [{"type": "string"}, {"type": "number"}]},
	    "tags": {"type": "array", "items": {"type": "string", "pattern": "^a"}, "maxItems": 3},
	    "obj": {"allOf": [{"type": "object", "properties": {"a": {"type": "string"}}, "required": ["a"]},
	                      {"properties": {"b": {"type": "boolean"}}}]},
	    "loop": {"$ref": "#"}
	  },
	  "required": ["path", "missing"]
	}`
	got := string(cleanSchemaForCCA(json.RawMessage(in)))
	want := `{"properties":{` +
		`"kind":{"enum":["x"],"type":"string"},` +
		`"level":{"description":"{enum: [1,2]}","type":"integer"},` +
		`"limit":{"description":"{minimum: 1}","type":"integer"},` +
		`"loop":{"properties":{"kind":{"enum":["x"],"type":"string"},"level":{"description":"{enum: [1,2]}","type":"integer"},"limit":{"description":"{minimum: 1}","type":"integer"},"loop":{},"mixed":{"description":"{one of: string | number}","type":"string"},"mode":{"description":"speed","enum":["fast","slow"],"type":"string"},"obj":{"properties":{"a":{"type":"string"},"b":{"type":"boolean"}},"required":["a"],"type":"object"},"opt":{"description":"optional","type":"string"},"path":{"description":"file\n{format: \"uri\", minLength: 1}","type":"string"},"tags":{"description":"{maxItems: 3}","items":{"description":"{pattern: \"^a\"}","type":"string"},"type":"array"}},"required":["path"],"type":"object"},` +
		`"mixed":{"description":"{one of: string | number}","type":"string"},` +
		`"mode":{"description":"speed","enum":["fast","slow"],"type":"string"},` +
		`"obj":{"properties":{"a":{"type":"string"},"b":{"type":"boolean"}},"required":["a"],"type":"object"},` +
		`"opt":{"description":"optional","type":"string"},` +
		`"path":{"description":"file\n{format: \"uri\", minLength: 1}","type":"string"},` +
		`"tags":{"description":"{maxItems: 3}","items":{"description":"{pattern: \"^a\"}","type":"string"},"type":"array"}` +
		`},"required":["path"],"type":"object"}`
	if got != want {
		t.Errorf("schema:\n got %s\nwant %s", got, want)
	}
	for _, in := range []string{``, `null`, `{"type":"string"}`, `not json`} {
		if got := string(cleanSchemaForCCA(json.RawMessage(in))); got != `{"properties":{},"type":"object"}` {
			t.Errorf("cleanSchemaForCCA(%q) = %s", in, got)
		}
	}
}

func agSSE(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	return b.String()
}

func TestParseAntigravityStream(t *testing.T) {
	in := agSSE(
		`{"response":{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"Let me "}]}}],"responseId":"r1"}}`,
		`{"response":{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"think","thoughtSignature":"VEhJTks="}]}}]}}`,
		`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"Reading "},{"text":"go.mod"}]}}]}}`,
		`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"","thoughtSignature":"VEVYVA=="}]}}]}}`,
		`{"response":{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read","args":{"path":"go.mod"}},"thoughtSignature":"RkM="},{"functionCall":{"id":"c2","name":"glob","args":{}}}]}}]}}`,
		`{"response":{"candidates":[{"content":{"role":"model"},"finishReason":"STOP"}]}}`,
		`{"response":{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"thoughtsTokenCount":7,"cachedContentTokenCount":40,"totalTokenCount":127}}}`,
	)
	var events []string
	msg, rid, err := parseAntigravityStream(context.Background(), "ag", strings.NewReader(in), func(e Event) {
		switch e.Type {
		case EvText:
			events = append(events, "text:"+e.Text)
		case EvThinking:
			events = append(events, "think:"+e.Text)
		case EvToolCall:
			events = append(events, "tool:"+e.ToolName)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if rid != "r1" || msg.StopReason != "tool_use" {
		t.Fatalf("rid=%q stop=%q", rid, msg.StopReason)
	}
	if got := strings.Join(events, "|"); got != "think:Let me |think:think|text:Reading |text:go.mod|tool:read|tool:glob" {
		t.Errorf("events = %s", got)
	}
	if u := msg.Usage; u.Input != 60 || u.CacheRead != 40 || u.Output != 27 {
		t.Errorf("usage = %+v", u)
	}
	b := msg.Blocks
	if len(b) != 4 {
		t.Fatalf("blocks = %+v", b)
	}
	if b[0].Type != BlockThinking || b[0].Text != "Let me think" || b[0].Signature != "VEhJTks=" {
		t.Errorf("thinking = %+v", b[0])
	}
	if b[1].Type != BlockText || b[1].Text != "Reading go.mod" || b[1].Signature != "VEVYVA==" {
		t.Errorf("text = %+v", b[1])
	}
	if b[2].Type != BlockToolUse || b[2].Name != "read" || !strings.HasPrefix(b[2].ID, "read_") || string(b[2].Input) != `{"path":"go.mod"}` || b[2].Signature != "RkM=" {
		t.Errorf("call 1 = %+v", b[2])
	}
	if b[3].ID != "c2" || string(b[3].Input) != `{}` {
		t.Errorf("call 2 = %+v", b[3])
	}
	// Thoughts are buffered before the first chunk: only the answer is timed.
	if msg.Usage.Output != 27 || msg.GenOutput != 20 {
		t.Errorf("output %d, gen output %d", msg.Usage.Output, msg.GenOutput)
	}
}

func TestParseAntigravityStreamEndings(t *testing.T) {
	text := `{"response":{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}}`
	cases := []struct {
		name, in, stop string
		check          func(error) bool
	}{
		{"stop", agSSE(text, `{"response":{"candidates":[{"finishReason":"STOP"}]}}`), "end_turn", nil},
		{"max tokens", agSSE(text, `{"response":{"candidates":[{"finishReason":"MAX_TOKENS"}]}}`), "max_tokens", nil},
		// A call cut off by the limit still parses: the limit must not read as a finished tool turn.
		{"max tokens in a call", agSSE(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"w1","name":"write","args":{"path":"a.go","content":"package a\nfunc"}}}]},"finishReason":"MAX_TOKENS"}]}}`), "max_tokens", nil},
		{"safety", agSSE(text, `{"response":{"candidates":[{"finishReason":"SAFETY"}]}}`), "", func(err error) bool {
			return strings.Contains(err.Error(), "SAFETY")
		}},
		{"empty", agSSE(`{"response":{"candidates":[{"finishReason":"STOP"}]}}`), "", func(err error) bool {
			var ae *APIError
			return errors.As(err, &ae) && ae.retryable()
		}},
		{"truncated", agSSE(text), "", func(err error) bool {
			var se *streamError
			return errors.As(err, &se)
		}},
		{"thought only", agSSE(`{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"hmm"}]},"finishReason":"STOP"}]}}`), "", func(err error) bool {
			return strings.Contains(err.Error(), "thought-only")
		}},
		{"error chunk", agSSE(`{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`), "", func(err error) bool {
			var ae *APIError
			return errors.As(err, &ae) && ae.Status == 429 && !ae.retryable()
		}},
		{"blocked", agSSE(`{"response":{"promptFeedback":{"blockReason":"SAFETY"}}}`), "", func(err error) bool {
			return strings.Contains(err.Error(), "Request blocked by Google (SAFETY)")
		}},
	}
	for _, c := range cases {
		msg, _, err := parseAntigravityStream(context.Background(), "ag", strings.NewReader(c.in), func(Event) {})
		if c.check == nil {
			if err != nil || msg.StopReason != c.stop {
				t.Errorf("%s: err=%v stop=%q", c.name, err, msg.StopReason)
			}
			continue
		}
		if err == nil || !c.check(err) {
			t.Errorf("%s: unexpected err %v", c.name, err)
		}
	}
}

func TestAntigravityAPIError(t *testing.T) {
	body := `{"error":{"code":429,"message":"You have exhausted your capacity on this model. Your quota will reset after 1h.","status":"RESOURCE_EXHAUSTED",` +
		`"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"3600s"}]}}`
	err := antigravityAPIError(&APIError{Provider: "ag", Status: 429, Body: body})
	var ae *APIError
	if !errors.As(err, &ae) || ae.RetryAfter != time.Hour || ae.retryable() ||
		ae.Body != "You have exhausted your capacity on this model. Your quota will reset after 1h. (RESOURCE_EXHAUSTED)" {
		t.Fatalf("err = %#v", err)
	}
	v := `{"error":{"code":403,"message":"denied","details":[{"reason":"VALIDATION_REQUIRED","metadata":{"validation_url":"https://x/verify"}}]}}`
	if err := antigravityAPIError(&APIError{Status: 403, Body: v}); !strings.Contains(err.Error(), "Visit https://x/verify") {
		t.Errorf("validation err = %v", err)
	}
}

func TestAntigravityEndpointFallback(t *testing.T) {
	t.Setenv("TEGO_ANTIGRAVITY_VERSION", "1.2.3")
	var hits []string
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, "bad")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":{"code":503,"message":"overloaded"}}`)
	}))
	defer bad.Close()
	var gotBody map[string]any
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, "good")
		if r.URL.Path != "/v1internal:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" ||
			r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Accept") != "text/event-stream" ||
			!strings.HasPrefix(r.Header.Get("User-Agent"), "antigravity/hub/1.2.3 ") {
			t.Errorf("request %s %v", r.URL, r.Header)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		io.WriteString(w, agSSE(
			`{"response":{"responseId":"exec-1","candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}}`))
	}))
	defer good.Close()

	p := newAntigravityProvider(AntigravityKind, "", "proj", nil, staticToken("tok"))
	p.endpoints = []string{bad.URL, good.URL}
	req := &Request{Model: "gemini-3.8-flash", SessionID: "s", Messages: []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "hi"}}}}}
	msg, err := p.Stream(context.Background(), req, nil)
	if err != nil || msg.Blocks[0].Text != "ok" || msg.Provider != AntigravityKind || msg.Model != "gemini-3.8-flash" {
		t.Fatalf("msg=%+v err=%v", msg, err)
	}
	if fmt.Sprint(hits) != "[bad good]" {
		t.Fatalf("hits = %v", hits)
	}
	// The endpoint that worked is tried first next time, and the response id
	// becomes the last_execution_id label.
	hits = nil
	if _, err := p.Stream(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(hits) != "[good]" {
		t.Fatalf("second hits = %v", hits)
	}
	labels := gotBody["request"].(map[string]any)["labels"].(map[string]any)
	if labels["last_execution_id"] != "exec-1" || labels["last_step_index"] != "2" {
		t.Errorf("labels = %v", labels)
	}
}

func TestAntigravityModelIDs(t *testing.T) {
	var pl agModelsPayload
	raw := `{"models":{
	  "gemini-3.8-flash-low":{},"gemini-3.8-flash-medium":{},"gemini-3.8-flash-high":{},"gemini-3.8-flash-tiered":{},
	  "gemini-3.5-flash-extra-low":{},"gemini-3-flash-agent":{},"gemini-3-flash":{},
	  "gemini-3.1-pro-low":{},"gemini-pro-agent":{},"claude-opus-4-6-thinking":{},"claude-sonnet-4-6":{},
	  "gpt-oss-120b-medium":{},"gemini-2.5-flash-thinking":{},"gemini-2.5-flash":{},"gemini-2.5-pro":{},
	  "chat_20706":{"isInternal":true},"tab_flash_lite_preview":{},"gemini-3.1-flash-image":{},"gemini-3.5-flash-lite":{}},
	  "imageGenerationModelIds":["gemini-3.1-flash-image"]}`
	if err := json.Unmarshal([]byte(raw), &pl); err != nil {
		t.Fatal(err)
	}
	want := "[claude-opus-4-6 claude-sonnet-4-6 gemini-2.5-flash gemini-3-flash gemini-3.1-pro gemini-3.5-flash gemini-3.5-flash-lite gemini-3.8-flash gpt-oss-120b]"
	if got := fmt.Sprint(pl.ids()); got != want {
		t.Errorf("ids = %s", got)
	}
}

func TestAnthropicDropsAntigravitySignatures(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "hi"}}},
		{Role: RoleAssistant, Provider: AntigravityKind, Model: "claude-opus-4-6", Blocks: []Block{
			{Type: BlockThinking, Text: "gemini thought", Signature: "c2ln"},
			{Type: BlockText, Text: "hello", Signature: "c2ln"},
		}},
		{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "again"}}},
	}
	got := jsonOf(anthropicMessages(msgs, false, ephemeral, nil, nil))
	if strings.Contains(got, "thinking") || strings.Contains(got, "c2ln") {
		t.Errorf("antigravity signatures replayed to Anthropic: %s", got)
	}
}
