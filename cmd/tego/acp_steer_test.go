package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/acp"
	"github.com/akbxr/tego/internal/llm"
)

// Prompts sent while one runs steer it (acp_steer.go).

type promptResult struct {
	resp acp.PromptResponse
	err  error
	at   time.Time
}

// promptAsync sends a prompt and returns where its answer will come.
func (e *editor) promptAsync(sid, text string) <-chan promptResult {
	ch := make(chan promptResult, 1)
	go func() {
		var r promptResult
		r.err = e.call("session/prompt", acp.PromptRequest{SessionID: sid, Prompt: []acp.ContentBlock{acp.Text(text)}}, &r.resp)
		r.at = time.Now()
		ch <- r
	}()
	return ch
}

func awaitAnswer(t *testing.T, ch <-chan promptResult, what string) promptResult {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("%s: %v", what, r.err)
		}
		return r
	case <-time.After(20 * time.Second):
		t.Fatalf("%s: no answer", what)
	}
	return promptResult{}
}

// steersQueued reports how many steering messages wait in the session.
func steersQueued(sess *acpSession) int {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return len(sess.steers)
}

// blockUntil is a model reply that signals it started, then waits for
// release before giving reply.
func blockUntil(started chan<- struct{}, release <-chan struct{}, reply func(context.Context) *llm.Message) func(context.Context) *llm.Message {
	return func(ctx context.Context) *llm.Message {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil
		}
		return reply(ctx)
	}
}

// userTexts lists the text of the user messages in the history that are not
// reminders, tool results or the prompts' environment notes.
func userTexts(sess *acpSession) []string {
	var out []string
	for _, m := range sess.ag.Messages {
		if m.Role == llm.RoleUser && len(m.Blocks) > 0 && m.Blocks[0].Type == llm.BlockText && !strings.HasPrefix(m.Blocks[0].Text, "<system-reminder>") {
			out = append(out, m.Blocks[0].Text)
		}
	}
	return out
}

// Two prompts sent during a tool round go in, in order, right after its
// results and before the next request; every request is answered when the
// turn ends.
func TestACPSteerDuringTurn(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	started, release := make(chan struct{}), make(chan struct{})
	model := &scripted{replies: []func(context.Context) *llm.Message{
		blockUntil(started, release, useTool("c1", "glob", map[string]any{"pattern": "*.go"})),
		func(context.Context) *llm.Message { return say("switched to b") },
	}}
	ed := startACP(t, home, model)
	sid := newACPSession(t, ed, project)
	sess := ed.srv.session(sid)

	first := ed.promptAsync(sid, "look at a")
	<-started
	steer1 := ed.promptAsync(sid, "use b instead")
	waitFor(t, 5*time.Second, "the first steer", func() bool { return steersQueued(sess) == 1 })
	steer2 := ed.promptAsync(sid, "and keep it short")
	waitFor(t, 5*time.Second, "the second steer", func() bool { return steersQueued(sess) == 2 })
	close(release)

	r0, r1, r2 := awaitAnswer(t, first, "prompt"), awaitAnswer(t, steer1, "steer 1"), awaitAnswer(t, steer2, "steer 2")
	for i, r := range []promptResult{r0, r1, r2} {
		if r.resp.StopReason != "end_turn" {
			t.Fatalf("answer %d: %q", i, r.resp.StopReason)
		}
	}
	model.mu.Lock()
	prompts := append([]string(nil), model.prompts...)
	model.mu.Unlock()
	if len(prompts) != 2 || prompts[1] != "and keep it short" {
		t.Fatalf("requests' last prompts %q", prompts)
	}
	if got := userTexts(sess); strings.Join(got, "|") != "look at a|use b instead|and keep it short" {
		t.Fatalf("user messages %q", got)
	}
	// The steering went in after the tool round: tool result, then steers.
	msgs := sess.ag.Messages
	n := len(msgs)
	if msgs[n-4].Blocks[0].Type != llm.BlockToolResult || msgs[n-3].Blocks[0].Text != "use b instead" || msgs[n-2].Blocks[0].Text != "and keep it short" {
		t.Fatalf("history tail %+v", msgs[n-4:])
	}
	// Not echoed: the editor shows what it sends.
	if u := ed.text("user_message_chunk"); u != "" {
		t.Fatalf("echoed %q", u)
	}
}

// bgWait plays an agent that starts a background subagent that never ends,
// waits for it with task_status, and answers what comes after.
type bgWait struct {
	mu       sync.Mutex
	requests []string // the last user text of each main-agent request
}

func (p *bgWait) Name() string { return "fake" }

func (p *bgWait) Stream(ctx context.Context, req *llm.Request, onEvent func(llm.Event)) (*llm.Message, error) {
	if req.Class == llm.ClassAuxiliary {
		return say("Test title"), nil
	}
	if req.Messages[0].Blocks[0].Text == "dig" { // the subagent
		<-ctx.Done()
		return nil, ctx.Err()
	}
	var last string
	toolResult := ""
	for i := len(req.Messages) - 1; i >= 0 && req.Messages[i].Role != llm.RoleAssistant; i-- {
		for _, b := range req.Messages[i].Blocks {
			if b.Type == llm.BlockToolResult {
				toolResult = b.Content
			} else if last == "" {
				last = b.Text
			}
		}
	}
	p.mu.Lock()
	p.requests = append(p.requests, last)
	n := len(p.requests)
	p.mu.Unlock()
	call := func(name string, input any) (*llm.Message, error) {
		in, _ := json.Marshal(input)
		return &llm.Message{Role: llm.RoleAssistant, StopReason: "tool_use",
			Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "c" + name, Name: name, Input: in}}}, nil
	}
	switch n {
	case 1:
		return call("task", map[string]any{"background": true, "tasks": []map[string]string{{"name": "Digger", "prompt": "dig"}}})
	case 2:
		return call("task_status", map[string]any{"ids": []string{"t1"}, "wait": 600})
	}
	onEvent(llm.Event{Type: llm.EvText, Text: "after the wait: " + toolResult})
	return say("after the wait: " + toolResult), nil
}

// A steering prompt ends a task_status wait: the model gets it at once,
// not after the wait.
func TestACPSteerEndsAWait(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	model := &bgWait{}
	ed := startACP(t, home, model)
	sid := newACPSession(t, ed, project)

	first := ed.promptAsync(sid, "find the bug")
	waitFor(t, 10*time.Second, "the wait", func() bool {
		for _, u := range ed.seen("tool_call_update") {
			if u["toolCallId"] == "ctask_status" && u["status"] == "in_progress" {
				return true
			}
		}
		return false
	})
	start := time.Now()
	steer := ed.promptAsync(sid, "stop waiting, look at parse.go")
	r0, r1 := awaitAnswer(t, first, "prompt"), awaitAnswer(t, steer, "steer")
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the wait ran on for %v", took)
	}
	if r0.resp.StopReason != "end_turn" || r1.resp.StopReason != "end_turn" {
		t.Fatalf("answers %q %q", r0.resp.StopReason, r1.resp.StopReason)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.requests) != 3 || model.requests[2] != "stop waiting, look at parse.go" {
		t.Fatalf("requests %q", model.requests)
	}
	if out := ed.text("agent_message_chunk"); !strings.Contains(out, "Stopped waiting: the user sent a message") {
		t.Fatalf("answer %q", out)
	}
}

// A prompt sent just after the agent last looked for steering, as the
// turn ends, runs as the next turn before the prompts are answered.
func TestACPSteerAtTurnEnd(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	model := &scripted{replies: []func(context.Context) *llm.Message{
		func(context.Context) *llm.Message { return say("first answer") },
		func(context.Context) *llm.Message { return say("late answer") },
	}}
	ed := startACP(t, home, model)
	sid := newACPSession(t, ed, project)
	sess := ed.srv.session(sid)

	var late <-chan promptResult
	take := sess.ag.Steer
	sess.ag.Steer = func() [][]llm.Block {
		got := take()
		if late == nil { // the agent saw none; one arrives right after
			late = ed.promptAsync(sid, "one more thing")
			waitFor(t, 5*time.Second, "the late steer", func() bool { return steersQueued(sess) == 1 })
		}
		return got
	}
	r0 := awaitAnswer(t, ed.promptAsync(sid, "hello"), "prompt")
	r1 := awaitAnswer(t, late, "late steer")
	if r0.resp.StopReason != "end_turn" || r1.resp.StopReason != "end_turn" {
		t.Fatalf("answers %q %q", r0.resp.StopReason, r1.resp.StopReason)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if strings.Join(model.prompts, "|") != "hello|one more thing" {
		t.Fatalf("requests %q", model.prompts)
	}
	if out := ed.text("agent_message_chunk"); out != "first answerlate answer" {
		t.Fatalf("answer %q", out)
	}
}

// session/cancel stops the turn and its steering; a prompt sent right after
// the cancel runs as a turn of its own once the cancelled one ended.
func TestACPSteerCancel(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	started := make(chan struct{})
	model := &scripted{replies: []func(context.Context) *llm.Message{
		func(ctx context.Context) *llm.Message { close(started); <-ctx.Done(); return nil },
	}}
	ed := startACP(t, home, model)
	sid := newACPSession(t, ed, project)
	sess := ed.srv.session(sid)

	first := ed.promptAsync(sid, "long job")
	<-started
	steer := ed.promptAsync(sid, "never mind")
	waitFor(t, 5*time.Second, "the steer", func() bool { return steersQueued(sess) == 1 })
	if err := ed.conn.Notify("session/cancel", acp.SessionRef{SessionID: sid}); err != nil {
		t.Fatal(err)
	}
	next := ed.promptAsync(sid, "do this instead") // as Zed's "send now" does
	r0, r1, r2 := awaitAnswer(t, first, "prompt"), awaitAnswer(t, steer, "steer"), awaitAnswer(t, next, "next")
	if r0.resp.StopReason != "cancelled" || r1.resp.StopReason != "cancelled" || r2.resp.StopReason != "end_turn" {
		t.Fatalf("answers %q %q %q", r0.resp.StopReason, r1.resp.StopReason, r2.resp.StopReason)
	}
	if r2.at.Before(r0.at) {
		t.Fatal("the next prompt ended before the cancelled one")
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if strings.Join(model.prompts, "|") != "long job|do this instead" {
		t.Fatalf("requests %q", model.prompts)
	}
	if got := userTexts(sess); slices.Contains(got, "never mind") {
		t.Fatalf("the cancelled steer went in: %q", got)
	}
}

// A command sent during a turn does not steer it: it runs after it.
func TestACPSteerCommandWaits(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	started, release := make(chan struct{}), make(chan struct{})
	model := &scripted{replies: []func(context.Context) *llm.Message{
		blockUntil(started, release, func(context.Context) *llm.Message { return say("done") }),
	}}
	ed := startACP(t, home, model)
	sid := newACPSession(t, ed, project)
	sess := ed.srv.session(sid)

	first := ed.promptAsync(sid, "work")
	<-started
	cmd := ed.promptAsync(sid, "/session")
	waitFor(t, 5*time.Second, "the command to wait", func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return len(sess.waiting) == 1
	})
	close(release)
	r0, r1 := awaitAnswer(t, first, "prompt"), awaitAnswer(t, cmd, "command")
	if r0.resp.StopReason != "end_turn" || r1.resp.StopReason != "end_turn" {
		t.Fatalf("answers %+v %+v", r0, r1)
	}
	// On the wire, the prompt's answer comes before anything the command
	// streams: the editor would take that for the prompt's turn.
	answered, streamed := -1, -1
	for i, l := range ed.wire.lines() {
		if answered < 0 && strings.Contains(l, `"stopReason":"end_turn"`) {
			answered = i
		}
		if streamed < 0 && strings.Contains(l, "agent_message_chunk") && strings.Contains(l, sid) && !strings.Contains(l, "done") {
			streamed = i
		}
	}
	if answered < 0 || streamed < answered {
		t.Fatalf("the command streamed (line %d) before the prompt's answer (line %d)", streamed, answered)
	}
	if out := ed.text("agent_message_chunk"); !strings.Contains(out, sid) {
		t.Fatalf("/session did not run: %q", out)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.prompts) != 1 {
		t.Fatalf("the command reached the model: %q", model.prompts)
	}
}

// claude-code-acp's steering extension: offered, answered at once, and the
// message reaches the running turn.
func TestACPSteerExtension(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	started, release := make(chan struct{}), make(chan struct{})
	model := &scripted{replies: []func(context.Context) *llm.Message{
		blockUntil(started, release, func(context.Context) *llm.Message { return say("first") }),
	}}
	ed := startACP(t, home, model)
	var init struct {
		Meta struct {
			Steering struct {
				Supported bool `json:"supported"`
			} `json:"steering"`
		} `json:"_meta"`
	}
	if err := ed.call("initialize", acp.InitializeRequest{ProtocolVersion: 1}, &init); err != nil || !init.Meta.Steering.Supported {
		t.Fatalf("steering not offered: %+v %v", init, err)
	}
	sid := newACPSession(t, ed, project)
	steer := func(text string, promptRequired bool) string {
		t.Helper()
		req := map[string]any{"sessionId": sid, "prompt": []acp.ContentBlock{acp.Text(text)}}
		if promptRequired {
			req["_meta"] = map[string]any{"steering": map[string]any{"idleBehavior": "promptRequired"}}
		}
		var resp steerResponse
		if err := ed.call(steerMethod, req, &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Outcome
	}
	if out := steer("anything", true); out != "promptRequired" {
		t.Fatalf("idle: %q", out)
	}
	first := ed.promptAsync(sid, "work")
	<-started
	if out := steer("also this", false); out != "injected" {
		t.Fatalf("running: %q", out)
	}
	close(release)
	if r := awaitAnswer(t, first, "prompt"); r.resp.StopReason != "end_turn" {
		t.Fatalf("answer %q", r.resp.StopReason)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if strings.Join(model.prompts, "|") != "work|also this" {
		t.Fatalf("requests %q", model.prompts)
	}
}
