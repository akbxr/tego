package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/lineedit"
)

// Todo and subagent display:
//   - above the editor while a turn runs: a TODO panel (the current phase
//     expanded, other phases collapsed) and a Subagents panel with one live
//     row per subagent of the running task call
//   - in the transcript: a framed Todo box after every todo call and a Task
//     box per task call (prompts when it starts, results when it ends)
//   - ←← on an empty editor (or /agents) opens a subagent and prints its
//     activity log and latest answer

const (
	panelPhaseItems = 8 // items of the current phase shown in the TODO panel
	panelAgents     = 6 // subagents shown with their own rows
)

// todoMark is the glyph and colour of a todo item.
func todoMark(c palette, status string) (mark, color string) {
	switch status {
	case "completed":
		return "✓", c.green
	case "in_progress":
		return "▸", c.yellow + c.bold
	}
	return "○", c.dim
}

// panel is the lineedit Panel callback: TODO then Subagents. It runs with
// the TUI lock held, so it only reads snapshots.
func (a *app) panel(width int) []string {
	ag := a.footerAgent.Load()
	if ag == nil {
		return nil
	}
	c := a.status.c
	todos, rev := ag.TodoState()
	out := todoPanel(c, todos, a.todoThisTurn(a.status.snapshot().busy, rev))
	out = append(out, agentsPanel(c, ag.Subagents(), width, a.cwd)...)
	out = append(out, a.pendingPanel(c, width)...)
	live := a.ui.writingBox(width) // an edit or write being written
	if cmd, since, tail := a.ui.runningBash(); cmd != "" {
		live = a.ui.runningBox(cmd, time.Since(since), tail, width)
	}
	if box := a.ui.approvalLive(width); box != nil {
		live = box // a call waiting for the user: what it would do, above the picker
	}
	if live != nil {
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, live...)
		out = append(out, "") // a gap before the Working… line
	}
	return out
}

var bashSpinner = []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}

// shimmer draws s dim with a bright spot that sweeps left to right, then
// rests a few frames before starting again.
func shimmer(c palette, s string, frame int) string {
	return sweep(s, frame, c.reset+c.dim, c.reset, c.reset+c.bold) + c.reset
}

// glow is shimmer for text that must stay readable: plain, with a bold
// coloured spot sweeping across it (the activity line's "Working…").
func glow(c palette, s string, frame int) string {
	return sweep(s, frame, c.reset, c.reset+c.bold, c.reset+c.bold+c.magenta) + c.reset
}

// sweep styles each rune of s by its distance from a spot that moves one
// rune per frame, then rests a few frames before starting again: peak on
// the spot, near next to it, base elsewhere.
func sweep(s string, frame int, base, near, peak string) string {
	r := []rune(s)
	pos := frame%(len(r)+6) - 1 // -1 and past the end: resting
	var b strings.Builder
	for i, ch := range r {
		switch d := i - pos; {
		case d == 0:
			b.WriteString(peak)
		case d == -1 || d == 1:
			b.WriteString(near)
		default:
			b.WriteString(base)
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// todoThisTurn reports whether the running job updated the todo list (rev
// is its current revision), which is when a finished list is still shown.
func (a *app) todoThisTurn(busy bool, rev int) bool {
	return busy && int64(rev) != a.todoRevAtStart.Load()
}

var userHome, _ = os.UserHomeDir()

// shortenPaths writes paths under cwd relative and others under the home
// directory as ~/… (like tool-call lines).
func shortenPaths(s, cwd string) string {
	if cwd != "" {
		s = strings.ReplaceAll(s, cwd+string(filepath.Separator), "")
	}
	if userHome != "" {
		s = strings.ReplaceAll(s, userHome+string(filepath.Separator), "~"+string(filepath.Separator))
	}
	return s
}

// todoPanel shows the list while something is left to do. A finished list
// stays only during the turn that updated it (thisTurn), so a later turn
// doesn't carry an old, done list around.
func todoPanel(c palette, todos []agent.Todo, thisTurn bool) []string {
	if len(todos) == 0 {
		return nil
	}
	phases := agent.TodoPhases(todos)
	done := 0
	for _, p := range phases {
		done += p.Done
	}
	if !thisTurn && done == len(todos) {
		return nil
	}
	lines := []string{fmt.Sprintf(" %sTODO%s %s%d/%d%s", c.yellow+c.bold, c.reset, c.dim, done, len(todos), c.reset)}
	branch := func(last bool) string {
		if last {
			return "└─ "
		}
		return "├─ "
	}
	items := func(prefix string, list []agent.Todo) {
		start, end := 0, len(list)
		if end > panelPhaseItems {
			// Keep the first unfinished item in view.
			cur := 0
			for i, td := range list {
				if td.Status != "completed" {
					cur = i
					break
				}
			}
			start = min(max(cur-2, 0), end-panelPhaseItems)
			end = start + panelPhaseItems
		}
		if start > 0 {
			lines = append(lines, fmt.Sprintf("%s%s├─ … %d done%s", prefix, c.dim, start, c.reset))
		}
		for i := start; i < end; i++ {
			mark, color := todoMark(c, list[i].Status)
			last := i == len(list)-1
			lines = append(lines, prefix+c.dim+branch(last && end == len(list))+c.reset+color+mark+" "+list[i].Content+c.reset)
		}
		if end < len(list) {
			lines = append(lines, fmt.Sprintf("%s%s└─ … %d more%s", prefix, c.dim, len(list)-end, c.reset))
		}
	}
	if len(phases) == 1 && phases[0].Name == "" {
		items("  ", phases[0].Items)
		return lines
	}
	// The current phase is the first one with work left (else the last).
	cur := len(phases) - 1
	for i, p := range phases {
		if p.Done < len(p.Items) {
			cur = i
			break
		}
	}
	for i, p := range phases {
		head := fmt.Sprintf("%d. %s", i+1, agent.PhaseName(phases, i))
		count := fmt.Sprintf(" · %d/%d", p.Done, len(p.Items))
		if i == cur {
			lines = append(lines, "  "+c.dim+"├─ "+c.reset+c.yellow+c.bold+head+c.reset+c.dim+count+c.reset)
			items("  "+c.dim+"│  "+c.reset, p.Items)
			continue
		}
		color := c.dim
		if p.Done == len(p.Items) {
			color = c.green
		}
		lines = append(lines, "  "+c.dim+"├─ "+c.reset+color+head+count+c.reset)
	}
	return append(lines, "  "+c.dim+"└─────"+c.reset)
}

// agentsPanel shows the subagents of a task call while any is queued or
// running.
func agentsPanel(c palette, subs []agent.SubagentStatus, width int, cwd string) []string {
	running, queued := 0, 0
	for _, s := range subs {
		switch s.State {
		case agent.SubRunning:
			running++
		case agent.SubQueued:
			queued++
		}
	}
	if running+queued == 0 {
		return nil
	}
	head := fmt.Sprintf(" %sSubagents%s %s%d running", c.yellow+c.bold, c.reset, c.dim, running)
	if queued > 0 {
		head += fmt.Sprintf(" · %d queued", queued)
	}
	lines := []string{head + c.reset}
	// Running first, then queued, then finished: the interesting rows stay
	// visible when there are many.
	order := make([]int, 0, len(subs))
	for _, want := range []string{agent.SubRunning, agent.SubQueued, agent.SubDone, agent.SubFailed} {
		for i, s := range subs {
			if s.State == want {
				order = append(order, i)
			}
		}
	}
	shown := order
	if len(shown) > panelAgents {
		shown = shown[:panelAgents]
	}
	for n, i := range shown {
		s := subs[i]
		last := n == len(shown)-1 && len(shown) == len(order)
		br, cont := "├─ ", "│  "
		if last {
			br, cont = "└─ ", "   "
		}
		lines = append(lines, "  "+c.dim+br+c.reset+subRow(c, s, true))
		if s.State == agent.SubRunning && s.Last != "" {
			act := shortenPaths(s.Last, cwd)
			if lim := width - 12; lim > 10 && runewidth.StringWidth(act) > lim {
				act = runewidth.Truncate(act, lim, "…")
			}
			lines = append(lines, "  "+c.dim+cont+"└ "+act+c.reset)
		}
	}
	if rest := len(order) - len(shown); rest > 0 {
		lines = append(lines, fmt.Sprintf("  %s└─ … %d more%s", c.dim, rest, c.reset))
	}
	return lines
}

// subRow is "● Name · 12.3s · 3 tools · 2 req · ↑12k ↓800" with a state
// mark; live rows animate the running mark.
func subRow(c palette, s agent.SubagentStatus, live bool) string {
	var mark, color string
	switch s.State {
	case agent.SubRunning:
		mark, color = "●", c.magenta
		if live {
			mark = spinner[int(time.Since(s.Started)/(100*time.Millisecond))%len(spinner)]
		}
	case agent.SubQueued:
		mark, color = "○", c.dim
	case agent.SubDone:
		mark, color = "✓", c.green
	default:
		mark, color = "✗", c.red
	}
	parts := []string{color + mark + " " + c.reset + c.bold + s.Name + c.reset}
	switch {
	case s.State == agent.SubQueued:
		parts = append(parts, c.dim+"queued"+c.reset)
	case !s.Started.IsZero():
		end := s.Ended
		if end.IsZero() {
			end = time.Now()
		}
		stats := []string{fmt.Sprintf("%.1fs", end.Sub(s.Started).Seconds())}
		if s.Tools > 0 {
			stats = append(stats, plural(s.Tools, "tool"))
		}
		if s.Requests > 0 {
			stats = append(stats, fmt.Sprintf("%d req", s.Requests))
		}
		if s.Window > 0 && s.CtxTokens > 0 {
			stats = append(stats, fmt.Sprintf("%.1f%%/%s", float64(s.CtxTokens)*100/float64(s.Window), fmtTokens(s.Window)))
		}
		if u := s.Usage; u.Output > 0 {
			stats = append(stats, fmt.Sprintf("↑%s ↓%s", fmtTokens(u.Input+u.CacheRead+u.CacheWrite), fmtTokens(u.Output)))
		}
		parts = append(parts, c.dim+strings.Join(stats, " · ")+c.reset)
	}
	if s.State == agent.SubFailed && s.Err != "" {
		parts = append(parts, c.red+s.Err+c.reset)
	}
	return strings.Join(parts, c.dim+" · "+c.reset)
}

// runningAgents counts subagents queued or running (for the header).
func (a *app) runningAgents() int {
	ag := a.footerAgent.Load()
	if ag == nil {
		return 0
	}
	n := 0
	for _, s := range ag.Subagents() {
		if s.State == agent.SubRunning || s.State == agent.SubQueued {
			n++
		}
	}
	return n
}

// ---- transcript boxes ----

// todoBox frames the list a todo call set, every phase expanded.
func (u *termUI) todoBox(input json.RawMessage, output string, isErr bool) string {
	c, border := u.dc, u.dc.dim
	var b strings.Builder
	list, err := agent.ParseTodos(input)
	if isErr || err != nil {
		border = c.red
		b.WriteString(u.boxTop("Todo", "", "✗", border))
		u.plainRows(&b, strings.Split(strings.TrimRight(output, "\n"), "\n"), c.red, border, boxBodyMax)
		b.WriteString(u.boxBottom("", border))
		return b.String()
	}
	done := 0
	for _, td := range list {
		if td.Status == "completed" {
			done++
		}
	}
	subject := fmt.Sprintf("%d/%d done", done, len(list))
	if len(list) == 0 {
		subject = "cleared"
	}
	b.WriteString(u.boxTop("Todo", subject, "", border))
	item := func(indent string, td agent.Todo, last bool) {
		mark, color := todoMark(c, td.Status)
		br := "├─ "
		if last {
			br = "└─ "
		}
		text := indent + br + mark + " " + td.Content
		for i, l := range wrapCells(text, u.inner()) {
			if i == 0 {
				// Re-style the first row: tree dim, mark and text coloured.
				l = c.dim + indent + br + c.reset + color + strings.TrimPrefix(l, indent+br)
			} else {
				l = color + l
			}
			u.rowClip(&b, l, runewidth.StringWidth(stripSGR(l)), border)
		}
	}
	phases := agent.TodoPhases(list)
	for pi, ph := range phases {
		indent := ""
		if name := agent.PhaseName(phases, pi); name != "" {
			head := fmt.Sprintf("%d. %s", pi+1, name)
			count := fmt.Sprintf("  %d/%d", ph.Done, len(ph.Items))
			color := c.yellow + c.bold
			if ph.Done == len(ph.Items) {
				color = c.green + c.bold
			}
			u.rowClip(&b, color+head+c.reset+c.dim+count, runewidth.StringWidth(head+count), border)
			indent = "  "
		}
		for i, td := range ph.Items {
			item(indent, td, i == len(ph.Items)-1)
		}
	}
	b.WriteString(u.boxBottom("", border))
	return b.String()
}

type taskInput struct {
	Tasks []struct {
		Name   string `json:"name"`
		Prompt string `json:"prompt"`
	} `json:"tasks"`
}

// taskBoxTop is the head of the Task box: one row per subagent with the
// start of its prompt.
func (u *termUI) taskBoxTop(in taskInput, border string) string {
	c := u.dc
	var b strings.Builder
	b.WriteString(u.boxTop("Task", plural(len(in.Tasks), "agent"), "", border))
	for i, t := range in.Tasks {
		name := t.Name
		if name == "" {
			name = fmt.Sprintf("task%d", i+1)
		}
		prompt := strings.Join(strings.Fields(t.Prompt), " ")
		row := "• " + name + "  " + prompt
		styled := c.yellow + "• " + c.reset + c.bold + name + c.reset + c.dim + "  " + prompt
		u.rowClip(&b, styled, runewidth.StringWidth(row), border)
	}
	return b.String()
}

// taskReportRe finds each subagent's "## Name (status)" report header in
// the task tool output.
var taskReportRe = regexp.MustCompile(`(?m)^## (\S+) \((done|failed: .*|cancelled before it started)\)$`)

// taskBox frames a finished task call: the prompts, then each subagent's
// outcome (while it runs, the Subagents panel shows it live). The live
// stats are used when they belong to this call (same prompts, all
// finished); a replayed older call shows what its output says.
func (u *termUI) taskBox(input json.RawMessage, output string, isErr bool) string {
	var in taskInput
	_ = json.Unmarshal(input, &in)
	var subs []agent.SubagentStatus
	if u.subagents != nil {
		subs = u.subagents()
	}
	match := len(subs) == len(in.Tasks)
	for i := range subs {
		if !match {
			break
		}
		match = subs[i].Prompt == in.Tasks[i].Prompt && subs[i].State != agent.SubRunning && subs[i].State != agent.SubQueued
	}
	if !match {
		subs = nil
		for _, m := range taskReportRe.FindAllStringSubmatch(output, -1) {
			s := agent.SubagentStatus{Name: m[1], State: agent.SubDone}
			if m[2] != "done" {
				s.State, s.Err = agent.SubFailed, strings.TrimPrefix(m[2], "failed: ")
			}
			subs = append(subs, s)
		}
	}
	border := u.dc.dim
	failed := 0
	for _, s := range subs {
		if s.State == agent.SubFailed {
			failed++
		}
	}
	if failed > 0 || isErr {
		border = u.dc.red
	}
	var b strings.Builder
	b.WriteString(u.taskBoxTop(in, border))
	if len(subs) > 0 {
		fmt.Fprintf(&b, "%s├%s┤%s\n", border, strings.Repeat("─", max(u.width(), 14)-2), u.dc.reset)
		for _, s := range subs {
			row := subRow(u.dc, s, false)
			u.rowClip(&b, row, runewidth.StringWidth(stripSGR(row)), border)
		}
	}
	mark := ""
	switch {
	case failed > 0:
		mark = fmt.Sprintf("✗ %d failed", failed)
	case isErr:
		mark = "✗ failed"
	}
	b.WriteString(u.boxBottom(mark, border))
	return b.String()
}

// LiveSubagents tells the task tool that subagents are shown in the panel
// (no per-event info lines) when this UI draws boxes.
func (u *termUI) LiveSubagents() bool { return u.boxed("task") }

// stripSGR removes SGR escapes (for measuring styled text).
func stripSGR(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			if j := strings.IndexByte(s[i:], 'm'); j >= 0 {
				i += j
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ---- subagent viewer ----

// agentsCommand lets the user pick a subagent of the latest task call and
// prints what it did: its prompt, tool calls and latest answer.
func (a *app) agentsCommand() {
	subs := a.ag.Subagents()
	if len(subs) == 0 {
		a.ui.Dim("(no subagents yet)")
		return
	}
	c := a.ui.c
	items := make([]lineedit.PickItem, len(subs))
	sel := 0
	for i, s := range subs {
		items[i] = lineedit.PickItem{Label: stripSGR(subRow(c, s, false))}
		if p := strings.Join(strings.Fields(s.Prompt), " "); p != "" {
			items[i].Detail = p
		}
		if s.State == agent.SubRunning && subs[sel].State != agent.SubRunning {
			sel = i
		}
	}
	idx, err := a.tui.Pick("Subagents · enter: show its activity", items, "", sel)
	if err != nil || idx < 0 {
		return
	}
	s := a.ag.Subagents()[idx] // fresh: it may have moved on while picking
	var b strings.Builder
	w := 80
	if a.ui.width != nil {
		w = max(a.ui.width(), 20)
	}
	title := " " + stripSGR(subRow(c, s, false)) + " "
	fmt.Fprintf(&b, "%s──%s%s%s\n", c.dim, c.reset+c.bold+title+c.reset+c.dim, strings.Repeat("─", max(w-2-runewidth.StringWidth(title), 0)), c.reset)
	prompt := strings.Split(strings.TrimSpace(s.Prompt), "\n")
	for i, l := range prompt {
		if i == 4 {
			fmt.Fprintf(&b, "%s  … %d more prompt lines%s\n", c.dim, len(prompt)-4, c.reset)
			break
		}
		for _, r := range wrapCells(l, w-2) {
			fmt.Fprintf(&b, "%s  %s%s\n", c.dim, r, c.reset)
		}
	}
	if len(s.Log) > 0 {
		b.WriteString("\n")
	}
	for _, l := range s.Log {
		fmt.Fprintf(&b, "%s%s●%s %s%s\n", c.cyan, c.bold, c.reset+c.cyan, shortenPaths(l, a.cwd), c.reset)
	}
	if s.Text != "" {
		b.WriteString("\n")
		for _, r := range wrapCells(s.Text, w) {
			b.WriteString(r + "\n")
		}
	}
	a.ui.Finish()
	fmt.Fprint(a.out, b.String())
}
