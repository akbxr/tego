package agent

import (
	"fmt"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// Plan mode (/plan, Shift+Tab): the agent investigates and writes a plan,
// and nothing changes until the user approves it. Tools that only read run
// as usual; edits, writes, shell commands that are not on the read-only
// list and MCP tools that don't declare readOnlyHint are refused with a
// note, and subagents inherit the mode. Each prompt in plan mode carries a
// reminder, and the first prompt after it carries one that it is over.

const (
	planOnNote = "<system-reminder>Plan mode is on. Investigate (read, grep, glob, subagents, read-only commands such as " +
		"git log/diff, ls, find, go test) and design the change, but change nothing: edits, writes and other commands are refused. " +
		"Finish with a concise plan: the goal, the files and functions to change and how, the order, risks, and how to verify it. " +
		"Ask the user only about real decisions.</system-reminder>"
	planOffNote = "<system-reminder>Plan mode is off: you may change files and run commands again.</system-reminder>"
)

// planSafe tools run in plan mode whatever their input: they only read,
// keep the agent's own state, or run subagents that are in plan mode too.
var planSafe = map[string]bool{"read": true, "grep": true, "glob": true, "multi_grep": true, "todo": true,
	"task": true, "tool_search": true, "checkpoint": true, "rewind": true}

type readOnlyHinter interface{ ReadOnlyHint() bool }

func (a *Agent) planning() bool { return a.Plan != nil && a.Plan.Load() }

// planRefusal is the result for a call plan mode does not allow ("" if it may run).
func (a *Agent) planRefusal(t tools.Tool, call llm.Block) string {
	if !a.planning() || planSafe[call.Name] {
		return ""
	}
	if ro, ok := t.(tools.ReadOnly); ok && ro.ReadOnly() {
		return ""
	}
	if h, ok := t.(readOnlyHinter); ok && h.ReadOnlyHint() {
		return ""
	}
	why := tools.PlanBlocked(call.Name, call.Input)
	if why == "" {
		return ""
	}
	return fmt.Sprintf("Plan mode is on, so this %s call did not run: %s. While planning, investigate with read, grep, glob, "+
		"subagents and read-only commands (git log/diff/status, ls, find, cat, go test …), then present the plan; "+
		"the user turns plan mode off to carry it out.", call.Name, why)
}

// notePlan adds the plan mode reminder to a new prompt.
func (a *Agent) notePlan() {
	switch {
	case a.planning():
		a.append(llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: planOnNote}}})
		a.planSaid = true
	case a.planSaid:
		a.append(llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: planOffNote}}})
		a.planSaid = false
	}
}
