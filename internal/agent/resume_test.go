package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/session"
	"github.com/akbxr/tego/internal/tools"
)

// memProvider works (a tool call per response) until the latest prompt says
// "go on", then answers; it notes what each request started with and held.
type memProvider struct {
	mu    sync.Mutex
	n     int
	first []string // the first message's text, per request
	sizes []int    // messages per request
}

func (p *memProvider) Name() string { return "fake" }

func (p *memProvider) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	p.first = append(p.first, req.Messages[0].Blocks[0].Text)
	p.sizes = append(p.sizes, len(req.Messages))
	usage := &llm.Usage{Output: 1}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if m := req.Messages[i]; isPrompt(m) && !strings.HasSuffix(m.Blocks[0].Text, "</system-reminder>") {
			if strings.Contains(m.Blocks[0].Text, "go on") {
				return &llm.Message{Role: llm.RoleAssistant, StopReason: "end_turn", Usage: usage,
					Blocks: []llm.Block{{Type: llm.BlockText, Text: "finished"}}}, nil
			}
			break
		}
	}
	in, _ := json.Marshal(map[string]string{"pattern": fmt.Sprintf("*.r%d", p.n)})
	return &llm.Message{Role: llm.RoleAssistant, StopReason: "tool_use", Usage: usage, Blocks: []llm.Block{
		{Type: llm.BlockText, Text: fmt.Sprintf("working %d", p.n)},
		{Type: llm.BlockToolUse, ID: fmt.Sprintf("r%d", p.n), Name: "glob", Input: in},
	}}, nil
}

func taskCall(t *testing.T, task *TaskTool, spec map[string]string) (string, error) {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"tasks": []map[string]string{spec}})
	return task.Run(context.Background(), in)
}

// A subagent stopped at its limit goes on with its context when resumed,
// and its usage is counted once.
func TestResumeSubagent(t *testing.T) {
	prov := &memProvider{}
	parent := &Agent{Provider: prov, UI: nopUI{}}
	task := &TaskTool{Parent: parent, Env: &tools.Env{Cwd: t.TempDir()}, MaxTurns: 3}
	out, err := taskCall(t, task, map[string]string{"name": "Worker", "prompt": "work"})
	if err != nil || !strings.Contains(out, "failed: stopped at its limit") || !strings.Contains(out, `[resume: "Worker" goes on with its context]`) {
		t.Fatalf("%v\n%s", err, out)
	}
	stopped := prov.sizes[len(prov.sizes)-1]

	out, err = taskCall(t, task, map[string]string{"name": "Other", "prompt": "go on", "resume": "worker"})
	if err != nil || !strings.HasPrefix(out, "## Worker (done)\nfinished") || strings.Contains(out, "[resume:") {
		t.Fatalf("%v\n%s", err, out)
	}
	if prov.n != 4 || prov.first[3] != "work" || prov.sizes[3] <= stopped {
		t.Fatalf("resumed without its context: first %q, sizes %v", prov.first, prov.sizes)
	}
	// It is told its limit no longer holds: its history ends with "no tools".
	if e, _ := parent.takeEnded("Worker"); e == nil || !strings.HasPrefix(e.sub.Messages[stopped+1].Blocks[0].Text, resumedNote+"go on") {
		t.Fatalf("resumed prompt: %+v", e.sub.Messages[stopped:])
	} else {
		parent.keepEnded(e)
	}
	if u := parent.Usage(); u.Output != 4 {
		t.Fatalf("parent output %d, want 4 (each request once)", u.Output)
	}
	if subs := parent.Subagents(); len(subs) != 1 || subs[0].Name != "Worker" || subs[0].State != SubDone {
		t.Fatalf("panel %+v", subs)
	}

	// A done one can be asked for more too; an unknown one says what can be.
	if _, err := taskCall(t, task, map[string]string{"name": "W", "prompt": "go on", "resume": "Worker"}); err != nil {
		t.Fatal(err)
	}
	if _, err := taskCall(t, task, map[string]string{"name": "W", "prompt": "x", "resume": "Nobody"}); err == nil ||
		!strings.Contains(err.Error(), "these can be: Worker") {
		t.Fatalf("unknown: %v", err)
	}
	// Another session or branch forgets them.
	parent.SetHistory(nil)
	if _, err := taskCall(t, task, map[string]string{"name": "W", "prompt": "go on", "resume": "Worker"}); err == nil ||
		!strings.Contains(err.Error(), "none can be") {
		t.Fatalf("after SetHistory: %v", err)
	}
}

// One call can't resume the same subagent twice; a failed lookup gives back
// the ones it took.
func TestResumeTakesOnce(t *testing.T) {
	prov := &memProvider{}
	parent := &Agent{Provider: prov, UI: nopUI{}}
	task := &TaskTool{Parent: parent, Env: &tools.Env{Cwd: t.TempDir()}, MaxTurns: 2}
	if _, err := taskCall(t, task, map[string]string{"name": "A", "prompt": "work"}); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]any{"tasks": []map[string]string{
		{"name": "A", "prompt": "go on", "resume": "A"}, {"name": "A2", "prompt": "go on", "resume": "A"}}})
	if _, err := task.Run(context.Background(), in); err == nil {
		t.Fatal("resumed twice")
	}
	if out, err := taskCall(t, task, map[string]string{"name": "A", "prompt": "go on", "resume": "A"}); err != nil || !strings.Contains(out, "finished") {
		t.Fatalf("not given back: %v %s", err, out)
	}
}

func TestEndedKeepsTheLatest(t *testing.T) {
	a := &Agent{}
	for i := range endedMax + 2 {
		name := fmt.Sprintf("S%d", i)
		a.keepEnded(&endedSub{sub: &Agent{Name: name}, name: name, id: fmt.Sprintf("t%d", i)})
	}
	if _, err := a.takeEnded("t0"); err == nil {
		t.Fatal("the oldest was kept past the cap")
	}
	e, err := a.takeEnded("t5")
	if err != nil || e.sub.Name != "S5" {
		t.Fatalf("%v %v", e, err)
	}
	if e, err := a.takeEnded("s9"); err != nil || e.ref() != "t9" {
		t.Fatalf("by name: %v %v", e, err)
	}
}

// modelProvider notes the model of each request and answers at once.
type modelProvider struct {
	mu     sync.Mutex
	models []string
}

func (p *modelProvider) Name() string { return "fake" }

func (p *modelProvider) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.models = append(p.models, req.Model+":"+req.Thinking)
	return &llm.Message{Role: llm.RoleAssistant, StopReason: "end_turn", Usage: &llm.Usage{Output: 1},
		Blocks: []llm.Block{{Type: llm.BlockText, Text: "done"}}}, nil
}

// After a /model, a resumed subagent that ran on the session's model runs
// on the new one; one with a model of its own keeps it.
func TestResumeFollowsSessionModel(t *testing.T) {
	old, now, small := &modelProvider{}, &modelProvider{}, &modelProvider{}
	parent := &Agent{Provider: old, ProviderName: "a", Model: "opus", Thinking: "high", ContextWindow: 100, UI: nopUI{}}
	task := &TaskTool{Parent: parent, Env: &tools.Env{Cwd: t.TempDir()},
		Small: func() (SmallModel, bool) {
			return SmallModel{Provider: small, ProviderName: "a", Model: "haiku", Window: 50}, true
		}}
	for _, spec := range []map[string]string{{"name": "Main", "prompt": "x"}, {"name": "Small", "prompt": "x", "model": "small"}} {
		if _, err := taskCall(t, task, spec); err != nil {
			t.Fatal(err)
		}
	}
	parent.Provider, parent.Model, parent.Thinking, parent.ContextWindow = now, "fable", "xhigh", 200
	for _, name := range []string{"Main", "Small"} {
		if _, err := taskCall(t, task, map[string]string{"prompt": "more", "resume": name}); err != nil {
			t.Fatal(err)
		}
	}
	// The small model has requests of its own besides the subagent's two.
	if got := fmt.Sprint(old.models, now.models); got != "[opus:high] [fable:xhigh]" || strings.Count(fmt.Sprint(small.models), "haiku:high") != 2 {
		t.Fatalf("requests went to %s %s", got, small.models)
	}
	e, _ := parent.takeEnded("Main")
	if e == nil || e.sub.ContextWindow != 200 {
		t.Fatalf("resumed subagent: %+v", e)
	}
	if subs := parent.Subagents(); len(subs) != 1 || subs[0].Model != "haiku" {
		t.Fatalf("panel %+v", subs)
	}
}

// A subagent that ended is resumed after tego restarts: its history is in
// the session file. Each later record adds only what is new, and a branch
// from before it ran doesn't have it.
func TestResumeAfterRestart(t *testing.T) {
	base, cwd := t.TempDir(), t.TempDir()
	s, err := session.New(base, cwd)
	if err != nil {
		t.Fatal(err)
	}
	path := s.Path
	if err := s.Append(llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	first := s.Leaf() // where it is asked for
	prov := &memProvider{}
	parent := &Agent{Provider: prov, ProviderName: "a", Model: "opus", UI: nopUI{}, Session: s}
	task := &TaskTool{Parent: parent, Env: &tools.Env{Cwd: cwd}, MaxTurns: 3}
	if out, err := taskCall(t, task, map[string]string{"name": "Worker", "prompt": "work"}); err != nil || !strings.Contains(out, "stopped at its limit") {
		t.Fatalf("%v\n%s", err, out)
	}
	stopped := prov.sizes[len(prov.sizes)-1]
	if err := s.Append(llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "it stopped"}}}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	restart := func() (*Agent, *TaskTool, *memProvider, *infoUI) {
		t.Helper()
		s, msgs, err := session.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		prov, ui := &memProvider{}, &infoUI{}
		p := &Agent{Provider: prov, ProviderName: "a", Model: "fable", UI: ui, Session: s}
		p.SetHistory(msgs)
		return p, &TaskTool{Parent: p, Env: &tools.Env{Cwd: cwd}, MaxTurns: 3}, prov, ui
	}
	parent, task, prov, ui := restart()
	out, err := taskCall(t, task, map[string]string{"name": "x", "prompt": "go on", "resume": "worker"})
	if err != nil || !strings.HasPrefix(out, "## Worker (done)\nfinished") {
		t.Fatalf("%v\n%s", err, out)
	}
	if prov.n != 1 || prov.first[0] != "work" || prov.sizes[0] <= stopped {
		t.Fatalf("resumed without its context: first %q, sizes %v (it had %d)", prov.first, prov.sizes, stopped)
	}
	if !slices.Contains(ui.infos, "  [Worker] resumed on fable, the session's model now (it ran on opus)") {
		t.Fatalf("info %q", ui.infos)
	}
	resumedAt := prov.sizes[0]
	parent.Session.Close()

	// Again: it goes on from all it did, and the file adds only that.
	parent, task, prov, _ = restart()
	if _, err := taskCall(t, task, map[string]string{"name": "x", "prompt": "go on", "resume": "Worker"}); err != nil {
		t.Fatal(err)
	}
	if prov.n != 1 || prov.sizes[0] <= resumedAt {
		t.Fatalf("sizes %v, after %d", prov.sizes, resumedAt)
	}
	data, _ := os.ReadFile(path)
	if n, chained := strings.Count(string(data), `"type":"subagent"`), strings.Count(string(data), `"prev":`); n != 3 || chained != 2 {
		t.Fatalf("%d records, %d building on the one before", n, chained)
	}

	// A background task after a restart doesn't take the id of one restored.
	if _, err := parent.Session.SaveSubagent("bgkey", "", 0, json.RawMessage(`{"name":"Bg","id":"t1"}`),
		[]llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "work"}}}}); err != nil {
		t.Fatal(err)
	}
	parent.Session.Close()
	parent, task, _, _ = restart()
	bg := &Background{}
	t.Cleanup(func() { bg.Close(5 * time.Second) })
	task.Background = bg
	in, _ := json.Marshal(map[string]any{"tasks": []map[string]string{{"name": "New", "prompt": "go on"}}, "background": true})
	if out, err := task.Run(context.Background(), in); err != nil || !strings.Contains(out, "t2 New") {
		t.Fatalf("%v\n%s", err, out)
	}

	// Back to where it ran: it is there; before that, it isn't.
	for _, at := range []string{first, ""} {
		if _, err := parent.Session.Checkout(at); err != nil {
			t.Fatal(err)
		}
		parent.SetHistory(parent.Session.History(at))
		if _, err := parent.takeEnded("Worker"); (err == nil) != (at == first) {
			t.Fatalf("at %q: %v", at, err)
		}
	}
}

// subagent_model: a task that names no model runs on it, "main" on the
// session's; an agent's model and inherit win over it. Resumed after a
// /model, one on the default stays there.
func TestDefaultSubagentModel(t *testing.T) {
	fable, opus, small, next := &modelProvider{}, &modelProvider{}, &modelProvider{}, &modelProvider{}
	parent := &Agent{Provider: fable, ProviderName: "a", Model: "fable", Thinking: "high", UI: nopUI{}}
	task := &TaskTool{Parent: parent, Env: &tools.Env{Cwd: t.TempDir()}, DefaultModel: "opus",
		Small: func() (SmallModel, bool) { return SmallModel{Provider: small, ProviderName: "a", Model: "haiku"}, true },
		ResolveModel: func(spec string) (SmallModel, error) {
			if spec != "opus" {
				return SmallModel{}, fmt.Errorf("no %s", spec)
			}
			return SmallModel{Provider: opus, ProviderName: "a", Model: "opus"}, nil
		},
		Agents: func() []AgentDef {
			return []AgentDef{{Name: "plain"}, {Name: "same", Model: "inherit"}, {Name: "quick", Model: "small"}}
		}}
	if def := string(task.Def().Schema); !strings.Contains(def, `"enum":["default","main","small"]`) || !strings.Contains(def, "default (also when omitted): opus") {
		t.Fatalf("schema: %s", def)
	}
	for _, spec := range []map[string]string{
		{"name": "A", "prompt": "x"}, {"name": "B", "prompt": "x", "model": "default"}, {"name": "C", "prompt": "x", "model": "main"},
		{"name": "D", "prompt": "x", "agent": "plain"}, {"name": "E", "prompt": "x", "agent": "same"}, {"name": "F", "prompt": "x", "agent": "quick"},
	} {
		if _, err := taskCall(t, task, spec); err != nil {
			t.Fatal(err)
		}
	}
	if got := fmt.Sprint(len(opus.models), len(fable.models), len(small.models) >= 1); got != "3 2 true" {
		t.Fatalf("opus, session's, small: %s", got)
	}
	parent.Provider, parent.Model = next, "fable2"
	for _, name := range []string{"A", "C"} {
		if _, err := taskCall(t, task, map[string]string{"prompt": "more", "resume": name}); err != nil {
			t.Fatal(err)
		}
	}
	if len(opus.models) != 4 || len(next.models) != 1 {
		t.Fatalf("resumed: opus %d, the session's now %d", len(opus.models), len(next.models))
	}
}

// A subagent_models name as the default brings its thinking level.
func TestDefaultSubagentModelThinking(t *testing.T) {
	main, opus := &modelProvider{}, &modelProvider{}
	parent := &Agent{Provider: main, ProviderName: "a", Model: "fable", Thinking: "high", UI: nopUI{}}
	task := &TaskTool{Parent: parent, Env: &tools.Env{Cwd: t.TempDir()}, DefaultModel: "opus",
		Models: func() []SubagentModel {
			return []SubagentModel{{Name: "opus", Spec: "a/opus", Thinking: "xhigh", Resolve: func() (SmallModel, error) {
				return SmallModel{Provider: opus, ProviderName: "a", Model: "opus"}, nil
			}}}
		}}
	if _, err := taskCall(t, task, map[string]string{"name": "A", "prompt": "x"}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(opus.models, main.models) != "[opus:xhigh] []" {
		t.Fatalf("requests %v %v", opus.models, main.models)
	}
	if def := string(task.Def().Schema); !strings.Contains(def, "default (also when omitted): opus (a/opus)") {
		t.Fatalf("schema: %s", def)
	}
}
