package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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
	// Models are the named models a task can ask for besides those two
	// ("model": "gpt"), from other subscriptions too; nil: none.
	Models func() []SubagentModel
	// Agents are the defined agents a task can run as ("agent": "reviewer");
	// nil: none.
	Agents func() []AgentDef
	// ResolveModel resolves a model an agent definition names that is not
	// main, small or a named model: an alias (opus, sonnet, haiku) or
	// provider/model. Zero Provider and nil error: the session's model.
	ResolveModel func(spec string) (SmallModel, error)
	// Isolate gives a task ("isolation": "worktree") a git worktree of its
	// own: the folder to work in, a note on it for the subagent, and finish,
	// which removes the worktree if nothing is in it and says what became of
	// it. nil: tasks can't ask for one.
	Isolate func(task string) (dir, note string, finish func() string, err error)
	// Background runs the tasks of a call with "background": true; nil:
	// calls can't ask for it.
	Background *Background
}

// SubagentModel is a named model subagents can run on (config
// subagent_models).
type SubagentModel struct {
	Name     string
	Spec     string // provider/model
	Use      string // what it is good for, for the session's model
	Thinking string // its thinking level; "": the session's
	// Resolve builds or reuses its provider when a task asks for it.
	Resolve func() (SmallModel, error)
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
			t.agentProperty() + t.modelProperty() + t.isolationProperty() + `},"required":["name","prompt"]}}` + t.backgroundProperty() + `},"required":["tasks"]}`),
	}
}

// backgroundProperty is the call's "background" property, when tasks can
// run in the background.
func (t *TaskTool) backgroundProperty() string {
	if t.Background == nil {
		return ""
	}
	desc := "true: return at once and run the tasks in the background while you and the user go on; " +
		"each report comes in a later message when its task ends (task_status checks, waits for or cancels them). " +
		"For long tasks whose results your next step doesn't need. Don't do their work yourself meanwhile."
	if t.Isolate != nil {
		desc += " Background tasks that edit files should use isolation: worktree: you and the user keep editing this folder."
	}
	j, _ := json.Marshal(desc)
	return `,"background":{"type":"boolean","description":` + string(j) + `}`
}

// isolationProperty is the tasks' "isolation" property, when worktrees
// can be made.
func (t *TaskTool) isolationProperty() string {
	if t.Isolate == nil {
		return ""
	}
	return `,"isolation":{"type":"string","enum":["worktree"],"description":"worktree: the subagent works in a git worktree of its own, ` +
		`made from the last commit (uncommitted changes are not in it), and commits there. Lets subagents edit the same files in parallel. ` +
		`The report says the branch to merge; a worktree with no changes is removed."}`
}

func (t *TaskTool) agents() []AgentDef {
	if t.Agents == nil {
		return nil
	}
	return t.Agents()
}

// agentDescMax caps an agent's description in the schema: Claude Code's
// often carry long examples.
const agentDescMax = 300

// agentProperty is the tasks' "agent" property, when agents are defined.
func (t *TaskTool) agentProperty() string {
	defs := t.agents()
	if len(defs) == 0 {
		return ""
	}
	enum := make([]string, len(defs))
	var desc strings.Builder
	desc.WriteString("Run the task as one of these defined agents, with its own instructions, tools and model, when the task fits its description. Omit for a general subagent.")
	for i, d := range defs {
		enum[i] = d.Name
		s := d.Description
		if r := []rune(s); len(r) > agentDescMax {
			s = string(r[:agentDescMax]) + "…"
		}
		fmt.Fprintf(&desc, "\n- %s: %s", d.Name, s)
	}
	enumJSON, _ := json.Marshal(enum)
	descJSON, _ := json.Marshal(desc.String())
	return `,"agent":{"type":"string","enum":` + string(enumJSON) + `,"description":` + string(descJSON) + `}`
}

func (t *TaskTool) models() []SubagentModel {
	if t.Models == nil {
		return nil
	}
	return t.Models()
}

// modelProperty is the tasks' "model" property, when there is a model
// besides the session's: the small one, named ones.
func (t *TaskTool) modelProperty() string {
	s, hasSmall := t.small()
	named := t.models()
	if !hasSmall && len(named) == 0 {
		return ""
	}
	enum := []string{"main"}
	var desc strings.Builder
	if hasSmall {
		enum = append(enum, "small")
		desc.WriteString("small: " + s.Model + ", faster and much cheaper, for simple, well-defined work: finding files or code, " +
			"reading and summarising, running checks and reporting. ")
	}
	desc.WriteString("main (default): the session's model, for anything that needs judgment, design or careful edits.")
	for _, m := range named {
		enum = append(enum, m.Name)
		use := m.Use
		if use == "" {
			use = "when the user asks for it"
		}
		fmt.Fprintf(&desc, " %s: %s, %s.", m.Name, m.Spec, strings.TrimRight(use, ". "))
	}
	enumJSON, _ := json.Marshal(enum)
	descJSON, _ := json.Marshal(desc.String())
	return `,"model":{"type":"string","enum":` + string(enumJSON) + `,"description":` + string(descJSON) + `}`
}

// taskModel is the model one task runs on; zero Provider: the session's.
type taskModel struct {
	SmallModel
	thinking string
	note     string // why it runs on the session's model instead of the one asked for
}

// pickModels resolves the models the tasks asked for, each once.
func (t *TaskTool) pickModels(asked []string) []taskModel {
	small, hasSmall := t.small()
	var named map[string]SubagentModel
	resolved := map[string]taskModel{}
	out := make([]taskModel, len(asked))
	for i, name := range asked {
		if name == "" || name == "main" {
			continue
		}
		if name == "small" {
			if hasSmall {
				out[i] = taskModel{SmallModel: small}
			}
			continue
		}
		if m, ok := resolved[name]; ok {
			out[i] = m
			continue
		}
		if named == nil {
			named = map[string]SubagentModel{}
			for _, m := range t.models() {
				named[m.Name] = m
			}
		}
		var tm taskModel
		if m, ok := named[name]; !ok && t.ResolveModel != nil {
			// An agent's model: an alias or provider/model.
			if sm, err := t.ResolveModel(name); err != nil {
				tm.note = fmt.Sprintf("model %s is unavailable: %v", name, err)
			} else {
				tm = taskModel{SmallModel: sm}
			}
		} else if !ok || m.Resolve == nil {
			tm.note = fmt.Sprintf("model %q is not configured", name)
		} else if sm, err := m.Resolve(); err != nil {
			tm.note = fmt.Sprintf("model %s (%s) is unavailable: %v", name, m.Spec, err)
		} else {
			tm = taskModel{SmallModel: sm, thinking: m.Thinking}
		}
		resolved[name] = tm
		out[i] = tm
	}
	return out
}

// taskSpec is one task of a task call.
type taskSpec struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	Model  string `json:"model"` // "small", a named model, or main ("")
	Agent  string `json:"agent"` // a defined agent, or "" for a general subagent
	// Isolation "worktree": in a worktree of its own.
	Isolation string `json:"isolation"`
}

func (t *TaskTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Tasks      []taskSpec `json:"tasks"`
		Background bool       `json:"background"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	if len(in.Tasks) == 0 {
		return "", errors.New("tasks is empty")
	}
	if in.Background && t.Background == nil {
		return "", errors.New("background tasks are not available here; omit background")
	}
	defs := make([]*AgentDef, len(in.Tasks))
	anyAgent := false
	for _, task := range in.Tasks {
		anyAgent = anyAgent || task.Agent != ""
	}
	if anyAgent {
		byName := map[string]*AgentDef{}
		all := t.agents()
		names := make([]string, len(all))
		for i := range all {
			byName[all[i].Name], names[i] = &all[i], all[i].Name
		}
		for i, task := range in.Tasks {
			if task.Agent == "" {
				continue
			}
			if defs[i] = byName[task.Agent]; defs[i] == nil {
				if len(names) == 0 {
					return "", fmt.Errorf("no agent %q: no agents are defined; omit agent", task.Agent)
				}
				return "", fmt.Errorf("no agent %q; defined: %s", task.Agent, strings.Join(names, ", "))
			}
		}
	}
	p := t.Parent
	limit := t.concurrency()
	live := liveUI(p.UI)
	if len(in.Tasks) > limit && !live && !in.Background {
		p.UI.OnInfo(fmt.Sprintf("  %d subagents, %d at a time; the rest wait in queue", len(in.Tasks), limit))
	}
	asked := make([]string, len(in.Tasks))
	for i, task := range in.Tasks {
		asked[i] = task.Model
		if d := defs[i]; asked[i] == "" && d != nil && !strings.EqualFold(d.Model, "inherit") {
			asked[i] = d.Model // the task's own choice wins over the agent's
		}
	}
	models := t.pickModels(asked)
	states := make([]*SubagentStatus, len(in.Tasks))
	for i, task := range in.Tasks {
		name := task.Name
		if name == "" {
			name = fmt.Sprintf("task%d", i+1)
		}
		in.Tasks[i].Name = name
		states[i] = &SubagentStatus{Name: name, Prompt: task.Prompt, State: SubQueued, Window: p.ContextWindow}
		if defs[i] != nil {
			states[i].Agent = defs[i].Name
		}
		if m := models[i]; m.Provider != nil {
			states[i].Model, states[i].Window = m.Model, m.Window
		} else if m.note != "" {
			p.UI.OnInfo(fmt.Sprintf("  [%s] %s; it runs on %s", name, m.note, p.Model))
		}
	}
	if in.Background {
		return t.Background.start(t, in.Tasks, defs, models, states), nil
	}
	p.subMu.Lock()
	p.subs = states
	p.subMu.Unlock()

	slots := make(chan struct{}, limit)
	reports := make([]string, len(in.Tasks))
	var wg sync.WaitGroup
	for i, task := range in.Tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, report := t.runOne(ctx, task, defs[i], models[i], states[i], slots, live)
			reports[i] = fmt.Sprintf("## %s (%s)", task.Name, status)
			if report != "" {
				reports[i] += "\n" + report
			}
		}()
	}
	wg.Wait()
	return strings.Join(reports, "\n\n"), ctx.Err()
}

// shellOwners numbers the subagents' shell owners.
var shellOwners atomic.Int64

// runOne runs one task's subagent, once it has one of slots, to its end:
// its status ("done", "failed: …") and report. quiet: no info lines (a
// live panel shows it, or it runs in the background).
func (t *TaskTool) runOne(ctx context.Context, task taskSpec, def *AgentDef, model taskModel, st *SubagentStatus,
	slots chan struct{}, quiet bool) (status, report string) {
	p, name := t.Parent, task.Name
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		p.setSub(st, func(s *SubagentStatus) {
			s.State, s.Err, s.Ended = SubFailed, "cancelled before it started", time.Now()
		})
		return "cancelled before it started", ""
	}
	sub := &Agent{
		Provider: p.Provider, ProviderName: p.ProviderName, Model: p.Model, Thinking: p.Thinking, Fast: p.Fast,
		System: t.System, Tools: tools.Builtins(t.Env), Dynamic: p.Dynamic, LazyThreshold: p.LazyThreshold, SessionID: p.SessionID + "-" + name,
		ContextWindow: p.ContextWindow, AutoCompact: true, MaxTurns: 150, Subagent: true, ToolStat: p.ToolStat,
		CacheWritePremium: p.CacheWritePremium, Env: p.Env, Plan: p.Plan, MaxOutputTokens: p.MaxOutputTokens,
		Perms: p.Perms, Approve: p.Approve, Name: name, declined: p.declined, Hooks: p.Hooks,
	}
	if m := model; m.Provider != nil {
		sub.Provider, sub.ProviderName, sub.Model, sub.ContextWindow = m.Provider, m.ProviderName, m.Model, m.Window
		sub.Fast = false // fast mode is the session model's
		if m.thinking != "" {
			sub.Thinking = m.thinking
		}
	}
	failed := func(why, status string) (string, string) {
		p.setSub(st, func(s *SubagentStatus) { s.State, s.Err, s.Ended = SubFailed, why, time.Now() })
		return status, ""
	}
	var finish func() string
	if task.Isolation == "worktree" || def != nil && def.Worktree {
		if t.Isolate == nil {
			return failed("no worktrees here", "failed: it can't have a worktree here: not a git repository, or worktrees are off")
		}
		dir, note, fin, err := t.Isolate(name)
		if err != nil {
			return failed(err.Error(), fmt.Sprintf("failed: no worktree for it: %v", err))
		}
		// Its own tools, on the worktree on disk (not an editor's buffers).
		sub.Tools = tools.Builtins(t.Env.At(dir)) // its background shells are the session's to see and clean up
		sub.System += "\n\n# Worktree\n" + note
		finish = fin
	}
	if def != nil {
		def.apply(sub)
	}
	sub.UI = &subUI{name: name, parent: p, st: st, sub: sub, live: quiet}
	p.setSub(st, func(s *SubagentStatus) { s.State, s.Started = SubRunning, time.Now() })
	if !quiet {
		p.UI.OnInfo(fmt.Sprintf("  ⇢ [%s] started", name))
	}
	// The shells it starts are its own, and stop when it finishes: nothing
	// could read or stop them after.
	owner := fmt.Sprintf("%s#%d", name, shellOwners.Add(1))
	ctx = tools.WithShellOwner(ctx, owner)
	if t.Env != nil {
		sub.Notices = func() string { return t.Env.ShellNoticesFor(owner) }
	}
	err := sub.Run(ctx, task.Prompt)
	left := t.Env.StopShells(owner)
	p.addUsage(&sub.Total)
	report = strings.TrimSpace(sub.LastText())
	if len(left) > 0 {
		report += "\n\n[background shells it left running were stopped: " + strings.Join(left, ", ") + "]"
	}
	status = "done"
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
	if !quiet {
		p.UI.OnInfo(fmt.Sprintf("  ⇠ [%s] %s", name, status))
	}
	if n := model.note; n != "" {
		status += "; on the session's model: " + n
	}
	if finish != nil {
		report += "\n\n[worktree: " + finish() + "]"
	}
	return status, report
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
	ID             string // a background task's ("t1"); "" in a task call's own
	Agent          string // the defined agent it runs as, if any
	Model          string // set when it runs on another model than the session's
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

// A subagent's calls show in an editor (ACP) as the parent's, labelled with
// the subagent's name: the user may be asked about them.
func (s *subUI) OnCallStart(_, id, name string, input json.RawMessage) {
	if cu, ok := s.parent.UI.(CallUI); ok {
		cu.OnCallStart(s.name, id, name, input)
	}
}

func (s *subUI) OnCallRunning(_, id string) {
	if cu, ok := s.parent.UI.(CallUI); ok {
		cu.OnCallRunning(s.name, id)
	}
}

func (s *subUI) OnCallEnd(_, id, name string, input json.RawMessage, output string, isErr bool) {
	if cu, ok := s.parent.UI.(CallUI); ok {
		cu.OnCallEnd(s.name, id, name, input, output, isErr)
	}
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
