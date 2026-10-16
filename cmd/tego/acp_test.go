package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/acp"
	"github.com/akbxr/tego/internal/auth"
	"github.com/akbxr/tego/internal/config"
	"github.com/akbxr/tego/internal/llm"
)

// scripted is a model that answers from a script, one reply per request;
// past its end it says "done". Titles get a fixed answer.
type scripted struct {
	mu      sync.Mutex
	replies []func(ctx context.Context) *llm.Message
	prompts []string // each request's last user text that is not a reminder
}

func (s *scripted) Name() string { return "fake" }

func (s *scripted) Stream(ctx context.Context, req *llm.Request, onEvent func(llm.Event)) (*llm.Message, error) {
	if req.Class == llm.ClassAuxiliary {
		return &llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "Test title"}}, StopReason: "end_turn"}, nil
	}
	s.mu.Lock()
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if m := req.Messages[i]; m.Role == llm.RoleUser && len(m.Blocks) > 0 && m.Blocks[0].Type == llm.BlockText &&
			!strings.HasPrefix(m.Blocks[0].Text, "<system-reminder>") {
			s.prompts = append(s.prompts, m.Blocks[0].Text)
			break
		}
	}
	next := func(context.Context) *llm.Message { return say("done") }
	if len(s.replies) > 0 {
		next, s.replies = s.replies[0], s.replies[1:]
	}
	s.mu.Unlock()
	m := next(ctx)
	if ctx.Err() != nil {
		return &llm.Message{Role: llm.RoleAssistant, StopReason: "aborted"}, ctx.Err()
	}
	for _, b := range m.Blocks {
		if b.Type == llm.BlockText {
			onEvent(llm.Event{Type: llm.EvText, Text: b.Text})
		}
	}
	return m, nil
}

func say(text string) *llm.Message {
	return &llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: text}}, StopReason: "end_turn"}
}

func useTool(id, name string, input map[string]any) func(context.Context) *llm.Message {
	in, _ := json.Marshal(input)
	return func(context.Context) *llm.Message {
		return &llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: id, Name: name, Input: in}}, StopReason: "tool_use"}
	}
}

// editor is an ACP client with its own buffers and terminals.
type editor struct {
	t    *testing.T
	conn *acp.Conn

	mu       sync.Mutex
	updates  []map[string]any
	buffers  map[string]string
	commands []string // terminal commands run
	asked    []acp.PermissionRequest
	answer   string // permission option to pick
}

func (e *editor) handle(_ context.Context, method string, params json.RawMessage, _ bool) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch method {
	case "session/update":
		var n struct {
			Update map[string]any `json:"update"`
		}
		_ = json.Unmarshal(params, &n)
		e.updates = append(e.updates, n.Update)
		return nil, nil
	case "session/request_permission":
		var req acp.PermissionRequest
		_ = json.Unmarshal(params, &req)
		e.asked = append(e.asked, req)
		var resp acp.PermissionResponse
		resp.Outcome.Outcome, resp.Outcome.OptionID = "selected", e.answer
		return resp, nil
	case "fs/read_text_file":
		var req acp.ReadTextFileRequest
		_ = json.Unmarshal(params, &req)
		if s, ok := e.buffers[req.Path]; ok {
			return acp.ReadTextFileResponse{Content: s}, nil
		}
		data, err := os.ReadFile(req.Path)
		return acp.ReadTextFileResponse{Content: string(data)}, err
	case "fs/write_text_file":
		var req acp.WriteTextFileRequest
		_ = json.Unmarshal(params, &req)
		e.buffers[req.Path] = req.Content
		return nil, nil
	case "terminal/create":
		var req acp.CreateTerminalRequest
		_ = json.Unmarshal(params, &req)
		e.commands = append(e.commands, strings.Join(req.Args, " "))
		return acp.CreateTerminalResponse{TerminalID: "term-1"}, nil
	case "terminal/wait_for_exit":
		return map[string]any{"exitCode": 0}, nil
	case "terminal/output":
		zero := 0
		return acp.TerminalOutputResponse{Output: "hi\n", ExitStatus: &acp.ExitStatus{ExitCode: &zero}}, nil
	case "terminal/release", "terminal/kill":
		return nil, nil
	}
	return nil, &acp.Error{Code: acp.CodeMethodNotFound, Message: method}
}

func (e *editor) call(method string, params, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return e.conn.Call(ctx, method, params, result)
}

// seen returns the updates so far of kind sessionUpdate.
func (e *editor) seen(kind string) []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []map[string]any
	for _, u := range e.updates {
		if u["sessionUpdate"] == kind {
			out = append(out, u)
		}
	}
	return out
}

// startACP runs tego acp with model and a connected editor.
func startACP(t *testing.T, dir string, model llm.Provider) *editor {
	t.Helper()
	cfg := &config.Config{Dir: dir}
	store, err := auth.Open(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	toAgent, fromEditor := io.Pipe()
	toEditor, fromAgent := io.Pipe()
	srv := newACPServer(cfg, store, fromAgent)
	srv.provider = model
	ed := &editor{t: t, buffers: map[string]string{}, answer: "allow"}
	ed.conn = acp.NewConn(fromEditor, ed.handle)
	done := make(chan struct{})
	go func() {
		_ = srv.conn.Serve(context.Background(), toAgent)
		srv.closeAll()
		fromAgent.Close()
		close(done)
	}()
	go func() { _ = ed.conn.Serve(context.Background(), toEditor) }()
	t.Cleanup(func() {
		fromEditor.Close()
		<-done
	})
	var init acp.InitializeResponse
	if err := ed.call("initialize", acp.InitializeRequest{ProtocolVersion: 1, ClientCapabilities: acp.ClientCapabilities{
		FS: acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true}, Terminal: true}}, &init); err != nil {
		t.Fatal(err)
	}
	if init.ProtocolVersion != 1 || !init.AgentCapabilities.LoadSession {
		t.Fatalf("initialize: %+v", init)
	}
	return ed
}

func TestACPSession(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	a := filepath.Join(project, "a.go")
	if err := os.WriteFile(a, []byte("package a\n\nvar x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	model := &scripted{replies: []func(context.Context) *llm.Message{
		useTool("c1", "read", map[string]any{"path": "a.go"}),
		useTool("c2", "edit", map[string]any{"path": "a.go", "old_string": "x = 2", "new_string": "x = 3"}),
		useTool("c3", "bash", map[string]any{"command": "echo hi"}),
	}}
	ed := startACP(t, home, model)
	ed.buffers[a] = "package a\n\nvar x = 2 // unsaved\n" // open in the editor, not saved

	var setup acp.SessionSetup
	if err := ed.call("session/new", acp.NewSessionRequest{Cwd: project, MCPServers: []acp.MCPServer{}}, &setup); err != nil {
		t.Fatal(err)
	}
	if setup.SessionID == "" || setup.Modes == nil || setup.Modes.CurrentModeID != "yolo" || len(setup.ConfigOptions) != 2 {
		t.Fatalf("session/new: %+v", setup)
	}
	var resp acp.PromptResponse
	if err := ed.call("session/prompt", acp.PromptRequest{SessionID: setup.SessionID, Prompt: []acp.ContentBlock{acp.Text("bump x")}}, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("stop reason %q", resp.StopReason)
	}

	// The edit went to the buffer (where the unsaved text was), not the disk.
	if ed.buffers[a] != "package a\n\nvar x = 3 // unsaved\n" {
		t.Fatalf("buffer %q", ed.buffers[a])
	}
	if disk, _ := os.ReadFile(a); string(disk) != "package a\n\nvar x = 1\n" {
		t.Fatalf("disk %q", disk)
	}
	// Each call was announced, run and ended, with its content.
	calls := map[string]map[string]any{}
	for _, u := range ed.seen("tool_call") {
		calls[u["toolCallId"].(string)] = u
	}
	if len(calls) != 3 || calls["c1"]["kind"] != "read" || calls["c2"]["kind"] != "edit" || calls["c3"]["kind"] != "execute" {
		t.Fatalf("tool calls %v", calls)
	}
	ends := map[string]map[string]any{}
	for _, u := range ed.seen("tool_call_update") {
		if u["status"] == "completed" || u["status"] == "failed" {
			ends[u["toolCallId"].(string)] = u
		}
	}
	diff := ends["c2"]["content"].([]any)[0].(map[string]any)
	if ends["c2"]["status"] != "completed" || diff["type"] != "diff" || diff["oldText"] != "x = 2" || diff["newText"] != "x = 3" || diff["path"] != a {
		t.Fatalf("edit end %v", ends["c2"])
	}
	term := ends["c3"]["content"].([]any)[0].(map[string]any)
	if term["type"] != "terminal" || term["terminalId"] != "term-1" || len(ed.commands) != 1 || !strings.Contains(ed.commands[0], "echo hi") {
		t.Fatalf("bash end %v, commands %v", ends["c3"], ed.commands)
	}
	var text strings.Builder
	for _, u := range ed.seen("agent_message_chunk") {
		text.WriteString(u["content"].(map[string]any)["text"].(string))
	}
	if !strings.Contains(text.String(), "done") {
		t.Fatalf("answer %q", text.String())
	}
	if len(ed.seen("available_commands_update")) != 1 {
		t.Fatal("no commands announced")
	}

	// Ask before changes: the editor is asked, and a rejection stops the edit.
	if err := ed.call("session/set_mode", acp.SetModeRequest{SessionID: setup.SessionID, ModeID: "ask"}, nil); err != nil {
		t.Fatal(err)
	}
	model.mu.Lock()
	model.replies = append(model.replies,
		useTool("c4", "read", map[string]any{"path": "a.go"}),
		useTool("c5", "edit", map[string]any{"path": "a.go", "old_string": "x = 3", "new_string": "x = 4"}))
	model.mu.Unlock()
	ed.answer = "reject"
	if err := ed.call("session/prompt", acp.PromptRequest{SessionID: setup.SessionID, Prompt: []acp.ContentBlock{acp.Text("again")}}, &resp); err != nil {
		t.Fatal(err)
	}
	if len(ed.asked) != 1 || ed.asked[0].ToolCall.ToolCallID != "c5" || len(ed.asked[0].Options) < 2 {
		t.Fatalf("asked %+v", ed.asked)
	}
	if !strings.Contains(ed.buffers[a], "x = 3") {
		t.Fatalf("rejected edit ran: %q", ed.buffers[a])
	}

	// Cancelling a running prompt ends it as cancelled.
	model.mu.Lock()
	model.replies = append(model.replies, func(ctx context.Context) *llm.Message { <-ctx.Done(); return nil })
	model.mu.Unlock()
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = ed.conn.Notify("session/cancel", acp.SessionRef{SessionID: setup.SessionID})
	}()
	if err := ed.call("session/prompt", acp.PromptRequest{SessionID: setup.SessionID, Prompt: []acp.ContentBlock{acp.Text("wait")}}, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != "cancelled" {
		t.Fatalf("after cancel: %q", resp.StopReason)
	}

	// Another editor process loads the session: the conversation replays.
	ed2 := startACP(t, home, &scripted{})
	var list acp.ListSessionsResponse
	if err := ed2.call("session/list", acp.ListSessionsRequest{Cwd: project}, &list); err != nil || len(list.Sessions) != 1 || list.Sessions[0].SessionID != setup.SessionID {
		t.Fatalf("list %+v %v", list, err)
	}
	if err := ed2.call("session/load", acp.LoadSessionRequest{SessionID: setup.SessionID, Cwd: project, MCPServers: []acp.MCPServer{}}, &setup); err != nil {
		t.Fatal(err)
	}
	users := ed2.seen("user_message_chunk")
	if len(users) < 2 || users[0]["content"].(map[string]any)["text"] != "bump x" || len(ed2.seen("tool_call")) != 5 {
		t.Fatalf("replay: %d user chunks, %d tool calls", len(users), len(ed2.seen("tool_call")))
	}
}

// Slash commands: announced, run in the session, and told back.
func TestACPCommands(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", t.TempDir()) // no skills of the user's
	skill := filepath.Join(project, ".agents", "skills", "review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("---\nname: review\ndescription: Review the change\n---\nLook for bugs first.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	model := &scripted{}
	ed := startACP(t, home, model)
	var setup acp.SessionSetup
	if err := ed.call("session/new", acp.NewSessionRequest{Cwd: project, MCPServers: []acp.MCPServer{}}, &setup); err != nil {
		t.Fatal(err)
	}
	prompt := func(text string) string {
		t.Helper()
		ed.mu.Lock()
		ed.updates = nil
		ed.mu.Unlock()
		var resp acp.PromptResponse
		if err := ed.call("session/prompt", acp.PromptRequest{SessionID: setup.SessionID, Prompt: []acp.ContentBlock{acp.Text(text)}}, &resp); err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		var out strings.Builder
		for _, u := range ed.seen("agent_message_chunk") {
			out.WriteString(u["content"].(map[string]any)["text"].(string))
		}
		return out.String()
	}

	time.Sleep(50 * time.Millisecond) // the announcement follows the response
	ann := ed.seen("available_commands_update")
	if len(ann) != 1 {
		t.Fatal("no commands announced")
	}
	names := map[string]bool{}
	for _, c := range ann[0]["availableCommands"].([]any) {
		names[c.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"help", "compact", "plan", "permissions", "model", "thinking", "fast", "session", "usage", "stats", "mcp", "skills", "retry", "skill:review"} {
		if !names[want] {
			t.Errorf("/%s not announced (%v)", want, names)
		}
	}

	if out := prompt("/plan"); !strings.Contains(out, "Plan mode on") {
		t.Fatalf("/plan: %q", out)
	}
	if m := ed.seen("current_mode_update"); len(m) != 1 || m[0]["currentModeId"] != "plan" {
		t.Fatalf("mode updates %v", m)
	}
	if out := prompt("/permissions ask"); !strings.Contains(out, "`ask`") {
		t.Fatalf("/permissions: %q", out)
	}
	if m := ed.seen("current_mode_update"); len(m) != 1 || m[0]["currentModeId"] != "ask" {
		t.Fatalf("mode updates %v", m)
	}
	if out := prompt("/session"); !strings.Contains(out, setup.SessionID) || !strings.Contains(out, "fake/model") {
		t.Fatalf("/session: %q", out)
	}
	if out := prompt("/help"); !strings.Contains(out, "`/skill:review` Review the change") {
		t.Fatalf("/help: %q", out)
	}
	if len(model.prompts) != 0 {
		t.Fatalf("a command reached the model: %q", model.prompts)
	}
	// A skill runs as a prompt that carries it.
	prompt("/skill:review the last commit")
	if len(model.prompts) == 0 || !strings.Contains(model.prompts[0], "Look for bugs first.") || !strings.HasSuffix(model.prompts[0], "the last commit") {
		t.Fatalf("skill prompt %q", model.prompts)
	}
	// Not a command: an ordinary prompt.
	prompt("/usr/local/bin is on PATH?")
	if last := model.prompts[len(model.prompts)-1]; last != "/usr/local/bin is on PATH?" {
		t.Fatalf("path prompt %q", last)
	}
}

// A thread opened and closed without a prompt leaves no session behind.
func TestACPEmptySessionIsRemoved(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	ed := startACP(t, home, &scripted{})
	var setup acp.SessionSetup
	if err := ed.call("session/new", acp.NewSessionRequest{Cwd: project, MCPServers: []acp.MCPServer{}}, &setup); err != nil {
		t.Fatal(err)
	}
	if err := ed.call("session/close", acp.SessionRef{SessionID: setup.SessionID}, nil); err != nil {
		t.Fatal(err)
	}
	var list acp.ListSessionsResponse
	if err := ed.call("session/list", acp.ListSessionsRequest{Cwd: project}, &list); err != nil || len(list.Sessions) != 0 {
		t.Fatalf("list %+v %v", list, err)
	}
}
