package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akbxr/tego/internal/acp"
	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/auth"
	"github.com/akbxr/tego/internal/config"
	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/mcp"
	"github.com/akbxr/tego/internal/session"
	"github.com/akbxr/tego/internal/stats"
	"github.com/akbxr/tego/internal/tools"
	"github.com/akbxr/tego/internal/web"
)

// tego acp: tego as an Agent Client Protocol agent, for editors that run
// agents as subprocesses (Zed, JetBrains, Neovim, Emacs…). The editor sends
// prompts over stdin and shows what tego does from session/update
// notifications on stdout; nothing else may be written to stdout. Sessions
// are tego's own, in ~/.tego/sessions, so the terminal can resume them.
//
// When the editor offers it, files are read and written through its buffers
// (unsaved changes included, edits show up as they are made) and commands
// run in its terminals.

func cmdACP(cfg *config.Config, store *auth.Store, in io.Reader, out io.Writer) error {
	s := newACPServer(cfg, store, out)
	defer s.closeAll()
	return s.conn.Serve(context.Background(), in)
}

type acpServer struct {
	cfg   *config.Config
	store *auth.Store
	conn  *acp.Conn
	caps  acp.ClientCapabilities

	mu       sync.Mutex
	sessions map[string]*acpSession
	folders  map[string]string // agent session id (requests) -> project folder, for the stats

	// provider, when set, is every session's model (tests).
	provider llm.Provider
}

func newACPServer(cfg *config.Config, store *auth.Store, out io.Writer) *acpServer {
	s := &acpServer{cfg: cfg, store: store, sessions: map[string]*acpSession{}, folders: map[string]string{}}
	s.conn = acp.NewConn(out, s.handle)
	log := stats.OpenLog(cfg.Dir)
	llm.SetRequestHook(func(st llm.RequestStat) {
		s.mu.Lock()
		folder := s.folders[st.SessionID]
		s.mu.Unlock()
		log.Add(stats.RequestRecord(st, folder))
	})
	return s
}

func (s *acpServer) closeAll() {
	s.mu.Lock()
	all := make([]*acpSession, 0, len(s.sessions))
	for _, sess := range s.sessions {
		all = append(all, sess)
	}
	s.sessions = map[string]*acpSession{}
	s.mu.Unlock()
	for _, sess := range all {
		sess.close()
	}
}

func decodeParams[T any](params json.RawMessage) (T, error) {
	var v T
	if len(params) > 0 {
		if err := json.Unmarshal(params, &v); err != nil {
			return v, &acp.Error{Code: acp.CodeInvalidParams, Message: err.Error()}
		}
	}
	return v, nil
}

func (s *acpServer) handle(ctx context.Context, method string, params json.RawMessage, notification bool) (any, error) {
	switch method {
	case "initialize":
		req, err := decodeParams[acp.InitializeRequest](params)
		if err != nil {
			return nil, err
		}
		s.caps = req.ClientCapabilities
		return acp.InitializeResponse{
			ProtocolVersion: acp.ProtocolVersion,
			AgentCapabilities: acp.AgentCapabilities{
				LoadSession:         true,
				PromptCapabilities:  acp.PromptCapabilities{Image: true, EmbeddedContext: true},
				MCPCapabilities:     acp.MCPCapabilities{HTTP: true, SSE: true},
				SessionCapabilities: acp.SessionCapabilities{List: &struct{}{}, Resume: &struct{}{}, Close: &struct{}{}},
			},
			AuthMethods: []acp.AuthMethod{},
			AgentInfo:   &acp.Implementation{Name: "tego", Title: "tego", Version: version},
		}, nil
	case "authenticate":
		return struct{}{}, nil
	case "session/new":
		req, err := decodeParams[acp.NewSessionRequest](params)
		if err != nil {
			return nil, err
		}
		sess, err := s.open(req.Cwd, req.MCPServers, "")
		if err != nil {
			return nil, err
		}
		setup := sess.setup()
		setup.SessionID = sess.id
		return acp.Reply{Result: setup, Then: sess.announce}, nil
	case "session/load", "session/resume":
		req, err := decodeParams[acp.LoadSessionRequest](params)
		if err != nil {
			return nil, err
		}
		sess, err := s.open(req.Cwd, req.MCPServers, req.SessionID)
		if err != nil {
			return nil, err
		}
		if method == "session/load" {
			sess.replay() // the whole conversation, before the response
		}
		return acp.Reply{Result: sess.setup(), Then: sess.announce}, nil
	case "session/list":
		req, err := decodeParams[acp.ListSessionsRequest](params)
		if err != nil {
			return nil, err
		}
		return s.list(req.Cwd)
	case "session/close":
		req, err := decodeParams[acp.SessionRef](params)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		sess := s.sessions[req.SessionID]
		delete(s.sessions, req.SessionID)
		s.mu.Unlock()
		if sess != nil {
			sess.close()
		}
		return struct{}{}, nil
	case "session/cancel":
		req, _ := decodeParams[acp.SessionRef](params)
		if sess := s.session(req.SessionID); sess != nil {
			sess.cancelPrompt()
		}
		return nil, nil
	case "session/prompt":
		req, err := decodeParams[acp.PromptRequest](params)
		if err != nil {
			return nil, err
		}
		sess := s.session(req.SessionID)
		if sess == nil {
			return nil, unknownSession(req.SessionID)
		}
		return sess.prompt(ctx, req.Prompt)
	case "session/set_mode":
		req, err := decodeParams[acp.SetModeRequest](params)
		if err != nil {
			return nil, err
		}
		sess := s.session(req.SessionID)
		if sess == nil {
			return nil, unknownSession(req.SessionID)
		}
		return struct{}{}, sess.setMode(req.ModeID)
	case "session/set_config_option":
		req, err := decodeParams[acp.SetConfigOptionRequest](params)
		if err != nil {
			return nil, err
		}
		sess := s.session(req.SessionID)
		if sess == nil {
			return nil, unknownSession(req.SessionID)
		}
		var value string
		if err := json.Unmarshal(req.Value, &value); err != nil {
			return nil, &acp.Error{Code: acp.CodeInvalidParams, Message: "value must be a string"}
		}
		if err := sess.setConfig(req.ConfigID, value); err != nil {
			return nil, err
		}
		return acp.SetConfigOptionResponse{ConfigOptions: sess.configOptions()}, nil
	}
	if notification {
		return nil, nil
	}
	return nil, &acp.Error{Code: acp.CodeMethodNotFound, Message: "method not found: " + method}
}

func unknownSession(id string) error {
	return &acp.Error{Code: acp.CodeNotFound, Message: "unknown session " + id}
}

func (s *acpServer) session(id string) *acpSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// list is session/list: the sessions of cwd, or of every project.
func (s *acpServer) list(cwd string) (acp.ListSessionsResponse, error) {
	var infos []session.Info
	var err error
	if cwd != "" {
		infos, err = session.List(s.cfg.Dir, cwd)
	} else {
		infos, err = session.ListAll(s.cfg.Dir)
	}
	if err != nil {
		return acp.ListSessionsResponse{}, err
	}
	out := acp.ListSessionsResponse{Sessions: []acp.SessionInfo{}}
	for _, in := range infos {
		dir := in.Cwd
		if dir == "" {
			dir = cwd
		}
		out.Sessions = append(out.Sessions, acp.SessionInfo{SessionID: in.ID, Cwd: dir, Title: in.Preview,
			UpdatedAt: in.Modified.UTC().Format(time.RFC3339)})
	}
	return out, nil
}

// acpSession is one conversation with the editor: an agent, its tego
// session file and MCP servers.
type acpSession struct {
	srv  *acpServer
	id   string // the tego session's id
	cwd  string
	ag   *agent.Agent
	app  *app // the session's model choices (small model, web search)
	env  *tools.Env
	mcp  *mcp.Manager
	plan atomic.Bool

	mu        sync.Mutex
	cancel    context.CancelFunc // the running prompt's
	mcpWaited bool
	titling   bool
	title     string // the session's (its Session.Title is written from the titling goroutine)
	commands  map[string]agent.Command
	skills    []agent.Skill
	before    map[string]*string // a write call's file before it ran (nil: a new file), for its diff
	terms     map[string]string  // tool call -> the editor terminal showing it
}

// open starts a session in cwd: a new one, or the tego session with id.
func (s *acpServer) open(cwd string, servers []acp.MCPServer, id string) (*acpSession, error) {
	if !filepath.IsAbs(cwd) {
		return nil, &acp.Error{Code: acp.CodeInvalidParams, Message: "cwd must be an absolute path"}
	}
	cfg := s.cfg
	spec, err := cfg.DefaultModel(s.store)
	if err != nil && s.provider == nil {
		return nil, &acp.Error{Code: acp.CodeAuthRequired, Message: err.Error()}
	}
	sess := &acpSession{srv: s, cwd: cwd, before: map[string]*string{}, terms: map[string]string{},
		commands: agent.LoadCommands(cfg.Dir, cwd)}
	env := &tools.Env{Cwd: cwd}
	if s.caps.FS.ReadTextFile && s.caps.FS.WriteTextFile {
		env.Files = sess
	}
	if s.caps.Terminal {
		env.Terminal = sess
	}
	files := agent.LoadContextFiles(cfg.Dir, cwd)
	skills := agent.LoadSkills(cfg.Dir, cwd)
	sess.skills = skills
	ag := &agent.Agent{
		System:            agent.SystemPrompt(cwd, files, skills, agent.Interactive),
		Env:               func() string { return agent.EnvNote(cwd) },
		Notices:           env.ShellNotices,
		Thinking:          firstNonEmpty(cfg.Thinking, llm.ThinkingMedium),
		Fast:              cfg.Fast,
		AutoCompact:       cfg.AutoCompactEnabled(),
		CompactPercent:    cfg.CompactAt,
		KeepRecent:        cfg.KeepRecent,
		IdleCompactAt:     cfg.IdleCompactAt(),
		UI:                sess,
		SessionID:         uuid4(),
		LongCache:         true, // an editor session pauses between prompts like the terminal's
		Plan:              &sess.plan,
		Perms:             agent.NewPermissions(firstNonEmpty(cfg.Permissions, agent.PermYolo)),
		CacheWritePremium: statsPrices(cfg).CacheWritePremium,
	}
	ag.Approve = sess.approve
	ap := &app{cfg: cfg, store: s.store, cwd: cwd, ag: ag}
	task := &agent.TaskTool{Parent: ag, System: agent.SystemPrompt(cwd, files, skills, agent.SubagentMode), Env: env,
		Concurrency: cfg.Subagents, Small: ap.small, Models: ap.subagentModels}
	ag.Tools = append(tools.Builtins(env), task,
		&agent.TodoTool{Parent: ag},
		&agent.CheckpointTool{Parent: ag},
		&agent.RewindTool{Parent: ag},
		web.NewSearchTool(ap.nativeSearch))
	log := stats.OpenLog(cfg.Dir)
	ag.ToolStat = func(name string, isErr, subagent bool) {
		class := llm.ClassMain
		if subagent {
			class = llm.ClassSubagent
		}
		log.Add(stats.Record{Kind: "tool", TS: time.Now().UnixMilli(), Folder: cwd, Class: class, Tool: name, ToolError: isErr})
	}
	if s.provider != nil {
		ag.Provider, ag.ProviderName, ag.Model, ag.ContextWindow = s.provider, "fake", "model", 200_000
	} else if err := ap.setModel(spec); err != nil {
		return nil, &acp.Error{Code: acp.CodeAuthRequired, Message: err.Error()}
	}
	sess.ag, sess.app, sess.env = ag, ap, env

	if id == "" {
		f, err := session.New(cfg.Dir, cwd)
		if err != nil {
			return nil, err
		}
		ag.Session = f
	} else {
		path, err := session.Find(cfg.Dir, cwd, id)
		if err != nil {
			return nil, &acp.Error{Code: acp.CodeNotFound, Message: err.Error()}
		}
		f, msgs, err := session.Open(path)
		if err != nil {
			return nil, err
		}
		ag.Session = f
		ag.SetHistory(msgs)
	}
	sess.id, sess.title = ag.Session.ID, ag.Session.Title

	cfgs, err := mcp.LoadConfig(cfg.Dir, cwd)
	if err != nil {
		cfgs = nil
	}
	sess.mcp = mcp.StartWith(mergeMCP(cfgs, servers), cwd, filepath.Join(cfg.Dir, "logs"), mcpOptions(cfg))
	ag.Dynamic = sess.mcp.Tools
	switch cfg.MCPTools {
	case "eager":
		ag.LazyThreshold = -1
	case "lazy":
		ag.LazyThreshold = 0
	default:
		ag.LazyThreshold = 10
	}

	s.mu.Lock()
	if old := s.sessions[sess.id]; old != nil { // loaded again: the newest wins
		defer old.close()
	}
	s.sessions[sess.id] = sess
	s.folders[ag.SessionID] = cwd
	s.mu.Unlock()
	return sess, nil
}

// mergeMCP adds the editor's MCP servers to tego's own; the editor's win
// on a name both have.
func mergeMCP(own []mcp.ServerConfig, servers []acp.MCPServer) []mcp.ServerConfig {
	names := map[string]bool{}
	var out []mcp.ServerConfig
	for _, sv := range servers {
		c := mcp.ServerConfig{Name: sv.Name, Source: "editor"}
		switch sv.Type {
		case "http", "sse":
			c.Type, c.URL, c.Headers = sv.Type, sv.URL, map[string]string{}
			for _, h := range sv.Headers {
				c.Headers[h.Name] = h.Value
			}
		default:
			c.Type, c.Command, c.Args, c.Env = "stdio", sv.Command, sv.Args, map[string]string{}
			for _, e := range sv.Env {
				c.Env[e.Name] = e.Value
			}
		}
		names[sv.Name] = true
		out = append(out, c)
	}
	for _, c := range own {
		if !names[c.Name] {
			out = append(out, c)
		}
	}
	return out
}

func (sess *acpSession) close() {
	sess.cancelPrompt()
	if sess.mcp != nil {
		sess.mcp.Close()
	}
	s := sess.ag.Session
	s.Close()
	if len(sess.ag.Messages) == 0 {
		_ = os.Remove(s.Path) // a thread opened and left empty: no session to list
	}
}

func (sess *acpSession) notify(update any) {
	_ = sess.srv.conn.Notify("session/update", acp.SessionNotification{SessionID: sess.id, Update: update})
}

// announce tells the editor what the session offers beyond the prompt.
func (sess *acpSession) announce() {
	sess.notify(acp.CommandsUpdate{SessionUpdate: "available_commands_update", AvailableCommands: sess.commandList()})
	sess.mu.Lock()
	t := sess.title
	sess.mu.Unlock()
	if t != "" {
		sess.notify(acp.SessionInfoUpdate{SessionUpdate: "session_info_update", Title: t})
	}
	sess.sendPlan()
}

// Modes: tego's permission modes, and plan mode.
var acpModes = []acp.Mode{
	{ID: agent.PermYolo, Name: "Run everything", Description: "Edits and commands run without asking"},
	{ID: agent.PermEdits, Name: "Ask before commands", Description: "Edits run; shell commands and MCP tools that change things ask first"},
	{ID: agent.PermAsk, Name: "Ask before changes", Description: "Edits, commands and MCP tools that change things ask first"},
	{ID: "plan", Name: "Plan", Description: "Investigate and propose a plan; change nothing"},
}

func (sess *acpSession) setup() acp.SessionSetup {
	return acp.SessionSetup{Modes: &acp.ModeState{CurrentModeID: sess.mode(), AvailableModes: acpModes},
		ConfigOptions: sess.configOptions()}
}

func (sess *acpSession) mode() string {
	if sess.plan.Load() {
		return "plan"
	}
	return sess.ag.Perms.Mode()
}

func (sess *acpSession) setMode(id string) error {
	switch {
	case id == "plan":
		sess.plan.Store(true)
	case agent.ValidPermission(id):
		sess.plan.Store(false)
		sess.ag.Perms.SetMode(id)
	default:
		return &acp.Error{Code: acp.CodeInvalidParams, Message: "unknown mode " + id}
	}
	return nil
}

// configOptions: the model and the thinking level.
func (sess *acpSession) configOptions() []acp.ConfigOption {
	current := sess.ag.ProviderName + "/" + sess.ag.Model
	models := []acp.SelectOption{}
	seen := map[string]bool{}
	add := func(spec string) {
		if !seen[spec] {
			seen[spec] = true
			models = append(models, acp.SelectOption{Value: spec, Name: spec})
		}
	}
	add(current)
	for _, spec := range cachedModels(sess.srv.cfg, sess.srv.store) {
		add(spec)
	}
	levels := make([]acp.SelectOption, len(llm.ThinkingLevels))
	for i, l := range llm.ThinkingLevels {
		levels[i] = acp.SelectOption{Value: l, Name: l}
	}
	return []acp.ConfigOption{
		{ID: "model", Name: "Model", Category: "model", Type: "select", CurrentValue: current, Options: models},
		{ID: "thinking", Name: "Thinking", Category: "thought_level", Type: "select",
			CurrentValue: firstNonEmpty(sess.ag.Thinking, llm.ThinkingOff), Options: levels},
	}
}

// cachedModels lists the models tego last fetched (models.json), for the
// providers with credentials, without asking the providers now.
func cachedModels(cfg *config.Config, store *auth.Store) []string {
	cache := map[string]modelCacheEntry{}
	if data, err := os.ReadFile(filepath.Join(cfg.Dir, "models.json")); err == nil {
		_ = json.Unmarshal(data, &cache)
	}
	var out []string
	for _, p := range cfg.ProviderNames() {
		if !cfg.Available(p, store) {
			continue
		}
		models := cache[p].Models
		if len(models) == 0 {
			if m := config.DefaultModelFor(p); m != "" {
				models = []string{m}
			}
		}
		for _, m := range models {
			out = append(out, p+"/"+m)
		}
	}
	return out
}

func (sess *acpSession) setConfig(id, value string) error {
	switch id {
	case "model":
		if err := sess.app.setModel(value); err != nil {
			return &acp.Error{Code: acp.CodeInvalidParams, Message: err.Error()}
		}
	case "thinking":
		if !slices.Contains(llm.ThinkingLevels, value) {
			return &acp.Error{Code: acp.CodeInvalidParams, Message: "unknown thinking level " + value}
		}
		sess.ag.Thinking = value
	default:
		return &acp.Error{Code: acp.CodeInvalidParams, Message: "unknown option " + id}
	}
	return nil
}

func (sess *acpSession) cancelPrompt() {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.cancel != nil {
		sess.cancel()
	}
}

// prompt is session/prompt: one turn, streamed as updates.
func (sess *acpSession) prompt(ctx context.Context, blocks []acp.ContentBlock) (any, error) {
	sess.mu.Lock()
	if sess.cancel != nil {
		sess.mu.Unlock()
		return nil, &acp.Error{Code: acp.CodeInvalidRequest, Message: "a prompt is already running in this session"}
	}
	ctx, cancel := context.WithCancel(ctx)
	sess.cancel = cancel
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		sess.cancel = nil
		sess.mu.Unlock()
		cancel()
	}()

	text, images := promptContent(blocks)
	if strings.HasPrefix(text, "/") {
		r, ok := sess.slash(ctx, text)
		switch {
		case !ok: // not a command: an ordinary prompt
		case r.job != nil:
			err := r.job(ctx)
			if ctx.Err() == nil && err != nil && !strings.HasPrefix(err.Error(), "stopped after") {
				return nil, err
			}
			return acp.PromptResponse{StopReason: stopReason(ctx, err)}, nil
		case r.prompt != "":
			text = r.prompt
		default:
			sess.OnText(r.reply)
			return acp.PromptResponse{StopReason: "end_turn"}, nil
		}
	}
	if strings.TrimSpace(text) == "" && len(images) == 0 {
		return acp.PromptResponse{StopReason: "end_turn"}, nil
	}
	sess.waitMCP()
	sess.maybeTitle(text)
	err := sess.ag.Run(ctx, text, images...)
	if ctx.Err() == nil && err != nil && !strings.HasPrefix(err.Error(), "stopped after") {
		return nil, err
	}
	return acp.PromptResponse{StopReason: stopReason(ctx, err)}, nil
}

func stopReason(ctx context.Context, err error) string {
	switch {
	case ctx.Err() != nil:
		return "cancelled"
	case err != nil:
		return "max_turn_requests"
	}
	return "end_turn"
}

// promptContent turns the editor's prompt into tego's: text, with embedded
// files inline and linked ones named, plus images.
func promptContent(blocks []acp.ContentBlock) (string, []llm.Block) {
	var text []string
	var images []llm.Block
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text = append(text, b.Text)
		case "image":
			images = append(images, llm.Block{Type: llm.BlockImage, MediaType: b.MimeType, Data: b.Data})
		case "resource":
			if r := b.Resource; r != nil && r.Text != "" {
				text = append(text, fmt.Sprintf("<file path=%q>\n%s\n</file>", uriPath(r.URI), r.Text))
			}
		case "resource_link":
			text = append(text, "@"+uriPath(b.URI))
		}
	}
	return strings.Join(text, "\n"), images
}

// uriPath is a file URI's path, anything else as is.
func uriPath(uri string) string {
	if u, err := url.Parse(uri); err == nil && u.Scheme == "file" {
		return u.Path
	}
	return uri
}

// waitMCP gives MCP servers still connecting a head start before the first
// request, as the terminal does.
func (sess *acpSession) waitMCP() {
	sess.mu.Lock()
	waited := sess.mcpWaited
	sess.mcpWaited = true
	sess.mu.Unlock()
	if waited {
		return
	}
	for _, st := range sess.mcp.Status() {
		if st.State == "connecting" {
			sess.mcp.Wait(10 * time.Second)
			return
		}
	}
}

// maybeTitle names the session after its first prompt, in the background.
func (sess *acpSession) maybeTitle(prompt string) {
	sess.mu.Lock()
	if sess.titling || sess.title != "" {
		sess.mu.Unlock()
		return
	}
	sess.titling = true
	sess.mu.Unlock()
	p, model := sess.ag.Provider, sess.ag.Model
	if s, ok := sess.app.small(); ok {
		p, model = s.Provider, s.Model
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		title, err := agent.GenerateTitle(ctx, p, model, prompt)
		if err != nil || title == "" {
			return
		}
		sess.mu.Lock()
		sess.title = title
		sess.mu.Unlock()
		_ = sess.ag.Session.SetTitle(title)
		sess.notify(acp.SessionInfoUpdate{SessionUpdate: "session_info_update", Title: title, UpdatedAt: time.Now().UTC().Format(time.RFC3339)})
	}()
}

// The agent's UI: everything becomes session/update notifications.

func (sess *acpSession) OnText(delta string) {
	sess.notify(acp.Chunk{SessionUpdate: "agent_message_chunk", Content: acp.Text(delta)})
}

func (sess *acpSession) OnThinking(delta string) {
	sess.notify(acp.Chunk{SessionUpdate: "agent_thought_chunk", Content: acp.Text(delta)})
}

func (sess *acpSession) OnInfo(msg string) {
	sess.notify(acp.Chunk{SessionUpdate: "agent_thought_chunk", Content: acp.Text("\n\n_" + msg + "_\n\n")})
}

func (sess *acpSession) OnToolStart(string)                                 {}
func (sess *acpSession) OnToolCall(string, json.RawMessage)                 {}
func (sess *acpSession) OnToolResult(string, json.RawMessage, string, bool) {}
func (sess *acpSession) OnTurnEnd(*llm.Message)                             {}

// LiveSubagents: a subagent's calls show as tool calls (OnCall*), not as
// lines of text.
func (sess *acpSession) LiveSubagents() bool { return true }

// toolKind is the ACP kind of a tool, which picks its icon.
func toolKind(name string) string {
	switch name {
	case "read":
		return "read"
	case "edit", "write":
		return "edit"
	case "grep", "glob", "multi_grep", "tool_search":
		return "search"
	case "bash", "bash_output":
		return "execute"
	case "web_search":
		return "fetch"
	case "todo", "task", "checkpoint", "rewind":
		return "think"
	}
	return "other"
}

type callInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	oldNew
	Edits []oldNew `json:"edits"`
}

type oldNew struct {
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

func (sess *acpSession) abs(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(sess.cwd, p)
}

func (sess *acpSession) OnCallStart(agentName, id, name string, input json.RawMessage) {
	title := tools.Summary(name, input)
	if agentName != "" {
		title = "[" + agentName + "] " + title
	}
	var in callInput
	_ = json.Unmarshal(input, &in)
	call := acp.ToolCall{SessionUpdate: "tool_call", ToolCallID: id, Title: title, Kind: toolKind(name), Status: "pending", RawInput: input}
	if p := sess.abs(in.Path); p != "" && (name == "read" || name == "edit" || name == "write") {
		call.Locations = []acp.Location{{Path: p}}
	}
	if name == "write" && in.Path != "" { // its diff needs the file as it was
		var before *string
		if data, err := sess.readForDiff(sess.abs(in.Path)); err == nil {
			before = &data
		}
		sess.mu.Lock()
		sess.before[id] = before
		sess.mu.Unlock()
	}
	sess.notify(call)
}

func (sess *acpSession) readForDiff(p string) (string, error) {
	if sess.env.Files != nil {
		if _, err := os.Stat(p); err != nil {
			return "", err
		}
		return sess.ReadText(context.Background(), p)
	}
	data, err := os.ReadFile(p)
	return string(data), err
}

func (sess *acpSession) OnCallRunning(_, id string) {
	sess.notify(acp.ToolCall{SessionUpdate: "tool_call_update", ToolCallID: id, Status: "in_progress"})
}

// outputMax bounds the tool output shown in the editor; the model gets it all.
const outputMax = 16 << 10

func (sess *acpSession) OnCallEnd(_, id, name string, input json.RawMessage, output string, isErr bool) {
	upd := acp.ToolCall{SessionUpdate: "tool_call_update", ToolCallID: id, Status: "completed"}
	if isErr {
		upd.Status = "failed"
	}
	var in callInput
	_ = json.Unmarshal(input, &in)
	sess.mu.Lock()
	before, hadBefore := sess.before[id]
	delete(sess.before, id)
	term := sess.terms[id]
	delete(sess.terms, id)
	sess.mu.Unlock()
	switch {
	case !isErr && name == "edit":
		edits := in.Edits
		if len(edits) == 0 {
			edits = []oldNew{in.oldNew}
		}
		for _, e := range edits {
			old, nw := e.OldString, e.NewString
			upd.Content = append(upd.Content, acp.ToolCallContent{Type: "diff", Path: sess.abs(in.Path), OldText: &old, NewText: &nw})
		}
	case !isErr && name == "write" && hadBefore:
		content := in.Content
		upd.Content = []acp.ToolCallContent{{Type: "diff", Path: sess.abs(in.Path), OldText: before, NewText: &content}}
	case term != "":
		upd.Content = []acp.ToolCallContent{{Type: "terminal", TerminalID: term}}
	default:
		if len(output) > outputMax {
			output = output[:outputMax] + "\n…"
		}
		block := acp.Text("```\n" + output + "\n```")
		upd.Content = []acp.ToolCallContent{{Type: "content", Content: &block}}
	}
	sess.notify(upd)
	if name == "todo" {
		sess.sendPlan()
	}
}

// sendPlan shows the todo list as the editor's plan.
func (sess *acpSession) sendPlan() {
	todos, _ := sess.ag.TodoState()
	if len(todos) == 0 {
		return
	}
	entries := make([]acp.PlanEntry, len(todos))
	for i, t := range todos {
		content := t.Content
		if t.Phase != "" {
			content = t.Phase + ": " + content
		}
		entries[i] = acp.PlanEntry{Content: content, Priority: "medium", Status: t.Status}
	}
	sess.notify(acp.PlanUpdate{SessionUpdate: "plan", Entries: entries})
}

// approve asks the editor about a call the permission mode holds back.
func (sess *acpSession) approve(ctx context.Context, req agent.Approval) (agent.Decision, error) {
	title := req.Summary
	if req.Agent != "" {
		title = "[" + req.Agent + "] " + title
	}
	opts := []acp.PermissionOption{{OptionID: "allow", Name: "Allow", Kind: "allow_once"}}
	if req.Rule != "" {
		opts = append(opts, acp.PermissionOption{OptionID: "always", Name: "Always allow " + req.Rule + " in this session", Kind: "allow_always"})
	}
	opts = append(opts, acp.PermissionOption{OptionID: "reject", Name: "Reject", Kind: "reject_once"})
	var resp acp.PermissionResponse
	err := sess.srv.conn.Call(ctx, "session/request_permission", acp.PermissionRequest{
		SessionID: sess.id,
		ToolCall:  acp.ToolCall{ToolCallID: req.CallID, Title: title, Kind: toolKind(req.Tool), Status: "pending", RawInput: req.Input},
		Options:   opts,
	}, &resp)
	if err != nil {
		return agent.Decision{}, err
	}
	switch resp.Outcome.OptionID {
	case "allow":
		return agent.Decision{Allow: true}, nil
	case "always":
		return agent.Decision{Allow: true, Always: true}, nil
	}
	return agent.Decision{}, nil // rejected, or the prompt was cancelled
}

// The editor's buffers (tools.TextFiles).

func (sess *acpSession) ReadText(ctx context.Context, path string) (string, error) {
	var resp acp.ReadTextFileResponse
	err := sess.srv.conn.Call(ctx, "fs/read_text_file", acp.ReadTextFileRequest{SessionID: sess.id, Path: path}, &resp)
	return resp.Content, err
}

func (sess *acpSession) WriteText(ctx context.Context, path, content string) error {
	return sess.srv.conn.Call(ctx, "fs/write_text_file", acp.WriteTextFileRequest{SessionID: sess.id, Path: path, Content: content}, nil)
}

// The editor's terminal (tools.Terminal): the command shows live in its
// tool call.

func (sess *acpSession) Run(ctx context.Context, command, cwd string, timeout time.Duration, limit int) (tools.TerminalResult, error) {
	conn := sess.srv.conn
	shell := "bash"
	if _, err := os.Stat("/bin/bash"); err != nil {
		shell = "sh"
	}
	var created acp.CreateTerminalResponse
	if err := conn.Call(ctx, "terminal/create", acp.CreateTerminalRequest{
		SessionID: sess.id, Command: shell, Args: []string{"-c", command}, Cwd: cwd, OutputByteLimit: limit,
		Env: []acp.EnvVariable{{Name: "PAGER", Value: "cat"}, {Name: "GIT_PAGER", Value: "cat"}, {Name: "NO_COLOR", Value: "1"}},
	}, &created); err != nil {
		return tools.TerminalResult{}, err
	}
	ref := acp.TerminalRef{SessionID: sess.id, TerminalID: created.TerminalID}
	// Released (the editor keeps showing its output) whatever happens.
	defer func() { _ = conn.Call(context.Background(), "terminal/release", ref, nil) }()
	if id := tools.CallID(ctx); id != "" {
		sess.mu.Lock()
		sess.terms[id] = created.TerminalID
		sess.mu.Unlock()
		sess.notify(acp.ToolCall{SessionUpdate: "tool_call_update", ToolCallID: id,
			Content: []acp.ToolCallContent{{Type: "terminal", TerminalID: created.TerminalID}}})
	}

	exited := make(chan error, 1)
	wctx, stopWait := context.WithCancel(context.Background())
	defer stopWait()
	go func() { exited <- conn.Call(wctx, "terminal/wait_for_exit", ref, nil) }()
	var res tools.TerminalResult
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-exited:
		if err != nil {
			return res, err
		}
	case <-timer.C:
		res.TimedOut = true
		_ = conn.Call(context.Background(), "terminal/kill", ref, nil)
	case <-ctx.Done():
		_ = conn.Call(context.Background(), "terminal/kill", ref, nil)
	}
	var out acp.TerminalOutputResponse
	if err := conn.Call(context.Background(), "terminal/output", ref, &out); err != nil {
		return res, err
	}
	res.Output, res.Truncated = out.Output, out.Truncated
	if st := out.ExitStatus; st != nil {
		if st.ExitCode != nil {
			res.ExitCode = *st.ExitCode
		}
		res.Signal = st.Signal
	}
	return res, nil
}

// replay sends a loaded session's conversation to the editor.
func (sess *acpSession) replay() {
	msgs := sess.ag.Messages
	results := map[string]llm.Block{}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolResult {
				results[b.ToolUseID] = b
			}
		}
	}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			switch {
			case b.Type == llm.BlockText && m.Role == llm.RoleUser:
				if strings.HasPrefix(b.Text, "<system-reminder>") {
					continue
				}
				sess.notify(acp.Chunk{SessionUpdate: "user_message_chunk", Content: acp.Text(b.Text)})
			case b.Type == llm.BlockText:
				sess.notify(acp.Chunk{SessionUpdate: "agent_message_chunk", Content: acp.Text(b.Text)})
			case b.Type == llm.BlockThinking && b.Text != "":
				sess.notify(acp.Chunk{SessionUpdate: "agent_thought_chunk", Content: acp.Text(b.Text)})
			case b.Type == llm.BlockToolUse:
				r := results[b.ID]
				sess.OnCallStart("", b.ID, b.Name, b.Input)
				sess.OnCallEnd("", b.ID, b.Name, b.Input, r.Content, r.IsError)
			}
		}
	}
	sess.sendPlan()
}
