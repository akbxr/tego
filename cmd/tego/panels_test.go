package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/agent"
)

func TestTodoPanel(t *testing.T) {
	todos := []agent.Todo{
		{Content: "read", Status: "completed", Phase: "Review"},
		{Content: "fix a", Status: "in_progress", Phase: "Fix"},
		{Content: "fix b", Status: "pending", Phase: "Fix"},
		{Content: "test", Status: "pending", Phase: "Verify"},
	}
	got := strings.Join(todoPanel(palette{}, todos, true), "\n")
	// Only the phase being worked on is expanded.
	want := " TODO 1/4\n  ├─ 1. Review · 1/1\n  ├─ 2. Fix · 0/2\n  │  ├─ ▸ fix a\n  │  └─ ○ fix b\n  ├─ 3. Verify · 0/1\n  └─────"
	if got != want {
		t.Fatalf("panel:\n%s\nwant:\n%s", got, want)
	}
	// An unfinished list shows even in a turn that didn't touch it.
	if p := todoPanel(palette{}, todos, false); len(p) == 0 {
		t.Fatal("unfinished list hidden")
	}
	for i := range todos {
		todos[i].Status = "completed"
	}
	// A finished list shows only in the turn that finished it, not in
	// later turns of the session (or while idle).
	if p := todoPanel(palette{}, todos, false); p != nil {
		t.Fatalf("old finished list shown: %q", p)
	}
	if p := todoPanel(palette{}, todos, true); len(p) == 0 {
		t.Fatal("list hidden in the turn that finished it")
	}
}

func TestAgentsPanel(t *testing.T) {
	start := time.Now().Add(-3 * time.Second)
	subs := []agent.SubagentStatus{
		{Name: "Done", State: agent.SubDone, Started: start, Ended: start.Add(time.Second)},
		{Name: "Run", State: agent.SubRunning, Started: start, Tools: 2, Last: "read /w/a.go"},
		{Name: "Wait", State: agent.SubQueued},
	}
	got := strings.Join(agentsPanel(palette{}, subs, 100, "/w"), "\n")
	for _, want := range []string{"Subagents 1 running · 1 queued", "├─ ", " Run · 3.", "2 tools", "│  └ read a.go", "○ Wait · queued", "└─ ✓ Done · 1.0s"} {
		if !strings.Contains(got, want) {
			t.Fatalf("panel lacks %q:\n%s", want, got)
		}
	}
	// Running rows come first.
	if strings.Index(got, "Run") > strings.Index(got, "Wait") || strings.Index(got, "Wait") > strings.Index(got, "Done") {
		t.Fatalf("order:\n%s", got)
	}
	subs[1].State = agent.SubDone
	subs[2].State = agent.SubDone
	if p := agentsPanel(palette{}, subs, 100, "/w"); p != nil {
		t.Fatalf("panel after the task: %q", p)
	}
}

func TestTaskBoxReplay(t *testing.T) {
	u := &termUI{out: io.Discard, diag: io.Discard, atBOL: true, width: func() int { return 60 }}
	// The live state belongs to a later call: the box must use the output.
	u.subagents = func() []agent.SubagentStatus {
		return []agent.SubagentStatus{{Name: "Other", Prompt: "x", State: agent.SubDone}}
	}
	in := json.RawMessage(`{"tasks":[{"name":"A","prompt":"look at a"},{"name":"B","prompt":"look at b"}]}`)
	out := "## A (done)\nfine\n\n## B (failed: boom)\n(no report)"
	box := u.taskBox(in, out, false)
	for _, want := range []string{"Task: 2 agents", "• A  look at a", "✓ A", "✗ B", "boom", "✗ 1 failed"} {
		if !strings.Contains(box, want) {
			t.Fatalf("box lacks %q:\n%s", want, box)
		}
	}
	if strings.Contains(box, "Other") {
		t.Fatalf("box used another call's subagents:\n%s", box)
	}
}
