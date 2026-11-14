package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

type fakeMCP struct{ name, desc string }

func (f fakeMCP) Def() llm.Tool {
	return llm.Tool{Name: f.name, Description: f.desc, Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}
}
func (f fakeMCP) Run(context.Context, json.RawMessage) (string, error) { return "ran " + f.name, nil }

func catalog() []tools.Tool {
	var out []tools.Tool
	for i := range 30 {
		out = append(out, fakeMCP{fmt.Sprintf("mcp__studio__tool%d", i), "does thing number " + fmt.Sprint(i)})
	}
	out = append(out,
		fakeMCP{"mcp__studio__capture_screenshot", "Capture the viewport as an image"},
		fakeMCP{"mcp__docs__get_docs", "Read library documentation"})
	return out
}

func names(defs []llm.Tool) []string {
	var out []string
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

func TestDeferredMCPToolsLoadOnSearch(t *testing.T) {
	a := &Agent{UI: nopUI{}, Dynamic: catalog, MCPTools: "lazy"}
	defs := names(a.toolDefs())
	if len(defs) != 1 || defs[0] != "tool_search" {
		t.Fatalf("lazy mode should advertise only tool_search, got %v", defs)
	}
	if d := a.toolDefs()[0].Description; !strings.Contains(d, "studio (31 tools)") || !strings.Contains(d, "docs (1 tools)") {
		t.Fatalf("search description should list servers: %s", d)
	}

	out, err := a.tool("tool_search").Run(context.Background(), json.RawMessage(`{"query":"screenshot"}`))
	if err != nil || !strings.Contains(out, "mcp__studio__capture_screenshot") || !strings.Contains(out, "path (string, required)") {
		t.Fatalf("search output: %q %v", out, err)
	}
	if defs := names(a.toolDefs()); len(defs) != 2 || defs[1] != "mcp__studio__capture_screenshot" {
		t.Fatalf("found tool should be loaded for the next request, got %v", defs)
	}

	// A server name lists tools without loading them.
	out, _ = a.tool("tool_search").Run(context.Background(), json.RawMessage(`{"query":"studio"}`))
	if !strings.Contains(out, "31 tools") || len(a.toolDefs()) != 2 {
		t.Fatalf("server listing should not load tools: %q", out)
	}

	// Calling a deferred tool by exact name works and loads it.
	if tl := a.tool("mcp__docs__get_docs"); tl == nil {
		t.Fatal("deferred tool should resolve by name")
	}
	if len(a.toolDefs()) != 3 {
		t.Fatalf("called tool should stay loaded: %v", names(a.toolDefs()))
	}
}

// deferring is a provider that loads deferred tools itself, as Claude does.
type deferring struct{ countingProvider }

func (deferring) ClearsToolResults(string) bool { return false }
func (deferring) DefersTools(string) bool       { return true }

// big is n MCP tools of about 650 tokens each, as the Blender server's.
func big(n int) func() []tools.Tool {
	return func() []tools.Tool {
		var out []tools.Tool
		for i := range n {
			out = append(out, fakeMCP{fmt.Sprintf("mcp__blender__tool%d", i), strings.Repeat("Run a step in Blender. ", 110)})
		}
		return out
	}
}

func TestMCPToolModes(t *testing.T) {
	eager := &Agent{UI: nopUI{}, Dynamic: catalog, MCPTools: "eager"}
	if n := len(eager.toolDefs()); n != 32 {
		t.Fatalf("eager mode sends all tools, got %d", n)
	}
	few := func() []tools.Tool { return catalog()[30:] }
	for _, mode := range []string{"", "auto"} {
		// Elsewhere than Claude, small servers stay inline, big ones are deferred.
		if n := names((&Agent{UI: nopUI{}, Dynamic: few, MCPTools: mode}).toolDefs()); len(n) != 2 || n[0] == "tool_search" {
			t.Errorf("%q: few small tools should stay inline, got %v", mode, n)
		}
		if n := names((&Agent{UI: nopUI{}, Dynamic: big(9), MCPTools: mode}).toolDefs()); len(n) != 1 || n[0] != "tool_search" {
			t.Errorf("%q: 9 big tools should be deferred, got %v", mode, n)
		}
		// On Claude every MCP tool is deferred, however few.
		c := &Agent{Provider: &deferring{}, UI: nopUI{}, Dynamic: few, MCPTools: mode}
		if loaded, deferred := defNames(c.toolDefs()); len(loaded) != 1 || loaded[0] != "tool_search" || len(deferred) != 2 {
			t.Errorf("%q on Claude: loaded %v, deferred %v", mode, loaded, deferred)
		}
	}
	if toolTokens(big(9)()) < 5000 {
		t.Errorf("big tools take %d tokens", toolTokens(big(9)()))
	}
}

func TestResumeKeepsUsedMCPToolsLoaded(t *testing.T) {
	a := &Agent{UI: nopUI{}, Dynamic: catalog, MCPTools: "lazy"}
	a.SetHistory([]llm.Message{{Role: llm.RoleAssistant, Blocks: []llm.Block{
		{Type: llm.BlockToolUse, ID: "1", Name: "mcp__studio__tool7", Input: json.RawMessage(`{}`)}}}})
	if defs := names(a.toolDefs()); len(defs) != 2 || defs[1] != "mcp__studio__tool7" {
		t.Fatalf("tool used in history should be loaded, got %v", defs)
	}
}
