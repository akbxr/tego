package main

import (
	"sync"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/llm"
)

// The small model does side work for the session's model: session titles,
// native web search, and subagents the model gives "model": "small" (file
// hunts, summaries, checks). Compaction stays on the session's model: it
// reads the whole history, which that model has cached.

// smallProviders caches providers built for a small model on another
// provider than the session's (small_model "openrouter/…").
var smallProviders struct {
	sync.Mutex
	m map[string]llm.Provider
}

// small is the session's cheaper model (config small_model, default per
// provider); ok is false when there is none or it is the session's model.
// Called from the agent's goroutine too (task), so it only reads.
func (a *app) small() (agent.SmallModel, bool) {
	main := a.ag.Provider
	if main == nil {
		return agent.SmallModel{}, false
	}
	spec := a.cfg.SmallModelSpec(a.ag.ProviderName)
	if spec == "" {
		return agent.SmallModel{}, false
	}
	prov, model, err := a.cfg.ParseModel(spec, a.store)
	if err != nil || prov == a.ag.ProviderName && model == a.ag.Model {
		return agent.SmallModel{}, false
	}
	p := main
	if prov != a.ag.ProviderName {
		smallProviders.Lock()
		defer smallProviders.Unlock()
		if p = smallProviders.m[prov]; p == nil {
			if p, err = a.cfg.NewProvider(prov, a.store); err != nil {
				return agent.SmallModel{}, false
			}
			if smallProviders.m == nil {
				smallProviders.m = map[string]llm.Provider{}
			}
			smallProviders.m[prov] = p
		}
	}
	return agent.SmallModel{Provider: p, ProviderName: prov, Model: model, Window: a.cfg.ContextWindowFor(prov, model)}, true
}
