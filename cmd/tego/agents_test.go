package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/auth"
	"github.com/akbxr/tego/internal/config"
	"github.com/akbxr/tego/internal/llm"
)

// An agent file's model: Claude's aliases go to the session's model when it
// is of that family, else to Anthropic; provider/model needs a login.
func TestResolveAgentModel(t *testing.T) {
	dir := t.TempDir()
	store, err := auth.Open(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.Add("anthropic", auth.Credential{Type: "api_key", Key: "sk-ant-test-123456"})
	t.Setenv("XAI_API_KEY", "")
	main := nameOnly{}
	a := &app{cfg: &config.Config{Dir: dir}, store: store, ag: &agent.Agent{Provider: main, ProviderName: "anthropic", Model: "claude-opus-5-5"}}

	if m, err := a.resolveModel("opus"); err != nil || m.Provider != nil {
		t.Errorf("opus on an Opus session: %+v %v, want the session's", m, err)
	}
	if m, err := a.resolveModel("haiku"); err != nil || m.Provider != llm.Provider(main) || m.Model != "claude-haiku-5-5" {
		t.Errorf("haiku: %+v %v", m, err)
	}
	// On another subscription, an alias still means Claude.
	a.ag.ProviderName, a.ag.Model = "openai-codex", "gpt-5.6-terra"
	if m, err := a.resolveModel("Sonnet"); err != nil || m.ProviderName != "anthropic" || m.Model != "claude-sonnet-5-5" || m.Provider == llm.Provider(main) {
		t.Errorf("sonnet from Codex: %+v %v", m, err)
	}
	if _, err := a.resolveModel("xai/grok-5"); err == nil || !strings.Contains(err.Error(), "not logged in to xai") {
		t.Errorf("grok without a login: %v", err)
	}
	if m, err := a.resolveModel("openai-codex/gpt-5.6-terra"); err != nil || m.Provider != nil {
		t.Errorf("the session's own model: %+v %v", m, err)
	}
}
