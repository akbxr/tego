package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// seesTodos is a slow tool (a command, subagents) that records the todo
// list as it was when it ran.
type seesTodos struct {
	parent *Agent
	saw    *int
}

func (t seesTodos) Def() llm.Tool { return llm.Tool{Name: "slow"} }
func (t seesTodos) Run(context.Context, json.RawMessage) (string, error) {
	todos, _ := t.parent.TodoState()
	*t.saw = len(todos)
	return "ran", nil
}

// A todo update sent after other calls in one response shows while they
// run: the panel lagged behind until they ended.
func TestTodoCallsRunFirst(t *testing.T) {
	saw := -1
	a := &Agent{}
	a.Tools = []tools.Tool{seesTodos{a, &saw}, &TodoTool{Parent: a}}
	todo := `{"todos":[{"content":"Step A","status":"in_progress"},{"content":"Step B","status":"pending"}]}`
	runWith(t, a,
		assistant(call("s1", "slow", `{}`), call("t1", "todo", todo)),
		assistant(text("done")),
		assistant(text("done")), // after the reminder about the unfinished list
	)
	if saw != 2 {
		t.Fatalf("the slow call saw %d todos: the list was set after it ran", saw)
	}
	// Results still answer the calls in call order.
	m := a.Messages[2]
	if m.Blocks[0].ToolUseID != "s1" || m.Blocks[1].ToolUseID != "t1" || m.Blocks[0].Content != "ran" {
		t.Fatalf("results %+v", m.Blocks)
	}
}
