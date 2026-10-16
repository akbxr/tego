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
	"github.com/akbxr/tego/internal/tools"
)

// DefaultSubagentConcurrency is how many subagents run at once when not
// configured; extra tasks queue (there is no cap on the number of tasks).
const DefaultSubagentConcurrency = 8

// TaskTool runs independent subagents in parallel, each with a fresh
// context and the builtin tools (no nested task).
type TaskTool struct {
	Parent      *Agent
	System      string // subagent system prompt
	Env         *tools.Env
	Concurrency int // max subagents running at once; <= 0 means DefaultSubagentConcurrency
	// Small is the cheaper model a task can ask for with "model": "small"
	// (ok false: there is none, and every task runs on the parent's model).
	Small func() (SmallModel, bool)
}

// SmallModel is a cheaper model for side work: simple subagents, titles.
type SmallModel struct {
	Provider     llm.Provider
	ProviderName string
	Model        string
	Window       int // context window
}

func (t *TaskTool) small() (SmallModel, bool) {
	if t.Small == nil {
		return SmallModel{}, false
	}
	return t.Small()
}

func (t *TaskTool) concurrency() int {
	if t.Concurrency > 0 {
		return t.Concurrency
	}
	return DefaultSubagentConcurrency
}

func (t *TaskTool) Def() llm.Tool {
	return llm.Tool{
		Name: "task",
		Description: fmt.Sprintf("Run subagents for independent subtasks (broad research, changes in separate areas). Any number of tasks; up to %d run at once, the rest queue. ", t.concurrency()) +
			"Each starts with an empty context and the read/write/edit/bash/grep/glob (and MCP) tools, and returns its final report. " +
			"Prompts must be complete and self-contained: goal, relevant files, constraints, expected output. Never give two subagents overlapping files to edit.",
		Schema: json.RawMessage(`{"type":"object","properties":{"tasks":{"type":"array","minItems":1,"items":{"type":"object","properties":{"name":{"type":"string","description":"short CamelCase label"},"prompt":{"type":"string"}` +
			t.modelProperty() + `},"required":["name","prompt"]}}},"required":["tasks"]}`),
	}
}

// modelProperty is the tasks' "model" property, when there is a small model.
func (t *TaskTool) modelProperty() string {
	s, ok := t.small()
	if !ok {
		return ""
	}
	desc, _ := json.Marshal("small: " + s.Model + ", faster and much cheaper, for simple, well-defined work: finding files or code, " +
		"reading and summarising, running checks and reporting. main (default): the session's model, for anything that needs judgment, " +
		"design or careful edits.")
	return `,"model":{"type":"string","enum":["main","small"],"description":` + string(desc) + `}`
}

func (t *TaskTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Tasks []struct {
			Name   string `json:"name"`
			Prompt string `json:"prompt"`
			Model  string `json:"model"` // "small" or main ("")
		} `json:"tasks"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	if len(in.Tasks) == 0 {
		return "", errors.New("tasks is empty")
	}
	p := t.Parent
	limit := t.concurrency()
	live := liveUI(p.UI)
	if len(in.Tasks) > limit && !live {
		p.UI.OnInfo(fmt.Sprintf("  %d subagents, %d at a time; the rest wait in queue", len(in.Tasks), limit))
	}
	small, hasSmall := t.small()
	states := make([]*SubagentStatus, len(in.Tasks))
	for i, task := range in.Tasks {
		name := task.Name
		if name == "" {
			name = fmt.Sprintf("task%d", i+1)
		}
		states[i] = &SubagentStatus{Name: name, Prompt: task.Prompt, State: SubQueued, Window: p.ContextWindow}
		if task.Model == "small" && hasSmall {
			states[i].Model, states[i].Window = small.Model, small.Window
		}
	}
	p.subMu.Lock()
	p.subs = states
	p.subMu.Unlock()

	slots := make(chan struct{}, limit)
	reports := make([]string, len(in.Tasks))
	var wg sync.WaitGroup
	for i, task := range in.Tasks {
		st := states[i]
		name := st.Name
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				p.setSub(st, func(s *SubagentStatus) { s.State, s.Err = SubFailed, "cancelled before it started" })
				reports[i] = fmt.Sprintf("## %s (cancelled before it started)", name)
				return
			}
			sub := &Agent{
				Provider: p.Provider, ProviderName: p.ProviderName, Model: p.Model, Thinking: p.Thinking, Fast: p.Fast,
				System: t.System, Tools: tools.Builtins(t.Env), Dynamic: p.Dynamic, LazyThreshold: p.LazyThreshold, SessionID: p.SessionID + "-" + name,
				ContextWindow: p.ContextWindow, AutoCompact: true, MaxTurns: 150, Subagent: true, ToolStat: p.ToolStat,
				CacheWritePremium: p.CacheWritePremium, Env: p.Env, Plan: p.Plan,
				Perms: p.Perms, Approve: p.Approve, Name: name,
			}
			if st.Model != "" {
				sub.Provider, sub.ProviderName, sub.Model, sub.ContextWindow = small.Provider, small.ProviderName, small.Model, small.Window
				sub.Fast = false // fast mode is Opus only
			}
			sub.UI = &subUI{name: name, parent: p, st: st, sub: sub, live: live}
			p.setSub(st, func(s *SubagentStatus) { s.State, s.Started = SubRunning, time.Now() })
			if !live {
				p.UI.OnInfo(fmt.Sprintf("  ⇢ [%s] started", name))
			}
			err := sub.Run(ctx, task.Prompt)
			p.addUsage(&sub.Total)
			report := strings.TrimSpace(sub.LastText())
			status := "done"
			if err != nil {
				status = "failed: " + err.Error()
				if report == "" {
					report = "(no report)"
				}
			}
			p.setSub(st, func(s *SubagentStatus) {
				s.State, s.Ended, s.Usage = SubDone, time.Now(), sub.Usage()
				if err != nil {
					s.State, s.Err = SubFailed, err.Error()
				}
			})
			if !live {
				p.UI.OnInfo(fmt.Sprintf("  ⇠ [%s] %s", name, status))
			}
			reports[i] = fmt.Sprintf("## %s (%s)\n%s", name, status, report)
		}()
	}
	wg.Wait()
	return strings.Join(reports, "\n\n"), ctx.Err()
}

// Subagent states.
const (
	SubQueued  = "queued"
	SubRunning = "running"
	SubDone    = "done"
	SubFailed  = "failed"
)

// SubagentStatus is the live state of one subagent of the latest task
// call, for the UI.
type SubagentStatus struct {
	Name, Prompt   string
	Model          string // set when it runs on the small model
	State          string // SubQueued, SubRunning, SubDone, SubFailed
	Err            string
	Started, Ended time.Time
	Tools          int    // tool calls made
	Requests       int    // model requests made
	Last           string // summary of the latest tool call
	CtxTokens      int    // context of its latest request
	Window         int
	Usage          llm.Usage
	Log            []string // tool-call summaries and notices, oldest first (capped)
	Text           string   // text of its latest answer (capped)
}

const (
	subLogMax  = 200
	subTextMax = 4000
)

// Subagents returns a copy of the subagents of the latest task call
// (goroutine-safe); nil before the first one.
func (a *Agent) Subagents() []SubagentStatus {
	a.subMu.Lock()
	defer a.subMu.Unlock()
	out := make([]SubagentStatus, len(a.subs))
	for i, s := range a.subs {
		out[i] = *s
		out[i].Log = append([]string(nil), s.Log...)
	}
	return out
}

func (a *Agent) setSub(s *SubagentStatus, fn func(*SubagentStatus)) {
	a.subMu.Lock()
	fn(s)
	a.subMu.Unlock()
}

// liveUI reports whether ui shows subagents itself (a live panel), so the
// task tool need not print a line per subagent event.
func liveUI(ui UI) bool {
	l, ok := ui.(interface{ LiveSubagents() bool })
	return ok && l.LiveSubagents()
}

// subUI records a subagent's activity in its status and, unless the
// parent UI shows subagents live, forwards tool activity as info lines.
type subUI struct {
	name   string
	parent *Agent
	st     *SubagentStatus
	sub    *Agent
	live   bool
}

func (s *subUI) log(line string) {
	s.parent.setSub(s.st, func(st *SubagentStatus) {
		if len(st.Log) >= subLogMax {
			st.Log = append(st.Log[:0], st.Log[1:]...)
		}
		st.Log = append(st.Log, line)
	})
}

func (s *subUI) OnText(string)                                      {}
func (s *subUI) OnThinking(string)                                  {}
func (s *subUI) OnToolStart(string)                                 {}
func (s *subUI) OnToolResult(string, json.RawMessage, string, bool) {}

func (s *subUI) OnTurnEnd(m *llm.Message) {
	var text strings.Builder
	if m != nil {
		for _, b := range m.Blocks {
			if b.Type == llm.BlockText {
				text.WriteString(b.Text)
			}
		}
	}
	ctx := s.sub.LastCtx
	s.parent.setSub(s.st, func(st *SubagentStatus) {
		st.Requests++
		st.CtxTokens = ctx
		if t := strings.TrimSpace(text.String()); t != "" {
			if len(t) > subTextMax {
				t = t[:subTextMax] + "…"
			}
			st.Text = t
		}
	})
}

func (s *subUI) OnInfo(msg string) {
	s.log(msg)
	if !s.live {
		s.parent.UI.OnInfo(fmt.Sprintf("  · [%s] %s", s.name, msg))
	}
}

func (s *subUI) OnToolCall(name string, input json.RawMessage) {
	sum := tools.Summary(name, input)
	s.parent.setSub(s.st, func(st *SubagentStatus) { st.Tools++; st.Last = sum })
	s.log(sum)
	if !s.live {
		s.parent.UI.OnInfo(fmt.Sprintf("  · [%s] %s", s.name, sum))
	}
}
