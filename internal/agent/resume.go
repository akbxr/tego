package agent

import (
	"fmt"
	"slices"
	"strings"
)

// resumedNote starts the prompt of a resumed subagent.
const resumedNote = "<system-reminder>The agent that started you resumed you with your context. Any limit you reached is reset: " +
	"you have a full budget of requests again and your tools work. Earlier notes that told you to stop no longer apply.</system-reminder>\n\n"

// endedMax is how many ended subagents a session keeps to resume.
const endedMax = 8

// endedSub is a subagent that ended, kept with its context so a later task
// can resume it ("resume" on a task) instead of starting over: one stopped at
// its turn limit or by the watchdog, interrupted, or done and asked for more.
// Worktree subagents are not kept: their worktree is committed or removed
// when they end.
type endedSub struct {
	sub   *Agent
	id    string // its background id ("t3"), if it ran in the background
	agent string // its agent definition's name, if any
	task  string // what it was asked, for its watchdog
}

// ref is how a task names it in "resume".
func (e *endedSub) ref() string {
	if e.id != "" {
		return e.id
	}
	return e.sub.Name
}

// keepEnded keeps e, forgetting the oldest past endedMax.
func (a *Agent) keepEnded(e *endedSub) {
	a.endedMu.Lock()
	defer a.endedMu.Unlock()
	a.ended = append(a.ended, e)
	if len(a.ended) > endedMax {
		a.ended = slices.Delete(a.ended, 0, len(a.ended)-endedMax)
	}
}

// takeEnded takes out the ended subagent ref names, a background id or a
// name (the latest of that name), so that no other task resumes it too.
func (a *Agent) takeEnded(ref string) (*endedSub, error) {
	a.endedMu.Lock()
	defer a.endedMu.Unlock()
	ref = strings.TrimSpace(ref)
	idx := slices.IndexFunc(a.ended, func(e *endedSub) bool { return e.id != "" && e.id == ref })
	if idx < 0 {
		for i := len(a.ended) - 1; i >= 0; i-- {
			if strings.EqualFold(a.ended[i].sub.Name, ref) {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		if len(a.ended) == 0 {
			return nil, fmt.Errorf("no subagent %q to resume: none can be in this session (worktree subagents can't be); start a new one", ref)
		}
		refs := make([]string, len(a.ended))
		for i, e := range a.ended {
			refs[i] = e.ref()
		}
		return nil, fmt.Errorf("no subagent %q to resume; these can be: %s", ref, strings.Join(refs, ", "))
	}
	e := a.ended[idx]
	a.ended = slices.Delete(a.ended, idx, idx+1)
	return e, nil
}

// forgetEnded drops the kept subagents: another session or branch.
func (a *Agent) forgetEnded() {
	a.endedMu.Lock()
	a.ended = nil
	a.endedMu.Unlock()
}
