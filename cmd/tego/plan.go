package main

import (
	"github.com/akbxr/tego/internal/lineedit"
)

// implementPrompt is sent when the user approves a plan.
const implementPrompt = "Implement the plan."

// setPlan turns plan mode on or off (/plan, Shift+Tab). It takes effect at
// the next tool call, even mid-turn; the model is reminded at its next prompt.
func (a *app) setPlan(on bool) {
	if a.plan.Load() == on {
		return
	}
	a.plan.Store(on)
	if on {
		a.ui.Dim("plan mode on: the agent investigates and proposes, and changes nothing (Shift+Tab or /plan to leave)")
	} else {
		a.ui.Dim("plan mode off: the agent may change files again")
	}
	a.refreshStatus()
	a.tui.Redraw()
}

// offerPlan asks what to do with the plan a plan-mode turn just presented.
func (a *app) offerPlan(done chan<- error) {
	idx, err := a.tui.Pick("Plan ready: what next?", []lineedit.PickItem{
		{Label: "Implement it", Detail: "leave plan mode and carry out the plan"},
		{Label: "Keep planning", Detail: "type your feedback; plan mode stays on"},
		{Label: "Leave plan mode", Detail: "without implementing"},
	}, "", 0)
	switch {
	case err != nil || idx == 1:
		return
	case idx == 0:
		a.setPlan(false)
		a.startPrompt(implementPrompt, done)
	case idx == 2:
		a.setPlan(false)
	}
}
