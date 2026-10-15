package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/akbxr/tego/internal/tools"
)

// hinted is an MCP-like tool that declares readOnlyHint.
type hinted struct{ runsTool }

func (hinted) ReadOnlyHint() bool { return true }

func TestPlanModeRefusesChanges(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	_ = os.WriteFile(p, []byte("package a\n"), 0o644)
	var mcpRuns, otherRuns int
	plan := &atomic.Bool{}
	plan.Store(true)
	a := &Agent{Plan: plan, Tools: append(tools.Builtins(&tools.Env{Cwd: dir}),
		hinted{runsTool{"mcp__docs__search", &mcpRuns}}, runsTool{"mcp__db__drop", &otherRuns})}
	in := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	runWith(t, a,
		assistant(call("r1", "read", in(map[string]any{"path": p}))),
		assistant(call("e1", "edit", in(map[string]any{"path": p, "old_string": "package a", "new_string": "package b"}))),
		assistant(call("w1", "write", in(map[string]any{"path": filepath.Join(dir, "b.go"), "content": "x"}))),
		assistant(call("b1", "bash", in(map[string]any{"command": "ls"}))),
		assistant(call("b2", "bash", in(map[string]any{"command": "touch c.go"}))),
		assistant(call("m1", "mcp__docs__search", `{}`), call("m2", "mcp__db__drop", `{}`)),
		assistant(text("Plan: rename the package.")),
	)
	for id, refused := range map[string]bool{"r1": false, "e1": true, "w1": true, "b1": false, "b2": true, "m1": false, "m2": true} {
		res, _ := resultFor(a.Messages, id)
		got := res.IsError && strings.Contains(res.Content, "Plan mode is on")
		if got != refused {
			t.Errorf("%s: refused=%v (%q)", id, got, res.Content)
		}
	}
	if b, _ := os.ReadFile(p); string(b) != "package a\n" {
		t.Fatalf("file changed in plan mode: %q", b)
	}
	for _, f := range []string{"b.go", "c.go"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Fatalf("%s created in plan mode", f)
		}
	}
	if mcpRuns != 1 || otherRuns != 0 {
		t.Fatalf("mcp runs: hinted %d, other %d", mcpRuns, otherRuns)
	}
}

func TestPlanReminders(t *testing.T) {
	plan := &atomic.Bool{}
	plan.Store(true)
	a := &Agent{Plan: plan}
	count := func(s string) int {
		n := 0
		for _, m := range a.Messages {
			for _, b := range m.Blocks {
				if b.Text == s {
					n++
				}
			}
		}
		return n
	}
	runWith(t, a, assistant(text("plan 1")))
	runWith(t, a, assistant(text("plan 2"))) // every prompt in plan mode is reminded
	plan.Store(false)
	runWith(t, a, assistant(text("doing it")))
	runWith(t, a, assistant(text("more"))) // off is said once
	if count(planOnNote) != 2 || count(planOffNote) != 1 {
		t.Fatalf("on %d, off %d", count(planOnNote), count(planOffNote))
	}
	// Without plan mode ever on, nothing is said.
	b := &Agent{}
	runWith(t, b, assistant(text("x")))
	for _, m := range b.Messages {
		for _, bl := range m.Blocks {
			if strings.Contains(bl.Text, "Plan mode") {
				t.Fatal("plan note without plan mode")
			}
		}
	}
}

func TestPlanModeKeepsTodosOpen(t *testing.T) {
	plan := &atomic.Bool{}
	plan.Store(true)
	a := &Agent{Plan: plan}
	a.Tools = []tools.Tool{&TodoTool{Parent: a}}
	runWith(t, a,
		assistant(call("t1", "todo", `{"todos":[{"content":"Edit a.go","status":"pending"}]}`)),
		assistant(text("Plan: edit a.go.")),
	)
	for _, m := range a.Messages {
		for _, b := range m.Blocks {
			if strings.Contains(b.Text, "todo list") {
				t.Fatalf("todo nudge in plan mode: %q", b.Text)
			}
		}
	}
}
