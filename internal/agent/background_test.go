package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// gateProvider answers "report for <prompt>" once the prompt's gate opens.
type gateProvider struct {
	mu        sync.Mutex
	gates     map[string]chan struct{}
	cur, peak int
}

func (p *gateProvider) gate(prompt string) chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gates == nil {
		p.gates = map[string]chan struct{}{}
	}
	if p.gates[prompt] == nil {
		p.gates[prompt] = make(chan struct{})
	}
	return p.gates[prompt]
}

func (p *gateProvider) open(prompt string) { close(p.gate(prompt)) }

func (p *gateProvider) Name() string { return "fake" }

func (p *gateProvider) Stream(ctx context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	prompt := req.Messages[0].Blocks[0].Text
	p.mu.Lock()
	p.cur++
	p.peak = max(p.peak, p.cur)
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.cur--; p.mu.Unlock() }()
	select {
	case <-p.gate(prompt):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &llm.Message{Role: llm.RoleAssistant, StopReason: "end_turn",
		Blocks: []llm.Block{{Type: llm.BlockText, Text: "report for " + prompt}}}, nil
}

type bgFixture struct {
	prov   *gateProvider
	bg     *Background
	task   *TaskTool
	status *TaskStatusTool
	done   chan string
}

func newBGFixture(t *testing.T, concurrency int) *bgFixture {
	f := &bgFixture{prov: &gateProvider{}, bg: &Background{}, done: make(chan string, 16)}
	f.bg.OnDone = func(id string) { f.done <- id }
	parent := &Agent{Provider: f.prov, UI: nopUI{}}
	f.task = &TaskTool{Parent: parent, Env: &tools.Env{Cwd: t.TempDir()}, Concurrency: concurrency, Background: f.bg}
	f.status = &TaskStatusTool{BG: f.bg}
	t.Cleanup(func() { f.bg.Close(5 * time.Second) })
	return f
}

func (f *bgFixture) start(t *testing.T, tasks ...string) string {
	t.Helper()
	var list []map[string]string
	for i := 0; i+1 < len(tasks); i += 2 {
		list = append(list, map[string]string{"name": tasks[i], "prompt": tasks[i+1]})
	}
	in, _ := json.Marshal(map[string]any{"tasks": list, "background": true})
	out, err := f.task.Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *bgFixture) waitDone(t *testing.T, want string) {
	t.Helper()
	select {
	case id := <-f.done:
		if id != want {
			t.Fatalf("OnDone(%s), want %s", id, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never ended", want)
	}
}

func (f *bgFixture) taskStatus(t *testing.T, in string) string {
	t.Helper()
	out, err := f.status.Run(context.Background(), json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A background call returns at once; each report is handed over once, as
// its task ends.
func TestBackgroundTasks(t *testing.T) {
	f := newBGFixture(t, 4)
	out := f.start(t, "Alpha", "pa", "Beta", "pb")
	if !strings.Contains(out, "Started in the background: t1 Alpha, t2 Beta.") {
		t.Fatalf("start: %s", out)
	}
	if n := f.bg.Running(); n != 2 {
		t.Fatalf("running %d", n)
	}
	if f.bg.Pending() || f.bg.Take() != "" {
		t.Fatal("a report before any task ended")
	}
	st := f.bg.Statuses()
	if len(st) != 2 || st[0].ID != "t1" || st[1].ID != "t2" || st[0].Name != "Alpha" {
		t.Fatalf("statuses %+v", st)
	}

	f.prov.open("pa")
	f.waitDone(t, "t1")
	if !f.bg.Pending() {
		t.Fatal("t1's report isn't pending")
	}
	got := f.bg.Take()
	if !strings.Contains(got, "## t1 Alpha (done)\nTask: pa\n\nreport for pa") || strings.Contains(got, "t2") {
		t.Fatalf("take: %s", got)
	}
	if f.bg.Take() != "" || f.bg.Pending() {
		t.Fatal("t1's report was handed over twice")
	}

	// task_status: t2 still runs; waiting for it brings its report, once.
	if s := f.taskStatus(t, `{}`); !strings.Contains(s, "t1 Alpha: done") || !strings.Contains(s, "report already delivered") ||
		!strings.Contains(s, "t2 Beta: running") && !strings.Contains(s, "t2 Beta: queued") {
		t.Fatalf("status: %s", s)
	}
	go func() { time.Sleep(50 * time.Millisecond); f.prov.open("pb") }()
	s := f.taskStatus(t, `{"ids":["t2"],"wait":30}`)
	if !strings.Contains(s, "t2 Beta: done") || !strings.Contains(s, "## t2 Beta (done)") || !strings.Contains(s, "report for pb") {
		t.Fatalf("wait: %s", s)
	}
	f.waitDone(t, "t2")
	if f.bg.Pending() || f.bg.Take() != "" {
		t.Fatal("t2's report came twice")
	}
	if s := f.taskStatus(t, `{"ids":["t2"]}`); strings.Contains(s, "## t2") || !strings.Contains(s, "report already delivered") {
		t.Fatalf("status after: %s", s)
	}
}

// Cancelled tasks end with a report that says so; Reset stops the rest
// quietly; a wait gives up at its limit.
func TestBackgroundCancelAndReset(t *testing.T) {
	f := newBGFixture(t, 4)
	f.start(t, "Slow", "never", "Other", "never2")
	start := time.Now()
	if s := f.taskStatus(t, `{"wait":0.2}`); !strings.Contains(s, "t1 Slow: running") {
		t.Fatalf("wait past its limit: %s", s)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("the wait ignored its limit")
	}
	if s := f.taskStatus(t, `{"ids":["t1"],"cancel":true}`); !strings.Contains(s, "Stopped 1 task(s).") || !strings.Contains(s, "## t1 Slow (failed: context canceled)") {
		t.Fatalf("cancel: %s", s)
	}
	f.waitDone(t, "t1")
	if _, err := f.status.Run(context.Background(), json.RawMessage(`{"ids":["t9"]}`)); err == nil || !strings.Contains(err.Error(), "no background task t9") {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := f.status.Run(context.Background(), json.RawMessage(`{"cancel":true}`)); err == nil {
		t.Fatal("cancel without ids")
	}

	// Stopped by the user: no report, no OnDone.
	f.start(t, "Unwanted", "never3")
	if !f.bg.Stop("t3") {
		t.Fatal("Stop t3")
	}
	if err := f.bg.Wait(context.Background(), []string{"t3"}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if f.bg.Stop("t3") || f.bg.Stop("t9") {
		t.Fatal("Stop of a task that isn't running")
	}
	if f.bg.Pending() {
		t.Fatal("a stopped task's report is pending")
	}
	if s := f.taskStatus(t, `{"ids":["t3"]}`); !strings.Contains(s, "stopped") || strings.Contains(s, "## t3") {
		t.Fatalf("stopped: %s", s)
	}

	if n := f.bg.Reset(); n != 1 {
		t.Fatalf("reset stopped %d", n)
	}
	select {
	case id := <-f.done:
		t.Fatalf("OnDone(%s) after Reset", id)
	case <-time.After(100 * time.Millisecond):
	}
	if f.bg.Pending() || len(f.bg.Statuses()) != 0 || f.bg.Running() != 0 {
		t.Fatal("Reset left tasks behind")
	}
	// Tasks start again after a Reset, with new ids.
	if out := f.start(t, "Again", "pa"); !strings.Contains(out, "t4 Again") {
		t.Fatalf("after reset: %s", out)
	}
	f.prov.open("pa")
	f.waitDone(t, "t4")
}

// The concurrency limit holds across background calls.
func TestBackgroundConcurrencyAcrossCalls(t *testing.T) {
	f := newBGFixture(t, 1)
	f.start(t, "A", "p1")
	f.start(t, "B", "p2")
	time.Sleep(50 * time.Millisecond)
	// Either may get the slot first; the other waits.
	if st := f.bg.Statuses(); (st[0].State == SubQueued) == (st[1].State == SubQueued) {
		t.Fatalf("not one running and one queued: %s, %s", st[0].State, st[1].State)
	}
	f.prov.open("p1")
	f.prov.open("p2")
	if err := f.bg.Wait(context.Background(), nil, 5*time.Second); err != nil || f.bg.Running() != 0 {
		t.Fatalf("not both ended: %v", err)
	}
	f.prov.mu.Lock()
	defer f.prov.mu.Unlock()
	if f.prov.peak != 1 {
		t.Fatalf("peak %d", f.prov.peak)
	}
}

// A background call never blocks on, or touches, the call's own subagents
// panel; without a registry, the property isn't offered and asking fails.
func TestBackgroundSchema(t *testing.T) {
	f := newBGFixture(t, 2)
	var schema map[string]any
	if err := json.Unmarshal(f.task.Def().Schema, &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema["properties"].(map[string]any)["background"]; !ok {
		t.Fatal("no background property")
	}
	f.start(t, "A", "p1")
	if subs := f.task.Parent.Subagents(); len(subs) != 0 {
		t.Fatalf("a background task in the call's panel: %+v", subs)
	}
	f.prov.open("p1")
	f.waitDone(t, "t1")

	plain := &TaskTool{Parent: &Agent{Provider: f.prov, UI: nopUI{}}, Env: &tools.Env{Cwd: t.TempDir()}}
	if strings.Contains(string(plain.Def().Schema), "background") {
		t.Fatal("background offered without a registry")
	}
	if _, err := plain.Run(context.Background(), json.RawMessage(`{"tasks":[{"name":"A","prompt":"p"}],"background":true}`)); err == nil {
		t.Fatal("background ran without a registry")
	}
}

// The session's next request carries the reports, through Notices.
func TestBackgroundReportsReachTheSession(t *testing.T) {
	f := newBGFixture(t, 2)
	f.start(t, "A", "p1")
	f.prov.open("p1")
	f.waitDone(t, "t1")
	var seen string
	rec := providerFunc(func(req *llm.Request) *llm.Message {
		for _, m := range req.Messages {
			for _, b := range m.Blocks {
				seen += b.Text + "\n"
			}
		}
		return &llm.Message{Role: llm.RoleAssistant, StopReason: "end_turn", Blocks: []llm.Block{{Type: llm.BlockText, Text: "ok"}}}
	})
	main := &Agent{Provider: rec, UI: nopUI{}, Notices: f.bg.Notice}
	if err := main.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, "<system-reminder>") || !strings.Contains(seen, "## t1 A (done)") || !strings.Contains(seen, "report for p1") {
		t.Fatalf("request: %s", seen)
	}
}

type providerFunc func(*llm.Request) *llm.Message

func (providerFunc) Name() string { return "fake" }
func (f providerFunc) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	return f(req), nil
}
