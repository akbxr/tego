package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/config"
	"github.com/akbxr/tego/internal/mcp"
)

// mcpOptions are the MCP manager's options from the config: local servers
// run only while used unless mcp_idle is "off".
func mcpOptions(cfg *config.Config) mcp.Options {
	return mcp.Options{Home: cfg.Dir, IdleAfter: cfg.MCPIdleAfter(), CacheDir: filepath.Join(cfg.Dir, "mcp-cache")}
}

// startMCP connects configured MCP servers in the background; their tools
// join the agent's tool list as they come up. It returns a note for the user
// when some of the project's servers are not trusted yet.
func (a *app) startMCP() string {
	cfgs, err := mcp.LoadConfig(a.cfg.Dir, a.cwd)
	if err != nil {
		a.ui.Errorf("mcp config: %v", err)
	}
	a.mcp = mcp.StartWith(cfgs, a.cwd, filepath.Join(a.cfg.Dir, "logs"), mcpOptions(a.cfg))
	a.ag.Dynamic = a.mcp.Tools
	a.ag.MCPTools = a.cfg.MCPTools
	untrusted := untrustedMCP(a.mcp)
	if len(untrusted) == 0 {
		return ""
	}
	if !a.interactive {
		return fmt.Sprintf("this project has %s not trusted yet (%s), which don't run: tego trust here runs them",
			plural(len(untrusted), "MCP server"), strings.Join(untrusted, ", "))
	}
	return fmt.Sprintf("this project has %s not trusted yet (%s): /mcp shows them, /mcp trust runs them",
		plural(len(untrusted), "MCP server"), strings.Join(untrusted, ", "))
}

func untrustedMCP(m *mcp.Manager) []string {
	var out []string
	for _, s := range m.Status() {
		if s.State == mcp.StateUntrusted {
			out = append(out, s.Name)
		}
	}
	return out
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

func (a *app) mcpCommand(args string) {
	if args == "trust" {
		names := a.mcp.Trust()
		if len(names) == 0 {
			a.ui.Dim("no untrusted MCP servers")
			return
		}
		a.ui.Dim("trusted %s (until their config changes); starting…", strings.Join(names, ", "))
		a.mcp.Wait(5 * time.Second) // the list below shows any still connecting
	}
	st := a.mcp.Status()
	if len(st) == 0 {
		fmt.Fprintln(a.out, "no MCP servers configured (mcpServers in ~/.tego/mcp.json, ./.mcp.json or ./.tego/mcp.json)")
		return
	}
	c := a.ui.c
	for _, s := range st {
		color := c.dim
		switch s.State {
		case mcp.StateConnected:
			color = c.green
		case mcp.StateFailed:
			color = c.red
		case mcp.StateConnecting, mcp.StateUntrusted:
			color = c.yellow
		}
		line := fmt.Sprintf("  %s●%s %-20s %s%-10s%s %-6s", color, c.reset, s.Name, color, s.State, c.reset, s.Transport)
		if s.State == mcp.StateConnected || s.State == mcp.StateIdle {
			line += fmt.Sprintf(" %d tools", s.Tools)
		}
		if s.State == mcp.StateIdle && s.Err == "" {
			line += c.dim + "  not running; starts at its next call" + c.reset
		}
		if s.Err != "" {
			msg := strings.ReplaceAll(s.Err, "\n", " ")
			if s.State == mcp.StateUntrusted {
				msg += "; /mcp trust to run"
			}
			line += "  " + c.dim + msg + c.reset
		}
		fmt.Fprintln(a.out, line)
	}
	a.ui.Dim("server logs: %s", filepath.Join(a.cfg.Dir, "logs"))
}
