package main

import (
	"context"
	"errors"
	"strings"

	"github.com/akbxr/tego/internal/config"
	"github.com/akbxr/tego/internal/llm"
)

// nativeSearch runs web_search through the current provider's built-in
// search (Anthropic server tool, Codex web_search, Gemini grounding on
// Antigravity). Anthropic and Antigravity run it on the small model (Haiku
// 5.5 searches as well as Haiku 4.5 did, at a tenth of the price; Gemini
// 3.5 Flash-Lite gave 3.8 Flash's answer in 2.7s against 4.4s). Providers
// without native search, or a failed one, make web_search fall through to
// Exa/Brave/Tavily/DuckDuckGo.
func (a *app) nativeSearch(ctx context.Context, query string) (*llm.WebResult, error) {
	ws, ok := a.ag.Provider.(llm.WebSearcher)
	if !ok {
		return nil, errors.New("provider has no native web search")
	}
	model, kind := a.ag.Model, a.cfg.Kind(a.ag.ProviderName)
	switch {
	case a.cfg.WebModel != "":
		model = a.cfg.WebModel
	case kind == "anthropic" || kind == llm.AntigravityKind:
		if s, ok := a.small(); ok && s.ProviderName == a.ag.ProviderName {
			model = s.Model
		}
	}
	if kind == llm.AntigravityKind && !strings.HasPrefix(model, "gemini") {
		// Claude on Antigravity: grounding is Gemini's.
		model = config.DefaultSmallModelFor(llm.AntigravityKind)
	}
	return ws.WebSearch(ctx, model, query)
}
