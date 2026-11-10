package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/akbxr/tego/internal/llm"
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
		a.keepEnded(&endedSub{sub: &Agent{Name: fmt.Sprintf("S%d", i)}, id: fmt.Sprintf("t%d", i)})
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
