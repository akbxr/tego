package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// namedTool succeeds (or fails) under any name.
type namedTool struct {
	name string
	fail bool
}

func (t namedTool) Def() llm.Tool { return llm.Tool{Name: t.name} }
func (t namedTool) Run(context.Context, json.RawMessage) (string, error) {
	if t.fail {
		return "", context.Canceled
	}
	return "ok", nil
}

func assistant(blocks ...llm.Block) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Blocks: blocks}
}

func reminders(msgs []llm.Message) []string {
	var out []string
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if m.Role == llm.RoleUser && strings.HasPrefix(b.Text, "<system-reminder>") {
				out = append(out, b.Text)
			}
		}
	}
	return out
}

func runWith(t *testing.T, a *Agent, replies ...llm.Message) *recorder {
	t.Helper()
	prov := &recorder{replies: replies}
	a.Provider, a.UI = prov, nopUI{}
	if a.Tools == nil {
		a.Tools = []tools.Tool{namedTool{name: "edit"}, namedTool{name: "write"}, namedTool{name: "bash"}}
	}
	if err := a.Run(context.Background(), "do it"); err != nil {
		t.Fatal(err)
	}
	if len(prov.replies) != 0 {
		t.Fatalf("%d replies left unused", len(prov.replies))
	}
	return prov
}

func TestVerifyNudgeAfterUnverifiedEdit(t *testing.T) {
	a := &Agent{}
	runWith(t, a,
		assistant(call("e1", "edit", `{"path":"main.go"}`)),
		assistant(text("done")),
		assistant(call("b1", "bash", `{"command":"go test ./..."}`)),
		assistant(text("done, tests pass")),
	)
	r := reminders(a.Messages)
	if len(r) != 1 || !strings.Contains(r[0], `"main.go"`) || !strings.Contains(r[0], "verify") {
		t.Fatalf("reminders: %q", r)
	}
}

func TestVerifyNudgeOnlyOnce(t *testing.T) {
	a := &Agent{}
	runWith(t, a,
		assistant(call("e1", "write", `{"path":"a.py"}`)),
		assistant(text("done")),
		assistant(text("no tests exist here")), // declined: the loop must end
	)
	if r := reminders(a.Messages); len(r) != 1 {
		t.Fatalf("want exactly one reminder, got %q", r)
	}
}

func TestNoVerifyNudge(t *testing.T) {
	cases := map[string][]llm.Message{
		"bash after edit": {assistant(call("e1", "edit", `{"path":"a.go"}`), call("b1", "bash", `{"command":"go build"}`)), assistant(text("done"))},
		"docs only":       {assistant(call("e1", "edit", `{"path":"README.md"}`), call("e2", "write", `{"path":"notes.TXT"}`)), assistant(text("done"))},
		"no edits":        {assistant(text("answer"))},
		"bash only":       {assistant(call("b1", "bash", `{"command":"ls"}`)), assistant(text("done"))},
	}
	for name, replies := range cases {
		t.Run(name, func(t *testing.T) {
			a := &Agent{}
			runWith(t, a, replies...)
			if r := reminders(a.Messages); len(r) != 0 {
				t.Fatalf("unexpected reminder %q", r)
			}
		})
	}
}

func TestFailedEditIsNotTracked(t *testing.T) {
	a := &Agent{Tools: []tools.Tool{namedTool{name: "edit", fail: true}}}
	runWith(t, a, assistant(call("e1", "edit", `{"path":"a.go"}`)), assistant(text("could not edit")))
	if r := reminders(a.Messages); len(r) != 0 {
		t.Fatalf("unexpected reminder %q", r)
	}
}

func TestBashBeforeEditDoesNotCount(t *testing.T) {
	a := &Agent{}
	a.trackEdits(
		[]llm.Block{call("b1", "bash", `{}`), call("e1", "edit", `{"path":"x.go"}`), call("e2", "edit", `{"path":"x.go"}`)},
		[]llm.Block{result("b1", "ok"), result("e1", "ok"), result("e2", "ok")})
	if !reflect.DeepEqual(a.stop.unverified, []string{"x.go"}) {
		t.Fatalf("unverified = %q", a.stop.unverified)
	}
}

func TestSubagentGetsNoStopNudges(t *testing.T) {
	a := &Agent{Subagent: true}
	runWith(t, a, assistant(call("e1", "edit", `{"path":"a.go"}`)), assistant(text("report")))
	if got := a.LastText(); got != "report" {
		t.Fatalf("subagent report replaced: %q", got)
	}
}

func TestTodoNudge(t *testing.T) {
	a := &Agent{}
	a.Tools = []tools.Tool{&TodoTool{Parent: a}}
	runWith(t, a,
		assistant(call("t1", "todo", `{"todos":[{"content":"step one","status":"completed"},{"content":"step two","status":"in_progress"}]}`)),
		assistant(text("done")),
		assistant(text("waiting for you")),
	)
	r := reminders(a.Messages)
	if len(r) != 1 || !strings.Contains(r[0], `1 unfinished item(s): "step two"`) {
		t.Fatalf("reminders: %q", r)
	}

	// The next prompt does not touch the stale list: no reminder.
	n := len(a.Messages)
	runWith(t, a, assistant(text("unrelated answer")))
	if r := reminders(a.Messages[n:]); len(r) != 0 {
		t.Fatalf("stale list nagged: %q", r)
	}
}

func TestTodoNudgeNotForFinishedList(t *testing.T) {
	a := &Agent{}
	a.Tools = []tools.Tool{&TodoTool{Parent: a}}
	runWith(t, a,
		assistant(call("t1", "todo", `{"todos":[{"content":"only","status":"completed"}]}`)),
		assistant(text("done")),
	)
	if r := reminders(a.Messages); len(r) != 0 {
		t.Fatalf("unexpected reminder %q", r)
	}
}

func TestCompactCarriesTodosAndResumeRestoresThem(t *testing.T) {
	a := &Agent{Provider: &summaryProvider{}, UI: nopUI{}, ContextWindow: 200_000, KeepRecent: 1}
	a.Messages = toolHistory(6, 1000)
	want := []Todo{
		{Content: "explore", Status: "completed", Phase: "Review"},
		{Content: "fix a. b 2/3", Status: "in_progress", Phase: "Fix"},
		{Content: "loose end", Status: "pending"},
	}
	a.todos = want
	if err := a.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if s := a.Messages[0].Blocks[0].Text; !strings.Contains(s, "Current todo list:\nTodo 1/3") {
		t.Fatalf("summary lacks the todo list:\n%s", s)
	}

	b := &Agent{UI: nopUI{}}
	b.SetHistory(a.Messages)
	if got := b.Todos(); !reflect.DeepEqual(got, want) {
		t.Fatalf("restored %+v, want %+v", got, want)
	}
}

func TestResumeRestoresTodosFromRewind(t *testing.T) {
	a := &Agent{UI: nopUI{}}
	a.Tools = []tools.Tool{&TodoTool{Parent: a}, &CheckpointTool{Parent: a}, &RewindTool{Parent: a}, echoTool{}}
	a.Messages = []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{text("task")}}}
	step(t, a, call("t1", "todo", `{"todos":[{"content":"explore","status":"in_progress"}]}`), call("c1", "checkpoint", `{"goal":"g"}`))
	step(t, a, call("t2", "todo", `{"todos":[{"content":"explore","status":"completed"},{"content":"fix","status":"pending"}]}`))
	step(t, a, call("w1", "rewind", `{"report":"found it"}`))

	b := &Agent{UI: nopUI{}}
	b.SetHistory(a.Messages) // the t2 call was dropped by the rewind
	want := []Todo{{Content: "explore", Status: "completed"}, {Content: "fix", Status: "pending"}}
	if got := b.Todos(); !reflect.DeepEqual(got, want) {
		t.Fatalf("restored %+v, want %+v", got, want)
	}
}

func TestTodosFromTextRejectsMalformed(t *testing.T) {
	for _, s := range []string{
		"no list here",
		todoListHeading + "Todo 2/2\n✓ a\n",            // one item missing
		todoListHeading + "Todo x/2\n✓ a\n✓ b",         // bad header
		todoListHeading + "Todo 1/1\nsomething else\n", // unknown line
	} {
		if list, ok := todosFromText(s); ok {
			t.Fatalf("%q parsed as %+v", s, list)
		}
	}
}
