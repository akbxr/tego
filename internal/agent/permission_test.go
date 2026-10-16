package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// approver answers from a script by tool call, and records what it was asked.
type approver struct {
	mu     sync.Mutex
	asked  []Approval
	answer func(Approval) Decision
}

func (ap *approver) approve(_ context.Context, req Approval) (Decision, error) {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	ap.asked = append(ap.asked, req)
	return ap.answer(req), nil
}

func (ap *approver) summaries() []string {
	var s []string
	for _, a := range ap.asked {
		s = append(s, a.Summary)
	}
	return s
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

func permAgent(t *testing.T, mode string, ap *approver) (*Agent, string, *int) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644)
	var mcpRuns int
	a := &Agent{Perms: NewPermissions(mode), Tools: append(tools.Builtins(&tools.Env{Cwd: dir}),
		hinted{runsTool{"mcp__docs__search", new(int)}}, runsTool{"mcp__db__drop", &mcpRuns})}
	if ap != nil {
		a.Approve = ap.approve
	}
	return a, dir, &mcpRuns
}

// Which calls each mode asks about: only what may change things, and in
// edits mode not file edits.
func TestPermissionModesAsk(t *testing.T) {
	for _, c := range []struct {
		mode string
		want []string // calls asked about, by id
	}{
		{PermYolo, nil},
		{PermEdits, []string{"b2", "m2"}},
		{PermAsk, []string{"e1", "w1", "b2", "m2"}},
	} {
		t.Run(c.mode, func(t *testing.T) {
			ap := &approver{answer: func(Approval) Decision { return Decision{Allow: true} }}
			a, dir, mcpRuns := permAgent(t, c.mode, ap)
			p := filepath.Join(dir, "a.go")
			runWith(t, a,
				assistant(call("r1", "read", jsonOf(map[string]any{"path": p})), call("g1", "grep", jsonOf(map[string]any{"pattern": "x"}))),
				assistant(call("e1", "edit", jsonOf(map[string]any{"path": p, "old_string": "package a", "new_string": "package b"}))),
				assistant(call("w1", "write", jsonOf(map[string]any{"path": filepath.Join(dir, "b.go"), "content": "package a\n"}))),
				assistant(call("b1", "bash", jsonOf(map[string]any{"command": "git status; ls"}))),
				assistant(call("b2", "bash", jsonOf(map[string]any{"command": "touch c.go"}))),
				assistant(call("m1", "mcp__docs__search", `{}`), call("m2", "mcp__db__drop", `{}`)),
				assistant(text("done")),
			)
			var asked []string
			for _, q := range ap.asked {
				for _, m := range a.Messages {
					for _, b := range m.Blocks {
						if b.Type == llm.BlockToolUse && string(b.Input) == string(q.Input) && b.Name == q.Tool {
							asked = append(asked, b.ID)
						}
					}
				}
			}
			if strings.Join(asked, ",") != strings.Join(c.want, ",") {
				t.Fatalf("asked about %v, want %v (%q)", asked, c.want, ap.summaries())
			}
			// Approved: everything ran.
			if _, err := os.Stat(filepath.Join(dir, "c.go")); err != nil || *mcpRuns != 1 {
				t.Fatalf("approved calls didn't run: %v, mcp %d", err, *mcpRuns)
			}
		})
	}
}

func TestDeclinedCallDoesNotRun(t *testing.T) {
	ap := &approver{answer: func(Approval) Decision {
		return Decision{Feedback: "use the Makefile target instead"}
	}}
	a, dir, _ := permAgent(t, PermAsk, ap)
	p := filepath.Join(dir, "a.go")
	runWith(t, a,
		assistant(call("e1", "edit", jsonOf(map[string]any{"path": p, "old_string": "package a", "new_string": "package b"}))),
		assistant(text("ok")),
	)
	res, _ := resultFor(a.Messages, "e1")
	if !res.IsError || !strings.Contains(res.Content, "declined") || !strings.Contains(res.Content, "use the Makefile target instead") {
		t.Fatalf("result %q", res.Content)
	}
	if b, _ := os.ReadFile(p); string(b) != "package a\n" {
		t.Fatalf("declined edit changed the file: %q", b)
	}
}

// "Don't ask again" covers the same commands (by name and subcommand), not
// a line that also runs something else.
func TestAlwaysAllowRules(t *testing.T) {
	ap := &approver{answer: func(Approval) Decision { return Decision{Allow: true, Always: true} }}
	a, dir, _ := permAgent(t, PermAsk, ap)
	p := filepath.Join(dir, "a.go")
	bash := func(id, cmd string) llm.Block { return call(id, "bash", jsonOf(map[string]any{"command": cmd})) }
	runWith(t, a,
		assistant(bash("b1", "go vet ./... && touch x")),
		assistant(bash("b2", "go vet ./a")),       // covered
		assistant(bash("b3", "touch y")),          // covered: touch
		assistant(bash("b4", "go vet . && rm x")), // rm is not
		assistant(bash("b5", "bash -c 'go vet'")), // never covered
		assistant(call("e1", "edit", jsonOf(map[string]any{"path": p, "old_string": "package a", "new_string": "package b"}))),
		assistant(call("e2", "edit", jsonOf(map[string]any{"path": p, "old_string": "package b", "new_string": "package c"}))), // covered: file edits
		assistant(text("done")),
		assistant(text("done")), // after the reminder to verify the edits
	)
	want := []string{"go vet ./... && touch x", "go vet . && rm x", "bash -c 'go vet'", "a.go"}
	got := ap.summaries()
	if len(got) != len(want) {
		t.Fatalf("asked %q, want %q", got, want)
	}
	for i := range want {
		if !strings.Contains(got[i], want[i]) {
			t.Fatalf("asked %q, want %q", got, want)
		}
	}
	if ap.asked[0].Rule != "`go vet`, `touch` commands" || ap.asked[2].Rule != "" || ap.asked[3].Rule != "file edits" {
		t.Fatalf("rules %q / %q / %q", ap.asked[0].Rule, ap.asked[2].Rule, ap.asked[3].Rule)
	}
}

// Print mode (nobody to ask): the call is refused with a note, not run.
func TestNoApproverRefuses(t *testing.T) {
	a, dir, _ := permAgent(t, PermEdits, nil)
	runWith(t, a,
		assistant(call("b1", "bash", jsonOf(map[string]any{"command": "touch c.go"}))),
		assistant(text("ok")),
	)
	res, _ := resultFor(a.Messages, "b1")
	if !res.IsError || !strings.Contains(res.Content, "nobody can approve") {
		t.Fatalf("result %q", res.Content)
	}
	if _, err := os.Stat(filepath.Join(dir, "c.go")); err == nil {
		t.Fatal("the command ran")
	}
}

// Subagents ask through the parent's approver, named, and share its rules.
func TestSubagentsAskThroughTheParent(t *testing.T) {
	ap := &approver{answer: func(Approval) Decision { return Decision{Allow: true, Always: true} }}
	a, dir, _ := permAgent(t, PermAsk, ap)
	sub := &Agent{Perms: a.Perms, Approve: a.Approve, Name: "explorer", Tools: tools.Builtins(&tools.Env{Cwd: dir}), Subagent: true}
	runWith(t, sub,
		assistant(call("b1", "bash", jsonOf(map[string]any{"command": "touch s.go"}))),
		assistant(text("ok")),
	)
	if len(ap.asked) != 1 || ap.asked[0].Agent != "explorer" {
		t.Fatalf("asked %+v", ap.asked)
	}
	// The rule the subagent's approval made covers the parent too.
	runWith(t, a,
		assistant(call("b2", "bash", jsonOf(map[string]any{"command": "touch p.go"}))),
		assistant(text("ok")),
	)
	if len(ap.asked) != 1 {
		t.Fatalf("parent asked again: %q", ap.summaries())
	}
}

func TestPermissionModeSwitch(t *testing.T) {
	p := NewPermissions("bogus")
	if p.Mode() != PermYolo {
		t.Fatalf("invalid mode starts as %q", p.Mode())
	}
	p.SetMode(PermAsk)
	p.SetMode("nope")
	if p.Mode() != PermAsk {
		t.Fatalf("mode %q", p.Mode())
	}
	var nilPerms *Permissions
	if nilPerms.Mode() != PermYolo {
		t.Fatal("nil permissions must run everything")
	}
}
