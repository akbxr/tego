package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A /btw question reads the conversation's cache with its thinking: on a
// login it goes with the conversation's betas, as the utility ones refused
// it ("thinking.adaptive.block_binding: Extra inputs are not permitted"). A
// title keeps the utility betas.
func TestAuxiliaryBetas(t *testing.T) {
	f := &fastServer{}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	p := &anthropicProvider{name: "anthropic", base: srv.URL, oauth: staticToken("tok")}
	msgs := []Message{
		{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "hi"}}},
		{Role: RoleAssistant, Blocks: []Block{{Type: BlockText, Text: "hello"}}},
		{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "<btw>what did I ask?</btw>"}}},
	}
	for _, r := range []*Request{
		{Model: "claude-opus-5-5", Thinking: ThinkingLow, Class: ClassAuxiliary, CachePrefix: 2, Messages: msgs},
		{Model: "claude-haiku-4-5", MaxTokens: 64, Class: ClassAuxiliary, Messages: msgs[:1]},
		{Model: "claude-opus-5-5", Thinking: ThinkingLow, Messages: msgs},
	} {
		if _, err := p.Stream(context.Background(), r, nil); err != nil {
			t.Fatal(err)
		}
	}
	if f.betas[0] != claudeAgentBetas || f.betas[1] != claudeUtilityBetas || f.betas[2] != claudeAgentBetas {
		t.Fatalf("betas:\n%q", f.betas)
	}
}
