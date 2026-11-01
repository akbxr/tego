package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/acp"
	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/llm"
)

// What tego acp shares with the terminal: /undo, background tasks, the
// prompt cache keep-alive and extensions.

// newACPSession opens a session in project.
func newACPSession(t *testing.T, ed *editor, project string) string {
	t.Helper()
	var setup acp.SessionSetup
	if err := ed.call("session/new", acp.NewSessionRequest{Cwd: project, MCPServers: []acp.MCPServer{}}, &setup); err != nil {
		t.Fatal(err)
	}
	return setup.SessionID
}

// prompt sends text and returns the agent's message chunks for it.
func (e *editor) prompt(sid, text string) string {
	e.t.Helper()
	e.mu.Lock()
	e.updates = nil
	e.mu.Unlock()
	var resp acp.PromptResponse
	if err := e.call("session/prompt", acp.PromptRequest{SessionID: sid, Prompt: []acp.ContentBlock{acp.Text(text)}}, &resp); err != nil {
		e.t.Fatalf("%s: %v", text, err)
	}
	return e.text("agent_message_chunk")
}

// text joins the text of the updates of kind so far.
func (e *editor) text(kind string) string {
	var out strings.Builder
	for _, u := range e.seen(kind) {
		if c, ok := u["content"].(map[string]any); ok {
			out.WriteString(fmt.Sprint(c["text"]))
		}
	}
	return out.String()
}

// waitFor polls cond for up to d.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestACPUndo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", t.TempDir())
	a := filepath.Join(project, "a.txt")
	if err := os.WriteFile(a, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	model := &scripted{replies: []func(context.Context) *llm.Message{
		func(context.Context) *llm.Message { // a change on disk, as a command would make
			_ = os.WriteFile(a, []byte("two\n"), 0o644)
			_ = os.WriteFile(filepath.Join(project, "new.txt"), []byte("new\n"), 0o644)
			return say("changed")
		},
	}}
	ed := startACP(t, home, model)
	sid := newACPSession(t, ed, project)
	if out := ed.prompt(sid, "/undo"); !strings.Contains(out, "Nothing to undo yet") {
		t.Fatalf("/undo before a prompt: %q", out)
	}
	ed.prompt(sid, "change a")
	if out := ed.prompt(sid, "/undo list"); !strings.Contains(out, "1. before: change a") {
		t.Fatalf("/undo list: %q", out)
	}
	if out := ed.prompt(sid, "/undo 2"); !strings.Contains(out, "/undo N` (1 to 1)") {
		t.Fatalf("/undo 2: %q", out)
	}
	out := ed.prompt(sid, "/undo")
	if !strings.Contains(out, `before the prompt "change a"`) || !strings.Contains(out, "a.txt") {
		t.Fatalf("/undo: %q", out)
	}
	if data, _ := os.ReadFile(a); string(data) != "one\n" {
		t.Fatalf("a.txt after /undo: %q", data)
	}
	if _, err := os.Stat(filepath.Join(project, "new.txt")); err == nil {
		t.Fatal("new.txt survived /undo")
	}
	// /undo undoes itself.
	ed.prompt(sid, "/undo")
	if data, _ := os.ReadFile(a); string(data) != "two\n" {
		t.Fatalf("a.txt after the second /undo: %q", data)
	}
	if len(model.prompts) != 1 {
		t.Fatalf("model prompts %q", model.prompts)
	}
}

// acpBG plays the agent and subagent of a background task. The subagent
// digs for a while (or until stopped, with hang); the main agent waits
// wait after starting it, so that it may end during the prompt.
type acpBG struct {
	mu        sync.Mutex
	hang      bool
	wait      time.Duration
	requests  []string // the last user text of each main-agent request
	cancelled bool     // the subagent was stopped
}

func (p *acpBG) Name() string { return "fake" }

func (p *acpBG) Stream(ctx context.Context, req *llm.Request, onEvent func(llm.Event)) (*llm.Message, error) {
	if req.Class == llm.ClassAuxiliary {
		return say("Test title"), nil
	}
	reply := func(s string) (*llm.Message, error) {
		onEvent(llm.Event{Type: llm.EvText, Text: s})
		return say(s), nil
	}
	if req.Messages[0].Blocks[0].Text == "dig" { // the subagent
		d := 100 * time.Millisecond
		if p.hang {
			d = time.Hour
		}
		select {
		case <-time.After(d):
		case <-ctx.Done():
			p.mu.Lock()
			p.cancelled = true
			p.mu.Unlock()
			return nil, ctx.Err()
		}
		return reply("found the bug in parse.go")
	}
	// What came since the agent last spoke: prompt, reminders, tool results.
	var lastText string
	toolResult := false
	for i := len(req.Messages) - 1; i >= 0 && req.Messages[i].Role != llm.RoleAssistant; i-- {
		for _, b := range req.Messages[i].Blocks {
			lastText += b.Text + b.Content
			toolResult = toolResult || b.Type == llm.BlockToolResult
		}
	}
	p.mu.Lock()
	p.requests = append(p.requests, lastText)
	p.mu.Unlock()
	switch {
	case strings.Contains(lastText, "Background tasks you started ended"):
		return reply("the subagent found the bug")
	case toolResult:
		time.Sleep(p.wait)
		return reply("started it")
	case !strings.Contains(lastText, "find the bug"):
		return reply("ok")
	}
	in, _ := json.Marshal(map[string]any{"background": true, "tasks": []map[string]string{{"name": "Digger", "prompt": "dig"}}})
	return &llm.Message{Role: llm.RoleAssistant, StopReason: "tool_use",
		Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "c1", Name: "task", Input: in}}}, nil
}

func TestACPSessionSetup(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	ed := startACP(t, home, &scripted{})
	sid := newACPSession(t, ed, project)
	sess := ed.srv.session(sid)
	if sess.ag.KeepAlive <= 0 || sess.kaStopped == nil {
		t.Fatalf("keep-alive off: %v", sess.ag.KeepAlive)
	}
	var names []string
	var task *agent.TaskTool
	for _, tl := range sess.ag.Tools {
		names = append(names, tl.Def().Name)
		if tt, ok := tl.(*agent.TaskTool); ok {
			task = tt
		}
	}
	if !strings.Contains(strings.Join(names, " "), "task_status") || task == nil || task.Background == nil {
		t.Fatalf("background tasks off: %v", names)
	}
	if sess.app.undo == nil {
		t.Fatal("no /undo snapshots")
	}
	if err := ed.call("session/close", acp.SessionRef{SessionID: sid}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sess.kaStopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the keep-alive outlived the session")
	}
}

// A background task that ends while the session is idle: a notice, and
// its report goes with the next prompt.
func TestACPBackgroundIdle(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	model := &acpBG{}
	ed := startACP(t, home, model)
	sid := newACPSession(t, ed, project)
	if out := ed.prompt(sid, "find the bug"); out != "started it" {
		t.Fatalf("first prompt: %q", out)
	}
	waitFor(t, 5*time.Second, "the task's notice", func() bool {
		return strings.Contains(ed.text("agent_thought_chunk"), "[Digger] t1 done in the background")
	})
	if th := ed.text("agent_thought_chunk"); !strings.Contains(th, "with your next prompt") {
		t.Fatalf("notice %q", th)
	}
	ed.prompt(sid, "thanks")
	model.mu.Lock()
	last := model.requests[len(model.requests)-1]
	model.mu.Unlock()
	if !strings.Contains(last, "found the bug in parse.go") {
		t.Fatalf("the report did not go with the next prompt: %q", last)
	}
	if out := ed.prompt(sid, "/tasks"); !strings.Contains(out, "✓ Digger ⇢t1") {
		t.Fatalf("/tasks: %q", out)
	}
}

// A background task that ends while the prompt runs: its report is
// answered before the prompt's response.
func TestACPBackgroundBusy(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	ed := startACP(t, home, &acpBG{wait: 400 * time.Millisecond})
	sid := newACPSession(t, ed, project)
	if out := ed.prompt(sid, "find the bug"); out != "started itthe subagent found the bug" {
		t.Fatalf("prompt: %q", out)
	}
}

// Closing the session stops its background tasks.
func TestACPBackgroundStops(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	model := &acpBG{hang: true}
	ed := startACP(t, home, model)
	sid := newACPSession(t, ed, project)
	ed.prompt(sid, "find the bug")
	if out := ed.prompt(sid, "/tasks"); !strings.Contains(out, "Digger ⇢t1") {
		t.Fatalf("/tasks: %q", out)
	}
	if err := ed.call("session/close", acp.SessionRef{SessionID: sid}, nil); err != nil {
		t.Fatal(err)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if !model.cancelled {
		t.Fatal("the background task outlived the session")
	}
}

const helloExt = `import json, sys
LOG = %q

def send(m):
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", **m}) + "\n")
    sys.stdout.flush()

def log(s):
    with open(LOG, "a") as f:
        f.write(s + "\n")

def ask(method, params):
    send({"id": 900, "method": method, "params": params})
    for l in sys.stdin:
        r = json.loads(l)
        if r.get("id") == 900 and "method" not in r:
            return r.get("result", {})

for line in sys.stdin:
    m = json.loads(line)
    meth = m.get("method")
    if meth == "initialize":
        send({"id": m["id"], "result": {"name": "hello", "events": ["input", "turn_end", "session_end"],
            "commands": [{"name": "hello", "description": "Say hello"}, {"name": "ask", "description": "Ask"}]}})
    elif meth == "command/execute":
        p = m["params"]
        if p["name"] == "hello":
            send({"method": "ui/notify", "params": {"message": "hello " + p["args"]}})
            send({"method": "session/sendMessage", "params": {"text": "say hi to " + p["args"]}})
        else:
            log("confirm:" + json.dumps(ask("ui/confirm", {"title": "Sure?", "message": "really"})))
            log("input:" + json.dumps(ask("ui/input", {"title": "Name?"})))
        send({"id": m["id"], "result": {}})
    elif meth == "event":
        ev, d = m["params"]["event"], m["params"]["data"]
        if ev == "input":
            if d["text"] == "ping":
                send({"id": m["id"], "result": {"handled": True}})
            else:
                send({"id": m["id"], "result": {"text": d["text"] + " (from hello)"}})
        else:
            log(ev + ":" + d.get("text", ""))
    elif meth == "shutdown":
        break
`

func TestACPExtensions(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("needs python3")
	}
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", t.TempDir())
	logFile := filepath.Join(t.TempDir(), "hello.log")
	if err := os.MkdirAll(filepath.Join(home, "extensions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "extensions", "hello.py"), []byte(fmt.Sprintf(helloExt, logFile)), 0o644); err != nil {
		t.Fatal(err)
	}
	logged := func() string { data, _ := os.ReadFile(logFile); return string(data) }
	model := &scripted{}
	ed := startACP(t, home, model)
	sid := newACPSession(t, ed, project)
	waitFor(t, 20*time.Second, "the extension's commands", func() bool {
		for _, u := range ed.seen("available_commands_update") {
			for _, c := range u["availableCommands"].([]any) {
				if c.(map[string]any)["name"] == "hello" {
					return true
				}
			}
		}
		return false
	})

	// A command: its notice shows, and the prompt it sends runs right after,
	// through the input hook.
	ed.prompt(sid, "/hello world")
	if th := ed.text("agent_thought_chunk"); !strings.Contains(th, "[hello] hello world") {
		t.Fatalf("notice %q", th)
	}
	if u := ed.text("user_message_chunk"); u != "say hi to world" {
		t.Fatalf("sent message shown as %q", u)
	}
	if len(model.prompts) != 1 || model.prompts[0] != "say hi to world (from hello)" {
		t.Fatalf("model prompts %q", model.prompts)
	}
	waitFor(t, 5*time.Second, "turn_end", func() bool { return strings.Contains(logged(), "turn_end:done") })

	// The input hook takes a prompt.
	ed.prompt(sid, "ping")
	if len(model.prompts) != 1 {
		t.Fatalf("a handled prompt reached the model: %q", model.prompts)
	}

	// Questions: confirm is a permission request, input can't be asked.
	ed.mu.Lock()
	ed.answer = "yes"
	ed.mu.Unlock()
	ed.prompt(sid, "/ask")
	if l := logged(); !strings.Contains(l, `confirm:{"ok": true}`) || !strings.Contains(l, `"cancelled": true`) {
		t.Fatalf("log %q", l)
	}
	ed.mu.Lock()
	asked := ed.asked[len(ed.asked)-1]
	ed.mu.Unlock()
	if asked.ToolCall.Title != "[hello] Sure?" {
		t.Fatalf("asked %+v", asked)
	}

	if out := ed.prompt(sid, "/extensions"); !strings.Contains(out, "hello") || !strings.Contains(out, "running") {
		t.Fatalf("/extensions: %q", out)
	}
	if err := ed.call("session/close", acp.SessionRef{SessionID: sid}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "session_end", func() bool { return strings.Contains(logged(), "session_end:") })
}
