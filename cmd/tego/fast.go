package main

import (
	"github.com/akbxr/tego/internal/llm"
)

// setFast turns fast mode on or off (/fast) and saves the choice. Fast mode
// changes what the requests cost, so it says so: twice the price per token,
// usage credits on a subscription, and a prompt cache rebuild when switched
// mid-session (the speed is part of the cache key).
func (a *app) setFast(on bool) {
	a.ag.Fast = on
	a.cfg.Fast = on
	if err := a.cfg.Save(); err != nil {
		a.ui.Errorf("saving fast mode: %v", err)
	}
	defer a.refreshStatus()
	supported := a.ag.Provider != nil && llm.SupportsFast(a.ag.Provider, a.ag.Model)
	rebuild := ""
	if supported && a.ag.LastCtx > 10_000 {
		rebuild = "; the next request rebuilds the prompt cache (~" + fmtTokens(a.ag.LastCtx) + " tokens uncached)"
	}
	if !on {
		if !supported {
			rebuild = ""
		}
		a.ui.Dim("fast mode off%s", rebuild)
		return
	}
	if !supported {
		a.ui.Dim("fast mode on, but %s doesn't have it: it runs on Claude Opus 5.5, Opus 5 and Opus 4.8 through Anthropic, and applies once you switch to one", a.ag.Model)
		return
	}
	msg := "fast mode on: up to 2.5x faster output at twice the price per token"
	if llm.FastBillsCredits(a.ag.Provider) {
		msg += "; on a Claude subscription it is billed from usage credits, even with plan usage left"
	}
	a.ui.Dim("%s%s", msg, rebuild)
}
