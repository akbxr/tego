package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/ext"
	"github.com/akbxr/tego/internal/session"
)

// After a turn that ended well, another may start without the user: an
// extension's stop handler asks for one (tests still fail), or the /goal
// set is not met yet, as a check by the small model finds.
const (
	stopRounds  = 3 // turns in a row extensions may ask for before it is the user's
	goalIdleMax = 3 // checked turns in a row without a tool call before the goal waits
)

// goalRun is the state of the session's /goal, under app.goalMu. The
// condition is also in the session file, so a resumed or restarted session
// keeps working on it.
type goalRun struct {
	init    bool
	sess    *session.Session // the session it is of
	cond    string           // "" = no goal
	since   time.Time
	turns   int    // turns checked
	reason  string // what the latest check said
	idle    int    // checked turns in a row without a tool call
	held    string // why it waits for the user's next prompt ("" = it runs)
	rounds  int    // turns in a row extensions asked for
	last    string // the latest goal that ended, for /goal
	checked bool   // terminal: the job that ended checked, and started no turn
}

// goalSyncLocked takes the session's goal when the session changed
// (/resume, /new, a restart); a.goalMu is held.
func (a *app) goalSyncLocked() *goalRun {
	g := &a.goal
	if s := a.ag.Session; !g.init || s != g.sess {
		*g = goalRun{init: true, sess: s, cond: s.Goal(), since: time.Now(), last: g.last}
	}
	return g
}

// goalFresh starts the user's prompt: a goal that waited goes on, and
// extensions get their rounds again.
func (a *app) goalFresh() {
	a.goalMu.Lock()
	g := a.goalSyncLocked()
	g.held, g.idle, g.rounds = "", 0, 0
	a.goalMu.Unlock()
}

// goalFailed holds the goal after a turn failed: it is the user's to look
// at first.
func (a *app) goalFailed(err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	why := "the turn failed"
	if errors.Is(err, agent.ErrRefused) {
		why = "the model refused to go on"
	}
	a.goalMu.Lock()
	g := a.goalSyncLocked()
	held := g.cond != "" && g.held == ""
	if held {
		g.held = why
	}
	a.goalMu.Unlock()
	if held {
		a.dim("◎ goal paused: " + why + ". It goes on after your next prompt; /goal clear ends it.")
	}
}

// goalLabel is the status bar's segment, "" without a goal.
func (a *app) goalLabel() string {
	a.goalMu.Lock()
	defer a.goalMu.Unlock()
	switch {
	case a.goal.cond == "":
		return ""
	case a.goal.held != "":
		return "◎ goal paused"
	}
	return "◎ goal"
}

// goalCommand is /goal: with a condition it sets the goal and returns the
// prompt that starts work on it; without, it says how the goal goes;
// "clear" ends it.
func (a *app) goalCommand(args string) (reply, prompt string) {
	a.goalMu.Lock()
	defer a.goalMu.Unlock()
	g := a.goalSyncLocked()
	switch strings.ToLower(args) {
	case "":
		if g.cond == "" {
			reply = "No goal set. /goal <condition> sets one: tego works toward it, and after each turn a small model checks the work for it."
			if g.last != "" {
				reply += "\nLast: " + g.last
			}
			return reply, ""
		}
		reply = fmt.Sprintf("◎ goal: %s\n  for %s · %s checked", g.cond, shortDuration(time.Since(g.since)), plural(g.turns, "turn"))
		if g.reason != "" {
			reply += "\n  latest check: " + g.reason
		}
		if g.held != "" {
			reply += "\n  paused: " + g.held + "; it goes on after your next prompt"
		}
		return reply, ""
	case "clear", "stop", "off", "reset", "none", "cancel":
		if g.cond == "" {
			return "No goal set", ""
		}
		cond := g.cond
		a.goalEndLocked("cleared after " + plural(g.turns, "turn") + ": " + cond)
		return "Goal cleared: " + cond, ""
	}
	if n := len([]rune(args)); n > 4000 {
		return fmt.Sprintf("The condition is %d characters; 4000 at most.", n), ""
	}
	*g = goalRun{init: true, sess: g.sess, cond: args, since: time.Now(), last: g.last}
	reply = "◎ goal set: " + args
	if err := g.sess.SetGoal(args); err != nil {
		reply += fmt.Sprintf(" (not recorded in the session file, so a resume won't keep it: %v)", err)
	}
	return reply, goalStartPrompt(args)
}

// goalEndLocked clears the goal; last says how it ended. No a.dim under
// a.goalMu: drawing the status bar takes it.
func (a *app) goalEndLocked(last string) {
	g := &a.goal
	_ = g.sess.SetGoal("") // a failed write fails the next message's too, which says so
	*g = goalRun{init: true, sess: g.sess, since: time.Now(), last: last}
}

func goalStartPrompt(cond string) string {
	return "Goal: " + cond + "\n\nWork toward this goal until it holds. After each of your turns a separate check reads what you did " +
		"and decides whether it holds. It can't run anything itself, so show the evidence: run the commands that prove it and let their output show."
}

func goalNextPrompt(cond, reason string) string {
	return "The goal is not met yet. The check after your turn says: " + reason + "\n\nGoal: " + cond +
		"\n\nKeep working toward it. (This comes from tego's goal check, not from the user.)"
}

func stopPrompt(name, why string) string {
	return "The extension " + name + " checked your work as the turn ended, and asks for more before it is done: " + why +
		"\n\n(This comes from the extension, not from the user.)"
}

// continues reports whether a turn that ended well may be followed by
// another without the user: what continuation decides.
func (a *app) continues() bool {
	if a.plan.Load() || a.bg.Running() > 0 {
		return false // plan mode changes nothing; background work: once its reports' turn ends
	}
	if a.ext != nil && a.ext.Wants(ext.EventStop) {
		return true
	}
	a.goalMu.Lock()
	defer a.goalMu.Unlock()
	g := a.goalSyncLocked()
	return g.cond != "" && g.held == ""
}

// continuation decides, once a turn ended well, whether another starts
// without the user, and returns its prompt ("" = wait for the user).
// Extensions are asked first, then the goal is checked.
func (a *app) continuation(ctx context.Context) string {
	if !a.continues() {
		return ""
	}
	if a.ext != nil && a.ext.Wants(ext.EventStop) {
		a.goalMu.Lock()
		rounds := a.goalSyncLocked().rounds
		a.goalMu.Unlock()
		name, why := a.ext.Stop(ctx, a.ag.LastText(), rounds)
		if ctx.Err() != nil {
			return ""
		}
		if why != "" {
			if rounds >= stopRounds {
				a.dim("◎ %s still asks for more after %d rounds: %s. Over to you.", name, rounds, clipRunes(why, 400))
				return ""
			}
			a.goalMu.Lock()
			a.goal.rounds++
			a.goalMu.Unlock()
			a.dim("◎ %s asks for another turn: %s", name, clipRunes(why, 400))
			return stopPrompt(name, why)
		}
	}
	return a.goalNext(ctx)
}

// goalNext checks the goal after a turn: "" when it is met, given up or
// waits for the user, else the next turn's prompt.
func (a *app) goalNext(ctx context.Context) string {
	a.goalMu.Lock()
	g := a.goalSyncLocked()
	if g.cond == "" || g.held != "" {
		a.goalMu.Unlock()
		return ""
	}
	if a.ag.LastTurnUsedTools() {
		g.idle = 0
	} else if g.idle++; g.idle >= goalIdleMax {
		g.held = fmt.Sprintf("%d turns in a row without a tool call", g.idle)
		a.goalMu.Unlock()
		a.dim("◎ goal paused: %d turns in a row without a tool call. It goes on after your next prompt; /goal clear ends it.", goalIdleMax)
		return ""
	}
	cond, turns := g.cond, g.turns
	a.goalMu.Unlock()

	v, err := a.ag.CheckGoal(ctx, a.goalModel(), cond, turns)
	next, say := a.goalVerdict(ctx, cond, v, err)
	if say != "" {
		a.dim("%s", say)
	}
	return next
}

// goalVerdict records what a check found: the next turn's prompt, and the
// line for the user.
func (a *app) goalVerdict(ctx context.Context, cond string, v agent.GoalVerdict, err error) (next, say string) {
	a.goalMu.Lock()
	defer a.goalMu.Unlock()
	g := a.goalSyncLocked()
	if g.cond != cond { // cleared or replaced meanwhile
		return "", ""
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", ""
		}
		g.held = "the check failed"
		return "", fmt.Sprintf("◎ goal paused: the check failed: %v. It goes on after your next prompt; /goal clear ends it.", err)
	}
	g.turns++
	g.reason = v.Reason
	took := fmt.Sprintf("after %s in %s", plural(g.turns, "turn"), shortDuration(time.Since(g.since)))
	switch v.State {
	case agent.GoalMet:
		a.goalEndLocked("met " + took + ": " + cond)
		return "", fmt.Sprintf("◎ goal met %s: %s", took, v.Reason)
	case agent.GoalImpossible:
		a.goalEndLocked("given up " + took + ": " + cond)
		return "", fmt.Sprintf("◎ goal given up %s, as the check finds it can't hold: %s", took, v.Reason)
	}
	return goalNextPrompt(cond, v.Reason), "◎ goal not met yet: " + v.Reason
}

// goalModel checks the goal: the small model, else the session's.
func (a *app) goalModel() agent.SmallModel {
	if m, ok := a.small(); ok {
		return m
	}
	return agent.SmallModel{Provider: a.ag.Provider, ProviderName: a.ag.ProviderName, Model: a.ag.Model}
}

// runContinuation runs a turn continuation asked for, as runTurn runs the
// user's, without the input hook: it is not the user's.
func (a *app) runContinuation(ctx context.Context, prompt string) error {
	line, _, _ := strings.Cut(prompt, "\n")
	a.snapshotFiles(ctx, line)
	err := a.ag.Run(ctx, prompt)
	if a.ext != nil {
		a.ext.Emit(ext.EventTurnEnd, map[string]any{"text": a.ag.LastText()})
	}
	a.goalFailed(err)
	return err
}

// startContinuation checks, once a job ended well in the terminal, whether
// another turn follows, and runs it: a job of its own, so the check shows
// and Esc stops it.
func (a *app) startContinuation(done chan<- error) {
	a.goalMu.Lock()
	checked := a.goal.checked
	a.goal.checked = false
	a.goalMu.Unlock()
	if checked || !a.continues() {
		return
	}
	a.jobMsg = "checking the goal"
	a.startJob(done, func(ctx context.Context) error {
		next := a.continuation(ctx)
		if next == "" {
			a.goalMu.Lock()
			a.goal.checked = true
			a.goalMu.Unlock()
			return nil
		}
		return a.runContinuation(ctx, next)
	})
}
