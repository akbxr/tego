package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/mcp"
)

// startMCP connects configured MCP servers in the background; their tools
// join the agent's tool list as they come up.
func (a *app) startMCP() {
	cfgs, err := mcp.LoadConfig(a.cfg.Dir, a.cwd)
	if err != nil {
		a.ui.Errorf("mcp config: %v", err)
	}
	a.mcp = mcp.Start(cfgs, a.cwd, filepath.Join(a.cfg.Dir, "logs"))
	a.ag.Dynamic = a.mcp.Tools
	switch a.cfg.MCPTools {
	case "eager":
		a.ag.LazyThreshold = -1
	case "lazy":
		a.ag.LazyThreshold = 0
	default: // auto: a couple of small servers stay inline, big ones are deferred
		a.ag.LazyThreshold = 10
	}
}

// waitMCP gives servers still connecting a bounded head start before the
// first request, so their tools are part of it. Later turns never wait.
func (a *app) waitMCP() {
	if a.mcpWaited {
		return
	}
	a.mcpWaited = true
	connecting := 0
	for _, s := range a.mcp.Status() {
		if s.State == "connecting" {
			connecting++
		}
	}
	if connecting == 0 {
		return
	}
	limit := 10 * time.Second
	if !a.interactive {
		limit = 30 * time.Second
	}
	a.ui.Dim("waiting for %d MCP server(s)…", connecting)
	a.mcp.Wait(limit)
	for _, s := range a.mcp.Status() {
		switch s.State {
		case "failed":
			a.ui.Errorf("mcp %s: %s (see /mcp)", s.Name, s.Err)
		case "connecting":
			a.ui.Dim("mcp %s still connecting; its tools will appear when ready", s.Name)
		}
	}
}

func (a *app) mcpCommand() {
	st := a.mcp.Status()
	if len(st) == 0 {
		fmt.Fprintln(a.out, "no MCP servers configured (mcpServers in ~/.tego/mcp.json, ./.mcp.json or ./.tego/mcp.json)")
		return
	}
	c := a.ui.c
	for _, s := range st {
		color := c.dim
		switch s.State {
		case "connected":
			color = c.green
		case "failed":
			color = c.red
		case "connecting":
			color = c.yellow
		}
		line := fmt.Sprintf("  %s●%s %-20s %s%-10s%s %-6s", color, c.reset, s.Name, color, s.State, c.reset, s.Transport)
		if s.State == "connected" {
			line += fmt.Sprintf(" %d tools", s.Tools)
		}
		if s.Err != "" {
			line += "  " + c.dim + strings.ReplaceAll(s.Err, "\n", " ") + c.reset
		}
		fmt.Fprintln(a.out, line)
	}
	a.ui.Dim("server logs: %s", filepath.Join(a.cfg.Dir, "logs"))
}
