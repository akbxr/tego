package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/session"
)

// /goal works until a check finds it holds: a check after each turn, its
// reason the next turn's guidance; met, it clears, and the session file
// says so. Turns without a tool call pause it until the user's prompt.
func TestGoal(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", t.TempDir())
	model := &scripted{
		replies: []func(context.Context) *llm.Message{
			useTool("c1", "bash", map[string]any{"command": "echo one"}),
			func(context.Context) *llm.Message { return say("first try") },
			func(context.Context) *llm.Message { return say("now it is there") },
		},
		verdicts: []string{"NOT_MET: done.txt is missing", "MET: the agent showed done.txt"},
	}
	ed := startACP(t, home, model)
	sid := newACPSession(t, ed, project)

	if out := ed.prompt(sid, "/goal"); !strings.Contains(out, "No goal set") {
		t.Fatalf("/goal before: %q", out)
	}
	ed.prompt(sid, "/goal done.txt exists")
	notes := ed.text("agent_thought_chunk")
	for _, want := range []string{"◎ goal set: done.txt exists", "◎ goal not met yet: done.txt is missing", "◎ goal met after 2 turns"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
	if len(model.prompts) != 3 || !strings.HasPrefix(model.prompts[0], "Goal: done.txt exists\n\nWork toward this goal") ||
		model.prompts[1] != model.prompts[0] || // the bash call's result
		!strings.HasPrefix(model.prompts[2], "The goal is not met yet. The check after your turn says: done.txt is missing") {
		t.Fatalf("prompts %q", model.prompts)
	}
	if len(model.checks) != 2 || !strings.Contains(model.checks[0], "<goal>\ndone.txt exists\n</goal>") ||
		!strings.Contains(model.checks[0], "→ ") || !strings.Contains(model.checks[1], `turns_checked_before="1"`) {
		t.Fatalf("checks %q", model.checks)
	}
	if out := ed.prompt(sid, "/goal"); !strings.Contains(out, "No goal set") || !strings.Contains(out, "Last: met after 2 turns") {
		t.Fatalf("/goal after: %q", out)
	}
	if g := sessionGoal(t, home, sid); g != "" {
		t.Fatalf("goal in the file after it was met: %q", g)
	}

	// No tool call in three turns in a row: it waits for the user.
	model.mu.Lock()
	model.prompts, model.checks = nil, nil
	model.verdicts = []string{"NOT_MET: nothing shown", "NOT_MET: still nothing", "MET: shown at last"}
	model.mu.Unlock()
	ed.prompt(sid, "/goal the answer is shown")
	if len(model.prompts) != 3 || len(model.checks) != 2 {
		t.Fatalf("%d turns, %d checks", len(model.prompts), len(model.checks))
	}
	if g := sessionGoal(t, home, sid); g != "the answer is shown" {
		t.Fatalf("goal in the file: %q", g)
	}
	if out := ed.prompt(sid, "/goal"); !strings.Contains(out, "◎ goal: the answer is shown") || !strings.Contains(out, "2 turns checked") ||
		!strings.Contains(out, "latest check: still nothing") || !strings.Contains(out, "paused: 3 turns in a row without a tool call") {
		t.Fatalf("/goal paused: %q", out)
	}
	// The user's prompt: it goes on, and is met.
	ed.prompt(sid, "show it")
	if len(model.prompts) != 4 || len(model.checks) != 3 {
		t.Fatalf("after the prompt: %d turns, %d checks", len(model.prompts), len(model.checks))
	}
	if out := ed.prompt(sid, "/goal clear"); out != "No goal set" {
		t.Fatalf("clear: %q", out)
	}

	// Cleared before it is met; a plain prompt checks nothing.
	model.mu.Lock()
	model.verdicts = []string{"NOT_MET: x", "NOT_MET: x", "NOT_MET: x"}
	model.mu.Unlock()
	ed.prompt(sid, "/goal never")
	if out := ed.prompt(sid, "/goal stop"); out != "Goal cleared: never" {
		t.Fatalf("stop: %q", out)
	}
	n := len(model.checks)
	ed.prompt(sid, "hello")
	if len(model.checks) != n {
		t.Fatal("checked without a goal")
	}
}

// sessionGoal is the goal the session file of id holds.
func sessionGoal(t *testing.T, home, id string) string {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(home, "sessions", "*", id+".jsonl"))
	if len(paths) != 1 {
		entries, _ := os.ReadDir(filepath.Join(home, "sessions"))
		t.Fatalf("session file of %s: %v (%v)", id, paths, entries)
	}
	s, _, err := session.Open(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	return s.Goal()
}
