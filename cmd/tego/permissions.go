package main

import (
	"context"
	"errors"
	"time"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/lineedit"
	"github.com/akbxr/tego/internal/tools"
)

// permissionHelp describes each mode for /permissions and --help.
var permissionHelp = map[string]string{
	agent.PermYolo:  "run everything without asking",
	agent.PermEdits: "file edits run; ask before commands and MCP tools that may change things",
	agent.PermAsk:   "ask before file edits, commands and MCP tools that may change things",
}

// approve asks the user about a call the permission mode makes wait
// (agent.Approver): the call's box with what it would do, then a picker.
// Esc declines. Calls of the agent and its subagents come one at a time.
func (a *app) approve(ctx context.Context, req agent.Approval) (agent.Decision, error) {
	a.ui.showApproval(req, tools.Preview(a.env, req.Tool, req.Input))
	defer a.ui.hideApproval()
	a.notify("Waiting for approval")
	items := []lineedit.PickItem{{Label: "Yes", Detail: "run it"}}
	always := -1
	if req.Rule != "" {
		always = len(items)
		items = append(items, lineedit.PickItem{Label: "Yes, and don't ask again", Detail: "for " + req.Rule + " in this session"})
	}
	no := len(items)
	items = append(items, lineedit.PickItem{Label: "No", Detail: "and tell the agent what to do instead"})
	title := "Allow this " + req.Tool + " call?"
	if req.Agent != "" {
		title = "Subagent " + req.Agent + ": allow this " + req.Tool + " call?"
	}
	idx, err := a.pickWhenFree(ctx, func() (int, error) { return a.tui.Pick(title, items, "", 0) })
	switch {
	case errors.Is(err, lineedit.ErrCanceled):
		return agent.Decision{}, nil // the call's box says it was declined
	case err != nil:
		return agent.Decision{}, err
	case idx == no:
		var fb string
		_, _ = a.pickWhenFree(ctx, func() (int, error) {
			var err error
			fb, err = a.tui.Ask("What should the agent do instead? (Enter to just decline)", "")
			return 0, err
		})
		return agent.Decision{Feedback: fb}, nil
	}
	if idx == always {
		a.ui.Dim("allowed %s for this session", req.Rule)
	}
	return agent.Decision{Allow: true, Always: idx == always}, nil
}

// pickWhenFree runs pick, waiting while another picker is open (the user
// may be in /model, or another subagent's approval is being answered).
func (a *app) pickWhenFree(ctx context.Context, pick func() (int, error)) (int, error) {
	for {
		idx, err := pick()
		if !errors.Is(err, lineedit.ErrBusy) {
			return idx, err
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// setPermissions switches the permission mode and saves it as the default.
func (a *app) setPermissions(mode string) {
	if !agent.ValidPermission(mode) {
		a.ui.Errorf("unknown permission mode %q (yolo, edits or ask)", mode)
		return
	}
	a.ag.Perms.SetMode(mode)
	a.cfg.Permissions = mode
	if err := a.cfg.Save(); err != nil {
		a.ui.Errorf("saving config: %v", err)
	}
	a.ui.Dim("permissions: %s (%s)", mode, permissionHelp[mode])
	a.refreshStatus()
}

// pickPermissions is /permissions without an argument.
func (a *app) pickPermissions() {
	cur := a.ag.Perms.Mode()
	items := make([]lineedit.PickItem, len(agent.PermissionModes))
	sel := 0
	for i, m := range agent.PermissionModes {
		items[i] = lineedit.PickItem{Label: m, Detail: permissionHelp[m]}
		if m == cur {
			sel = i
			items[i].Detail += " (current)"
		}
	}
	idx, err := a.tui.Pick("Permission mode", items, "", sel)
	if err != nil {
		return
	}
	a.setPermissions(agent.PermissionModes[idx])
}
