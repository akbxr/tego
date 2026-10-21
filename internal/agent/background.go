package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// Background runs the tasks of task calls with "background": true. The
// call returns at once; the subagents go on while the session does, and
// each report comes back once: in a notice before the session's next
// request (Notice), from task_status, or, when the session is idle, in a
// turn of its own the UI starts on OnDone (Take).
type Background struct {
	// OnDone is called on a task's goroutine when it ends (not when Reset or
	// Close stopped it).
	OnDone func(id string)

	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	slots  chan struct{} // the tasks running at once, across calls
	tasks  []*bgTask     // oldest first
	next   int           // the last id's number
	wg     sync.WaitGroup
	change chan struct{} // closed and replaced when a task ends
}

type bgTask struct {
	id     string
	prompt string
	st     *SubagentStatus
	status string // set when it ends: "done", "failed: …"
	report string
	parent *Agent
	cancel context.CancelFunc

	ended     bool
	delivered bool // its report was handed to the session
	dropped   bool // stopped by Reset or Close: nobody wants its report
}

// bgKeep caps the ended tasks kept for task_status and /tasks.
const bgKeep = 30

// start runs the tasks of one call in the background and says so.
func (b *Background) start(t *TaskTool, specs []taskSpec, defs []*AgentDef, models []taskModel, states []*SubagentStatus) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctx == nil {
		b.ctx, b.cancel = context.WithCancel(context.Background())
	}
	if b.slots == nil {
		b.slots = make(chan struct{}, t.concurrency())
	}
	if b.change == nil {
		b.change = make(chan struct{})
	}
	started := make([]string, len(specs))
	for i, spec := range specs {
		b.next++
		id := fmt.Sprintf("t%d", b.next)
		states[i].ID = id
		ctx, cancel := context.WithCancel(b.ctx)
		bt := &bgTask{id: id, prompt: spec.Prompt, st: states[i], parent: t.Parent, cancel: cancel}
		b.tasks = append(b.tasks, bt)
		started[i] = id + " " + spec.Name
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer cancel()
			status, report := t.runOne(ctx, spec, defs[i], models[i], states[i], b.slots, true)
			b.finish(bt, status, report)
		}()
	}
	b.pruneLocked()
	return fmt.Sprintf("Started in the background: %s. You and the user can go on; each report comes in a message when its task ends "+
		"(task_status checks, waits for or cancels them). Don't do their work yourself meanwhile.", strings.Join(started, ", "))
}

func (b *Background) finish(bt *bgTask, status, report string) {
	b.mu.Lock()
	bt.ended, bt.status, bt.report = true, status, report
	close(b.change)
	b.change = make(chan struct{})
	notify := !bt.dropped && b.OnDone != nil
	b.mu.Unlock()
	if notify {
		b.OnDone(bt.id)
	}
}

// pruneLocked forgets the oldest ended tasks past bgKeep.
func (b *Background) pruneLocked() {
	extra := len(b.tasks) - bgKeep
	if extra <= 0 {
		return
	}
	kept := b.tasks[:0]
	for _, bt := range b.tasks {
		if extra > 0 && bt.ended && (bt.delivered || bt.dropped) {
			extra--
			continue
		}
		kept = append(kept, bt)
	}
	b.tasks = kept
}

// section is an ended task's report as the session gets it.
func (bt *bgTask) section() string {
	prompt := strings.Join(strings.Fields(bt.prompt), " ")
	if r := []rune(prompt); len(r) > 200 {
		prompt = string(r[:200]) + "…"
	}
	s := fmt.Sprintf("## %s %s (%s)\nTask: %s", bt.id, bt.st.Name, bt.status, prompt)
	if bt.report != "" {
		s += "\n\n" + bt.report
	}
	return s
}

// takeLocked hands over the reports not delivered yet.
func (b *Background) takeLocked() []string {
	var out []string
	for _, bt := range b.tasks {
		if bt.ended && !bt.delivered && !bt.dropped {
			bt.delivered = true
			out = append(out, bt.section())
		}
	}
	return out
}

// Take returns the reports of the tasks that ended and were not delivered
// yet, as a message for the session ("" if none), and marks them delivered.
func (b *Background) Take() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	reports := b.takeLocked()
	b.mu.Unlock()
	if len(reports) == 0 {
		return ""
	}
	return "Background tasks you started ended. Their reports (from subagents, not the user):\n\n" + strings.Join(reports, "\n\n")
}

// Notice is Take for Agent.Notices.
func (b *Background) Notice() string { return b.Take() }

// Pending reports whether a report waits for delivery.
func (b *Background) Pending() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, bt := range b.tasks {
		if bt.ended && !bt.delivered && !bt.dropped {
			return true
		}
	}
	return false
}

// Running counts the tasks queued or running.
func (b *Background) Running() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, bt := range b.tasks {
		if !bt.ended {
			n++
		}
	}
	return n
}

// Statuses returns a copy of the tasks' states, oldest first.
func (b *Background) Statuses() []SubagentStatus {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]SubagentStatus, 0, len(b.tasks))
	for _, bt := range b.tasks {
		bt.parent.subMu.Lock()
		s := *bt.st
		s.Log = append([]string(nil), bt.st.Log...)
		bt.parent.subMu.Unlock()
		out = append(out, s)
	}
	return out
}

// Wait waits until the tasks in ids (none: all that run now) ended, d
// passed (<= 0: no limit) or ctx is done.
func (b *Background) Wait(ctx context.Context, ids []string, d time.Duration) error {
	if b == nil {
		return nil
	}
	var timeout <-chan time.Time
	if d > 0 {
		tm := time.NewTimer(d)
		defer tm.Stop()
		timeout = tm.C
	}
	for {
		b.mu.Lock()
		waiting := false
		for _, bt := range b.tasks {
			if !bt.ended && (len(ids) == 0 || contains(ids, bt.id)) {
				waiting = true
			}
		}
		change := b.change
		b.mu.Unlock()
		if !waiting {
			return nil
		}
		select {
		case <-change:
		case <-timeout:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Cancel stops the tasks in ids that still run; their reports say so.
// unknown are the ids of no task.
func (b *Background) Cancel(ids []string) (stopped int, unknown []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range ids {
		bt := b.findLocked(id)
		switch {
		case bt == nil:
			unknown = append(unknown, id)
		case !bt.ended:
			bt.cancel()
			stopped++
		}
	}
	return stopped, unknown
}

// Stop stops the task id for the user: no report, no OnDone. False if it
// isn't running (it ended meanwhile, or there is no such task).
func (b *Background) Stop(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	bt := b.findLocked(id)
	if bt == nil || bt.ended {
		return false
	}
	bt.dropped = true
	bt.cancel()
	return true
}

func (b *Background) findLocked(id string) *bgTask {
	for _, bt := range b.tasks {
		if bt.id == id {
			return bt
		}
	}
	return nil
}

// Reset stops the running tasks and forgets them all, for a new session.
// It returns how many it stopped.
func (b *Background) Reset() int {
	n := b.Close(10 * time.Second)
	if b != nil {
		b.mu.Lock()
		b.tasks, b.ctx, b.cancel = nil, nil, nil
		b.mu.Unlock()
	}
	return n
}

// Close stops the running tasks, drops their reports and waits up to d for
// them to clean up (worktrees). It returns how many it stopped.
func (b *Background) Close(d time.Duration) int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	n := 0
	for _, bt := range b.tasks {
		if !bt.ended {
			bt.dropped = true
			n++
		}
	}
	if b.cancel != nil {
		b.cancel()
	}
	b.mu.Unlock()
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
	return n
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// TaskStatusTool checks on, waits for and cancels background tasks.
type TaskStatusTool struct{ BG *Background }

// taskWaitMax caps one task_status wait.
const taskWaitMax = 600

func (t *TaskStatusTool) Def() llm.Tool {
	return llm.Tool{
		Name: "task_status",
		Description: "Check on the background tasks (task with background: true): their state, and the reports of those that ended and you haven't seen. " +
			"Without ids, all of this session's. wait: seconds to wait for them (without ids, all running ones) to end, up to 600; use it only when you can't go on without them. " +
			"cancel: true stops the tasks in ids.",
		Schema: json.RawMessage(`{"type":"object","properties":{"ids":{"type":"array","items":{"type":"string"},"description":"task ids, like t1"},` +
			`"wait":{"type":"number","description":"seconds to wait for them to end"},"cancel":{"type":"boolean","description":"stop the tasks in ids"}}}`),
	}
}

func (t *TaskStatusTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		IDs    []string `json:"ids"`
		Wait   float64  `json:"wait"`
		Cancel bool     `json:"cancel"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return "", err
		}
	}
	b := t.BG
	var head []string
	if in.Cancel {
		if len(in.IDs) == 0 {
			return "", errors.New("cancel needs ids")
		}
		n, unknown := b.Cancel(in.IDs)
		if len(unknown) > 0 {
			return "", fmt.Errorf("no background task %s", strings.Join(unknown, ", "))
		}
		head = append(head, fmt.Sprintf("Stopped %d task(s).", n))
		_ = b.Wait(ctx, in.IDs, 10*time.Second) // let them end, so their state shows
	} else if in.Wait > 0 {
		if err := b.Wait(ctx, in.IDs, time.Duration(min(in.Wait, taskWaitMax)*float64(time.Second))); err != nil {
			return "", err
		}
	}
	b.mu.Lock()
	var rows []string
	for _, bt := range b.tasks {
		if len(in.IDs) > 0 && !contains(in.IDs, bt.id) {
			continue
		}
		rows = append(rows, bt.rowLocked())
	}
	var reports []string
	for _, bt := range b.tasks {
		if (len(in.IDs) == 0 || contains(in.IDs, bt.id)) && bt.ended && !bt.delivered && !bt.dropped {
			bt.delivered = true
			reports = append(reports, bt.section())
		}
	}
	b.mu.Unlock()
	if len(in.IDs) > 0 && len(rows) < len(in.IDs) {
		var unknown []string
		for _, id := range in.IDs {
			found := false
			for _, r := range rows {
				found = found || strings.HasPrefix(r, id+" ")
			}
			if !found {
				unknown = append(unknown, id)
			}
		}
		return "", fmt.Errorf("no background task %s", strings.Join(unknown, ", "))
	}
	if len(rows) == 0 {
		return "No background tasks in this session.", nil
	}
	out := strings.Join(append(head, rows...), "\n")
	if len(reports) > 0 {
		out += "\n\n" + strings.Join(reports, "\n\n")
	}
	return out, nil
}

// rowLocked is one line of task_status: "t1 Name: running · 1m12s · 14
// tools · last: grep foo".
func (bt *bgTask) rowLocked() string {
	bt.parent.subMu.Lock()
	s := *bt.st
	bt.parent.subMu.Unlock()
	state := s.State
	if bt.ended {
		state = bt.status
	}
	parts := []string{state}
	if !s.Started.IsZero() {
		end := s.Ended
		if end.IsZero() {
			end = time.Now()
		}
		parts = append(parts, end.Sub(s.Started).Round(time.Second).String())
	}
	if s.Tools > 0 {
		parts = append(parts, fmt.Sprintf("%d tool calls", s.Tools))
	}
	switch {
	case !bt.ended && s.Last != "":
		parts = append(parts, "last: "+s.Last)
	case bt.ended && bt.delivered:
		parts = append(parts, "report already delivered")
	case bt.ended && bt.dropped:
		parts = append(parts, "stopped")
	}
	return bt.id + " " + s.Name + ": " + strings.Join(parts, " · ")
}
