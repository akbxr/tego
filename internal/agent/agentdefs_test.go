package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAgents(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	home := filepath.Join(userHome, ".tego")
	repo := filepath.Join(userHome, "code", "repo")
	cwd := filepath.Join(repo, "pkg")

	writeFile(t, filepath.Join(userHome, ".claude", "agents", "reviewer.md"), `---
name: reviewer
description: Reviews code. Use after edits.
tools: Read, Grep, Glob, Bash
model: opus
---
User's reviewer.`)
	// The project's of the same name wins, in a nested folder too.
	writeFile(t, filepath.Join(repo, ".claude", "agents", "review", "reviewer.md"), `---
name: reviewer
description: >
  Reviews this repo's code
  for its conventions.
tools:
  - Read
  - "Grep"
disallowedTools: [Bash]
model: inherit
effort: high
maxTurns: 20
isolation: worktree
---

Check the conventions in CONTRIBUTING.md.
`)
	writeFile(t, filepath.Join(home, "agents", "scout.md"), "---\ndescription: Finds files.\neffort: enormous\n---\nFind.")
	writeFile(t, filepath.Join(cwd, ".tego", "agents", "nodesc.md"), "---\nname: nodesc\n---\nNo description: skipped.")
	writeFile(t, filepath.Join(cwd, ".tego", "agents", "bad.md"), "---\nname: bad:name\ndescription: x\n---\n")
	writeFile(t, filepath.Join(cwd, ".tego", "agents", "notes.txt"), "not an agent")

	defs := LoadAgents(home, cwd)
	var names []string
	for _, d := range defs {
		names = append(names, d.Name)
	}
	if !slices.Equal(names, []string{"reviewer", "scout"}) {
		t.Fatalf("agents %v", names)
	}
	r, s := defs[0], defs[1]
	if r.Description != "Reviews this repo's code for its conventions." || r.Prompt != "Check the conventions in CONTRIBUTING.md." ||
		!slices.Equal(r.Tools, []string{"Read", "Grep"}) || !slices.Equal(r.Disallowed, []string{"Bash"}) ||
		r.Model != "inherit" || r.Thinking != "high" || r.MaxTurns != 20 || !r.Worktree {
		t.Fatalf("project reviewer %+v", r)
	}
	if s.Description != "Finds files." || s.Thinking != "" || s.Tools != nil {
		t.Fatalf("scout (named from its file) %+v", s)
	}
}

func TestAgentToolFilter(t *testing.T) {
	names := func(d AgentDef) []string {
		var out []string
		for _, n := range []string{"read", "write", "edit", "bash", "bash_output", "grep", "glob", "mcp__github__search", "mcp__github__create_pr", "mcp__db__query"} {
			if d.allowsTool(n) {
				out = append(out, n)
			}
		}
		return out
	}
	for _, c := range []struct {
		def  AgentDef
		want string
	}{
		{AgentDef{}, "read write edit bash bash_output grep glob mcp__github__search mcp__github__create_pr mcp__db__query"},
		{AgentDef{Tools: []string{}}, ""},
		{AgentDef{Tools: []string{"Read", "Grep", "Glob", "Bash(go test:*)"}}, "read bash bash_output grep glob"},
		{AgentDef{Tools: []string{"read", "MultiEdit", "mcp__github"}}, "read edit mcp__github__search mcp__github__create_pr"},
		{AgentDef{Disallowed: []string{"Write", "Edit", "mcp__*"}}, "read bash bash_output grep glob"},
		{AgentDef{Tools: []string{"mcp__github__*", "Read"}, Disallowed: []string{"mcp__github__create_pr"}}, "read mcp__github__search"},
		{AgentDef{Tools: []string{"WebFetch", "TodoWrite"}}, ""}, // tools tego's subagents don't have
	} {
		if got := strings.Join(names(c.def), " "); got != c.want {
			t.Errorf("tools %v disallowed %v: got %q, want %q", c.def.Tools, c.def.Disallowed, got, c.want)
		}
	}
}

// A task in a worktree works there, not in the session's folder, and its
// report says what became of the worktree.
func TestTaskInWorktree(t *testing.T) {
	main, wt := t.TempDir(), t.TempDir()
	write := func() *llm.Message {
		return &llm.Message{Role: llm.RoleAssistant, StopReason: "tool_use", Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "w1", Name: "write",
			Input: llm.ToolInput([]byte(`{"path":"made.txt","content":"x"}`))}}}
	}
	done := &llm.Message{Role: llm.RoleAssistant, StopReason: "end_turn", Blocks: []llm.Block{text("wrote it")}}
	prov := &scripted{replies: []*llm.Message{write(), done}}
	parent := &Agent{Provider: prov, Model: "m", ContextWindow: 100_000, UI: nopUI{}}
	var isolated []string
	finished := 0
	task := &TaskTool{Parent: parent, System: "BASE", Env: &tools.Env{Cwd: main},
		Isolate: func(name string) (string, string, func() string, error) {
			isolated = append(isolated, name)
			return wt, "WORKTREE NOTE", func() string { finished++; return "kept on branch tego/x" }, nil
		}}
	if !strings.Contains(string(task.Def().Schema), `"isolation":{"type":"string","enum":["worktree"]`) {
		t.Fatal("schema lacks isolation")
	}
	in, _ := json.Marshal(map[string]any{"tasks": []map[string]string{{"name": "Fix", "prompt": "write it", "isolation": "worktree"}}})
	out, err := task.Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, "made.txt")); err != nil {
		t.Error("the file isn't in the worktree")
	}
	if _, err := os.Stat(filepath.Join(main, "made.txt")); err == nil {
		t.Error("the file went into the session's folder")
	}
	if !strings.HasSuffix(prov.reqs[0].System, "\n\n# Worktree\nWORKTREE NOTE") || finished != 1 || !slices.Equal(isolated, []string{"Fix"}) {
		t.Errorf("system %q, finished %d, isolated %v", prov.reqs[0].System, finished, isolated)
	}
	if !strings.Contains(out, "wrote it\n\n[worktree: kept on branch tego/x]") {
		t.Errorf("report %s", out)
	}

	// An agent with isolation: worktree gets one without asking; an agent
	// limited to read gets only that, in the worktree too.
	prov.replies = []*llm.Message{done}
	task.Agents = func() []AgentDef {
		return []AgentDef{{Name: "fixer", Description: "Fixes.", Worktree: true, Tools: []string{"Read"}}}
	}
	in, _ = json.Marshal(map[string]any{"tasks": []map[string]string{{"name": "F", "prompt": "fix", "agent": "fixer"}}})
	if _, err := task.Run(context.Background(), in); err != nil || finished != 2 {
		t.Fatalf("err %v, finished %d", err, finished)
	}
	if got := toolNames(*prov.reqs[len(prov.reqs)-1]); got != "read" {
		t.Errorf("fixer's tools %q", got)
	}

	// No worktree can be made: the task fails rather than share the folder.
	task.Isolate = func(string) (string, string, func() string, error) { return "", "", nil, os.ErrPermission }
	out, _ = task.Run(context.Background(), in)
	if !strings.Contains(out, "## F (failed: no worktree for it: permission denied)") {
		t.Errorf("report %s", out)
	}
	task.Isolate = nil
	if strings.Contains(string(task.Def().Schema), "isolation") {
		t.Error("schema offers isolation without worktrees")
	}
	out, _ = task.Run(context.Background(), in)
	if !strings.Contains(out, "## F (failed: it can't have a worktree here") {
		t.Errorf("report %s", out)
	}
}

// requestRecorder records the requests it gets.
type requestRecorder struct {
	mu   sync.Mutex
	reqs []llm.Request
}

func (r *requestRecorder) Name() string { return "rec" }
func (r *requestRecorder) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, *req)
	r.mu.Unlock()
	return &llm.Message{Role: llm.RoleAssistant, StopReason: "end_turn", Blocks: []llm.Block{text("report")}}, nil
}

func (r *requestRecorder) find(t *testing.T, prompt string) llm.Request {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, q := range r.reqs {
		for _, m := range q.Messages {
			for _, b := range m.Blocks {
				if b.Type == llm.BlockText && strings.Contains(b.Text, prompt) {
					return q
				}
			}
		}
	}
	t.Fatalf("no request with %q", prompt)
	return llm.Request{}
}

func toolNames(req llm.Request) string {
	var out []string
	for _, tl := range req.Tools {
		out = append(out, tl.Name)
	}
	return strings.Join(out, " ")
}

func TestTaskRunsDefinedAgents(t *testing.T) {
	mainP, otherP := &requestRecorder{}, &requestRecorder{}
	parent := &Agent{Provider: mainP, ProviderName: "anthropic", Model: "claude-sonnet-5-5", Thinking: "medium", ContextWindow: 1_000_000, UI: nopUI{}}
	defs := []AgentDef{
		{Name: "reviewer", Description: "Reviews code.", Prompt: "Be strict.", Tools: []string{"Read", "Grep"}, Model: "opus", Thinking: "high"},
		{Name: "scout", Description: strings.Repeat("long ", 100), Model: "inherit"},
	}
	var asked []string
	task := &TaskTool{Parent: parent, System: "BASE", Env: &tools.Env{Cwd: t.TempDir()},
		Agents: func() []AgentDef { return defs },
		ResolveModel: func(spec string) (SmallModel, error) {
			asked = append(asked, spec)
			return SmallModel{Provider: otherP, ProviderName: "anthropic", Model: "claude-opus-5-5", Window: 1_000_000}, nil
		}}

	schema := string(task.Def().Schema)
	if !strings.Contains(schema, `"agent":{"type":"string","enum":["reviewer","scout"]`) || !strings.Contains(schema, `- reviewer: Reviews code.`) ||
		strings.Contains(schema, strings.Repeat("long ", 70)) {
		t.Fatalf("schema: %s", schema)
	}

	in, _ := json.Marshal(map[string]any{"tasks": []map[string]string{
		{"name": "Review", "prompt": "review A", "agent": "reviewer"},
		{"name": "ReviewHere", "prompt": "review B", "agent": "reviewer", "model": "main"},
		{"name": "Scout", "prompt": "scout C", "agent": "scout"},
		{"name": "Plain", "prompt": "plain D"},
	}})
	out, err := task.Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked, []string{"opus"}) {
		t.Errorf("resolved %v", asked)
	}
	a := otherP.find(t, "review A")
	if a.Model != "claude-opus-5-5" || a.Thinking != "high" || a.System != "BASE\n\n# Your role: reviewer\nBe strict." || toolNames(a) != "read grep" {
		t.Errorf("reviewer ran on %s thinking %s tools %q system %q", a.Model, a.Thinking, toolNames(a), a.System)
	}
	// "model": "main" keeps it on the session's model, still as the agent.
	if b := mainP.find(t, "review B"); b.Model != "claude-sonnet-5-5" || toolNames(b) != "read grep" || b.Thinking != "high" {
		t.Errorf("review on main: %s %q %s", b.Model, toolNames(b), b.Thinking)
	}
	if c := mainP.find(t, "scout C"); c.Model != "claude-sonnet-5-5" || c.System != "BASE" || !strings.Contains(toolNames(c), "write") {
		t.Errorf("scout: %s %q %q", c.Model, c.System, toolNames(c))
	}
	if d := mainP.find(t, "plain D"); d.System != "BASE" || d.Thinking != "medium" {
		t.Errorf("plain: %q %s", d.System, d.Thinking)
	}
	subs := parent.Subagents()
	if subs[0].Agent != "reviewer" || subs[0].Model != "claude-opus-5-5" || subs[3].Agent != "" {
		t.Errorf("statuses %+v / %+v", subs[0], subs[3])
	}
	if !strings.Contains(out, "## Review (done)") {
		t.Errorf("report %s", out)
	}

	// An agent that isn't defined fails the call before anything runs.
	in, _ = json.Marshal(map[string]any{"tasks": []map[string]string{{"name": "X", "prompt": "x", "agent": "nobody"}}})
	if _, err := task.Run(context.Background(), in); err == nil || !strings.Contains(err.Error(), `no agent "nobody"; defined: reviewer, scout`) {
		t.Errorf("err %v", err)
	}
	// No agents: the schema has no agent property.
	task.Agents = nil
	if strings.Contains(string(task.Def().Schema), `"agent"`) {
		t.Error("schema offers agents when none are defined")
	}
}
