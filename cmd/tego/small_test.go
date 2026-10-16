package main

import (
	"context"
	"testing"

	"github.com/akbxr/tego/internal/agent"
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
