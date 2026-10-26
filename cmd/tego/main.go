// Command tego is a lightweight terminal coding agent.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/auth"
	"github.com/akbxr/tego/internal/config"
	"github.com/akbxr/tego/internal/ext"
	"github.com/akbxr/tego/internal/lineedit"
	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/lsp"
	"github.com/akbxr/tego/internal/mcp"
	"github.com/akbxr/tego/internal/session"
	"github.com/akbxr/tego/internal/tools"
	"github.com/akbxr/tego/internal/undo"
	"github.com/akbxr/tego/internal/web"
	"github.com/akbxr/tego/internal/worktree"
)

// version is set by the release build (-ldflags "-X main.version=0.2.0").
// Otherwise it comes from the build info: the module version for go install
// …@v0.2.0, or "dev" plus the commit for a build from a checkout, which
// must not look like an old release and offer an update.
var version = ""

func init() {
	if version == "" {
		version = buildVersion()
	}
}

func buildVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := strings.TrimPrefix(bi.Main.Version, "v"); v != "" && v != "(devel)" {
		return v
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	v := "dev"
	if len(rev) >= 7 {
		v += "-" + rev[:7]
	}
	if dirty {
		v += "-dirty"
	}
	return v
}

const usage = `tego — lightweight coding agent

Usage:
  tego [flags] [prompt...]        interactive (prompt optional)
  tego -p [flags] prompt...       print mode: run once, print answer, exit
  echo "..." | tego [flags]       piped stdin implies print mode
  tego login [provider]           Claude Pro/Max, ChatGPT, Google Antigravity, or store an API key
  tego logout <provider>
  tego usage                      subscription limits of every logged-in account
  tego acp                        Agent Client Protocol agent on stdio, for editors (Zed, JetBrains, Neovim…)
  tego stats [-r 7d] [-j]         usage statistics in the terminal (-j: JSON)
  tego stats serve                usage dashboard in the browser
  tego update [--check]           install the latest release
  tego worktree [rm <name>|prune] the git worktrees tego made for this repository

Flags:
  -m, --model provider/model      e.g. anthropic/claude-sonnet-4-5, openai-codex/gpt-5, ollama/qwen3
  --fast                          fast mode (Claude Opus 4.8+): faster output, twice the price
  --permissions yolo|edits|ask    which tool calls wait for your approval (default yolo: none)
  -t, --thinking off|low|medium|high|xhigh|max
  -c, --continue                  continue the most recent session in this directory
  -r, --resume <id>               resume a session by id prefix (no id: list)
  -w, --worktree [name]           work in a git worktree of its own, on branch tego/<name> (made if new)
  -p, --print                     non-interactive
      --no-session                don't persist the session
  -v, --version
  -h, --help

Config: ~/.tego/config.json (TEGO_HOME overrides ~/.tego)`

type options struct {
	model, thinking, resume string
	permissions             string
	print, cont, noSession  bool
	fast                    bool
	listResume              bool
	worktree                bool   // -w: work in a worktree
	worktreeName            string // its name ("": a new one)
	prompt                  []string
}

func parseArgs(args []string) (*options, error) {
	o := &options{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func() (string, error) {
			if k, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(k, "--") {
				return v, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		name, _, _ := strings.Cut(a, "=")
		var err error
		switch name {
		case "-m", "--model":
			o.model, err = val()
		case "-t", "--thinking":
			o.thinking, err = val()
		case "-r", "--resume":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && !strings.Contains(a, "=") {
				o.resume, err = val()
			} else if strings.Contains(a, "=") {
				o.resume, err = val()
			} else {
				o.listResume = true
			}
		case "-w", "--worktree":
			o.worktree = true
			if strings.Contains(a, "=") || i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				o.worktreeName, err = val()
			}
		case "-c", "--continue":
			o.cont = true
		case "-p", "--print":
			o.print = true
		case "--fast":
			o.fast = true
		case "--permissions":
			o.permissions, err = val()
		case "--no-session":
			o.noSession = true
		case "-v", "--version":
			fmt.Println("tego", version)
			os.Exit(0)
		case "-h", "--help":
			fmt.Println(usage)
			os.Exit(0)
		case "--":
			o.prompt = append(o.prompt, args[i+1:]...)
			return o, nil
		default:
			if strings.HasPrefix(a, "-") && len(a) > 1 {
				return nil, fmt.Errorf("unknown flag %s", a)
			}
			o.prompt = append(o.prompt, a)
		}
		if err != nil {
			return nil, err
		}
	}
	return o, nil
}

func main() {
	err := run()
	var rs *restartError
	if errors.As(err, &rs) {
		err = rs.exec() // returns only when the exec fails
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tego:", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	llm.SetGzipRequests(cfg.GzipEnabled())
	store, err := auth.Open(filepath.Join(cfg.Dir, "auth.json"))
	if err != nil {
		return err
	}
	if len(args) > 0 {
		switch args[0] {
		case "login":
			return cmdLogin(cfg, store, args[1:])
		case "logout":
			if len(args) < 2 {
				return errors.New("usage: tego logout <provider>")
			}
			if err := store.Delete(args[1]); err != nil {
				return err
			}
			fmt.Println("logged out of", args[1])
			return nil
		case "stats":
			return cmdStats(cfg, args[1:])
		case "acp":
			return cmdACP(cfg, store, os.Stdin, os.Stdout)
		case "update":
			return cmdUpdate(cfg.Dir, args[1:])
		case "worktree", "worktrees":
			return cmdWorktree(cfg.Dir, args[1:])
		case "usage":
			start := time.Now()
			all := fetchUsage(context.Background(), cfg, store)
			renderUsage(os.Stdout, newPalette(os.Stdout), all, time.Since(start), nil)
			return nil
		}
	}
	o, err := parseArgs(args)
	if err != nil {
		return fmt.Errorf("%w\n\n%s", err, usage)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	var notes []string // said once the UI is up
	var wt *worktree.Tree
	if o.worktree {
		if o.resume != "" || o.listResume {
			return errors.New("-w and -r don't go together: a session resumes in its own folder, worktree included")
		}
		var created bool
		if wt, created, err = worktree.Open(cfg.Dir, cwd, o.worktreeName); err != nil {
			if wt == nil {
				return err
			}
			notes = append(notes, err.Error())
		}
		if err := os.Chdir(wt.Path); err != nil {
			return err
		}
		cwd = wt.Path
		_ = os.Setenv("PWD", cwd)
		verb := "working in worktree"
		if created {
			verb = "made worktree"
		}
		notes = append(notes, fmt.Sprintf("%s %s, on branch %s", verb, shortHome(wt.Path), wt.Branch))
	}
	if o.resume != "" {
		// A session from another project runs in that project's folder.
		if path, err := session.Find(cfg.Dir, cwd, o.resume); err == nil {
			if dir := sessionDir(path); dir != "" && !sameDir(dir, cwd) {
				if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
					// A worktree removed since, say: resume it here.
					notes = append(notes, fmt.Sprintf("the session's folder %s no longer exists, so it resumes here", shortHome(dir)))
				} else {
					if err := os.Chdir(dir); err != nil {
						return fmt.Errorf("session folder %s: %w", dir, err)
					}
					cwd = dir
					_ = os.Setenv("PWD", dir)
				}
			}
			o.resume = path
		}
	}
	stdinTTY := term.IsTerminal(int(os.Stdin.Fd()))
	if !stdinTTY {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		if s := strings.TrimSpace(string(data)); s != "" {
			o.prompt = append(o.prompt, "\n\n"+s)
		}
		o.print = true
	}
	if o.listResume && (o.print || !term.IsTerminal(int(os.Stdout.Fd()))) {
		return listSessions(cfg, cwd)
	}

	spec := o.model
	var noModel error
	if spec == "" {
		if spec, err = cfg.DefaultModel(store); err != nil {
			if o.print {
				return err
			}
			// Interactive: start anyway so the user can /login from the TUI.
			spec, noModel = "", err
		}
	}
	thinking := firstNonEmpty(o.thinking, cfg.Thinking, llm.ThinkingMedium)
	if !validThinking(thinking) {
		return fmt.Errorf("invalid thinking level %q", thinking)
	}

	perms := firstNonEmpty(o.permissions, cfg.Permissions, agent.PermYolo)
	if !agent.ValidPermission(perms) {
		return fmt.Errorf("invalid permission mode %q (yolo, edits or ask)", perms)
	}
	app := &app{cfg: cfg, store: store, cwd: cwd, noSession: o.noSession, interactive: !o.print, out: os.Stdout, icons: icons(cfg.Icons),
		fast: o.fast || cfg.Fast, perms: perms, worktree: wt}
	if app.interactive {
		app.undo = undo.Open(cfg.Dir, cwd)
	}
	imageIcon = app.icons.image
	// Loaded before the TUI starts: its autocomplete reads them from another goroutine.
	app.skills = agent.LoadSkills(cfg.Dir, cwd)
	app.commands = agent.LoadCommands(cfg.Dir, cwd)
	app.agentDefs = agent.LoadAgents(cfg.Dir, cwd)
	if app.interactive {
		pal := colorPalette()
		app.skillRecent = loadSkillRecency(filepath.Join(cfg.Dir, "history"))
		app.status = newStatusBar(pal, cwd, app.icons)
		if wt != nil {
			// The repository, not the long path under ~/.tego; the branch follows.
			app.status.cwd = shortHome(wt.Repo) + " (worktree " + wt.Name + ")"
		}
		app.status.agents = app.runningAgents
		app.status.plan = app.plan.Load
		app.status.shells = func() int {
			if e := app.footerEnv.Load(); e != nil {
				return e.RunningShells()
			}
			return 0
		}
		app.status.shellPort = func() string {
			if e := app.footerEnv.Load(); e != nil {
				return e.RunningPort()
			}
			return ""
		}
		t, err := lineedit.Start(lineedit.Options{
			HistoryPath: filepath.Join(cfg.Dir, "history"),
			Prompt:      pal.magenta + pal.bold + "❯" + pal.reset + " ",
			Suggest:     app.suggest,
			CtrlV:       app.pasteImage,
			Paste:       app.pasteText,
			Header:      app.status.header,
			Status:      app.status.status,
			Activity:    app.status.activity,
			Panel:       app.panel,
			Margin:      1, // keep content off the terminal edges
		})
		if err != nil {
			return err
		}
		defer t.Close()
		app.tui, app.out = t, t.Writer()
		app.ui = newTUIUI(t.Writer(), t.ContentWidth, t.ContentHeight)
		app.ui.onTurnEnd = app.refreshStatus
		app.ui.beforeStream = app.redrawIfResized
		app.ui.toolRun = app.toolRunning
		app.status.rate = app.ui.rate
	} else {
		app.ui = newPrintUI()
	}
	app.ui.cwd = cwd
	if home, err := os.UserHomeDir(); err == nil {
		app.ui.home = home
	}
	if err := app.init(spec, thinking); err != nil {
		return err
	}
	var lspNote string
	app.lsp, lspNote = newLSP(cfg, app.cwd, func(server, reason string) {
		if app.interactive {
			app.ui.Dim("%s did not start (%s); edits get a syntax check only", server, reason)
		}
	})
	if lspNote != "" {
		notes = append(notes, lspNote)
	}
	if app.lsp != nil {
		app.env.LSP = app.lsp
	}
	app.ui.subagents = app.ag.Subagents
	recordStats(cfg, cwd, app.ag, app.ui)
	app.footerAgent.Store(app.ag) // the footer's panels may read it from now on
	app.footerEnv.Store(app.env)
	// Deferred before the rest, so it runs after them: the session is closed
	// and the background shells stopped before the worktree may go, and the
	// terminal is still there to ask.
	defer app.finishWorktree()
	defer app.closeSession()
	app.startMCP()
	defer app.mcp.Close()
	if n := app.startExtensions(); n != "" {
		notes = append(notes, n)
	}
	defer app.ext.Close()
	// Background shells (dev servers, watchers) live as long as tego.
	defer func() {
		if n := app.env.KillShells(); n > 0 && app.interactive {
			app.ui.Dim("stopped %s", plural(n, "background shell"))
		}
	}()
	defer app.lsp.Close()
	defer app.stopBackground()
	if app.interactive {
		app.banner()
		updateOff := cfg.UpdateCheck == "off" || os.Getenv("TEGO_NO_UPDATE_CHECK") != ""
		if v := updateNotice(cfg.Dir, version, updateOff); v != "" {
			app.updateBanner(v)
		}
		go refreshUpdateCache(cfg.Dir, version, updateOff) // for the next start; never blocks this one
		if noModel != nil {
			app.ui.OnInfo("No model available yet: run /login to sign in (Claude, ChatGPT, Google Antigravity) or store an API key, then /model.")
		}
		for _, n := range notes {
			app.ui.Dim("%s", n)
		}
		app.refreshStatus()
		app.refreshGit()
		app.refreshFiles()
	} else {
		for _, n := range notes {
			fmt.Fprintln(os.Stderr, "tego:", n)
		}
	}
	resume := func(ref string) error {
		if app.interactive {
			app.resumeAndShow(ref)
			return nil
		}
		return app.resume(ref)
	}
	switch {
	case o.listResume && app.interactive:
		app.pickSession("")
		if app.restart != nil {
			return app.restart
		}
	case o.resume != "":
		if err := resume(o.resume); err != nil {
			return err
		}
	case o.cont:
		list, err := session.List(cfg.Dir, cwd)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			return errors.New("no previous session in this directory")
		}
		if err := resume(list[0].Path); err != nil {
			return err
		}
	}

	app.refreshStatus()
	prompt := strings.TrimSpace(strings.Join(o.prompt, " "))
	if o.print {
		if prompt == "" {
			return errors.New("print mode needs a prompt")
		}
		err := app.withInterrupt(func(ctx context.Context) error {
			if err := app.runTurn(ctx, prompt); err != nil {
				return err
			}
			return app.drainBackground(ctx) // their reports, before exiting
		})
		app.ui.Finish()
		return err
	}
	// Only the interactive agent owns the pane: a `tego -p` started by
	// another agent's shell in the same pane must not take it over.
	app.herdr = newHerdr()
	defer app.herdr.close()
	if err := app.repl(prompt); err != nil {
		return err
	}
	if app.restart != nil {
		return app.restart // main execs it once the deferred cleanup has run
	}
	return nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func validThinking(t string) bool { return llm.ThinkingRank(t) >= 0 }

func uuid4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type app struct {
	cfg         *config.Config
	store       *auth.Store
	cwd         string
	subModels   subModelCache  // subagent_models a task can ask for
	restart     *restartError  // set by /resume to continue in another project
	worktree    *worktree.Tree // -w: the worktree this session works in
	undo        *undo.Store    // file snapshots before each prompt (interactive only)
	ui          *termUI
	ag          *agent.Agent
	env         *tools.Env
	lsp         *lsp.Manager // language servers checking edits; nil when off
	skills      []agent.Skill
	agentDefs   []agent.AgentDef // agents/*.md
	skillRecent *skillRecency    // nil outside the TUI
	commands    map[string]agent.Command
	noSession   bool
	interactive bool
	out         io.Writer // where command output goes (TUI content or stdout)
	tui         *lineedit.TUI
	status      *statusBar

	// running job (TUI mode); cancel is nil when idle
	cancel    context.CancelFunc
	jobStart  time.Time
	jobBefore llm.Usage
	// Messages typed while busy (see steer.go), guarded by pendMu: steer
	// goes into the running turn, queue (follow-ups) waits for its end.
	pendMu sync.Mutex
	steer  []string
	queue  []string

	attachMu sync.Mutex   // guards pending/imageSeq/clipErr (input and job goroutines)
	pending  []attachment // images (pasted, dragged or typed paths) not yet sent
	imageSeq int
	clipErr  error   // last clipboard failure, reported on the next submit
	icons    iconSet // status line and image tokens

	mcp       *mcp.Manager
	mcpWaited bool
	ext       *ext.Manager // nil: no extensions
	extMsgs   chan string  // prompts extensions send (sendMessage), taken by the REPL
	bg        *agent.Background
	bgDone    chan struct{} // a background task ended: the REPL may hand its report over

	files fileIndex // project paths for the @ picker

	login          *pendingLogin    // browser login in progress (/login)
	loginDone      chan loginResult // its result, handled by the event loop
	titleDone      chan titleResult // generated session titles, handled by the event loop
	titlingFor     *session.Session // session whose title is being generated
	reloadProvider string           // provider logged into during a turn; reloaded after it
	herdr          *herdrReporter   // set inside a herdr pane (interactive only)
	// footerAgent is ag for the TUI goroutine, which starts drawing before
	// init creates the agent.
	footerAgent atomic.Pointer[agent.Agent]
	footerEnv   atomic.Pointer[tools.Env] // likewise, for the background shell count
	footerBG    atomic.Pointer[agent.Background]
	// todoRevAtStart is the todo revision when the current job started
	// (written by the loop, read by the footer).
	todoRevAtStart atomic.Int64
	// redrawWanted: the terminal was resized during a turn; the
	// conversation is redrawn at the next streamed delta, or when the turn
	// ends (redrawIfResized).
	redrawWanted atomic.Bool
	// drawMu: a redraw while a tool runs (toolRunning); toolBusy: the main
	// agent's tool is running, so its goroutine leaves the history alone.
	drawMu   sync.Mutex
	toolBusy bool
	// plan is plan mode (/plan, Shift+Tab), shared with the agent; planTurn
	// marks a running prompt that started in plan mode.
	plan     atomic.Bool
	fast     bool   // fast mode at start (--fast or the config); the agent's Fast after that
	perms    string // permission mode at start (--permissions or the config); the agent's Perms after that
	planTurn bool
}

func (a *app) init(spec, thinking string) error {
	a.env = &tools.Env{Cwd: a.cwd}
	a.env.RecordShells(a.shellRecord()) // for the tego after a crash
	files := agent.LoadContextFiles(a.cfg.Dir, a.cwd)
	mode := agent.Interactive
	if !a.interactive {
		mode = agent.Headless // -p: nobody can answer a question
	}
	a.ag = &agent.Agent{
		System:          agent.SystemPrompt(a.cwd, files, a.skills, mode),
		Env:             func() string { return agent.EnvNote(a.cwd) },
		Notices:         a.env.ShellNotices,
		Thinking:        thinking,
		Fast:            a.fast,
		AutoCompact:     a.cfg.AutoCompactEnabled(),
		MaxOutputTokens: a.cfg.MaxOutput,
		CompactPercent:  a.cfg.CompactAt,
		KeepRecent:      a.cfg.KeepRecent,
		IdleCompactAt:   a.cfg.IdleCompactAt(),
		UI:              a.ui,
		SessionID:       uuid4(),
		// Interactive sessions pause between prompts: keep the prompt cache
		// for an hour. One-shot -p runs and subagents keep the cheaper 5 min.
		LongCache: a.interactive,
		Plan:      &a.plan,
		Perms:     agent.NewPermissions(a.perms),
		// From the models.dev prices: models with a cache-write premium get
		// a more conservative warm elision threshold.
		CacheWritePremium: statsPrices(a.cfg).CacheWritePremium,
	}
	if a.tui != nil {
		a.ag.Steer = a.takeSteering // messages typed mid-turn steer it
		a.ag.Approve = a.approve    // without a terminal, calls that need approval are refused
	}
	task := &agent.TaskTool{Parent: a.ag, System: agent.SystemPrompt(a.cwd, files, a.skills, agent.SubagentMode), Env: a.env, Concurrency: a.cfg.Subagents,
		Small: a.small, Models: a.subagentModels, Agents: a.agents, ResolveModel: a.resolveModel, Isolate: isolator(a.cfg.Dir, a.cwd)}
	a.setupBackground(task)
	if a.tui != nil {
		// Shells a prompt left running get a word before the turn ends.
		a.ag.Shells = func(since time.Time) []tools.ShellInfo { return a.env.ShellsSince(since, "") }
	}
	a.ag.Tools = append(tools.Builtins(a.env), task, &agent.TaskStatusTool{BG: a.bg},
		&agent.TodoTool{Parent: a.ag},
		&agent.CheckpointTool{Parent: a.ag},
		&agent.RewindTool{Parent: a.ag},
		web.NewSearchTool(a.nativeSearch))
	if spec != "" {
		if err := a.setModel(spec); err != nil {
			return err
		}
	}
	return a.newSession()
}

func (a *app) setModel(spec string) error {
	prov, model, err := a.cfg.ParseModel(spec, a.store)
	if err != nil {
		return err
	}
	p, err := a.cfg.NewProvider(prov, a.store)
	if err != nil {
		return err
	}
	if pool, ok := p.(*llm.Pool); ok && a.ui != nil {
		pool.OnSwitch = a.ui.OnInfo // account switches show up in the transcript
	}
	a.ag.Provider, a.ag.ProviderName, a.ag.Model = p, prov, model
	a.ag.ContextWindow = a.cfg.ContextWindowFor(prov, model)
	a.ext.SetModel(a.extModel()) // nil before the extensions start
	return nil
}

func (a *app) newSession() error {
	a.resetBackground()
	a.closeSession()
	a.ag.SetHistory(nil)
	a.ag.Total = llm.Usage{}
	a.ag.SessionID = uuid4()
	if a.noSession {
		return nil
	}
	s, err := session.New(a.cfg.Dir, a.cwd)
	if err != nil {
		return err
	}
	a.ag.Session = s
	return nil
}

func (a *app) resume(ref string) error {
	path, err := session.Find(a.cfg.Dir, a.cwd, ref)
	if err != nil {
		return err
	}
	s, msgs, err := session.Open(path)
	if err != nil {
		return err
	}
	a.resetBackground()
	a.closeSession()
	a.ag.Session = s
	a.ag.SetHistory(msgs)
	a.ui.Dim("resumed %s (%d messages)", s.ID, len(msgs))
	return nil
}

// closeSession closes the current session file, deleting it if nothing was
// ever recorded so aborted launches leave no empty sessions behind.
func (a *app) closeSession() {
	s := a.ag.Session
	if s == nil {
		return
	}
	s.Close()
	if len(a.ag.Messages) == 0 {
		_ = os.Remove(s.Path)
	}
	a.ag.Session = nil
}

// runTurn sends one prompt (with attached files, skills and images)
// through the agent.
func (a *app) runTurn(ctx context.Context, prompt string) error {
	a.waitMCP()
	if a.ext != nil {
		a.waitExtensions()
		p, handled := a.ext.Input(ctx, prompt)
		if handled {
			return nil // an extension took it
		}
		prompt = p
	}
	a.snapshotFiles(ctx, prompt)
	blocks := a.userBlocks(prompt)
	err := a.ag.Run(ctx, blocks[0].Text, blocks[1:]...)
	if a.ext != nil {
		a.ext.Emit(ext.EventTurnEnd, map[string]any{"text": a.ag.LastText()})
	}
	return err
}

// userBlocks turns a typed prompt into the blocks of a user message: the
// text, then the @files, /skill: instructions and images it refers to.
func (a *app) userBlocks(prompt string) []llm.Block {
	files := a.collectMentions(prompt)
	// "/skill:<name>" references in what the user typed; a prompt that
	// starts with /skill: already embeds that one skill.
	typed := displayPrompt(prompt)
	var embedded string
	if strings.HasPrefix(prompt, `<skill name="`) {
		embedded, _, _ = strings.Cut(strings.TrimPrefix(typed, "/skill:"), " ")
	}
	files = append(files, a.skillBlocks(typed, embedded)...)
	prompt, imgs := a.collectImages(a.tokenizeImages(prompt)) // -p prompts arrive with raw paths
	return append(append([]llm.Block{{Type: llm.BlockText, Text: prompt}}, files...), imgs...)
}

func (a *app) withInterrupt(fn func(ctx context.Context) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		select {
		case <-sig:
			cancel()
		case <-ctx.Done():
		}
	}()
	err := fn(ctx)
	if errors.Is(err, context.Canceled) {
		a.ui.OnInfo("interrupted")
		return nil
	}
	return err
}

func listSessions(cfg *config.Config, cwd string) error {
	list, err := session.List(cfg.Dir, cwd)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("no sessions in this directory")
		return nil
	}
	for i, s := range list {
		if i >= 20 {
			break
		}
		fmt.Printf("%s  %s  %3d msgs  %s\n", s.ID, s.Modified.Format("Jan 02 15:04"), s.Count, s.Preview)
	}
	return nil
}

func cmdLogin(cfg *config.Config, store *auth.Store, args []string) error {
	if len(args) == 0 {
		fmt.Println("Subscription login:")
		fmt.Println("  tego login anthropic            Claude Pro/Max")
		fmt.Println("  tego login openai-codex         ChatGPT Plus/Pro (Codex)")
		fmt.Println("  tego login google-antigravity   Google Antigravity (Gemini / Claude via Google account)")
		fmt.Println("Store an API key:")
		fmt.Printf("  tego login <%s>\n", strings.Join(cfg.ProviderNames(), "|"))
		return nil
	}
	name := args[0]
	if len(args) > 1 && args[1] == "--key" {
		return storeKey(store, name)
	}
	for _, p := range config.OAuthProviders {
		if p == name {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()
			if err := auth.Login(ctx, store, name, os.Stdin, os.Stdout); err != nil {
				return err
			}
			fmt.Println("logged in to", name)
			return nil
		}
	}
	found := false
	for _, p := range cfg.ProviderNames() {
		found = found || p == name
	}
	if !found {
		return fmt.Errorf("unknown provider %q", name)
	}
	return storeKey(store, name)
}

func storeKey(store *auth.Store, name string) error {
	fmt.Printf("API key for %s: ", name)
	var key []byte
	var err error
	if term.IsTerminal(int(os.Stdin.Fd())) {
		key, err = term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
	} else {
		var line string
		line, err = bufio.NewReader(os.Stdin).ReadString('\n')
		key = []byte(line)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	k := strings.TrimSpace(string(key))
	if k == "" {
		return errors.New("empty key")
	}
	stored, replaced, err := store.Add(name, auth.Credential{Type: "api_key", Key: k})
	if err != nil {
		return err
	}
	if replaced {
		fmt.Println("key already stored for", name)
	} else {
		fmt.Printf("saved key for %s (%s; %d account(s))\n", name, stored.Label(), len(store.Accounts(name)))
	}
	return nil
}
