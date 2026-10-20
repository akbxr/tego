package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/ext"
	"github.com/akbxr/tego/internal/lineedit"
	"github.com/akbxr/tego/internal/tools"
)

// Extensions (internal/ext) run beside the session: their hooks see the
// agent's tool calls, their tools join MCP's, their commands the REPL's.

// startExtensions runs the extensions found for the session, in the
// background; their tools join after MCP's. It returns a note for the
// user when some of the project's are not trusted yet.
func (a *app) startExtensions() string {
	cands := ext.Discover(a.cfg.Dir, a.cwd)
	if len(cands) == 0 {
		return ""
	}
	a.extMsgs = make(chan string, 16)
	a.ext = ext.Start(cands, ext.Options{Home: a.cfg.Dir, Cwd: a.cwd, SessionID: a.ag.SessionID, Version: version, Host: extHost{a}})
	a.ag.Hooks = a.ext
	mcpTools := a.ag.Dynamic
	a.ag.Dynamic = func() []tools.Tool {
		var ts []tools.Tool
		if mcpTools != nil {
			ts = mcpTools()
		}
		taken := map[string]bool{}
		for _, t := range a.ag.Tools {
			taken[t.Def().Name] = true
		}
		for _, t := range ts {
			taken[t.Def().Name] = true
		}
		return append(ts[:len(ts):len(ts)], a.ext.Tools(func(n string) bool { return taken[n] })...)
	}
	var untrusted []string
	for _, s := range a.ext.Status() {
		if s.State == ext.StateUntrusted {
			untrusted = append(untrusted, s.Name)
		}
	}
	if len(untrusted) == 0 {
		return ""
	}
	if !a.interactive {
		return fmt.Sprintf("this project has %s not trusted yet (%s), which don't run: trust them in an interactive tego with /extensions trust",
			plural(len(untrusted), "extension"), strings.Join(untrusted, ", "))
	}
	return fmt.Sprintf("this project has %s not trusted yet (%s): /extensions shows them, /extensions trust runs them",
		plural(len(untrusted), "extension"), strings.Join(untrusted, ", "))
}

// waitExtensions lets the extensions finish starting before a turn uses
// their hooks and tools.
func (a *app) waitExtensions() { a.ext.Wait(10 * time.Second) }

// extensionsCommand is /extensions [trust].
func (a *app) extensionsCommand(args string) {
	if args == "trust" {
		names := a.ext.Trust()
		if len(names) == 0 {
			a.ui.Dim("no untrusted extensions")
			return
		}
		a.ext.Wait(10 * time.Second)
		a.ui.Dim("trusted and started %s (until their files change)", strings.Join(names, ", "))
	}
	st := a.ext.Status()
	if len(st) == 0 {
		a.ui.Dim("no extensions: put them in ~/.tego/extensions/ or the project's .tego/extensions/ (.ts, .js, .py or an executable)")
		return
	}
	c := a.ui.c
	var b strings.Builder
	for _, s := range st {
		color := c.dim
		switch s.State {
		case ext.StateRunning:
			color = c.green
		case ext.StateFailed:
			color = c.red
		case ext.StateUntrusted:
			color = c.yellow
		}
		where := "user"
		if s.Project {
			where = "project"
		}
		fmt.Fprintf(&b, "  %s●%s %-20s %s%-9s%s %-7s %s\n", color, c.reset, s.Name, color, s.State, c.reset, where, c.dim+shortHome(s.Path)+c.reset)
		var parts []string
		if len(s.Tools) > 0 {
			parts = append(parts, "tools: "+strings.Join(s.Tools, ", "))
		}
		if len(s.Commands) > 0 {
			parts = append(parts, "commands: /"+strings.Join(s.Commands, ", /"))
		}
		if len(s.Events) > 0 {
			parts = append(parts, "on: "+strings.Join(s.Events, ", "))
		}
		if s.Err != "" {
			parts = append(parts, s.Err)
		}
		if len(parts) > 0 {
			fmt.Fprintf(&b, "    %s%s%s\n", c.dim, strings.Join(parts, " · "), c.reset)
		}
	}
	a.ui.Finish()
	fmt.Fprint(a.out, b.String())
}

// extHost answers what extensions ask of tego.
type extHost struct{ a *app }

func (h extHost) Notify(name, msg, level string) {
	a := h.a
	line := fmt.Sprintf("[%s] %s", name, msg)
	switch level {
	case "error":
		a.ui.Errorf("%s", line)
	case "warning", "warn":
		a.ui.OnInfo(line)
	default:
		a.ui.Dim("%s", line)
	}
	if a.tui != nil {
		a.tui.Redraw()
	}
}

func (h extHost) Confirm(ctx context.Context, name, title, msg string) (bool, error) {
	a := h.a
	if a.tui == nil {
		return false, nil // -p: nobody can say yes
	}
	items := []lineedit.PickItem{{Label: "Yes", Detail: msg}, {Label: "No"}}
	idx, err := a.pickWhenFree(ctx, func() (int, error) { return a.tui.Pick("["+name+"] "+title, items, "", 0) })
	return idx == 0 && err == nil, nil
}

func (h extHost) Select(ctx context.Context, name, title string, options []string) (int, error) {
	a := h.a
	if a.tui == nil {
		return -1, nil
	}
	items := make([]lineedit.PickItem, len(options))
	for i, o := range options {
		items[i] = lineedit.PickItem{Label: o}
	}
	idx, err := a.pickWhenFree(ctx, func() (int, error) { return a.tui.Pick("["+name+"] "+title, items, "", 0) })
	if err != nil {
		return -1, nil
	}
	return idx, nil
}

func (h extHost) Input(ctx context.Context, name, title, placeholder string) (string, bool, error) {
	a := h.a
	if a.tui == nil {
		return "", false, nil
	}
	if placeholder != "" {
		title += " (" + placeholder + ")"
	}
	for {
		s, err := a.tui.Ask("["+name+"] "+title, "")
		if !errors.Is(err, lineedit.ErrBusy) {
			return s, err == nil, nil
		}
		select {
		case <-ctx.Done():
			return "", false, nil
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func (h extHost) SendMessage(name, text string) {
	a := h.a
	if strings.TrimSpace(text) == "" {
		return
	}
	if a.extMsgs == nil || a.tui == nil {
		a.ui.Dim("[%s] wanted to send a message, which -p can't take: %s", name, text)
		return
	}
	select {
	case a.extMsgs <- text:
	default:
		a.ui.Errorf("[%s] too many messages queued; dropped: %s", name, text)
	}
}
