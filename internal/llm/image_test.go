package llm

import (
	"encoding/json"
	"testing"
)

var testImage = Block{Type: BlockImage, MediaType: "image/png", Data: "iVBORw0KGgo="}

const testDataURL = "data:image/png;base64,iVBORw0KGgo="

func imageHistory() []Message {
	return []Message{
		{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "look"}, testImage}},
		{Role: RoleAssistant, Blocks: []Block{
			{Type: BlockToolUse, ID: "a", Name: "read"},
			{Type: BlockToolUse, ID: "b", Name: "read"},
		}},
		// Tool results split across messages: images must follow the whole run.
		{Role: RoleUser, Blocks: []Block{{Type: BlockToolResult, ToolUseID: "a", Content: "Image x.png", Images: []Block{testImage}}}},
		{Role: RoleUser, Blocks: []Block{{Type: BlockToolResult, ToolUseID: "b", Content: "ok"}}},
	}
}

func decodeJSON(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
}

func TestAnthropicImages(t *testing.T) {
	p := &anthropicProvider{name: "anthropic", base: "x"}
	body, err := p.buildBody(&Request{Model: "claude-sonnet-4-5", Messages: imageHistory()}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Messages []struct {
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	decodeJSON(t, body, &r)
	if len(r.Messages) != 3 {
		t.Fatalf("want 3 messages: %s", body)
	}
	var img struct {
		Type   string            `json:"type"`
		Source map[string]string `json:"source"`
	}
	decodeJSON(t, r.Messages[0].Content[1], &img)
	if img.Type != "image" || img.Source["type"] != "base64" || img.Source["media_type"] != "image/png" || img.Source["data"] != testImage.Data {
		t.Fatalf("user image: %s", r.Messages[0].Content[1])
	}
	last := r.Messages[2].Content
	var tr struct {
		Type         string `json:"type"`
		Content      []map[string]any
		CacheControl any `json:"cache_control"`
	}
	decodeJSON(t, last[0], &tr)
	if tr.Type != "tool_result" || len(tr.Content) != 2 || tr.Content[0]["text"] != "Image x.png" || tr.Content[1]["type"] != "image" {
		t.Fatalf("tool_result with image: %s", last[0])
	}
	var plain struct {
		Content      string `json:"content"`
		CacheControl any    `json:"cache_control"`
	}
	decodeJSON(t, last[1], &plain)
	if plain.Content != "ok" || plain.CacheControl == nil {
		t.Fatalf("plain tool_result / breakpoint: %s", last[1])
	}

	// Cache breakpoint on a trailing image block.
	body, err = p.buildBody(&Request{Model: "claude-sonnet-4-5", Messages: imageHistory()[:1]}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	decodeJSON(t, body, &r)
	var tail map[string]any
	decodeJSON(t, r.Messages[0].Content[1], &tail)
	if tail["type"] != "image" || tail["cache_control"] == nil {
		t.Fatalf("image breakpoint: %s", body)
	}
}

func TestOpenAIImages(t *testing.T) {
	msgs := append(imageHistory(), Message{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "next"}}})
	body, err := openaiBody(&Request{Model: "gpt-4o", Messages: msgs}, false)
	if err != nil {
		t.Fatal(err)
	}
	type part struct {
		Type     string            `json:"type"`
		Text     string            `json:"text"`
		ImageURL map[string]string `json:"image_url"`
	}
	var r struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
	}
	decodeJSON(t, body, &r)
	var roles []string
	for _, m := range r.Messages {
		roles = append(roles, m.Role+":"+m.ToolCallID)
	}
	if got := len(r.Messages); got != 6 || r.Messages[2].ToolCallID != "a" || r.Messages[3].ToolCallID != "b" || r.Messages[4].Role != "user" || r.Messages[5].Role != "user" {
		t.Fatalf("message order %v: %s", roles, body)
	}
	var parts []part
	decodeJSON(t, r.Messages[0].Content, &parts)
	if len(parts) != 2 || parts[0].Text != "look" || parts[1].Type != "image_url" || parts[1].ImageURL["url"] != testDataURL {
		t.Fatalf("user parts: %s", r.Messages[0].Content)
	}
	decodeJSON(t, r.Messages[4].Content, &parts)
	if len(parts) != 2 || parts[0].Text != "Images returned by tool call a:" || parts[1].ImageURL["url"] != testDataURL {
		t.Fatalf("tool image message: %s", r.Messages[4].Content)
	}
	var s string
	decodeJSON(t, r.Messages[5].Content, &s)
	if s != "next" {
		t.Fatalf("trailing text: %s", r.Messages[5].Content)
	}
}

func TestCodexImages(t *testing.T) {
	body, err := codexBody(&Request{Model: "gpt-5", Messages: imageHistory()})
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Input []struct {
			Type    string              `json:"type"`
			Role    string              `json:"role"`
			Content []map[string]string `json:"content"`
			CallID  string              `json:"call_id"`
			Output  json.RawMessage     `json:"output"`
		} `json:"input"`
	}
	decodeJSON(t, body, &r)
	if len(r.Input) != 5 {
		t.Fatalf("want 5 items: %s", body)
	}
	c := r.Input[0].Content
	if r.Input[0].Role != "user" || len(c) != 2 || c[0]["type"] != "input_text" || c[1]["type"] != "input_image" || c[1]["image_url"] != testDataURL {
		t.Fatalf("user content: %v", c)
	}
	var out []map[string]string
	decodeJSON(t, r.Input[3].Output, &out)
	if r.Input[3].CallID != "a" || len(out) != 2 || out[0]["text"] != "Image x.png" || out[1]["type"] != "input_image" || out[1]["image_url"] != testDataURL {
		t.Fatalf("function_call_output with image: %s", r.Input[3].Output)
	}
	var s string
	decodeJSON(t, r.Input[4].Output, &s)
	if s != "ok" {
		t.Fatalf("plain output: %s", r.Input[4].Output)
	}
}
