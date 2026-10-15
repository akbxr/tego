package main

import (
	"strings"

	"github.com/akbxr/tego/internal/llm"
)

// Messages typed while the agent works, like pi and Claude Code:
//   - Enter steers the running turn. The message goes in before the next
//     model request: after the current answer and its tool calls finish
//     (a running tool is never interrupted), or right away when the model
//     would stop.
//   - Alt+Enter queues a follow-up, sent as a new prompt once the turn ends.
//     Slash commands and !shell lines are always follow-ups.
//   - Alt+↑ puts everything not sent yet back into the editor, and so does
//     interrupting the turn (Esc, Ctrl+C).

// steerable reports whether a line can go into a running turn.
func steerable(line string) bool {
	return !isSlashCommand(line) && !strings.HasPrefix(line, "!")
}

// addPending records a message typed while busy (steer or follow-up).
func (a *app) addPending(line string, steer bool) {
	a.pendMu.Lock()
	if steer {
		a.steer = append(a.steer, line)
	} else {
		a.queue = append(a.queue, line)
	}
	a.pendMu.Unlock()
	a.tui.Redraw()
}

// pendingLines returns copies of the waiting steering messages and follow-ups.
func (a *app) pendingLines() (steer, queue []string) {
	a.pendMu.Lock()
	defer a.pendMu.Unlock()
	return append([]string(nil), a.steer...), append([]string(nil), a.queue...)
}

func (a *app) pendingCount() int {
	a.pendMu.Lock()
	defer a.pendMu.Unlock()
	return len(a.steer) + len(a.queue)
}

// takeSteering is the agent's Steer hook (it runs on the agent's
// goroutine): it shows each waiting message as a prompt block and turns it
// into a user message with its attachments.
func (a *app) takeSteering() [][]llm.Block {
	a.pendMu.Lock()
	lines := a.steer
	a.steer = nil
	a.pendMu.Unlock()
	if len(lines) == 0 {
		return nil
	}
	a.tui.Redraw()
	out := make([][]llm.Block, 0, len(lines))
	for _, l := range lines {
		a.ui.Finish()
		a.echoPrompt(l)
		out = append(out, a.userBlocks(l))
	}
	return out
}

// popFollowUp takes the next message to dispatch after a turn: steering
// messages that arrived too late for it first, then follow-ups.
func (a *app) popFollowUp() (string, bool) {
	a.pendMu.Lock()
	defer a.pendMu.Unlock()
	if len(a.steer) > 0 {
		a.queue = append(a.steer, a.queue...)
		a.steer = nil
	}
	if len(a.queue) == 0 {
		return "", false
	}
	line := a.queue[0]
	a.queue = a.queue[1:]
	return line, true
}

// restorePending moves every message not sent yet into the editor, in the
// order typed, ahead of what the editor holds (cur). It reports whether
// there was any.
func (a *app) restorePending(cur string) bool {
	a.pendMu.Lock()
	lines := append(a.steer, a.queue...)
	a.steer, a.queue = nil, nil
	a.pendMu.Unlock()
	if len(lines) == 0 {
		return false
	}
	text := strings.Join(lines, "\n")
	if strings.TrimSpace(cur) != "" {
		text += "\n" + cur
	}
	a.tui.SetInput(text)
	return true
}

// pendingPanel lists the waiting messages above the editor: steering ones
// (sent at the next step) and follow-ups (sent after the turn).
func (a *app) pendingPanel(c palette, width int) []string {
	steer, queue := a.pendingLines()
	n := len(steer) + len(queue)
	if n == 0 {
		return nil
	}
	lines := []string{" " + c.yellow + c.bold + "Queued" + c.reset + c.dim + " · alt+↑ edit" + c.reset}
	row := func(i int, mark, color, label, text string) {
		br := "├─ "
		if i == n-1 {
			br = "└─ "
		}
		text = strings.Join(strings.Fields(text), " ")
		if lim := width - 20; lim > 10 && len([]rune(text)) > lim {
			text = string([]rune(text)[:lim-1]) + "…"
		}
		lines = append(lines, "  "+c.dim+br+c.reset+color+mark+" "+label+c.reset+c.dim+" · "+c.reset+text)
	}
	for i, s := range steer {
		row(i, "↳", c.yellow, "next step", s)
	}
	for i, q := range queue {
		row(len(steer)+i, "⏸", c.dim, "after turn", q)
	}
	return lines
}
