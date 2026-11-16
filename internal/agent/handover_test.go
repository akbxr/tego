package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/tools"
)

// callAsync runs a task call (not background) on ctx; its result comes on
// the channel.
func (f *bgFixture) callAsync(ctx context.Context, tasks ...string) chan [2]string {
	var list []map[string]string
	for i := 0; i+1 < len(tasks); i += 2 {
		list = append(list, map[string]string{"name": tasks[i], "prompt": tasks[i+1]})
	}
	in, _ := json.Marshal(map[string]any{"tasks": list})
	out := make(chan [2]string, 1)
	go func() {
		res, err := f.task.Run(ctx, in)
		e := ""
		if err != nil {
			e = err.Error()
		}
		out <- [2]string{res, e}
	}()
	return out
}

func awaitCall(t *testing.T, ch chan [2]string, what string) (string, string) {
	t.Helper()
	select {
	case r := <-ch:
		return r[0], r[1]
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: the task call never returned", what)
		return "", ""
	}
}

// subState waits until the call's subagent name is in state.
func subState(t *testing.T, a *Agent, name, state string) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		for _, s := range a.Subagents() {
			if s.Name == name && s.State == state {
				return
			}
		}
	}
	t.Fatalf("%s never got %s: %+v", name, state, a.Subagents())
}

// A message sent while a task call waits ends the wait: the subagents
// still at work go on in the background, the ones done report in the
// call's result, and the moved ones report as background tasks.
func TestMessageMovesCallTasksToBackground(t *testing.T) {
	f := newBGFixture(t, 0)
	parent := f.task.Parent
	ctx, cancel := context.WithCancel(tools.WithWake(context.Background(), parent.wakeChan()))
	call := f.callAsync(ctx, "Quick", "quick job", "Slow", "slow job")
	f.prov.open("quick job")
	subState(t, parent, "Quick", SubDone)
	subState(t, parent, "Slow", SubRunning)
	if parent.TellSubagent("Quick", "x") || !parent.TellSubagent("Slow", "keep it short") {
		t.Fatal("TellSubagent: only a running one takes the user's message")
	}

	parent.SteerArrived()
	out, err := awaitCall(t, call, "after the message")
	if err != "" {
		t.Fatalf("error %s", err)
	}
	for _, want := range []string{"## Quick (done)\nreport for quick job", "they went on in the background: t1 Slow", "task_status"} {
		if !strings.Contains(out, want) {
			t.Fatalf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "## Slow") {
		t.Fatalf("the moved one reported in the call:\n%s", out)
	}
	if f.bg.Running() != 1 {
		t.Fatalf("background running %d, want 1", f.bg.Running())
	}
	for _, s := range parent.Subagents() {
		if s.Name == "Slow" {
			t.Fatalf("the moved one still shows with the call's: %+v", parent.Subagents())
		}
	}

	// Interrupting the turn now leaves it be: it is a background task.
	cancel()
	time.Sleep(50 * time.Millisecond)
	if f.bg.Running() != 1 {
		t.Fatal("interrupting the turn stopped the moved subagent")
	}
	if st := f.taskStatus(t, `{"ids":["t1"]}`); !strings.Contains(st, "t1 Slow: running") {
		t.Fatalf("task_status:\n%s", st)
	}
	f.prov.open("slow job")
	f.waitDone(t, "t1")
	if rep := f.bg.Take(); !strings.Contains(rep, "## t1 Slow (done)") || !strings.Contains(rep, "report for slow job") {
		t.Fatalf("its report:\n%s", rep)
	}
	if ref := parent.endedID("t1"); !ref {
		t.Fatal("the moved subagent can't be resumed by its id")
	}
}

// Without a message, and when interrupted, a task call is what it was.
func TestTaskCallWithoutMessage(t *testing.T) {
	f := newBGFixture(t, 0)
	parent := f.task.Parent
	ctx := tools.WithWake(context.Background(), parent.wakeChan())
	call := f.callAsync(ctx, "A", "a job")
	f.prov.open("a job")
	if out, err := awaitCall(t, call, "to its end"); err != "" || out != "## A (done)\nreport for a job" {
		t.Fatalf("%q %q", out, err)
	}

	ctx, cancel := context.WithCancel(tools.WithWake(context.Background(), parent.wakeChan()))
	call = f.callAsync(ctx, "B", "b job")
	subState(t, parent, "B", SubRunning)
	cancel()
	out, err := awaitCall(t, call, "interrupted")
	if err != context.Canceled.Error() || !strings.Contains(out, "## B (failed: ") {
		t.Fatalf("%q %q", out, err)
	}
	if f.bg.Running() != 0 || len(f.bg.Statuses()) != 0 {
		t.Fatalf("an interrupted call left background tasks: %+v", f.bg.Statuses())
	}
}

// Without background tasks to move them to, a message doesn't end the
// wait, even one that came before the call.
func TestMessageWithoutBackground(t *testing.T) {
	f := newBGFixture(t, 0)
	parent := f.task.Parent
	parent.SteerArrived() // came before the call: its wait would end at once
	f.task.Background = nil
	call := f.callAsync(tools.WithWake(context.Background(), parent.wakeChan()), "A", "a job")
	select {
	case r := <-call:
		t.Fatalf("returned before its subagent ended: %q", r)
	case <-time.After(100 * time.Millisecond):
	}
	f.prov.open("a job")
	if out, _ := awaitCall(t, call, "without background tasks"); out != "## A (done)\nreport for a job" {
		t.Fatalf("%q", out)
	}
}

// Closing the session stops a moved subagent, with no report.
func TestCloseStopsMovedTasks(t *testing.T) {
	f := newBGFixture(t, 0)
	parent := f.task.Parent
	call := f.callAsync(tools.WithWake(context.Background(), parent.wakeChan()), "Slow", "slow job")
	subState(t, parent, "Slow", SubRunning)
	parent.SteerArrived()
	if out, _ := awaitCall(t, call, "after the message"); !strings.Contains(out, "t1 Slow") {
		t.Fatalf("%q", out)
	}
	done := make(chan int, 1)
	go func() { done <- f.bg.Close(5 * time.Second) }()
	select {
	case n := <-done:
		if n != 1 {
			t.Fatalf("Close stopped %d, want 1", n)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Close hung")
	}
	select {
	case id := <-f.done:
		t.Fatalf("OnDone(%s) for a task Close stopped", id)
	default:
	}
	if f.bg.Running() != 0 {
		t.Fatal("still running after Close")
	}
}
