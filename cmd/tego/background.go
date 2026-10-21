package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/lineedit"
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
	if len(subs) == 0 {
		a.ui.Dim("no background tasks (tego runs them when it starts subagents with background: true)")
		return
	}
	c := a.ui.c
	items := make([]lineedit.PickItem, len(subs))
	sel := len(subs) - 1
	for i, s := range subs {
		items[i] = lineedit.PickItem{Label: stripSGR(subRow(c, s, false)), Detail: strings.Join(strings.Fields(s.Prompt), " ")}
	}
	idx, err := a.tui.Pick("Background tasks · enter: show or stop", items, "", sel)
	if err != nil || idx < 0 {
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
