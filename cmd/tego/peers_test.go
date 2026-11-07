package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/config"
	"github.com/akbxr/tego/internal/lineedit"
	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/peer"
	"github.com/akbxr/tego/internal/session"
)

// peerApp is a terminal app without a terminal, its session open.
func peerApp(t *testing.T, model llm.Provider) *app {
	t.Helper()
	dir, cwd := t.TempDir(), t.TempDir()
	a := &app{cfg: &config.Config{Dir: dir, SessionMessages: true}, cwd: cwd, interactive: true,
		ui:  &termUI{out: io.Discard, diag: io.Discard, atBOL: true, width: func() int { return 80 }},
		out: io.Discard, tui: lineedit.Offscreen(lineedit.Options{}, io.Discard)}
	a.status = newStatusBar(palette{}, cwd, iconPresets["unicode"])
	a.ag = &agent.Agent{Provider: model, Model: "m", UI: a.ui, Steer: a.takeSteering}
	a.takesMessages = true
	a.setupPeers()
	s, err := session.New(dir, cwd)
	if err != nil {
		t.Fatal(err)
	}
	a.setSession(s)
	t.Cleanup(func() { s.Close() })
	return a
}

// An idle terminal session starts a turn to answer a message; mid-turn a
// message steers the running turn; the end of a long chain waits for the
// user's next prompt.
func TestTerminalTakesSessionMessages(t *testing.T) {
	model := &scripted{}
	a := peerApp(t, model)
	id := a.ag.Session.ID
	done := make(chan error, 1)

	msg := peer.Message{To: id, From: "other-1", FromTitle: "render", Text: "the render finished", Hops: 1, CanReply: true}
	if out := a.receivePeer(msg, done); out.Status != "started" {
		t.Fatalf("idle: %+v", out)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	a.finishJob(nil)
	model.mu.Lock()
	got := strings.Join(model.prompts, "\n")
	model.mu.Unlock()
	if !strings.Contains(got, `<session-message from="other-1" title="render"`) || !strings.Contains(got, "the render finished") ||
		!strings.Contains(got, `session_send to id "other-1"`) {
		t.Fatalf("the turn's prompt: %q", got)
	}
	if a.peers.hops.Load() != 1 {
		t.Fatalf("hops %d", a.peers.hops.Load())
	}

	// Mid-turn: it steers.
	_, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	if out := a.receivePeer(peer.Message{To: id, From: "other-1", Text: "also check the audio", Hops: 3}, done); out.Status != "steered" {
		t.Fatalf("busy: %+v", out)
	}
	steer := a.takeSteering()
	if len(steer) != 1 || !strings.Contains(steer[0][0].Text, "also check the audio") || a.peers.hops.Load() != 3 {
		t.Fatalf("steering %v, hops %d", steer, a.peers.hops.Load())
	}
	a.cancel = nil

	// Too long a chain: held for the next request.
	if out := a.receivePeer(peer.Message{To: id, From: "other-1", Text: "and again", Hops: maxPeerHops + 1}, done); out.Status != "held" {
		t.Fatalf("long chain: %+v", out)
	}
	if n := a.ag.Notices(); !strings.Contains(n, "and again") || a.ag.Notices() != "" {
		t.Fatalf("held note %q", n)
	}
	select {
	case <-done:
		t.Fatal("a held message started a turn")
	case <-time.After(50 * time.Millisecond):
	}

	// Another session's id: gone.
	if out := a.receivePeer(peer.Message{To: "nope", Text: "x"}, done); out.Status != "gone" {
		t.Fatalf("wrong session: %+v", out)
	}
}

// A message that came after the turn's last request gets a turn of its
// own once the turn ends; after an interrupt it waits.
func TestTerminalPeerLeftovers(t *testing.T) {
	model := &scripted{}
	a := peerApp(t, model)
	done := make(chan error, 1)
	a.peers.steer = []peer.Message{{To: a.ag.Session.ID, From: "o", Text: "late one", Hops: 2}}
	a.peerLeftovers(false, done)
	if n := a.ag.Notices(); !strings.Contains(n, "late one") {
		t.Fatalf("after an interrupt: %q", n)
	}
	a.peers.steer = []peer.Message{{To: a.ag.Session.ID, From: "o", Text: "late two", Hops: 2}}
	a.peerLeftovers(true, done)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	a.finishJob(nil)
	model.mu.Lock()
	defer model.mu.Unlock()
	if last := model.prompts[len(model.prompts)-1]; !strings.Contains(last, "late two") {
		t.Fatalf("prompt %q", last)
	}
}

// Two sessions over the sockets: one sends, the other answers; the open
// list shows both, and session_send refuses its own session.
func TestSessionSendBetweenProcesses(t *testing.T) {
	model := &scripted{}
	a := peerApp(t, model)
	a.listenPeers()
	defer a.peers.hub.Close()
	// The REPL's part: deliveries are handled as they come.
	done := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case d := <-a.peers.in:
				d.reply <- a.receivePeer(d.m, done)
			case <-stop:
				return
			}
		}
	}()
	open := peer.List(context.Background(), a.cfg.Dir)
	if len(open) != 1 || open[0].ID != a.ag.Session.ID || open[0].Kind != "terminal" {
		t.Fatalf("open %+v", open)
	}
	// Open now with no message yet: listed from any folder.
	list := &peer.SessionsTool{Base: a.cfg.Dir, Cwd: t.TempDir()}
	if out, _ := list.Run(context.Background(), []byte(`{"action":"list"}`)); !strings.Contains(out, "Open now, in every folder:\n"+a.ag.Session.ID) || !strings.Contains(out, "idle, terminal") {
		t.Fatalf("list:\n%s", out)
	}
	send := &peer.SendTool{Base: a.cfg.Dir, Self: func() peer.Self { return peer.Self{ID: "sender-1", Title: "pipeline"} }, CanReply: true}
	out, err := send.Run(context.Background(), []byte(`{"id":"`+a.ag.Session.ID[:15]+`","text":"status?"}`))
	if err != nil || !strings.Contains(out, "started a turn to answer") {
		t.Fatalf("send: %q %v", out, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	self := &peer.SendTool{Base: a.cfg.Dir, Self: a.peerSelf}
	if _, err := self.Run(context.Background(), []byte(`{"id":"`+a.ag.Session.ID+`","text":"hi"}`)); err == nil || !strings.Contains(err.Error(), "this session") {
		t.Fatalf("to itself: %v", err)
	}
	if _, err := send.Run(context.Background(), []byte(`{"id":"20990101","text":"hi"}`)); err == nil || !strings.Contains(err.Error(), "not open") {
		t.Fatalf("to a closed one: %v", err)
	}
}

// In an editor a message steers the running turn; an idle session holds
// it for the next prompt (an editor can't start a turn) and says so.
func TestACPTakesSessionMessages(t *testing.T) {
	release := make(chan struct{})
	model := &scripted{replies: []func(context.Context) *llm.Message{
		func(context.Context) *llm.Message { <-release; return say("first") },
		func(context.Context) *llm.Message { return say("answered the session") },
	}}
	ed := startACP(t, t.TempDir(), model)
	sid := newACPSession(t, ed, t.TempDir())
	ed.srv.session(sid).app.cfg.SessionMessages = true
	handler := (*acpPeers)(ed.srv)
	if open := handler.Open(); len(open) != 1 || open[0].ID != sid || open[0].Busy || open[0].Kind != "editor" {
		t.Fatalf("open %+v", open)
	}

	answered := make(chan string, 1)
	go func() { answered <- ed.prompt(sid, "work on it") }()
	waitFor(t, 5*time.Second, "the turn", func() bool { return handler.Open()[0].Busy })
	if out := handler.Deliver(peer.Message{To: sid, From: "o1", Text: "the render finished", Hops: 1}); out.Status != "steered" {
		t.Fatalf("busy: %+v", out)
	}
	close(release)
	if got := <-answered; !strings.Contains(got, "answered the session") {
		t.Fatalf("answer %q", got)
	}
	model.mu.Lock()
	last := model.prompts[len(model.prompts)-1]
	model.mu.Unlock()
	if !strings.Contains(last, "the render finished") {
		t.Fatalf("not steered: %q", last)
	}

	if out := handler.Deliver(peer.Message{To: sid, From: "o1", Text: "one more thing", Hops: 1}); out.Status != "held" || !strings.Contains(out.Note, "editor") {
		t.Fatalf("idle: %+v", out)
	}
	waitFor(t, 5*time.Second, "the held message shown", func() bool {
		return strings.Contains(ed.text("agent_thought_chunk"), "one more thing")
	})
	if n := ed.srv.session(sid).ag.Notices(); !strings.Contains(n, "one more thing") {
		t.Fatalf("not held for the next prompt: %q", n)
	}
}
