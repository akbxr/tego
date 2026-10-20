package main

import (
	"fmt"
	"strings"

	"github.com/akbxr/tego/internal/agent"
)

// Agent definitions (agents/*.md, Claude Code's format) give a subagent its
// own instructions, tools and model; the session's model picks one per task.

func (a *app) agents() []agent.AgentDef { return a.agentDefs }

// claudeAliases are the model aliases of Claude Code's agent files.
var claudeAliases = map[string]string{
	"opus":   "claude-opus-5-5",
	"sonnet": "claude-sonnet-5-5",
	"haiku":  "claude-haiku-5-5",
	"fable":  "claude-fable-5-1",
}

// resolveModel is the model an agent definition names: an alias of
// Claude's (the session's model when it is of that family already, as in
// Claude Code), or provider/model. Zero Provider: the session's model.
func (a *app) resolveModel(spec string) (agent.SmallModel, error) {
	if id := claudeAliases[strings.ToLower(spec)]; id != "" {
		if strings.HasPrefix(a.ag.Model, "claude-"+strings.ToLower(spec)) {
			return agent.SmallModel{}, nil
		}
		prov := "anthropic"
		if a.cfg.Kind(a.ag.ProviderName) == "anthropic" { // a gateway for Anthropic's API
			prov = a.ag.ProviderName
		}
		spec = prov + "/" + id
	}
	prov, model, err := a.cfg.ParseModel(spec, a.store)
	if err != nil {
		return agent.SmallModel{}, err
	}
	if prov == a.ag.ProviderName && model == a.ag.Model {
		return agent.SmallModel{}, nil
	}
	if prov != a.ag.ProviderName && !a.cfg.Available(prov, a.store) {
		return agent.SmallModel{}, fmt.Errorf("not logged in to %s (tego login %s)", prov, prov)
	}
	p, err := a.providerFor(prov)
	if err != nil {
		return agent.SmallModel{}, err
	}
	return agent.SmallModel{Provider: p, ProviderName: prov, Model: model, Window: a.cfg.ContextWindowFor(prov, model)}, nil
}

// listAgentDefs shows the defined agents (/agents before any task ran).
func (a *app) listAgentDefs() {
	if len(a.agentDefs) == 0 {
		a.ui.Dim("(no subagents yet; define agents in .tego/agents/*.md or .claude/agents/*.md)")
		return
	}
	c := a.ui.c
	var b strings.Builder
	fmt.Fprintf(&b, "%sAgents the model can run tasks as:%s\n", c.dim, c.reset)
	for _, d := range a.agentDefs {
		model := d.Model
		if model == "" {
			model = "inherit"
		}
		desc := d.Description
		if r := []rune(desc); len(r) > 100 {
			desc = string(r[:100]) + "…"
		}
		fmt.Fprintf(&b, "  %s%-20s%s %s%s · %s%s\n    %s\n", c.bold, d.Name, c.reset, c.dim, model, shortHome(d.Path), c.reset, desc)
	}
	a.ui.Finish()
	fmt.Fprint(a.out, b.String())
}
