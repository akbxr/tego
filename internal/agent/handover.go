package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/akbxr/tego/internal/tools"
)

// A task call waits for its subagents, but not past a message sent to the
// running turn (the user's steering, a peer session's): when one comes,
// the subagents still at work go on as background tasks and the call
// returns, so the agent reads the message now rather than after them. It
// can pass it on to them (task_status message), cancel them or wait for
// them; nothing they did is lost. Interrupting the turn before that stops
// them, as it always did. Without background tasks (a subagent's own task
// calls, if any) the call waits to the end.

// handOverNote ends a task call that a message cut short.
const handOverNote = "A message came while these subagents worked, so they went on in the background: %s. " +
	"Each report comes in a message when its task ends (task_status checks, waits for, messages or cancels them). " +
	"Read the message, which follows: pass on what changes their work with task_status message, cancel them if it makes their work moot, " +
	"or let them finish. Don't do their work yourself meanwhile."

// callTask is one task of a call that runs them itself.
type callTask struct {
	ended          bool
	status, report string
	cancel         context.CancelFunc
	bt             *bgTask // set when it moved to the background
}

// runCall runs a call's tasks, at most limit at once, and waits for them
// (or for a message, see above): their reports, one section each.
func (t *TaskTool) runCall(ctx context.Context, specs []taskSpec, defs []*AgentDef, models []taskModel,
	states []*SubagentStatus, limit int, quiet bool) (string, error) {
	var wake <-chan struct{}
	if t.Background != nil {
		wake = tools.Wake(ctx)
	}
	// Each runs on a context of its own, which the call's stops only while
	// the call waits for it.
	base := context.WithoutCancel(ctx)
	slots := make(chan struct{}, limit)
	var mu sync.Mutex
	runs := make([]*callTask, len(specs))
	var wg sync.WaitGroup
	for i, task := range specs {
		taskCtx, cancel := context.WithCancel(base)
		runs[i] = &callTask{cancel: cancel}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()
			status, report := t.runOne(taskCtx, task, defs[i], models[i], states[i], slots, quiet)
			mu.Lock()
			r := runs[i]
			r.ended, r.status, r.report = true, status, report
			bt := r.bt
			mu.Unlock()
			if bt != nil {
				t.Background.adoptedDone(bt, status, report)
			}
		}()
	}
	unlink := context.AfterFunc(ctx, func() {
		for _, r := range runs {
			r.cancel()
		}
	})
	defer unlink()
	all := make(chan struct{})
	go func() { wg.Wait(); close(all) }()
	select {
	case <-all:
	case <-ctx.Done():
		<-all
	case <-wake:
		if unlink() { // not interrupted meanwhile
			if out, ok := t.handOver(specs, states, runs, &mu); ok {
				return out, nil
			}
		}
		<-all
	}
	reports := make([]string, len(runs))
	for i, r := range runs {
		reports[i] = callSection(specs[i].Name, r.status, r.report)
	}
	return strings.Join(reports, "\n\n"), ctx.Err()
}

func callSection(name, status, report string) string {
	s := fmt.Sprintf("## %s (%s)", name, status)
	if report != "" {
		s += "\n" + report
	}
	return s
}

// handOver moves the tasks of runs still at work to the background and
// returns the call's result: the reports of those that ended, then where
// the others went. False when none is at work.
func (t *TaskTool) handOver(specs []taskSpec, states []*SubagentStatus, runs []*callTask, mu *sync.Mutex) (string, bool) {
	mu.Lock()
	defer mu.Unlock()
	working := false
	for _, r := range runs {
		working = working || !r.ended
	}
	if !working {
		return "", false
	}
	var sections, moved []string
	var stayed []*SubagentStatus
	for i, r := range runs {
		if r.ended {
			sections = append(sections, callSection(specs[i].Name, r.status, r.report))
			stayed = append(stayed, states[i])
			continue
		}
		r.bt = t.Background.adopt(t.Parent, specs[i].Prompt, states[i], r.cancel)
		moved = append(moved, r.bt.id+" "+specs[i].Name)
	}
	p := t.Parent
	p.subMu.Lock()
	p.subs = stayed // the moved ones show with the background tasks now
	p.subMu.Unlock()
	sections = append(sections, fmt.Sprintf(handOverNote, strings.Join(moved, ", ")))
	return strings.Join(sections, "\n\n"), true
}
