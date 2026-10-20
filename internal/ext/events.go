package ext

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// event sends a request event to each subscribed extension in turn; each
// sees what the ones before it changed (apply). It stops when apply says.
func (m *Manager) event(ctx context.Context, name string, data map[string]any, apply func(ext string, res json.RawMessage) (stop bool)) {
	for _, e := range m.running() {
		if !e.subscribed(name) {
			continue
		}
		c, err := e.conn()
		if err != nil {
			continue
		}
		hctx, cancel := context.WithTimeout(ctx, hookTimeout)
		var res json.RawMessage
		err = c.call(hctx, "event", map[string]any{"event": name, "data": data}, &res)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if m.o.Host != nil {
				m.o.Host.Notify(e.name(), fmt.Sprintf("%s handler failed: %v", name, err), "error")
			}
			continue
		}
		if len(res) > 0 && apply(e.name(), res) {
			return
		}
	}
}

// Emit sends a notification event to the subscribed extensions.
func (m *Manager) Emit(name string, data map[string]any) {
	for _, e := range m.running() {
		if !e.subscribed(name) {
			continue
		}
		if c, err := e.conn(); err == nil {
			_ = c.notifyExt("event", map[string]any{"event": name, "data": data})
		}
	}
}

// Input lets extensions change a prompt before it is sent, or take it
// (handled: nothing is sent).
func (m *Manager) Input(ctx context.Context, text string) (string, bool) {
	handled := false
	m.event(ctx, EventInput, map[string]any{"text": text}, func(_ string, raw json.RawMessage) bool {
		var r struct {
			Text    *string `json:"text"`
			Handled bool    `json:"handled"`
		}
		if json.Unmarshal(raw, &r) != nil {
			return false
		}
		if r.Text != nil {
			text = *r.Text
		}
		handled = r.Handled
		return handled
	})
	return text, handled
}

// BeforeTool lets extensions block a tool call (a reason) or change its
// input (nil: unchanged). agent is "" for the session's agent, else the
// subagent's name.
func (m *Manager) BeforeTool(ctx context.Context, agent, tool string, input json.RawMessage) (json.RawMessage, string) {
	if m == nil {
		return nil, ""
	}
	var changed json.RawMessage
	reason := ""
	m.event(ctx, EventToolCall, map[string]any{"tool": tool, "input": rawOrEmpty(input), "agent": agent}, func(ext string, raw json.RawMessage) bool {
		var r struct {
			Block  bool            `json:"block"`
			Reason string          `json:"reason"`
			Input  json.RawMessage `json:"input"`
		}
		if json.Unmarshal(raw, &r) != nil {
			return false
		}
		if r.Block {
			reason = strings.TrimSpace(r.Reason)
			if reason == "" {
				reason = "no reason given"
			}
			reason = ext + ": " + reason
			return true
		}
		if len(r.Input) > 0 && string(r.Input) != "null" && json.Valid(r.Input) {
			changed, input = r.Input, r.Input
		}
		return false
	})
	return changed, reason
}

// AfterTool lets extensions change a tool's output.
func (m *Manager) AfterTool(ctx context.Context, agent, tool string, input json.RawMessage, output string, isErr bool) (string, bool) {
	if m == nil {
		return output, false
	}
	changed := false
	m.event(ctx, EventToolResult, map[string]any{"tool": tool, "input": rawOrEmpty(input), "output": output, "is_error": isErr, "agent": agent},
		func(_ string, raw json.RawMessage) bool {
			var r struct {
				Output *string `json:"output"`
			}
			if json.Unmarshal(raw, &r) == nil && r.Output != nil {
				output, changed = *r.Output, true
			}
			return false
		})
	return output, changed
}

func rawOrEmpty(r json.RawMessage) json.RawMessage {
	if len(r) == 0 {
		return json.RawMessage("{}")
	}
	return r
}

// Commands are the slash commands of the running extensions.
func (m *Manager) Commands() []Command {
	var out []Command
	for _, e := range m.running() {
		e.mu.Lock()
		out = append(out, e.init.Commands...)
		e.mu.Unlock()
	}
	return out
}

// HasCommand says whether an extension has /name.
func (m *Manager) HasCommand(name string) bool {
	for _, c := range m.Commands() {
		if c.Name == name {
			return true
		}
	}
	return false
}

// RunCommand runs /name args in its extension.
func (m *Manager) RunCommand(ctx context.Context, name, args string) error {
	for _, e := range m.running() {
		e.mu.Lock()
		has := false
		for _, c := range e.init.Commands {
			has = has || c.Name == name
		}
		e.mu.Unlock()
		if !has {
			continue
		}
		c, err := e.conn()
		if err != nil {
			return err
		}
		return c.call(ctx, "command/execute", map[string]any{"name": name, "args": args}, nil)
	}
	return fmt.Errorf("no extension has /%s", name)
}

// Tools are the tools of the running extensions, except those named like
// one in taken (a builtin's).
func (m *Manager) Tools(taken func(string) bool) []tools.Tool {
	var out []tools.Tool
	seen := map[string]bool{}
	for _, e := range m.running() {
		e.mu.Lock()
		defs := e.init.Tools
		e.mu.Unlock()
		for _, d := range defs {
			if d.Name == "" || seen[d.Name] || taken != nil && taken(d.Name) {
				continue
			}
			seen[d.Name] = true
			out = append(out, &extTool{e: e, def: d})
		}
	}
	return out
}

type extTool struct {
	e   *extension
	def ToolDef
}

func (t *extTool) Def() llm.Tool {
	schema := t.def.Parameters
	if len(schema) == 0 || string(schema) == "null" {
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return llm.Tool{Name: t.def.Name, Description: t.def.Description, Schema: schema}
}

func (t *extTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	c, err := t.e.conn()
	if err != nil {
		return "", err
	}
	var r struct {
		Content string `json:"content"`
		IsError bool   `json:"is_error"`
	}
	if err := c.call(ctx, "tool/execute", map[string]any{"name": t.def.Name, "input": rawOrEmpty(input)}, &r); err != nil {
		return "", fmt.Errorf("extension %s: %w", t.e.name(), err)
	}
	if r.IsError {
		return "", fmt.Errorf("%s", r.Content)
	}
	return r.Content, nil
}
