package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/akbxr/tego/internal/llm"
)

type Todo struct {
	Content string `json:"content"`
	Status  string `json:"status"`          // pending | in_progress | completed
	Phase   string `json:"phase,omitempty"` // groups items, e.g. "Review", "Fix"
}

// TodoTool lets the model keep a visible task list; each call replaces it.
type TodoTool struct{ Parent *Agent }

func (t *TodoTool) Def() llm.Tool {
	return llm.Tool{
		Name: "todo",
		Description: "Maintain the task list for multi-step work (4+ distinct steps, or when the user lists several items). Each call REPLACES the whole list. " +
			"Keep exactly one item in_progress while working; when you finish an item, mark it completed and the next in_progress in the same response as that next item's first tool call, not as a turn of its own. Add items you discover. Skip for small tasks. " +
			"For larger work, give items a phase (e.g. \"Review\", \"Fix\", \"Verify\"); items of a phase are shown together and phases in order of first appearance.",
		Schema: json.RawMessage(`{"type":"object","properties":{"todos":{"type":"array","items":{"type":"object","properties":{"content":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","completed"]},"phase":{"type":"string","description":"optional phase name grouping items"}},"required":["content","status"]}}},"required":["todos"]}`),
	}
}

func (t *TodoTool) Run(_ context.Context, input json.RawMessage) (string, error) {
	list, err := ParseTodos(input)
	if err != nil {
		return "", err
	}
	a := t.Parent
	a.todoMu.Lock()
	a.todos = list
	a.todoRev++
	a.todoMu.Unlock()
	return RenderTodos(list), nil
}

// ParseTodos reads a todo tool input; a missing status means pending.
func ParseTodos(input json.RawMessage) ([]Todo, error) {
	var in struct {
		Todos []Todo `json:"todos"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	for i, td := range in.Todos {
		switch td.Status {
		case "pending", "in_progress", "completed":
		case "":
			in.Todos[i].Status = "pending"
		default:
			return nil, fmt.Errorf("todo %d: invalid status %q", i+1, td.Status)
		}
		if strings.TrimSpace(td.Content) == "" {
			return nil, errors.New("todo content must not be empty")
		}
		in.Todos[i].Phase = strings.TrimSpace(td.Phase)
	}
	return in.Todos, nil
}

// TodoPhase is one phase of the list with its items in order.
type TodoPhase struct {
	Name  string // "" for items without a phase
	Items []Todo
	Done  int
}

// TodoPhases groups the list by phase, phases in order of first appearance.
func TodoPhases(list []Todo) []TodoPhase {
	var out []TodoPhase
	index := map[string]int{}
	for _, td := range list {
		i, ok := index[td.Phase]
		if !ok {
			i = len(out)
			index[td.Phase] = i
			out = append(out, TodoPhase{Name: td.Phase})
		}
		out[i].Items = append(out[i].Items, td)
		if td.Status == "completed" {
			out[i].Done++
		}
	}
	return out
}

// PhaseName is the heading of phases[i]: its name, "Other" for the items
// without a phase when there are named phases, "" for a list without any.
func PhaseName(phases []TodoPhase, i int) string {
	if phases[i].Name == "" && len(phases) > 1 {
		return "Other"
	}
	return phases[i].Name
}

// RenderTodos is the plain-text list shown to the model.
func RenderTodos(list []Todo) string {
	if len(list) == 0 {
		return "Todo list cleared."
	}
	done := 0
	var b strings.Builder
	phases := TodoPhases(list)
	for pi, ph := range phases {
		done += ph.Done
		if name := PhaseName(phases, pi); name != "" {
			fmt.Fprintf(&b, "%d. %s %d/%d\n", pi+1, name, ph.Done, len(ph.Items))
		}
		for _, td := range ph.Items {
			mark := "○"
			switch td.Status {
			case "completed":
				mark = "✓"
			case "in_progress":
				mark = "▸"
			}
			fmt.Fprintf(&b, "%s %s\n", mark, td.Content)
		}
	}
	return fmt.Sprintf("Todo %d/%d\n", done, len(list)) + b.String()
}

const (
	// todoListHeading introduces a rendered todo list carried over in a
	// compaction summary or a rewind result; todosFromText finds it again.
	todoListHeading   = "\n\nCurrent todo list:\n"
	contextSummaryTag = "<context-summary>"
)

// todosFromText parses the last list rendered after todoListHeading in s
// (RenderTodos format), or reports false.
func todosFromText(s string) ([]Todo, bool) {
	i := strings.LastIndex(s, todoListHeading)
	if i < 0 {
		return nil, false
	}
	lines := strings.Split(s[i+len(todoListHeading):], "\n")
	head, ok := strings.CutPrefix(lines[0], "Todo ")
	_, total, ok2 := ratio(head)
	if !ok || !ok2 || total == 0 {
		return nil, false
	}
	marks := map[string]string{"✓": "completed", "▸": "in_progress", "○": "pending"}
	list := []Todo{}
	phase := ""
	for _, l := range lines[1:] {
		if len(list) == total {
			break
		}
		if mark, content, ok := strings.Cut(l, " "); ok && marks[mark] != "" {
			list = append(list, Todo{Content: content, Status: marks[mark], Phase: phase})
			continue
		}
		// A phase heading: "N. Name done/total".
		num, rest, ok := strings.Cut(l, ". ")
		sp := strings.LastIndex(rest, " ")
		if _, err := strconv.Atoi(num); err != nil || !ok || sp < 0 {
			return nil, false
		}
		if _, _, ok := ratio(rest[sp+1:]); !ok {
			return nil, false
		}
		phase = rest[:sp]
		if phase == "Other" { // PhaseName's label for the items without a phase
			phase = ""
		}
	}
	if len(list) != total {
		return nil, false
	}
	return list, true
}

// ratio parses "done/total".
func ratio(s string) (int, int, bool) {
	a, b, ok := strings.Cut(s, "/")
	x, err1 := strconv.Atoi(a)
	y, err2 := strconv.Atoi(b)
	return x, y, ok && err1 == nil && err2 == nil
}

// Todos returns a copy of the current task list (goroutine-safe).
func (a *Agent) Todos() []Todo {
	list, _ := a.TodoState()
	return list
}

// TodoState returns a copy of the task list and how many todo calls have
// set it so far (goroutine-safe), so the UI can tell whether a turn
// touched the list.
func (a *Agent) TodoState() ([]Todo, int) {
	a.todoMu.Lock()
	defer a.todoMu.Unlock()
	return append([]Todo(nil), a.todos...), a.todoRev
}

// SetHistory replaces the conversation (new/resumed session), clearing any
// checkpoint and restoring the latest todo list found in the history.
func (a *Agent) SetHistory(msgs []llm.Message) {
	a.Messages = msgs
	defer func() { a.LastCtx = a.EstimateContext() }() // resumed/branched history: show real usage, not 0
	a.cp, a.pendingRewind, a.elideFloor, a.cleared, a.clearMark = checkpointState{}, nil, 0, 0, 0
	a.restoreClearTrigger()
	a.ctxRewrite, a.ctxRewriteAt = 0, 0
	a.lastReply = a.Session.LeafTime() // a resumed session's cache may have expired
	a.activateFromHistory(msgs)
	todos := latestTodos(msgs)
	a.todoMu.Lock()
	a.todos = todos
	a.todoMu.Unlock()
}

// latestTodos is the newest todo list in msgs: the input of the last todo
// call, or the list carried over in a compaction summary or rewind result
// (whose todo calls are gone). nil when there is none.
func latestTodos(msgs []llm.Message) []Todo {
	rewinds := map[string]bool{}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolUse && b.Name == "rewind" {
				rewinds[b.ID] = true
			}
		}
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		for j := len(msgs[i].Blocks) - 1; j >= 0; j-- {
			b := msgs[i].Blocks[j]
			var list []Todo
			var ok bool
			switch {
			case b.Type == llm.BlockToolUse && b.Name == "todo":
				var err error
				list, err = ParseTodos(b.Input)
				ok = err == nil
			case b.Type == llm.BlockText && msgs[i].Role == llm.RoleUser && strings.HasPrefix(b.Text, contextSummaryTag):
				list, ok = todosFromText(b.Text)
			case b.Type == llm.BlockToolResult && rewinds[b.ToolUseID]:
				list, ok = todosFromText(b.Content)
			}
			if ok {
				if list == nil {
					list = []Todo{}
				}
				return list
			}
		}
	}
	return nil
}
