package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/lineedit"
	"github.com/akbxr/tego/internal/tools"
)

// setupBackground lets the agent run task subagents in the background
// (task with background: true) and adds task_status.
func (a *app) setupBackground(task *agent.TaskTool) {
	a.bg = &agent.Background{OnDone: a.backgroundDone}
	a.bgDone = make(chan struct{}, 1)
	a.footerBG.Store(a.bg)
	task.Background = a.bg
	shells := a.ag.Notices
	a.ag.Notices = func() string {
		var notes []string
		for _, n := range []string{shells(), a.bg.Notice()} {
			if n != "" {
				notes = append(notes, n)
			}
		}
		return strings.Join(notes, "\n\n")
	}
}

// backgroundDone says that a background task ended and wakes the REPL,
// which hands the report to the agent if nothing else does.
func (a *app) backgroundDone(id string) {
	for _, s := range a.bg.Statuses() {
		if s.ID != id {
			continue
		}
		mark, state := "✓", "done"
		if s.State == agent.SubFailed {
			mark, state = "✗", "failed"
			if s.Err != "" {
				state += ": " + s.Err
			}
		}
		line := fmt.Sprintf("%s [%s] %s %s in the background · %s", mark, s.Name, id, state, fmtElapsed(s.Ended.Sub(s.Started)))
		if s.Started.IsZero() {
			line = fmt.Sprintf("%s [%s] %s %s in the background", mark, s.Name, id, state)
		}
		if s.State == agent.SubFailed {
			a.ui.OnInfo(line)
		} else {
			a.ui.Dim("%s", line)
		}
	}
	if a.tui != nil {
		a.tui.Redraw()
	}
	select {
	case a.bgDone <- struct{}{}:
	default:
	}
}

func fmtElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}

// backgroundPrompt is the message that hands background reports to the
// agent in a turn of their own.
func backgroundPrompt(reports string) string {
	return "<system-reminder>" + reports + "\nTell the user what came of them, briefly, and go on if the work needs more.</system-reminder>"
}

// startBackgroundReports hands the reports that wait to the agent in a
// turn of their own; false if there were none.
func (a *app) startBackgroundReports(done chan<- error) bool {
	reports := a.bg.Take()
	if reports == "" {
		return false
	}
	a.startJob(done, func(ctx context.Context) error { return a.ag.Run(ctx, backgroundPrompt(reports)) })
	return true
}

// drainBackground (tego -p) waits for the background tasks and lets the
// agent answer their reports, until none runs.
func (a *app) drainBackground(ctx context.Context) error {
	for {
		if !a.bg.Pending() {
			if a.bg.Running() == 0 {
				return nil
			}
			if err := a.bg.Wait(ctx, nil, 0); err != nil {
				return err
			}
			continue
		}
		if reports := a.bg.Take(); reports != "" {
			if err := a.ag.Run(ctx, backgroundPrompt(reports)); err != nil {
				return err
			}
		}
	}
}

// stopBackground stops the background tasks when tego exits.
func (a *app) stopBackground() {
	if n := a.bg.Close(10 * time.Second); n > 0 && a.interactive {
		a.ui.Dim("stopped %s", plural(n, "background task"))
	}
}

// resetBackground stops the background tasks of the session being left.
func (a *app) resetBackground() {
	if n := a.bg.Reset(); n > 0 {
		a.ui.Dim("stopped %s of the previous session", plural(n, "background task"))
	}
}

// confirmExit asks before exiting while background tasks run.
func (a *app) confirmExit() bool {
	n := a.bg.Running()
	if n == 0 || a.tui == nil {
		return true
	}
	items := []lineedit.PickItem{
		{Label: "Stop them and exit"},
		{Label: "Keep working", Detail: "/tasks shows them"},
	}
	idx, err := a.tui.Pick(fmt.Sprintf("%s still running in the background", plural(n, "task")), items, "", 1)
	return err == nil && idx == 0
}

// shellRow is a background shell in /tasks: "▶ bg1 npm run dev · running
// for 3m0s · :3000".
func shellRow(s tools.ShellInfo) string {
	mark := "▶"
	if !s.Running {
		mark = "■"
	}
	parts := []string{mark + " " + s.ID + " " + oneLineCut(s.Command, 50), s.Status}
	if s.Port != "" {
		parts = append(parts, s.Port)
	}
	if s.Owner != "" {
		parts = append(parts, "by "+strings.SplitN(s.Owner, "#", 2)[0])
	}
	return strings.Join(parts, " · ")
}

func oneLineCut(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

// shellAction shows a shell's latest output, or stops it.
func (a *app) shellAction(s tools.ShellInfo) {
	if s.Running {
		act, err := a.tui.Pick(s.ID+" "+oneLineCut(s.Command, 60), []lineedit.PickItem{{Label: "Show its output"}, {Label: "Stop it"}}, "", 0)
		if err != nil || act < 0 {
			return
		}
		if act == 1 {
			if a.env.StopShell(s.ID) {
				a.ag.Notice(fmt.Sprintf("The user stopped background shell %s (%s).", s.ID, oneLineCut(s.Command, 60)))
				a.ui.Dim("stopped %s", s.ID)
			}
			return
		}
	}
	c := a.ui.c
	a.ui.Finish()
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s%s %s %s· %s%s\n", c.bold, s.ID, c.reset, s.Command, c.dim, s.Status, c.reset)
	if tail := a.env.ShellTail(s.ID, 40); tail != "" {
		fmt.Fprintf(&b, "%s%s%s\n", c.dim, tail, c.reset)
	} else {
		fmt.Fprintf(&b, "%s(no output)%s\n", c.dim, c.reset)
	}
	fmt.Fprint(a.out, b.String())
}

// shellRecord is where this tego keeps its running shells' groups.
func (a *app) shellRecord() string {
	return filepath.Join(a.cfg.Dir, "shells", fmt.Sprintf("%d.json", os.Getpid()))
}

// offerStaleShells asks about processes a tego that crashed left running.
func (a *app) offerStaleShells() {
	stale := tools.StaleShells(filepath.Dir(a.shellRecord()))
	if len(stale) == 0 {
		return
	}
	var lines []string
	for _, s := range stale {
		line := fmt.Sprintf("%s (in %s, started %s)", oneLineCut(s.Command, 50), s.Cwd, s.Started.Format("Jan 2 15:04"))
		if len(s.Members) > 0 {
			line += ": now " + strings.Join(s.Members, "; ")
		}
		lines = append(lines, line)
	}
	title := fmt.Sprintf("A tego that ended without stopping them left %s running", plural(len(stale), "background shell"))
	items := []lineedit.PickItem{
		{Label: "Stop them", Detail: strings.Join(lines, " | ")},
		{Label: "Leave them running", Detail: "tego won't ask about them again"},
	}
	idx, err := a.tui.Pick(title, items, "", 0)
	if err != nil || idx < 0 {
		return // asked again next time
	}
	if idx == 0 {
		tools.StopStale(stale)
		a.ui.Dim("stopped %s left by an earlier tego", plural(len(stale), "background shell"))
		return
	}
	tools.ForgetStale(stale)
}

// stopTask stops a background task for the user. It gets no report: the
// agent hears with its next request that the user stopped it.
func (a *app) stopTask(s agent.SubagentStatus) {
	if !a.bg.Stop(s.ID) {
		return // it ended meanwhile; its report comes as usual
	}
	a.ag.Notice(fmt.Sprintf("The user stopped background task %s %s; it won't report.", s.ID, s.Name))
	a.ui.Dim("stopped %s %s", s.ID, s.Name)
}

// runningBackground is the background tasks queued or running, for the
// footer (any goroutine).
func (a *app) runningBackground() []agent.SubagentStatus {
	var out []agent.SubagentStatus
	for _, s := range a.footerBG.Load().Statuses() {
		if s.State == agent.SubRunning || s.State == agent.SubQueued {
			out = append(out, s)
		}
	}
	return out
}

// tasksCommand (/tasks) lists the background tasks: pick one to see what
// it did, or stop it.
func (a *app) tasksCommand() {
	subs := a.bg.Statuses()
	shells := a.env.Shells()
	if len(subs) == 0 && len(shells) == 0 {
		a.ui.Dim("nothing runs in the background (subagents started with background: true, and shells, such as a dev server, show here)")
		return
	}
	c := a.ui.c
	items := make([]lineedit.PickItem, 0, len(subs)+len(shells))
	for _, s := range subs {
		items = append(items, lineedit.PickItem{Label: stripSGR(subRow(c, s, false)), Detail: strings.Join(strings.Fields(s.Prompt), " ")})
	}
	for _, s := range shells {
		items = append(items, lineedit.PickItem{Label: shellRow(s), Detail: strings.Join(strings.Fields(s.Command), " ")})
	}
	idx, err := a.tui.Pick("Background tasks and shells · enter: show or stop", items, "", len(items)-1)
	if err != nil || idx < 0 {
		return
	}
	if idx >= len(subs) {
		a.shellAction(shells[idx-len(subs)])
		return
	}
	s := subs[idx]
	if s.State == agent.SubRunning || s.State == agent.SubQueued {
		act, err := a.tui.Pick(s.ID+" "+s.Name, []lineedit.PickItem{{Label: "Show its activity"}, {Label: "Stop it"}}, "", 0)
		if err != nil || act < 0 {
			return
		}
		if act == 1 {
			a.stopTask(s)
			return
		}
	}
	for _, f := range a.bg.Statuses() { // fresh: it may have moved on while picking
		if f.ID == s.ID {
			s = f
		}
	}
	a.printSubagent(s)
}
