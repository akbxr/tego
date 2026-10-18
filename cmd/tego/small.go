package main

import (
	"sync"
	"time"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/config"
	"github.com/akbxr/tego/internal/llm"
)

// The small model does side work for the session's model: session titles,
// native web search, and subagents the model gives "model": "small" (file
// hunts, summaries, checks). Compaction stays on the session's model: it
// reads the whole history, which that model has cached.

// otherProviders caches providers built for side models on another
// provider than the session's (small_model "openrouter/…", subagent_models).
var otherProviders struct {
	sync.Mutex
	m map[string]llm.Provider
}

// providerFor is the session's provider for prov, else one built for it
// once and kept.
func (a *app) providerFor(prov string) (llm.Provider, error) {
	if prov == a.ag.ProviderName && a.ag.Provider != nil {
		return a.ag.Provider, nil
	}
	otherProviders.Lock()
	defer otherProviders.Unlock()
	if p := otherProviders.m[prov]; p != nil {
		return p, nil
	}
	p, err := a.cfg.NewProvider(prov, a.store)
	if err != nil {
		return nil, err
	}
	if otherProviders.m == nil {
		otherProviders.m = map[string]llm.Provider{}
	}
	otherProviders.m[prov] = p
	return p, nil
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
	p, err := a.providerFor(prov)
	if err != nil {
		return agent.SmallModel{}, false
	}
	return agent.SmallModel{Provider: p, ProviderName: prov, Model: model, Window: a.cfg.ContextWindowFor(prov, model)}, true
}

// subModelCache keeps the list of subagentModels for a while: the task
// tool's schema asks for it several times a request, and checking the
// logins reads auth.json.
type subModelCache struct {
	sync.Mutex
	key  string // the session's model it was made for
	at   time.Time
	list []agent.SubagentModel
}

const subModelTTL = 30 * time.Second

// subagentModels are the models of subagent_models a task can ask for by
// name: those whose provider is logged in (or needs no login), other than
// the session's model. Their providers are built when a task asks.
func (a *app) subagentModels() []agent.SubagentModel {
	if a.ag.Provider == nil || len(a.cfg.SubModels) == 0 {
		return nil
	}
	c := &a.subModels
	c.Lock()
	defer c.Unlock()
	key := a.ag.ProviderName + "/" + a.ag.Model
	if c.key != key || time.Since(c.at) > subModelTTL {
		c.key, c.at, c.list = key, time.Now(), a.listSubagentModels()
	}
	return c.list
}

func (a *app) listSubagentModels() []agent.SubagentModel {
	var out []agent.SubagentModel
	for _, m := range a.cfg.SubagentModelList() {
		prov, model, err := a.cfg.ParseModel(config.SubagentModelSpec(m.SubagentModel, a.ag.ProviderName), a.store)
		if err != nil || prov == a.ag.ProviderName && model == a.ag.Model ||
			prov != a.ag.ProviderName && !a.cfg.Available(prov, a.store) { // the session's provider is there
			continue
		}
		out = append(out, agent.SubagentModel{
			Name: m.Name, Spec: prov + "/" + model, Use: m.Use, Thinking: m.Thinking,
			Resolve: func() (agent.SmallModel, error) {
				p, err := a.providerFor(prov)
				if err != nil {
					return agent.SmallModel{}, err
				}
				return agent.SmallModel{Provider: p, ProviderName: prov, Model: model, Window: a.cfg.ContextWindowFor(prov, model)}, nil
			},
		})
	}
	return out
}
