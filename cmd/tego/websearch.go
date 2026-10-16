package main

import (
	"context"
	"errors"

	"github.com/akbxr/tego/internal/llm"
)

// nativeSearch runs web_search through the current provider's built-in
// search (Anthropic server tool, Codex web_search). Anthropic runs it on the
// small model (Haiku 5.5 searches as well as Haiku 4.5 did, at a tenth of
// the price). Providers without native search make web_search fall through
// to Exa/Brave/Tavily/DuckDuckGo.
func (a *app) nativeSearch(ctx context.Context, query string) (*llm.WebResult, error) {
	ws, ok := a.ag.Provider.(llm.WebSearcher)
	if !ok {
		return nil, errors.New("provider has no native web search")
	}
	model := a.ag.Model
	if a.cfg.WebModel != "" {
		model = a.cfg.WebModel
	} else if s, ok := a.small(); ok && s.ProviderName == a.ag.ProviderName && a.cfg.Kind(a.ag.ProviderName) == "anthropic" {
		model = s.Model
	}
	return ws.WebSearch(ctx, model, query)
}
