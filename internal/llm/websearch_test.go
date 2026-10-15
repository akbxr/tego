package llm

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAnthropicWebSearchBodyOAuth(t *testing.T) {
	p := &anthropicProvider{name: "anthropic", base: "x", oauth: staticToken("tok")}
	body, err := p.webSearchBody("claude-sonnet-5-5", "go release", "2.1.280", []aRawMessage{{Role: RoleUser, Content: "go release"}},
		p.turn(context.Background(), "s", ClassAuxiliary, nil))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, `"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.280.`) || strings.Contains(s, billingPlaceholder) {
		t.Errorf("billing header missing or unpatched: %s", s)
	}
	var r struct {
		System []aSystemBlock `json:"system"`
		Tools  []aServerTool  `json:"tools"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.System) != 3 || r.System[1].Text != claudeCodeInstruction || r.System[2].Text != webSearchInstruction {
		t.Errorf("system = %+v", r.System)
	}
	want := []aServerTool{{Type: "web_search_20250305", Name: "web_search", MaxUses: 5}}
	if !reflect.DeepEqual(r.Tools, want) {
		t.Errorf("tools = %+v", r.Tools)
	}
}

func TestParseAnthropicWebContent(t *testing.T) {
	const resp = `{"stop_reason":"end_turn","content":[
	 {"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"go release"}},
	 {"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[
	   {"type":"web_search_result","url":"https://go.dev/doc/devel/release","title":"Release History","page_age":"2 days ago","encrypted_content":"x"},
	   {"type":"web_search_result","url":"https://go.dev/blog/go1.27","title":"Go 1.27 is released"},
	   {"type":"web_search_result","url":"https://example.com/news","title":"News"}]},
	 {"type":"text","text":"Go 1.27 "},
	 {"type":"text","text":"was released in August 2026.","citations":[
	   {"type":"web_search_result_location","url":"https://go.dev/blog/go1.27","title":"Go 1.27 is released","cited_text":"..."},
	   {"type":"web_search_result_location","url":"https://go.dev/blog/go1.27","title":"Go 1.27 is released","cited_text":"..."}]},
	 {"type":"web_search_tool_result","tool_use_id":"srvtoolu_2","content":[{"type":"web_search_result","url":"https://go.dev/doc/devel/release","title":"dup"}]},
	 {"type":"text","text":" Patch 1.27.2 followed.","citations":[{"type":"web_search_result_location","url":"https://example.com/news","title":"News"}]}]}`
	var r aWebResponse
	if err := json.Unmarshal([]byte(resp), &r); err != nil {
		t.Fatal(err)
	}
	got, err := parseAnthropicWebContent(r.Content)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "Go 1.27 was released in August 2026. Patch 1.27.2 followed." {
		t.Errorf("text = %q", got.Text)
	}
	want := []Source{
		{"Go 1.27 is released", "https://go.dev/blog/go1.27"},
		{"News", "https://example.com/news"},
		{"Release History", "https://go.dev/doc/devel/release"},
	}
	if !reflect.DeepEqual(got.Sources, want) {
		t.Errorf("sources = %+v", got.Sources)
	}

	errResp := []json.RawMessage{json.RawMessage(`{"type":"web_search_tool_result","content":{"type":"web_search_tool_result_error","error_code":"max_uses_exceeded"}}`)}
	if _, err := parseAnthropicWebContent(errResp); err == nil || !strings.Contains(err.Error(), "max_uses_exceeded") {
		t.Errorf("err = %v", err)
	}
}

func TestParseCodexWebStream(t *testing.T) {
	events := []string{
		`{"type":"response.created","response":{"id":"r1"}}`,
		`{"type":"response.web_search_call.in_progress","item_id":"ws_1"}`,
		`{"type":"response.output_item.done","item":{"type":"web_search_call","id":"ws_1","action":{"type":"search","query":"go","sources":[{"type":"url","url":"https://go.dev/dl/?utm_source=openai"},{"type":"url","url":"https://example.com/a?x=1&utm_source=openai","title":"A"}]}}}`,
		`{"type":"response.output_text.delta","item_id":"m1","delta":"Go 1.27"}`,
		`{"type":"response.output_item.done","item":{"type":"message","id":"m1","content":[{"type":"output_text","text":"Go 1.27 is current ([go.dev](https://go.dev/dl/?utm_source=openai)).","annotations":[{"type":"url_citation","url":"https://go.dev/dl/?utm_source=openai","title":"Downloads","start_index":17,"end_index":60}]}]}}`,
		`{"type":"response.completed","response":{"status":"completed"}}`,
	}
	var sse strings.Builder
	for _, e := range events {
		sse.WriteString("data: " + e + "\n\n")
	}
	got, err := parseCodexWebStream(context.Background(), "openai-codex", strings.NewReader(sse.String()))
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "Go 1.27 is current ([go.dev](https://go.dev/dl/?utm_source=openai))." {
		t.Errorf("text = %q", got.Text)
	}
	want := []Source{{"Downloads", "https://go.dev/dl/"}, {"A", "https://example.com/a?x=1"}}
	if !reflect.DeepEqual(got.Sources, want) {
		t.Errorf("sources = %+v", got.Sources)
	}

	noSearch := "data: " + `{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"guess"}]}}` + "\n\n" +
		"data: " + `{"type":"response.completed","response":{}}` + "\n\n"
	if _, err := parseCodexWebStream(context.Background(), "openai-codex", strings.NewReader(noSearch)); err == nil {
		t.Error("expected an error for an answer without web_search_call")
	}
}
