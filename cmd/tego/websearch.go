package main

import (
	"context"
	"errors"

	"github.com/akbxr/tego/internal/llm"
)

// nativeSearch runs web_search through the current provider's built-in
// search (Anthropic server tool, Codex web_search). Anthropic uses a fast
// model for it. Providers without
// native search make web_search fall through to Exa/Brave/Tavily/DuckDuckGo.
func (a *app) nativeSearch(ctx context.Context, query string) (*llm.WebResult, error) {
	ws, ok := a.ag.Provider.(llm.WebSearcher)
	if !ok {
		return nil, errors.New("provider has no native web search")
	}
	model := a.ag.Model
	if a.cfg.WebModel != "" {
		model = a.cfg.WebModel
	} else if a.ag.ProviderName == "anthropic" {
		model = "claude-haiku-4-5"
	}
	return ws.WebSearch(ctx, model, query)
}
