package main

import (
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
