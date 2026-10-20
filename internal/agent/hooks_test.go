package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// inputTool echoes its input.
type inputTool struct{ name string }

func (t inputTool) Def() llm.Tool { return llm.Tool{Name: t.name} }
func (t inputTool) Run(_ context.Context, in json.RawMessage) (string, error) {
	return "ran with " + string(in), nil
}
func (t inputTool) ReadOnly() bool { return true }

type hookRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (h *hookRecorder) BeforeTool(_ context.Context, agent, tool string, in json.RawMessage) (json.RawMessage, string) {
	h.mu.Lock()
	h.calls = append(h.calls, agent+"/"+tool)
	h.mu.Unlock()
	switch {
	case strings.Contains(string(in), "danger"):
		return nil, "guard: not that"
	case tool == "echo":
		return json.RawMessage(`{"x":"changed"}`), ""
	}
	return nil, ""
}

func (h *hookRecorder) AfterTool(_ context.Context, agent, tool string, in json.RawMessage, out string, isErr bool) (string, bool) {
	if strings.Contains(out, "secret") {
		return strings.ReplaceAll(out, "secret", "[redacted]"), true
	}
	return out, false
}

func toolResults(req *llm.Request) []string {
	var out []string
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolResult {
				out = append(out, b.Content)
			}
		}
	}
	return out
}

func callMsg(calls ...llm.Block) *llm.Message {
	return &llm.Message{Role: llm.RoleAssistant, StopReason: "tool_use", Blocks: calls}
}

func use(id, name, input string) llm.Block {
	return llm.Block{Type: llm.BlockToolUse, ID: id, Name: name, Input: llm.ToolInput([]byte(input))}
}

func TestToolHooks(t *testing.T) {
	h := &hookRecorder{}
	done := &llm.Message{Role: llm.RoleAssistant, StopReason: "end_turn", Blocks: []llm.Block{text("ok")}}
	// One call alone (execute), then two read-only ones (a parallel batch: runOne).
	prov := &scripted{replies: []*llm.Message{
		callMsg(use("1", "echo", `{"x":"a"}`)),
		callMsg(use("2", "look", `{"q":"danger"}`), use("3", "look", `{"q":"secret"}`)),
		done,
	}}
	a := &Agent{Provider: prov, Model: "m", ContextWindow: 100_000, UI: nopUI{}, Hooks: h,
		Tools: []tools.Tool{inputTool{"echo"}, inputTool{"look"}}}
	if err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	got := toolResults(prov.reqs[2])
	want := []string{`ran with {"x":"changed"}`,
		"This look call was blocked by an extension (guard: not that); it did not run.",
		`ran with {"q":"[redacted]"}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("results:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Subagents' calls go through the same hooks, named.
	prov.replies = []*llm.Message{callMsg(use("4", "read", `{"path":"x"}`)), done}
	task := &TaskTool{Parent: a, Env: &tools.Env{Cwd: t.TempDir()}}
	in, _ := json.Marshal(map[string]any{"tasks": []map[string]string{{"name": "Sub", "prompt": "p"}}})
	if _, err := task.Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if last := h.calls[len(h.calls)-1]; last != "Sub/read" {
		t.Fatalf("calls %v", h.calls)
	}
}
