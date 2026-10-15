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
	a := &Agent{UI: nopUI{}, Dynamic: catalog, LazyThreshold: 10}
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

func TestMCPToolModes(t *testing.T) {
	eager := &Agent{UI: nopUI{}, Dynamic: catalog, LazyThreshold: -1}
	if n := len(eager.toolDefs()); n != 32 {
		t.Fatalf("eager mode sends all tools, got %d", n)
	}
	few := &Agent{UI: nopUI{}, Dynamic: func() []tools.Tool { return catalog()[30:] }, LazyThreshold: 10}
	if n := names(few.toolDefs()); len(n) != 2 || n[0] == "tool_search" {
		t.Fatalf("auto mode keeps few tools inline, got %v", n)
	}
}

func TestResumeKeepsUsedMCPToolsLoaded(t *testing.T) {
	a := &Agent{UI: nopUI{}, Dynamic: catalog, LazyThreshold: 0}
	a.SetHistory([]llm.Message{{Role: llm.RoleAssistant, Blocks: []llm.Block{
		{Type: llm.BlockToolUse, ID: "1", Name: "mcp__studio__tool7", Input: json.RawMessage(`{}`)}}}})
	if defs := names(a.toolDefs()); len(defs) != 2 || defs[1] != "mcp__studio__tool7" {
		t.Fatalf("tool used in history should be loaded, got %v", defs)
	}
}
