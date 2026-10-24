package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/akbxr/tego/internal/lineedit"
	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/session"
)

// promptText returns the typed text of a user prompt message ("" for tool
// results and injected reminders/summaries).
func promptText(m *llm.Message) string {
	if m == nil || m.Role != llm.RoleUser {
		return ""
	}
	var parts []string
	for _, b := range m.Blocks {
		switch b.Type {
		case llm.BlockToolResult:
			return ""
		case llm.BlockText:
			t := strings.TrimSpace(b.Text)
			if strings.HasPrefix(t, "<system-reminder>") || strings.HasPrefix(t, "<context-summary>") || strings.HasPrefix(t, mentionPrefix) || strings.HasPrefix(t, skillAttachPrefix) {
				continue
			}
			parts = append(parts, displayPrompt(b.Text))
		}
	}
	return strings.Join(parts, "\n")
}

// summaryOnly is the context-summary block of m, which compaction may put
// in front of the first kept prompt, as a message of its own.
func summaryOnly(m llm.Message) (llm.Message, bool) {
	if m.Role != llm.RoleUser {
		return llm.Message{}, false
	}
	for _, b := range m.Blocks {
		if b.Type == llm.BlockText && strings.HasPrefix(strings.TrimSpace(b.Text), "<context-summary>") {
			return llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{b}}, true
		}
	}
	return llm.Message{}, false
}

// historyBefore is msgs up to the prompt at i, keeping the summary that
// compaction may have merged into that prompt.
func historyBefore(msgs []llm.Message, i int) []llm.Message {
	out := append([]llm.Message(nil), msgs[:i]...)
	if s, ok := summaryOnly(msgs[i]); ok {
		out = append(out, s)
	}
	return out
}

// replPoint is a point to rewind to inside a compaction's replacement
// history: its summary, or a prompt or answer it kept verbatim.
type replPoint struct {
	prompt string // a prompt: it goes back into the editor
	answer string
	calls  int
	keep   []llm.Message // the history to continue from
	whole  bool          // keep is the whole replacement
}

// replacementPoints lists where one can continue from inside a compaction:
// its summary, then each prompt and answer it kept. A prompt is never left
// at the end of the history unanswered: picking it keeps what came before
// (the summary included) and puts it in the editor.
func replacementPoints(repl []llm.Message) []replPoint {
	var out []replPoint
	if len(repl) > 0 && promptText(&repl[0]) == "" && assistantText(&repl[0]) == "" {
		// A summary of its own; one merged into the first prompt comes with it.
		out = append(out, replPoint{keep: append([]llm.Message(nil), repl[0]), whole: len(repl) == 1})
	}
	for i := range repl {
		m := &repl[i]
		if p := promptText(m); p != "" {
			out = append(out, replPoint{prompt: p, keep: historyBefore(repl, i)})
			continue
		}
		t := assistantText(m)
		if t == "" {
			continue
		}
		end := i + 1
		c := toolCalls(m)
		if c > 0 && end < len(repl) && repl[end].Role == llm.RoleUser && promptText(&repl[end]) == "" {
			end++ // its tool results, so no call is left without one
		}
		out = append(out, replPoint{answer: t, calls: c, keep: append([]llm.Message(nil), repl[:end]...), whole: end == len(repl)})
	}
	return out
}

// checkoutInside continues from a point inside compaction node n: what it
// keeps is recorded as a new compaction below n, so the old path stays.
func checkoutInside(s *session.Session, n *session.Node, p replPoint) ([]llm.Message, error) {
	hist, err := s.Checkout(n.ID)
	if err != nil || p.whole {
		return hist, err
	}
	if err := s.Compacted(p.keep); err != nil {
		return nil, err
	}
	return s.History(s.Leaf()), nil
}

func assistantText(m *llm.Message) string {
	if m == nil || m.Role != llm.RoleAssistant {
		return ""
	}
	var b strings.Builder
	for _, bl := range m.Blocks {
		if bl.Type == llm.BlockText {
			b.WriteString(bl.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

// treeCommand shows the session tree and jumps to a node. Picking a prompt
// moves before it and puts it in the editor, so sending it (edited or not)
// starts a new branch; picking an answer continues from that answer.
func (a *app) treeCommand() {
	s := a.ag.Session
	if s == nil {
		a.ui.Errorf("no session file (started with --no-session)")
		return
	}
	type pick struct {
		node   *session.Node
		prompt string
	}
	var items []lineedit.PickItem
	var picks []pick
	sel := 0
	for _, row := range s.Tree() {
		n := row.Node
		var label string
		var p pick
		switch {
		case n.Replace != nil:
			label = "⇊ compacted"
		case promptText(n.Message) != "":
			p.prompt = promptText(n.Message)
			label = "❯ " + session.Oneline(p.prompt, 90)
		case assistantText(n.Message) != "":
			label = "◆ " + session.Oneline(assistantText(n.Message), 90)
		default:
			continue // tool calls/results: reachable through their prompt/answer
		}
		p.node = n
		mark := "○ "
		if row.OnPath {
			mark = "● "
			sel = len(items)
		}
		detail := ago(n.Time)
		if n.ID == s.Leaf() {
			detail += " · current"
		} else if row.Branch {
			detail += " · branch"
		}
		items = append(items, lineedit.PickItem{Label: strings.Repeat("  ", row.Depth) + mark + label, Detail: detail})
		picks = append(picks, p)
	}
	if len(items) == 0 {
		a.ui.Dim("session is empty")
		return
	}
	idx, err := a.tui.Pick("Session tree · ● current path · enter: jump (prompts go back into the editor)", items, "", sel)
	if err != nil {
		if !errors.Is(err, lineedit.ErrCanceled) {
			a.ui.Errorf("%v", err)
		}
		return
	}
	a.jumpTo(picks[idx].node, picks[idx].prompt)
}

// jumpTo moves the session's active point. For a prompt node it goes to
// just before the prompt and puts the prompt back in the editor (sending it
// starts a new branch); for anything else it continues right after it.
func (a *app) jumpTo(n *session.Node, prompt string) {
	s := a.ag.Session
	if n.Replace != nil {
		// The compaction as a whole (/tree): continue after it, unless it ends
		// with the prompt it was made for, which goes back into the editor.
		p := replPoint{keep: n.Replace, whole: true}
		if pts := replacementPoints(n.Replace); len(pts) > 0 && pts[len(pts)-1].prompt != "" && promptText(&n.Replace[len(n.Replace)-1]) != "" {
			p = pts[len(pts)-1]
		}
		a.jumpInside(n, p)
		return
	}
	target := n.Parent
	if prompt == "" {
		var err error
		if target, err = s.ContinueFrom(n); err != nil {
			a.ui.Errorf("%v", err)
			return
		}
	}
	hist, err := s.Checkout(target)
	if err != nil {
		a.ui.Errorf("%v", err)
		return
	}
	a.landAt(hist, prompt)
}

// jumpInside continues from a point inside compaction node n.
func (a *app) jumpInside(n *session.Node, p replPoint) {
	hist, err := checkoutInside(a.ag.Session, n, p)
	if err != nil {
		a.ui.Errorf("%v", err)
		return
	}
	a.landAt(hist, p.prompt)
}

// landAt makes hist the conversation after a rewind, with prompt (if any)
// back in the editor.
func (a *app) landAt(hist []llm.Message, prompt string) {
	a.ag.SetHistory(hist)
	a.redrawConversation()
	if prompt != "" {
		a.tui.SetInput(prompt)
		a.ui.Dim("rewound to before that prompt (%d messages kept); edit and send it to continue — the old path stays as a branch (/tree)", len(hist))
	} else {
		a.ui.Dim("rewound to that point (%d messages); the next prompt continues from here — the old path stays as a branch (/tree)", len(hist))
	}
	a.refreshStatus()
}

// rewindCommand (Esc Esc, /branch, /rewind) lists the turns of the current
// path, newest selected, and continues from the chosen point.
func (a *app) rewindCommand() {
	s := a.ag.Session
	if s == nil {
		a.ui.Errorf("no session file (started with --no-session)")
		return
	}
	type point struct {
		node   *session.Node
		prompt string
		inside *replPoint // a point inside compaction node
	}
	var items []lineedit.PickItem
	var points []point
	turn := 0
	for _, n := range s.ActivePath() {
		switch {
		case n.Replace != nil:
			// What the compaction kept verbatim can be rewound to as well.
			for _, p := range replacementPoints(n.Replace) {
				var label, detail string
				switch {
				case p.prompt != "":
					turn++
					label = "❯ " + session.Oneline(p.prompt, 100)
					detail = fmt.Sprintf("turn %d · compacted", turn)
				case p.answer != "":
					label = "  ◆ " + session.Oneline(p.answer, 96)
					if p.calls > 0 {
						label += fmt.Sprintf("  (+%d tool calls)", p.calls)
					}
					detail = "compacted"
				default:
					label, detail = "⇊ summary of the earlier conversation", ago(n.Time)
				}
				items = append(items, lineedit.PickItem{Label: label, Detail: detail})
				points = append(points, point{node: n, inside: &p})
			}
		case promptText(n.Message) != "":
			turn++
			p := promptText(n.Message)
			items = append(items, lineedit.PickItem{Label: "❯ " + session.Oneline(p, 100), Detail: fmt.Sprintf("turn %d · %s", turn, ago(n.Time))})
			points = append(points, point{node: n, prompt: p})
		case assistantText(n.Message) != "":
			label := "  ◆ " + session.Oneline(assistantText(n.Message), 96)
			if c := toolCalls(n.Message); c > 0 {
				label += fmt.Sprintf("  (+%d tool calls)", c)
			}
			items = append(items, lineedit.PickItem{Label: label, Detail: ago(n.Time)})
			points = append(points, point{node: n})
		}
	}
	if len(items) == 0 {
		a.ui.Dim("nothing to rewind to yet")
		return
	}
	idx, err := a.tui.Pick("⟲ Rewind · pick the point to continue from (❯ prompt: edit & resend it · ◆ answer: continue after it)", items, "", len(items)-1)
	if err != nil {
		if !errors.Is(err, lineedit.ErrCanceled) {
			a.ui.Errorf("%v", err)
		}
		return
	}
	if p := points[idx]; p.inside != nil {
		a.jumpInside(p.node, *p.inside)
		return
	}
	a.jumpTo(points[idx].node, points[idx].prompt)
}

func toolCalls(m *llm.Message) int {
	n := 0
	for _, b := range m.Blocks {
		if b.Type == llm.BlockToolUse {
			n++
		}
	}
	return n
}

// forkCommand starts a new session from the history before a chosen prompt
// of the current path, with that prompt in the editor.
func (a *app) forkCommand() {
	var items []lineedit.PickItem
	var at []int
	for i := range a.ag.Messages {
		if t := promptText(&a.ag.Messages[i]); t != "" {
			items = append(items, lineedit.PickItem{Label: "❯ " + session.Oneline(t, 100), Detail: fmt.Sprintf("#%d", len(items)+1)})
			at = append(at, i)
		}
	}
	if len(items) == 0 {
		a.ui.Dim("nothing to fork yet")
		return
	}
	idx, err := a.tui.Pick("Fork from prompt · new session with everything before it", items, "", len(items)-1)
	if err != nil {
		if !errors.Is(err, lineedit.ErrCanceled) {
			a.ui.Errorf("%v", err)
		}
		return
	}
	i := at[idx]
	prompt := promptText(&a.ag.Messages[i])
	hist := historyBefore(a.ag.Messages, i)
	if err := a.switchToCopy(hist); err != nil {
		a.ui.Errorf("%v", err)
		return
	}
	a.redrawConversation()
	a.tui.SetInput(prompt)
	a.ui.Dim("forked into session %s (%d messages); edit the prompt and send", a.sessionID(), len(hist))
}

// cloneCommand duplicates the current path into a new session.
func (a *app) cloneCommand() {
	hist := append([]llm.Message(nil), a.ag.Messages...)
	if err := a.switchToCopy(hist); err != nil {
		a.ui.Errorf("%v", err)
		return
	}
	a.ui.Dim("cloned into session %s (%d messages); the original is unchanged", a.sessionID(), len(hist))
}

// switchToCopy makes a new session holding hist and continues in it.
func (a *app) switchToCopy(hist []llm.Message) error {
	parent := a.sessionID()
	if a.noSession {
		a.ag.SetHistory(hist)
		return nil
	}
	s, err := session.NewFrom(a.cfg.Dir, a.cwd, hist, parent)
	if err != nil {
		return err
	}
	a.closeSession()
	a.ag.Session = s
	a.ag.SessionID = uuid4()
	a.ag.SetHistory(hist)
	return nil
}

// redrawConversation wipes the terminal (screen and scrollback) and prints
// the current history again, so turns undone by a rewind disappear from
// view instead of lingering above the new branch.
func (a *app) redrawConversation() {
	a.ui.Finish()
	a.tui.Clear()
	a.banner()
	a.replay()
}

// redrawIfResized redraws the conversation at the terminal's new width
// when it was resized during the turn: rows already printed were broken
// for the old width, so the terminal's reflow cuts each one in two. The
// UI calls it before each streamed delta, on the agent's goroutine: no
// tool runs then, and the history holds everything shown but the response
// being streamed, which is shown again after it.
func (a *app) redrawIfResized() {
	if !a.redrawWanted.Swap(false) {
		return
	}
	live := a.ui.takeLive()
	a.ui.Finish()
	a.tui.Clear()
	a.banner()
	a.replayMessages()
	a.ui.showLive(live)
}

func (a *app) sessionID() string {
	if a.ag.Session == nil {
		return "(none)"
	}
	return a.ag.Session.ID
}
