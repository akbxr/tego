package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/peer"
	"github.com/akbxr/tego/internal/session"
)

// Session messages (internal/peer): with "session_messages": true, an
// interactive tego and tego acp take messages from other sessions. A
// message to a session whose turn runs steers it; one to an idle terminal
// session starts a turn to answer it, unless it is the end of a long chain
// of messages between sessions (maxPeerHops), which waits for the user.
// An editor can't start a turn, so there it goes with the next prompt.

// maxPeerHops is how long a chain of messages between sessions may start
// turns by itself.
const maxPeerHops = 4

// peerState is an app's side of session messages.
type peerState struct {
	mu    sync.Mutex
	sess  *session.Session // the terminal's open session
	held  []string         // messages for the next request (a Notices note)
	steer []peer.Message   // the terminal's, for the running turn (under app.pendMu)

	hops atomic.Int32 // the chain that led to the running turn (0: the user's prompt)
	busy atomic.Bool  // the terminal runs a turn
	in   chan peerDelivery
	hub  *peer.Hub
}

type peerDelivery struct {
	m     peer.Message
	reply chan peer.Outcome
}

// setSession makes s the app's session.
func (a *app) setSession(s *session.Session) {
	a.ag.Session = s
	a.peers.mu.Lock()
	a.peers.sess = s
	a.peers.mu.Unlock()
}

// peerSelf is the session the tools run in.
func (a *app) peerSelf() peer.Self {
	self := peer.Self{Cwd: a.cwd, Hops: int(a.peers.hops.Load())}
	if s := a.ag.Session; s != nil {
		self.ID, self.Title = s.ID, s.TitleNow()
	}
	return self
}

// raiseHops notes that a message of a chain of hops reached the running
// turn: what it sends goes on from there.
func (p *peerState) raiseHops(hops int) {
	for {
		cur := p.hops.Load()
		if int32(hops) <= cur || p.hops.CompareAndSwap(cur, int32(hops)) {
			return
		}
	}
}

func (p *peerState) hold(text string) {
	p.mu.Lock()
	p.held = append(p.held, text)
	p.mu.Unlock()
}

// takeHeld is the held messages, for the next request's notes.
func (p *peerState) takeHeld() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := strings.Join(p.held, "\n\n")
	p.held = nil
	return out
}

// setupPeers adds the sessions tools and the held messages' notes.
func (a *app) setupPeers() {
	a.ag.Tools = append(a.ag.Tools, &peer.SessionsTool{Base: a.cfg.Dir, Cwd: a.cwd, Self: a.peerSelf})
	if a.cfg.SessionMessages {
		a.ag.Tools = append(a.ag.Tools, &peer.SendTool{Base: a.cfg.Dir, Self: a.peerSelf, CanReply: a.takesMessages})
	}
	notes := a.ag.Notices
	a.ag.Notices = func() string {
		var out []string
		if notes != nil {
			if n := notes(); n != "" {
				out = append(out, n)
			}
		}
		if n := a.peers.takeHeld(); n != "" {
			out = append(out, n)
		}
		return strings.Join(out, "\n\n")
	}
}

// listenPeers starts taking messages in the terminal.
func (a *app) listenPeers() {
	if !a.takesMessages {
		return
	}
	a.peers.in = make(chan peerDelivery, 16)
	hub, err := peer.Listen(a.cfg.Dir, (*termPeers)(a))
	if err != nil {
		a.ui.OnInfo("session messages are off: " + err.Error())
		return
	}
	a.peers.hub = hub
}

// termPeers is the terminal's peer.Handler.
type termPeers app

func (t *termPeers) Open() []peer.Open {
	a := (*app)(t)
	a.peers.mu.Lock()
	s := a.peers.sess
	a.peers.mu.Unlock()
	if s == nil {
		return nil
	}
	return []peer.Open{{ID: s.ID, Title: s.TitleNow(), Cwd: a.cwd, Busy: a.peers.busy.Load(), Kind: "terminal"}}
}

// Deliver hands m to the REPL, which owns the turn.
func (t *termPeers) Deliver(m peer.Message) peer.Outcome {
	a := (*app)(t)
	d := peerDelivery{m: m, reply: make(chan peer.Outcome, 1)}
	select {
	case a.peers.in <- d:
	default:
		return peer.Outcome{Status: "gone", Note: "its inbox is full"}
	}
	select {
	case out := <-d.reply:
		return out
	case <-time.After(3 * time.Second):
		return peer.Outcome{Status: "queued"}
	}
}

// receivePeer is what the REPL does with a message: steer the running
// turn, start one, or hold it for the user's next prompt.
func (a *app) receivePeer(m peer.Message, done chan<- error) peer.Outcome {
	if s := a.ag.Session; s == nil || s.ID != m.To {
		return peer.Outcome{Status: "gone"}
	}
	if a.cancel != nil {
		a.pendMu.Lock()
		a.peers.steer = append(a.peers.steer, m)
		a.pendMu.Unlock()
		a.peers.raiseHops(m.Hops)
		a.ag.SteerArrived() // a task_status or bash_output wait ends for it
		a.ui.Dim("✉ %s: read before the next request", m.Label())
		a.tui.Redraw()
		return peer.Outcome{Status: "steered"}
	}
	if m.Hops > maxPeerHops {
		a.peers.hold(m.Prompt())
		a.ui.OnInfo(fmt.Sprintf("✉ %s, held for your next prompt: it is message %d in a row between sessions", m.Label(), m.Hops))
		return peer.Outcome{Status: "held", Note: fmt.Sprintf("It is message %d in a row between sessions, so it doesn't start a turn by itself.", m.Hops)}
	}
	a.startPeerTurn([]peer.Message{m}, done)
	return peer.Outcome{Status: "started"}
}

// startPeerTurn starts a turn answering msgs.
func (a *app) startPeerTurn(msgs []peer.Message, done chan<- error) {
	hops := 0
	var prompts []string
	for _, m := range msgs {
		a.echoPeer(m)
		hops = max(hops, m.Hops)
		prompts = append(prompts, m.Prompt())
	}
	a.peers.hops.Store(int32(hops))
	a.maybeTitle(msgs[0].Text) // a session a message starts is named after it
	prompt := strings.Join(prompts, "\n\n")
	a.jobMsg = "Answering a " + msgs[0].Label()
	a.startJob(done, func(ctx context.Context) error {
		return a.extTurn(ctx, prompt, func(ctx context.Context, prompt string) error { return a.ag.Run(ctx, prompt) })
	})
}

// echoPeer shows a message from another session as a prompt.
func (a *app) echoPeer(m peer.Message) {
	a.echoPrompt("✉ " + m.Label() + "\n" + strings.TrimSpace(m.Text))
}

// takePeerSteering is the messages for the running turn, as user messages.
func (a *app) takePeerSteering() [][]llm.Block {
	a.pendMu.Lock()
	msgs := a.peers.steer
	a.peers.steer = nil
	a.pendMu.Unlock()
	out := make([][]llm.Block, 0, len(msgs))
	for _, m := range msgs {
		a.ui.Finish()
		a.echoPeer(m)
		out = append(out, []llm.Block{{Type: llm.BlockText, Text: m.Prompt()}})
	}
	return out
}

// peerLeftovers handles messages that came too late for the turn that
// ended: a turn of their own, or after an error or an interrupt, the next
// prompt's.
func (a *app) peerLeftovers(ok bool, done chan<- error) {
	a.pendMu.Lock()
	msgs := a.peers.steer
	a.peers.steer = nil
	a.pendMu.Unlock()
	if len(msgs) == 0 {
		return
	}
	a.ag.SteerTaken()
	if ok {
		a.startPeerTurn(msgs, done)
		return
	}
	for _, m := range msgs {
		a.peers.hold(m.Prompt())
		a.ui.OnInfo("✉ " + m.Label() + ", held for your next prompt")
	}
}

// The editor's side (tego acp): one listener for every session.

func (s *acpServer) listenPeers() *peer.Hub {
	if !s.cfg.SessionMessages {
		return nil
	}
	hub, err := peer.Listen(s.cfg.Dir, (*acpPeers)(s))
	if err != nil {
		return nil
	}
	return hub
}

type acpPeers acpServer

func (p *acpPeers) Open() []peer.Open {
	s := (*acpServer)(p)
	s.mu.Lock()
	list := make([]*acpSession, 0, len(s.sessions))
	for _, sess := range s.sessions {
		list = append(list, sess)
	}
	s.mu.Unlock()
	var out []peer.Open
	for _, sess := range list {
		sess.mu.Lock()
		o := peer.Open{ID: sess.id, Title: sess.title, Cwd: sess.cwd, Busy: sess.run != nil, Kind: "editor"}
		sess.mu.Unlock()
		out = append(out, o)
	}
	return out
}

// Deliver steers the running turn, else holds m for the next prompt: an
// editor can't start a turn by itself.
func (p *acpPeers) Deliver(m peer.Message) peer.Outcome {
	sess := (*acpServer)(p).session(m.To)
	if sess == nil {
		return peer.Outcome{Status: "gone"}
	}
	sess.mu.Lock()
	if run := sess.run; run != nil && run.ctx.Err() == nil {
		sess.addSteerLocked(m.Prompt(), nil)
		sess.mu.Unlock()
		sess.app.peers.raiseHops(m.Hops)
		sess.ag.SteerArrived()
		sess.OnInfo("✉ " + m.Label() + ": read before the next request")
		return peer.Outcome{Status: "steered"}
	}
	sess.mu.Unlock()
	sess.app.peers.hold(m.Prompt())
	sess.OnInfo("✉ " + m.Label() + ", for your next prompt (an editor can't start a turn by itself):\n\n" + strings.TrimSpace(m.Text))
	return peer.Outcome{Status: "held", Note: "It is open in an editor, which can't start a turn by itself; it goes with its user's next prompt."}
}
