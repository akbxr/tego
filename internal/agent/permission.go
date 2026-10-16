package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// Permission modes: which tool calls wait for the user's approval.
//   - yolo: none (the default, and how tego always ran).
//   - edits: shell commands and MCP tools that may change things; file
//     edits run.
//   - ask: those and file edits too.
//
// What only reads never asks: read, grep, glob, web search, subagents (their
// own calls ask), MCP tools that declare readOnlyHint, and shell commands on
// plan mode's read-only list. Plan mode refuses before a mode is asked.
const (
	PermYolo  = "yolo"
	PermEdits = "edits"
	PermAsk   = "ask"
)

// PermissionModes in the order /permissions offers them.
var PermissionModes = []string{PermYolo, PermEdits, PermAsk}

// ValidPermission reports whether mode is a permission mode.
func ValidPermission(mode string) bool { return slices.Contains(PermissionModes, mode) }

// fileTools are the tools that change files and nothing else.
var fileTools = map[string]bool{"write": true, "edit": true}

// Permissions is the mode and the session's "don't ask again" rules, shared
// by an agent and its subagents.
type Permissions struct {
	mu       sync.Mutex
	mode     string
	tools    map[string]bool // tools allowed for the session
	commands map[string]bool // shell command prefixes allowed ("go test")
}

// NewPermissions starts in mode (yolo if it is not one).
func NewPermissions(mode string) *Permissions {
	if !ValidPermission(mode) {
		mode = PermYolo
	}
	return &Permissions{mode: mode, tools: map[string]bool{}, commands: map[string]bool{}}
}

func (p *Permissions) Mode() string {
	if p == nil {
		return PermYolo
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mode
}

// SetMode switches the mode; the session's rules stay.
func (p *Permissions) SetMode(mode string) {
	if p == nil || !ValidPermission(mode) {
		return
	}
	p.mu.Lock()
	p.mode = mode
	p.mu.Unlock()
}

// Approval is a call waiting for the user.
type Approval struct {
	Agent   string // the subagent asking, "" for the main agent
	Tool    string
	Input   json.RawMessage
	Summary string // one line: the command, or the file and what changes
	// Rule is what "don't ask again" would allow for the rest of the session
	// ("go test commands", "file edits"); "" when no rule may cover the call.
	Rule string
	rule func(*Permissions)
}

// Decision is the user's answer.
type Decision struct {
	Allow    bool
	Always   bool   // and the Rule from now on
	Feedback string // for the model, with a refusal: what to do instead
}

// Approver asks the user about a call. A nil Approver (print mode, no
// terminal) can't ask, so the calls that would wait are refused.
type Approver func(ctx context.Context, a Approval) (Decision, error)

// approval returns the request for a call the mode makes wait, or nil if
// it runs without asking.
func (a *Agent) approval(t tools.Tool, call llm.Block) *Approval {
	p := a.Perms
	mode := p.Mode()
	if mode == PermYolo || planSafe[call.Name] {
		return nil
	}
	if ro, ok := t.(tools.ReadOnly); ok && ro.ReadOnly() {
		return nil
	}
	if h, ok := t.(readOnlyHinter); ok && h.ReadOnlyHint() {
		return nil
	}
	req := &Approval{Agent: a.Name, Tool: call.Name, Input: call.Input, Summary: tools.Summary(call.Name, call.Input)}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case fileTools[call.Name]:
		if mode == PermEdits || p.tools["edit"] {
			return nil
		}
		req.Rule = "file edits"
		req.rule = func(p *Permissions) { p.tools["edit"] = true }
	case call.Name == "bash" || call.Name == "bash_output":
		if tools.PlanBlocked(call.Name, call.Input) == "" {
			return nil // only reads
		}
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(call.Input, &in)
		prefixes, ok := tools.CommandPrefixes(in.Command)
		if ok && allCovered(prefixes, p.commands) {
			return nil
		}
		if ok {
			prefixes = slices.Compact(slices.Sorted(slices.Values(prefixes)))
			req.Rule = "`" + strings.Join(prefixes, "`, `") + "` commands"
			req.rule = func(p *Permissions) {
				for _, c := range prefixes {
					p.commands[c] = true
				}
			}
		}
	default:
		if p.tools[call.Name] {
			return nil
		}
		name := call.Name
		req.Rule = "`" + name + "` calls"
		req.rule = func(p *Permissions) { p.tools[name] = true }
	}
	return req
}

func allCovered(prefixes []string, allowed map[string]bool) bool {
	for _, c := range prefixes {
		if !allowed[c] {
			return false
		}
	}
	return true
}

// Declined starts the result of a call the user declined.
const Declined = "The user declined"

// approvalMu makes the approvals of an agent and its subagents one at a time.
var approvalMu sync.Mutex

// permissionRefusal asks the user about a call the mode makes wait. It
// returns the result for the model when the call may not run ("" if it may).
func (a *Agent) permissionRefusal(ctx context.Context, t tools.Tool, call llm.Block) string {
	req := a.approval(t, call)
	if req == nil {
		return ""
	}
	if a.Approve == nil {
		return fmt.Sprintf("This %s call did not run: the permission mode is %q and nobody can approve it here (no terminal). "+
			"Do the task without it, or tell the user it needs %q (or `--permissions yolo`).", call.Name, a.Perms.Mode(), req.Summary)
	}
	approvalMu.Lock()
	defer approvalMu.Unlock()
	// Approved for the rest of the session while waiting for the lock?
	if req = a.approval(t, call); req == nil {
		return ""
	}
	d, err := a.Approve(ctx, *req)
	switch {
	case ctx.Err() != nil:
		return "aborted by user"
	case err != nil:
		return fmt.Sprintf("This %s call did not run: asking the user failed (%v).", call.Name, err)
	case !d.Allow:
		msg := fmt.Sprintf("%s this %s call, so it did not run.", Declined, call.Name)
		if f := strings.TrimSpace(d.Feedback); f != "" {
			return msg + " They said: " + f
		}
		return msg + " Don't try the same thing another way; ask the user how to proceed if it isn't clear."
	}
	if d.Always && req.rule != nil {
		a.Perms.mu.Lock()
		req.rule(a.Perms)
		a.Perms.mu.Unlock()
	}
	return ""
}
