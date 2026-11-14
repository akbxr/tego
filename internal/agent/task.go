package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/session"
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
	// MaxTurns is the requests a subagent may make (an agent definition's
	// maxTurns overrides it); 0: no limit. The last one writes its report.
	MaxTurns int
	// Watchdog is how many requests a subagent makes between checks that
	// it is getting somewhere (watchdog.go), on the small model if there is
	// one; 0: no checks.
	Watchdog int
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
	// DefaultModel is the model a task runs on when it names none (config
	// subagent_model): a Models name, an alias or provider/model. "": the
	// session's. "main" still asks for the session's.
	DefaultModel string
	// Isolate gives a task ("isolation": "worktree") a git worktree of its
	// own: the folder to work in, a note on it for the subagent, and finish,
	// which removes the worktree if nothing is in it and says what became of
	// it. nil: tasks can't ask for one.
	Isolate func(task string) (dir, note string, finish func(ok bool) string, err error)
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
			"Prompts must be complete and self-contained: goal, relevant files, constraints, expected output. Never give two subagents overlapping files to edit. " +
			"A subagent that ended (stopped at a limit, interrupted, or done) can be resumed with its context: resume it rather than start over.",
		Schema: json.RawMessage(`{"type":"object","properties":{"tasks":{"type":"array","minItems":1,"items":{"type":"object","properties":{"name":{"type":"string","description":"short CamelCase label"},"prompt":{"type":"string"}` + resumeProperty +
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
		"each report comes in a later message when its task ends (task_status checks, waits for, messages or cancels them). " +
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
	if !hasSmall && len(named) == 0 && t.DefaultModel == "" {
		return ""
	}
	enum := []string{"main"}
	var desc strings.Builder
	if t.DefaultModel != "" {
		enum = []string{"default", "main"}
		label := t.DefaultModel
		for _, m := range named {
			if name, _ := splitLevel(label); m.Name == name {
				label += " (" + m.Spec + ")"
			}
		}
		desc.WriteString("default (also when omitted): " + label + ", the subagents' model, for most tasks. ")
	}
	if hasSmall {
		enum = append(enum, "small")
		desc.WriteString("small: " + s.Model + ", faster and much cheaper, for simple, well-defined work: finding files or code, " +
			"reading and summarising, running checks and reporting. ")
	}
	if t.DefaultModel != "" {
		desc.WriteString("main: the session's own model, for the tasks that need it most: hard judgment, design, delicate edits.")
	} else {
		desc.WriteString("main (default): the session's model, for anything that needs judgment, design or careful edits.")
	}
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

// splitLevel splits a ":level" thinking suffix off a model spec
// ("anthropic/claude-opus-5-5:xhigh"); a tag that is no level stays
// ("ollama/qwen3:8b").
func splitLevel(spec string) (string, string) {
	if i := strings.LastIndexByte(spec, ':'); i >= 0 && llm.ThinkingRank(spec[i+1:]) >= 0 {
		return spec[:i], spec[i+1:]
	}
	return spec, ""
}

// pickModels resolves the models the tasks asked for, each once. A
// ":level" suffix sets the thinking level.
func (t *TaskTool) pickModels(asked []string) []taskModel {
	out := make([]taskModel, len(asked))
	names := make([]string, len(asked))
	for i, spec := range asked {
		names[i], out[i].thinking = splitLevel(spec)
	}
	models := t.pickNamed(names)
	for i := range out {
		if level := out[i].thinking; level != "" {
			models[i].thinking = level
		}
		out[i] = models[i]
	}
	return out
}

func (t *TaskTool) pickNamed(asked []string) []taskModel {
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

// resumeProperty is the tasks' "resume" property (resume.go).
const resumeProperty = `,"resume":{"type":"string","description":"Resume a subagent of this session that ended, by its name or task id (t3), ` +
	`instead of starting a new one: it keeps its context, and prompt says what to do next. Its agent and folder stay its own, and its model too unless it ran on the session's: then it runs on the session's model as it is now. Worktree subagents can't be resumed."}`

// taskSpec is one task of a task call.
type taskSpec struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	Model  string `json:"model"` // "default" (""), "main", "small" or a named model
	Agent  string `json:"agent"` // a defined agent, or "" for a general subagent
	// Isolation "worktree": in a worktree of its own.
	Isolation string `json:"isolation"`
	Resume    string `json:"resume"` // an ended subagent to go on with

	resumed *endedSub        // the one Resume names, taken out of the kept ones
	spec    string           // the model it was asked to run on ("" = the session's)
	sess    *session.Session // the session's file when the task was called, to record it in
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
	p := t.Parent
	for i, task := range in.Tasks {
		if task.Resume == "" {
			continue
		}
		e, err := p.takeEnded(task.Resume)
		if err == nil && e.sub == nil { // restored from the session file
			err = t.revive(e) // one that can't be is not given back
		}
		if err != nil {
			for _, done := range in.Tasks[:i] { // give back the ones taken
				if done.resumed != nil {
					p.keepEnded(done.resumed)
				}
			}
			return "", err
		}
		// It goes on as what it was: its own name, agent and model. One
		// that ran on the session's model follows it to the one it is now.
		was := cmp.Or(e.ranOn, e.sub.Model)
		e.ranOn = ""
		if e.sub.followSession(p) || e.sub.sessionModel && was != e.sub.Model {
			p.UI.OnInfo(fmt.Sprintf("  [%s] resumed on %s, the session's model now (it ran on %s)", e.name, p.Model, was))
		}
		in.Tasks[i] = taskSpec{Name: e.name, Prompt: task.Prompt, Resume: task.Resume, resumed: e, spec: e.model}
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
	limit := t.concurrency()
	live := liveUI(p.UI)
	if len(in.Tasks) > limit && !live && !in.Background {
		p.UI.OnInfo(fmt.Sprintf("  %d subagents, %d at a time; the rest wait in queue", len(in.Tasks), limit))
	}
	asked := make([]string, len(in.Tasks))
	for i, task := range in.Tasks {
		asked[i] = task.Model
		if d := defs[i]; asked[i] == "" && d != nil {
			asked[i] = d.Model // the task's own choice wins over the agent's
			if strings.EqualFold(d.Model, "inherit") {
				asked[i] = "main"
			}
		}
		switch {
		case task.resumed != nil:
			asked[i] = "main" // it has its model
		case asked[i] == "" || asked[i] == "default":
			asked[i] = t.DefaultModel
		}
	}
	models := t.pickModels(asked)
	states := make([]*SubagentStatus, len(in.Tasks))
	for i, task := range in.Tasks {
		if task.resumed == nil {
			in.Tasks[i].spec = asked[i]
		}
		in.Tasks[i].sess = p.Session
		name := task.Name
		if name == "" {
			name = fmt.Sprintf("task%d", i+1)
		}
		in.Tasks[i].Name = name
		states[i] = &SubagentStatus{Name: name, Prompt: task.Prompt, State: SubQueued, Window: p.ContextWindow}
		if defs[i] != nil {
			states[i].Agent = defs[i].Name
		}
		if e := task.resumed; e != nil {
			states[i].Agent, states[i].Window = e.agent, e.sub.ContextWindow
			if !e.sub.sessionModel {
				states[i].Model = e.sub.Model
			}
		} else if m := models[i]; m.Provider != nil {
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
	failed := func(why, status string) (string, string) {
		p.setSub(st, func(s *SubagentStatus) { s.State, s.Err, s.Ended = SubFailed, why, time.Now() })
		return status, ""
	}
	var sub *Agent
	var finish func(ok bool) string
	asked := task.Prompt // all it was asked, for its watchdog
	prompt := task.Prompt
	if e := task.resumed; e != nil {
		sub, asked = e.sub, e.task+"\n\nThen: "+task.Prompt
		// Its history may end with "your last response, no tools".
		prompt = resumedNote + task.Prompt
	} else {
		sub, finish, status = t.newSub(task, def, model, failed)
		if sub == nil {
			return status, ""
		}
	}
	if t.Watchdog > 0 {
		w := &Watchdog{Every: t.Watchdog, Provider: sub.Provider, Model: sub.Model, Task: asked, from: len(sub.Messages)}
		if s, ok := t.small(); ok {
			w.Provider, w.Model = s.Provider, s.Model
		}
		sub.Watchdog = w
	}
	ui := &subUI{name: name, parent: p, st: st, sub: sub, live: quiet}
	sub.UI = ui
	sub.noteInterrupted() // calls cut off when it was stopped, if resumed
	inbox := &subInbox{}
	if ss, ok := p.UI.(SubagentSessions); ok {
		ui.sess, ui.key = ss, fmt.Sprintf("%s-%d", name, subKeys.Add(1))
		info := SubagentInfo{Name: name, Model: sub.Model, Cwd: sub.dir}
		if def != nil {
			info.Agent = def.Name
		}
		ss.SubagentStarted(ui.key, info)
		ss.SubagentPrompt(ui.key, task.Prompt)
		inbox.shown = func(text string) { ss.SubagentPrompt(ui.key, text) }
	}
	inbox.attach(sub)
	defer inbox.detach()
	p.setSub(st, func(s *SubagentStatus) { s.State, s.Started, s.inbox = SubRunning, time.Now(), inbox })
	if !quiet {
		verb := "started"
		if task.resumed != nil {
			verb = "resumed"
		}
		p.UI.OnInfo(fmt.Sprintf("  ⇢ [%s] %s", name, verb))
	}
	// The shells it starts are its own, and stop when it finishes: nothing
	// could read or stop them after.
	owner := fmt.Sprintf("%s#%d", name, shellOwners.Add(1))
	ctx = tools.WithShellOwner(ctx, owner)
	if t.Env != nil {
		sub.Notices = func() string { return t.Env.ShellNoticesFor(owner) }
	}
	before := sub.Usage()
	err := sub.Run(ctx, prompt)
	inbox.detach()
	if ui.sess != nil {
		ui.sess.SubagentEnded(ui.key, err)
	}
	left := t.Env.StopShells(owner)
	after := sub.Usage()
	p.addUsage(&llm.Usage{Input: after.Input - before.Input, Output: after.Output - before.Output,
		CacheRead: after.CacheRead - before.CacheRead, CacheWrite: after.CacheWrite - before.CacheWrite})
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
		report += "\n\n[worktree: " + finish(err == nil) + "]"
		return status, report
	}
	agentName := ""
	if def != nil {
		agentName = def.Name
	} else if task.resumed != nil {
		agentName = task.resumed.agent
	}
	e := task.resumed // its records in the session file go on
	if e == nil {
		e = &endedSub{name: name, model: task.spec}
	}
	e.sub, e.id, e.agent, e.task = sub, st.ID, agentName, asked
	sub.Watchdog, sub.Notices = nil, nil
	if err := e.save(task.sess); err != nil { // before another task can take it
		p.UI.OnInfo(fmt.Sprintf("  [%s] its context could not be saved in the session file (%v): it can be resumed until tego exits", name, err))
	}
	p.keepEnded(e)
	if err != nil {
		report += fmt.Sprintf("\n\n[resume: %q goes on with its context]", e.ref())
	}
	return status, report
}

// newSub makes a new task's subagent, in its worktree if it has one
// (finish then commits or removes it). nil: it can't run (status says why).
func (t *TaskTool) newSub(task taskSpec, def *AgentDef, model taskModel,
	failed func(why, status string) (string, string)) (sub *Agent, finish func(ok bool) string, status string) {
	p, name := t.Parent, task.Name
	sub = &Agent{
		Provider: p.Provider, ProviderName: p.ProviderName, Model: p.Model, Thinking: p.Thinking, Fast: p.Fast,
		System: t.System, Tools: tools.Builtins(t.Env), Dynamic: p.Dynamic, MCPTools: p.MCPTools, SessionID: p.SessionID + "-" + name,
		ContextWindow: p.ContextWindow, AutoCompact: true, MaxTurns: t.MaxTurns, Subagent: true, ToolStat: p.ToolStat,
		CacheWritePremium: p.CacheWritePremium, Env: p.Env, Plan: p.Plan, MaxOutputTokens: p.MaxOutputTokens,
		Perms: p.Perms, Approve: p.Approve, Name: name, declined: p.declined, Hooks: p.Hooks,
	}
	// On the session's model by choice, not as the one a default or a name
	// gave (or that was unavailable): resumed after a /model, it follows.
	sub.sessionModel = model.Provider == nil && (task.spec == "" || task.spec == "main")
	if m := model; m.Provider != nil {
		sub.Provider, sub.ProviderName, sub.Model, sub.ContextWindow = m.Provider, m.ProviderName, m.Model, m.Window
		sub.Fast = false // fast mode is the session model's
	}
	if model.thinking != "" { // its own, or the level a ":level" asked for
		sub.Thinking = model.thinking
	}
	sub.sessionThinking = model.thinking == "" && (def == nil || def.Thinking == "")
	if task.Isolation == "worktree" || def != nil && def.Worktree {
		if t.Isolate == nil {
			status, _ = failed("no worktrees here", "failed: it can't have a worktree here: not a git repository, or worktrees are off")
			return nil, nil, status
		}
		dir, note, fin, err := t.Isolate(name)
		if err != nil {
			status, _ = failed(err.Error(), fmt.Sprintf("failed: no worktree for it: %v", err))
			return nil, nil, status
		}
		// Its own tools, on the worktree on disk (not an editor's buffers).
		sub.Tools = tools.Builtins(t.Env.At(dir)) // its background shells are the session's to see and clean up
		sub.dir = dir
		sub.System += "\n\n# Worktree\n" + note
		finish = fin
	}
	if def != nil {
		def.apply(sub)
	}
	return sub, finish, ""
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

	inbox *subInbox // messages sent to it while it runs
}

// SubagentSessions is a UI that shows each subagent as a session of its
// own (ACP's subagent sessions), not in the parent's stream. key is unique
// in the process; a resumed subagent runs under a new one.
type SubagentSessions interface {
	SubagentStarted(key string, info SubagentInfo)
	SubagentPrompt(key, text string)    // a message to it: its task, or one sent while it runs
	SubagentCall(key, callID string)    // the call is the subagent's
	SubagentText(key, delta string)     // its answer, streamed
	SubagentThinking(key, delta string) // its thinking, streamed
	SubagentEnded(key string, err error)
}

// SubagentInfo describes a subagent to a SubagentSessions UI.
type SubagentInfo struct {
	Name, Agent, Model string
	Cwd                string // its worktree's folder; "" for the session's
}

// subKeys numbers subagent runs for SubagentSessions keys.
var subKeys atomic.Int64

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
	sess   SubagentSessions // nil: the parent's UI shows no subagent sessions
	key    string
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
// the subagent's name (the user may be asked about them), or in its own
// session where the editor shows subagents.
func (s *subUI) OnCallStart(_, id, name string, input json.RawMessage) {
	if s.sess != nil {
		s.sess.SubagentCall(s.key, id)
	}
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

func (s *subUI) OnText(delta string) {
	if s.sess != nil {
		s.sess.SubagentText(s.key, delta)
	}
}

func (s *subUI) OnThinking(delta string) {
	if s.sess != nil {
		s.sess.SubagentThinking(s.key, delta)
	}
}

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
