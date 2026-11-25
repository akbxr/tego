package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// instructed is an MCP tool whose server has instructions.
type instructed struct {
	fakeMCP
	server, text string
}

func (i instructed) ServerInstructions() (string, string) { return i.server, i.text }

// A server's instructions reach the model once: with the first search that
// shows its tools, or else the first result of one of them; a resumed
// conversation that has them doesn't get them again.
func TestServerInstructionsOnce(t *testing.T) {
	cat := func() []tools.Tool {
		return []tools.Tool{
			instructed{fakeMCP{"mcp__blender__get_scene_info", "Scene info"}, "blender", "Call get_scene_info first."},
			instructed{fakeMCP{"mcp__blender__render", "Render a frame"}, "blender", "Call get_scene_info first."},
			instructed{fakeMCP{"mcp__docs__search", "Search docs"}, "docs", ""},
		}
	}
	a := &Agent{UI: nopUI{}, Dynamic: cat, MCPTools: "lazy"}
	search := func(q string) string {
		out, err := a.tool("tool_search").Run(context.Background(), json.RawMessage(`{"query":"`+q+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := search("docs"); strings.Contains(out, "mcp_server_instructions") {
		t.Fatalf("a server without instructions: %s", out)
	}
	out := search("scene")
	if !strings.Contains(out, `<mcp_server_instructions server="blender">`) || !strings.Contains(out, "Call get_scene_info first.") {
		t.Fatalf("first search: %s", out)
	}
	if out := search("render"); strings.Contains(out, "mcp_server_instructions") {
		t.Fatalf("given twice: %s", out)
	}
	// A keyword search gives the best match's server's, not every server's
	// that a match came from.
	d := &Agent{UI: nopUI{}, Dynamic: func() []tools.Tool {
		return []tools.Tool{
			instructed{fakeMCP{"mcp__a__render_scene", "Render the scene"}, "a", "A's notes."},
			instructed{fakeMCP{"mcp__b__scene_info", "Info"}, "b", "B's notes."},
		}
	}, MCPTools: "lazy"}
	out, _ = d.tool("tool_search").Run(context.Background(), json.RawMessage(`{"query":"render scene"}`))
	if !strings.Contains(out, "A's notes.") || strings.Contains(out, "B's notes.") {
		t.Fatalf("keyword search: %s", out)
	}
	out, _ = d.tool("tool_search").Run(context.Background(), json.RawMessage(`{"query":"select:mcp__b__scene_info"}`))
	if !strings.Contains(out, "B's notes.") {
		t.Fatalf("select: %s", out)
	}
	// Where the result goes as tool references, which take no text (Claude),
	// the notes go beside it, in the next request.
	e := &Agent{Provider: &deferring{}, UI: nopUI{}, Dynamic: d.Dynamic, MCPTools: "lazy"}
	out, _ = e.tool("tool_search").Run(context.Background(), json.RawMessage(`{"query":"render scene"}`))
	if strings.Contains(out, "A's notes.") || !strings.Contains(e.takeNotes(), "A's notes.") || e.takeNotes() != "" {
		t.Fatalf("deferred: %s", out)
	}
	res := a.run(context.Background(), a.tool("mcp__blender__render"), llm.Block{ID: "c1", Name: "mcp__blender__render"})
	if strings.Contains(res.Content, "mcp_server_instructions") {
		t.Fatalf("given again with a result: %s", res.Content)
	}

	// Eager tools: the first result carries them.
	b := &Agent{UI: nopUI{}, Dynamic: cat, MCPTools: "eager"}
	res = b.run(context.Background(), b.tool("mcp__blender__render"), llm.Block{ID: "c1", Name: "mcp__blender__render"})
	if !strings.HasPrefix(res.Content, "ran mcp__blender__render\n\n<mcp_server_instructions") {
		t.Fatalf("first result: %s", res.Content)
	}

	// A resumed conversation that has them.
	c := &Agent{UI: nopUI{}, Dynamic: cat, MCPTools: "eager"}
	c.activateFromHistory([]llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, Content: res.Content}}}})
	if res := c.run(context.Background(), c.tool("mcp__blender__get_scene_info"), llm.Block{ID: "c2"}); strings.Contains(res.Content, "mcp_server_instructions") {
		t.Fatalf("given again after a resume: %s", res.Content)
	}
}
