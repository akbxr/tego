package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/auth"
	"github.com/akbxr/tego/internal/config"
	"github.com/akbxr/tego/internal/llm"
)

type nameOnly struct{ llm.Provider }

func TestSmallModel(t *testing.T) {
	main := nameOnly{}
	a := &app{cfg: &config.Config{}, ag: &agent.Agent{Provider: main, ProviderName: "anthropic", Model: "claude-opus-5-5"}}
	s, ok := a.small()
	if !ok || s.Model != "claude-haiku-5-5" || s.ProviderName != "anthropic" || s.Provider != main || s.Window == 0 {
		t.Fatalf("small %+v ok=%v", s, ok)
	}
	// The session already runs on it, or it is off: none.
	a.ag.Model = "claude-haiku-5-5"
	if _, ok := a.small(); ok {
		t.Fatal("small model equal to the session's")
	}
	a.ag.Model, a.cfg.SmallModel = "claude-opus-5-5", "off"
	if _, ok := a.small(); ok {
		t.Fatal("small model when off")
	}
	// A provider without a default: none (the session's model does the side work).
	a.cfg.SmallModel, a.ag.ProviderName, a.ag.Model = "", "openai-codex", "gpt-5.6"
	if _, ok := a.small(); ok {
		t.Fatal("small model on a provider without one")
	}
}

// subagent_models offers the models whose provider is logged in, other than
// the session's, and builds a provider for another subscription once.
func TestSubagentModels(t *testing.T) {
	dir := t.TempDir()
	store, err := auth.Open(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.Add("openai", auth.Credential{Type: "api_key", Key: "sk-test-123456789"})
	t.Setenv("XAI_API_KEY", "")
	cfg := &config.Config{Dir: dir, SubModels: map[string]config.SubagentModel{
		"gpt":   {Model: "openai/gpt-5.5", Use: "review", Thinking: "high"},
		"grok":  {Model: "xai/grok-5"},               // no key
		"opus":  {Model: "claude-opus-5-5"},          // the session's model
		"haiku": {Model: "claude-haiku-5-5"},         // bare: the session's provider
		"local": {Model: "ollama/qwen3-coder"},       // needs no key
		"nope":  {Model: "no-such-provider/model-1"}, // unknown
	}}
	main := nameOnly{}
	a := &app{cfg: cfg, store: store, ag: &agent.Agent{Provider: main, ProviderName: "anthropic", Model: "claude-opus-5-5"}}
	var names []string
	for _, m := range a.subagentModels() {
		names = append(names, m.Name+"="+m.Spec)
	}
	if got := strings.Join(names, " "); got != "gpt=openai/gpt-5.5 haiku=anthropic/claude-haiku-5-5 local=ollama/qwen3-coder" {
		t.Fatalf("offered %s", got)
	}
	ms := a.subagentModels()
	if ms[0].Use != "review" || ms[0].Thinking != "high" {
		t.Fatalf("gpt %+v", ms[0])
	}
	g1, err := ms[0].Resolve()
	if err != nil || g1.Provider == nil || g1.Provider == llm.Provider(main) || g1.ProviderName != "openai" || g1.Window == 0 {
		t.Fatalf("gpt resolves to %+v, %v", g1, err)
	}
	if g2, _ := ms[0].Resolve(); g2.Provider != g1.Provider {
		t.Fatal("a second task built another provider")
	}
	if h, _ := ms[1].Resolve(); h.Provider != llm.Provider(main) {
		t.Fatal("a model of the session's provider got its own provider")
	}

	// Switching the session's model refreshes the list at once.
	a.ag.Model = "claude-haiku-5-5"
	for _, m := range a.subagentModels() {
		if m.Name == "haiku" {
			t.Fatal("offers the session's own model")
		}
	}
}

type searchRecorder struct {
	llm.Provider
	model string
}

func (s *searchRecorder) WebSearch(_ context.Context, model, _ string) (*llm.WebResult, error) {
	s.model = model
	return &llm.WebResult{}, nil
}

// Web search runs on the small model where the provider searches natively,
// and on Antigravity always on a Gemini model: grounding is Gemini's.
func TestNativeSearchModel(t *testing.T) {
	for _, c := range []struct{ provider, model, small, web, want string }{
		{"anthropic", "claude-opus-5-5", "", "", "claude-haiku-5-5"},
		{"anthropic", "claude-opus-5-5", "off", "", "claude-opus-5-5"},
		{"anthropic", "claude-opus-5-5", "", "claude-sonnet-5-5", "claude-sonnet-5-5"},
		{"google-antigravity", "gemini-3.8-flash", "", "", "gemini-3.5-flash-lite"},
		{"google-antigravity", "gemini-3.8-flash", "off", "", "gemini-3.8-flash"},
		{"google-antigravity", "claude-opus-4-6", "off", "", "gemini-3.5-flash-lite"},
		{"google-antigravity", "claude-opus-4-6", "", "gemini-3.8-flash", "gemini-3.8-flash"},
		{"openai-codex", "gpt-5.6", "", "", "gpt-5.6"},
	} {
		s := &searchRecorder{}
		a := &app{cfg: &config.Config{SmallModel: c.small, WebModel: c.web}, ag: &agent.Agent{Provider: s, ProviderName: c.provider, Model: c.model}}
		if _, err := a.nativeSearch(context.Background(), "q"); err != nil || s.model != c.want {
			t.Errorf("%s/%s small %q web %q: searched with %q, want %q (%v)", c.provider, c.model, c.small, c.web, s.model, c.want, err)
		}
	}
}
