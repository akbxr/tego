package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/akbxr/tego/internal/lineedit"
	"github.com/akbxr/tego/internal/llm"
)

var builtinCommands = []struct{ name, icon, help string }{
	{"help", "?", "Show commands"},
	{"model", "◆", "Pick a model, or /model <query|provider/id> (-r refreshes the list)"},
	{"thinking", "✦", "Pick or set thinking: off|low|medium|high|xhigh|max"},
	{"fast", "↯", "Fast mode on or off: Claude Opus 4.8+ writes up to 2.5x faster at twice the price"},
	{"resume", "↺", "Resume a different session"},
	{"branch", "⟲", "Rewind to a previous turn, keeping the old path as a branch (alias /rewind, Esc Esc)"},
	{"undo", "↶", "Put the files back as they were before a prompt of this session"},
	{"plan", "⏸", "Plan mode: investigate and propose, change nothing until you approve (Shift+Tab toggles)"},
	{"permissions", "⛉", "Which tool calls wait for your approval: /permissions [yolo|edits|ask]"},
	{"tree", "⑂", "Navigate the session tree (switch branches)"},
	{"agents", "⧉", "Show what a subagent of the latest task did (also ←← on an empty editor); /agents defined lists agents/*.md"},
	{"fork", "⑂", "New session from before a chosen prompt"},
	{"clone", "⧉", "Duplicate the current session into a new one"},
	{"compact", "⇊", "Summarise history to free context: /compact [focus]"},
	{"new", "+", "Start a fresh session (alias /clear)"},
	{"session", "i", "Session info and token usage"},
	{"retry", "↻", "Re-run the model on the current history (after an error)"},
	{"skills", "✧", "List discovered skills"},
	{"login", "→", "Log in: Claude Pro/Max, ChatGPT, Google Antigravity, or an API key (/login <url> to paste a redirect)"},
	{"logout", "←", "Remove stored credentials (one account or all)"},
	{"accounts", "◎", "Logged-in accounts, limits, and switching mode: /accounts [fallback|round-robin]"},
	{"usage", "▤", "Subscription usage and resets for every logged-in account"},
	{"stats", "▦", "Requests, tokens and estimated cost: /stats [24h|7d|30d|90d|all]"},
	{"mcp", "⌁", "MCP server status and tool counts"},
	{"exit", "×", "Quit (alias /quit, Ctrl+D)"},
}

// repl is the TUI event loop. Agent work runs in a goroutine ("job") while
// this loop keeps handling keys; input submitted during a job is queued
// and dispatched in order when it finishes.
func (a *app) repl(initial string) error {
	events := a.tui.Events()
	done := make(chan error, 1)
	a.loginDone = make(chan loginResult, 1)
	a.titleDone = make(chan titleResult, 1)
	defer func() {
		if a.login != nil {
			a.login.cancel()
		}
	}()
	defer func() {
		if a.cancel != nil { // exiting mid-turn: stop it and let it record the abort
			a.cancel()
			<-done
		}
	}()
	if initial != "" && a.dispatch(initial, done) {
		return nil
	}
	var lastInterrupt, lastEsc, lastLeft time.Time
	var resized <-chan time.Time
	redrawPending := false
	for {
		a.syncHerdr() // state and session after whatever the last event changed
		select {
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			switch ev.Kind {
			case lineedit.EvSubmit:
				line := strings.TrimSpace(ev.Text)
				if line == "" {
					continue
				}
				a.tui.AddHistory(line)
				if a.cancel != nil {
					// Busy: a prompt steers the running turn; commands and
					// shell lines wait for it to end.
					steer := steerable(line)
					if steer {
						line = a.tokenizeImages(line)
						if a.skillRecent != nil {
							a.skillRecent.touch(line)
						}
					}
					a.addPending(line, steer)
					continue
				}
				if a.dispatch(line, done) {
					return nil
				}
			case lineedit.EvEscape:
				if a.cancel != nil {
					a.cancel()
					a.restorePending(a.tui.Input())
					lastEsc = time.Time{} // an interrupting Esc must not arm the rewind picker
					continue
				}
				// Idle: a second Esc shortly after the first opens the rewind picker.
				if time.Since(lastEsc) < 600*time.Millisecond {
					lastEsc = time.Time{}
					a.rewindCommand()
				} else {
					lastEsc = time.Now()
				}
			case lineedit.EvInterrupt:
				if a.cancel != nil {
					a.cancel()
					a.restorePending(a.tui.Input())
					continue
				}
				if time.Since(lastInterrupt) < 2*time.Second {
					return nil
				}
				lastInterrupt = time.Now()
				a.ui.Dim("(Ctrl+C again to exit)")
			case lineedit.EvEOF:
				return nil
			case lineedit.EvResize:
				// Text already printed keeps the old width (rows are broken by
				// hand for the margins, frames and prompt blocks); re-render the
				// conversation at the new width once the drag settles.
				resized = time.After(250 * time.Millisecond)
			case lineedit.EvLeft:
				// Left twice on an empty editor: look at the subagents.
				if time.Since(lastLeft) < 600*time.Millisecond {
					lastLeft = time.Time{}
					a.agentsCommand()
				} else {
					lastLeft = time.Now()
				}
			case lineedit.EvFollowUp:
				line := strings.TrimSpace(ev.Text)
				if line == "" {
					continue
				}
				a.tui.AddHistory(line)
				if a.cancel == nil { // the turn ended meanwhile
					if a.dispatch(line, done) {
						return nil
					}
					continue
				}
				a.addPending(line, false)
			case lineedit.EvDequeue:
				a.restorePending(ev.Text)
			case lineedit.EvBackTab:
				a.setPlan(!a.plan.Load())
			}
		case res := <-a.loginDone:
			a.finishLogin(res)
		case r := <-a.titleDone:
			a.finishTitle(r)
		case <-resized:
			resized = nil
			if a.cancel != nil {
				redrawPending = true // don't disturb a streaming turn; redraw when it ends
				continue
			}
			a.redrawConversation()
		case err := <-done:
			a.finishJob(err)
			if errors.Is(err, context.Canceled) {
				// Interrupted: unsent messages go back to the editor, like pi.
				a.restorePending(a.tui.Input())
			}
			if err == nil && a.pendingCount() == 0 {
				a.notifyDone() // nothing left to do: tell the user
			}
			planned := a.planTurn
			a.planTurn = false
			if planned && err == nil && a.pendingCount() == 0 && a.plan.Load() && a.ag.LastText() != "" {
				a.offerPlan(done)
			}
			if redrawPending {
				redrawPending = false
				a.redrawConversation()
			}
			for a.cancel == nil {
				line, ok := a.popFollowUp()
				if !ok {
					break
				}
				if a.dispatch(line, done) {
					return nil
				}
			}
		}
	}
}

// dispatch runs one submitted line while idle; returns true to exit.
func (a *app) dispatch(line string, done chan<- error) bool {
	if !strings.HasPrefix(line, "!") {
		line = a.tokenizeImages(line) // typed image paths become "🖼 #n" before the prompt is shown
	}
	if a.skillRecent != nil {
		a.skillRecent.touch(line)
	}
	switch {
	case isSlashCommand(line):
		return a.command(line, done)
	case strings.HasPrefix(line, "!"):
		a.shell(strings.TrimSpace(line[1:]))
	default:
		a.startPrompt(line, done)
	}
	return false
}

// startPrompt sends line to the agent as a new turn.
func (a *app) startPrompt(line string, done chan<- error) {
	a.echoPrompt(line)
	a.maybeTitle(line)
	a.planTurn = a.plan.Load()
	a.startJob(done, func(ctx context.Context) error { return a.runTurn(ctx, line) })
}

// echoPrompt prints a sent prompt as a full-width shaded block with a blank
// shaded row above and below, so each turn is easy to find in the
// scrollback. Without colour it falls back to "❯ prompt".
func (a *app) echoPrompt(line string) {
	c := a.ui.c
	if c.reset == "" {
		fmt.Fprintf(a.out, "\n❯ %s\n", strings.ReplaceAll(line, "\n", "\n  "))
		return
	}
	w := 80
	if a.ui.width != nil {
		if n := a.ui.width(); n > 10 {
			w = n
		}
	}
	// Theme black behind bright white text: a darker band on dark themes,
	// a readable dark band on light ones. ESC[K paints the rest of the row.
	const style = "\x1b[40;97m"
	var b strings.Builder
	row := func(s string) {
		b.WriteString(style + s + "\x1b[K" + c.reset + "\n")
	}
	b.WriteString("\n")
	row("")
	// Image tokens ("🖼 #1") stand out in bold yellow.
	tok := regexp.MustCompile(regexp.QuoteMeta(a.icons.image) + ` #\d+`)
	for _, l := range wrapCells(strings.ReplaceAll(line, "\t", "    "), w-4) {
		row("  " + tok.ReplaceAllString(l, c.yellow+c.bold+"$0"+c.reset+style))
	}
	row("")
	fmt.Fprint(a.out, b.String())
	a.ui.AfterPrompt() // the first output after it starts one blank line lower
}

// wrapCells word-wraps text to rows of at most width terminal cells
// (breaking inside words longer than a row).
func wrapCells(text string, width int) []string {
	width = max(width, 10)
	var out []string
	for _, para := range strings.Split(text, "\n") {
		row, cells := "", 0
		for _, word := range strings.SplitAfter(para, " ") {
			ww := runewidth.StringWidth(word)
			if cells+ww > width && cells > 0 {
				out = append(out, strings.TrimRight(row, " "))
				row, cells = "", 0
			}
			for ww > width { // a word longer than a row
				head := runewidth.Truncate(word, width, "")
				out = append(out, head)
				word = word[len(head):]
				ww = runewidth.StringWidth(word)
			}
			row += word
			cells += ww
		}
		out = append(out, strings.TrimRight(row, " "))
	}
	return out
}

// startJob runs fn in the background; its result arrives on done.
func (a *app) startJob(done chan<- error, fn func(ctx context.Context) error) {
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.jobStart, a.jobBefore = time.Now(), a.ag.Usage()
	_, rev := a.ag.TodoState()
	a.todoRevAtStart.Store(int64(rev)) // a done list shows only if this job updates it
	a.ui.resetRate()
	a.status.update(func(s *statusInfo) { s.busy, s.busySince = true, a.jobStart })
	a.tui.SetBusy(true) // Alt+Enter now queues a follow-up
	a.tui.Redraw()
	go func() { done <- fn(ctx) }()
}

func (a *app) finishJob(err error) {
	a.cancel()
	a.cancel = nil
	a.ui.Finish()
	switch {
	case errors.Is(err, context.Canceled):
		a.ui.OnInfo("interrupted")
	case err != nil:
		a.ui.Errorf("%v", err)
		a.ui.Dim("(/retry to try again)")
	}
	a.usageLine(a.jobBefore, time.Since(a.jobStart))
	a.status.update(func(s *statusInfo) { s.busy = false })
	a.tui.SetBusy(false)
	a.refreshStatus()
	a.refreshGit()
	a.refreshFiles() // the turn may have created or deleted files
	if p := a.reloadProvider; p != "" {
		a.reloadProvider = ""
		a.afterLogin(p)
	}
}

// refreshStatus snapshots agent state for the footer. Call it from the
// goroutine that currently owns the agent (the job, or the loop when idle).
func (a *app) refreshStatus() {
	if a.tui == nil {
		return
	}
	u := a.ag.Usage()
	title := sessionTitle(a.ag.Messages)
	if a.ag.Session != nil && a.ag.Session.Title != "" {
		title = a.ag.Session.Title
	}
	todos, rev := a.ag.TodoState()
	done := 0
	for _, td := range todos {
		if td.Status == "completed" {
			done++
		}
	}
	a.status.update(func(s *statusInfo) {
		s.model, s.spec, s.thinking = modelLabel(a.ag.Model), a.ag.ProviderName+"/"+a.ag.Model, a.ag.Thinking
		s.fast = a.ag.Fast && a.ag.Provider != nil && llm.SupportsFast(a.ag.Provider, a.ag.Model)
		s.fastResting = s.fast && s.usage.Output > 0 && !a.ag.FastActive
		s.perms = a.ag.Perms.Mode()
		if a.ag.Provider == nil {
			s.model, s.spec = "no model · /login", "none"
		}
		s.ctxTokens, s.window, s.usage, s.title = a.ag.LastCtx, a.ag.ContextWindow, u, title
		s.todoDone, s.todoTotal = done, len(todos)
		// Like the TODO panel: a finished list counts only in the turn that
		// updated it, not in later turns or once idle.
		if done == len(todos) && !a.todoThisTurn(s.busy, rev) {
			s.todoDone, s.todoTotal = 0, 0
		}
	})
	a.tui.Redraw()
}

func (a *app) refreshGit() {
	go func() {
		g := gitStatus(a.cwd, a.ui.c, a.status.icons)
		a.status.update(func(s *statusInfo) { s.git = g })
		a.tui.Redraw()
	}()
}

// sessionTitle is the first line of the first prompt.
func sessionTitle(msgs []llm.Message) string {
	for _, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Blocks {
			if b.Type == llm.BlockText && strings.TrimSpace(b.Text) != "" && !strings.HasPrefix(b.Text, "<context-summary>") {
				line, _, _ := strings.Cut(displayPrompt(strings.TrimSpace(b.Text)), "\n")
				return line
			}
		}
	}
	return ""
}

// displayPrompt shows a /skill: invocation the way it was typed instead of
// the expanded skill body.
func displayPrompt(text string) string {
	text = showImageRefs(text)
	rest, ok := strings.CutPrefix(text, `<skill name="`)
	if !ok {
		return text
	}
	name, _, _ := strings.Cut(rest, `"`)
	_, req, _ := strings.Cut(rest, "</skill>")
	req = strings.TrimSpace(req)
	if req == "Apply this skill to the current task." {
		req = ""
	}
	return strings.TrimSpace("/skill:" + name + " " + req)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// isSlashCommand distinguishes "/model x" from a prompt that starts with an
// absolute path (e.g. a screenshot dragged into the terminal).
func isSlashCommand(line string) bool {
	if !strings.HasPrefix(line, "/") {
		return false
	}
	word, _, _ := strings.Cut(line[1:], " ")
	return word != "" && !strings.ContainsAny(word, `/\.`)
}

func (a *app) banner() {
	c := a.ui.c
	model := a.ag.ProviderName + "/" + a.ag.Model
	if a.ag.Provider == nil {
		model = "no model yet"
	}
	fmt.Fprintf(a.out, "%stego%s %s%s · %s%s\n", c.bold+c.magenta, c.reset, c.dim, version, model, c.reset)
	fmt.Fprintf(a.out, "%s/help commands · /model · /resume · !cmd shell · esc interrupts · esc esc rewinds · shift+enter newline · while working: enter steers, alt+enter queues · ctrl+v pastes images%s\n", c.dim, c.reset)
}

// updateBanner tells the user a newer release exists, under the banner.
func (a *app) updateBanner(latest string) {
	c := a.ui.c
	fmt.Fprintf(a.out, "%s⬆ Update available:%s %s%s → %s%s%s · run %stego update%s\n",
		c.bold+c.yellow, c.reset, c.dim, version, c.reset+c.green, latest, c.reset+c.dim, c.reset+c.cyan, c.reset)
}

func (a *app) usageLine(before llm.Usage, d time.Duration) {
	t := a.ag.Usage()
	in, out, cache := t.Input-before.Input, t.Output-before.Output, t.CacheRead-before.CacheRead
	if in == 0 && out == 0 && cache == 0 {
		return
	}
	pct := 0
	if a.ag.ContextWindow > 0 {
		pct = a.ag.LastCtx * 100 / a.ag.ContextWindow
	}
	rate := ""
	if r := a.ui.rate(); r > 0 {
		rate = fmt.Sprintf("%s%.0f tok/s%s%s · ", a.ui.c.green, r, a.ui.c.reset, a.ui.c.dim)
	}
	a.ui.Dim("%s%.1fs · %s in · %s cached · %s out · ctx %d%% of %s",
		rate, d.Seconds(), fmtTokens(in+t.CacheWrite-before.CacheWrite), fmtTokens(cache), fmtTokens(out), pct, fmtTokens(a.ag.ContextWindow))
}

// shell runs !cmd on the real terminal with the TUI suspended.
func (a *app) shell(cmdline string) {
	if cmdline == "" {
		return
	}
	resume := a.tui.Suspend()
	defer resume()
	_ = a.withInterrupt(func(ctx context.Context) error {
		cmd := exec.CommandContext(ctx, "bash", "-c", cmdline)
		cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = a.cwd, os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stdout, "%s%v%s\n", a.ui.c.dim, err, a.ui.c.reset)
		}
		return nil
	})
	a.refreshGit()
}

// suggest feeds the TUI's autocomplete popup: an "@path" token at the end
// of the line completes project files; "/" lists every command, typing
// filters by name (skills included), "/skill:" lists the skills most
// recently used first, and a "/skill:" token typed after other text
// completes a skill reference inside the prompt.
func (a *app) suggest(line string) []lineedit.Suggestion {
	if s := a.suggestFiles(line); s != nil {
		return s
	}
	if before, query, ok := midlineSkillToken(line); ok {
		return filterSuggestions(a.skillSuggestions(before), query)
	}
	if !strings.HasPrefix(line, "/") || strings.ContainsAny(line, " \n") {
		return nil
	}
	query := line[1:]
	if sk, ok := strings.CutPrefix(query, "skill:"); ok {
		return filterSuggestions(a.skillSuggestions(""), sk)
	}
	var all []lineedit.Suggestion
	if len(a.skills) > 0 {
		all = append(all, lineedit.Suggestion{Value: "/skill:", Label: "skill:", Description: plural(len(a.skills), "skill"), Icon: "✧"})
	}
	for _, c := range builtinCommands {
		desc := c.help
		if c.name == "model" && a.status != nil {
			// The popup runs on the TUI goroutine: read the snapshot, not the agent.
			desc = "Model: " + a.status.snapshot().spec
		}
		all = append(all, lineedit.Suggestion{Value: "/" + c.name, Label: c.name, Description: desc, Icon: c.icon, Final: true})
	}
	names := make([]string, 0, len(a.commands))
	for n := range a.commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		all = append(all, lineedit.Suggestion{Value: "/" + n, Label: n, Description: a.commands[n].Description, Icon: "▪", Final: true})
	}
	if query == "" {
		return all
	}
	// Typing a name also finds skills ("/front" → skill:frontend-design).
	return filterSuggestions(append(all, a.skillSuggestions("")...), query)
}

// filterSuggestions ranks by the command or skill name only (prefix
// first), not the description.
func filterSuggestions(all []lineedit.Suggestion, query string) []lineedit.Suggestion {
	if query == "" {
		return all
	}
	items := make([]lineedit.PickItem, len(all))
	for i, s := range all {
		items[i].Label = s.Label
		if name, ok := strings.CutPrefix(s.Label, "skill:"); ok && name != "" {
			items[i].Label = name
		}
	}
	hits := lineedit.Filter(items, query)
	out := make([]lineedit.Suggestion, len(hits))
	for i, h := range hits {
		out[i] = all[h]
	}
	return out
}

// runSkill sends a skill's instructions plus the user's request as a turn.
func (a *app) runSkill(name, args, line string, done chan<- error) {
	for _, s := range a.skills {
		if s.Name != name {
			continue
		}
		body, err := os.ReadFile(s.Path)
		if err != nil {
			a.ui.Errorf("skill %s: %v", name, err)
			return
		}
		req := args
		if req == "" {
			req = "Apply this skill to the current task."
		}
		prompt := fmt.Sprintf("<skill name=%q path=%q>\n%s\n</skill>\n\n%s", s.Name, s.Path, strings.TrimSpace(string(body)), req)
		a.echoPrompt(line)
		a.maybeTitle(line)
		a.startJob(done, func(ctx context.Context) error { return a.runTurn(ctx, prompt) })
		return
	}
	a.ui.Errorf("unknown skill %q (see /skills)", name)
}

// command handles a slash command while idle; long-running ones (compact,
// retry, custom prompts) become jobs. Returns true to exit.
func (a *app) command(line string, done chan<- error) bool {
	name, args, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	args = strings.TrimSpace(args)
	w := a.out
	switch name {
	case "exit", "quit", "q":
		return true
	case "help", "?":
		for _, c := range builtinCommands {
			fmt.Fprintf(w, "  /%-10s %s\n", c.name, c.help)
		}
		if len(a.commands) > 0 {
			names := make([]string, 0, len(a.commands))
			for n := range a.commands {
				names = append(names, n)
			}
			sort.Strings(names)
			fmt.Fprintln(w, "  custom:")
			for _, n := range names {
				fmt.Fprintf(w, "  /%-10s %s\n", n, a.commands[n].Description)
			}
		}
	case "model", "m":
		a.pickModel(args)
	case "thinking", "t":
		if args == "" {
			var ok bool
			if args, ok = a.pickThinking("Thinking level"); !ok {
				return false
			}
		}
		if !validThinking(args) {
			a.ui.Errorf("thinking must be off|low|medium|high|xhigh|max")
			return false
		}
		a.setThinking(args)
	case "fast":
		switch args {
		case "":
			a.setFast(!a.ag.Fast)
		case "on":
			a.setFast(true)
		case "off":
			a.setFast(false)
		default:
			a.ui.Errorf("usage: /fast [on|off]")
		}
	case "permissions":
		if args == "" {
			a.pickPermissions()
			break
		}
		a.setPermissions(args)
	case "compact":
		a.startJob(done, func(ctx context.Context) error { return a.ag.Compact(ctx, args) })
		return false
	case "new", "clear":
		if err := a.newSession(); err != nil {
			a.ui.Errorf("%v", err)
		}
		a.ui.Dim("new session")
	case "undo":
		a.undoFiles()
		return false
	case "plan":
		if args == "" {
			a.setPlan(!a.plan.Load())
			return false
		}
		a.setPlan(true) // /plan <prompt>: plan this right away
		a.startPrompt(args, done)
		return false
	case "resume":
		a.pickSession(args)
		return a.restart != nil // another project's session: tego restarts there
	case "session":
		t := a.ag.Usage()
		if a.ag.Session != nil {
			fmt.Fprintln(w, "session:", a.ag.Session.ID)
			fmt.Fprintln(w, "file:   ", a.ag.Session.Path)
		}
		fmt.Fprintf(w, "model:   %s/%s · thinking %s\n", a.ag.ProviderName, a.ag.Model, a.ag.Thinking)
		fmt.Fprintf(w, "messages: %d · context ~%s / %s\n", len(a.ag.Messages), fmtTokens(a.ag.LastCtx), fmtTokens(a.ag.ContextWindow))
		fmt.Fprintf(w, "tokens:  %s in · %s cache read · %s cache write · %s out\n",
			fmtTokens(t.Input), fmtTokens(t.CacheRead), fmtTokens(t.CacheWrite), fmtTokens(t.Output))
	case "retry":
		a.startJob(done, a.ag.Continue)
		return false
	case "login":
		a.loginCommand(args)
	case "logout":
		a.logoutCommand(args)
	case "accounts":
		a.accountsCommand(args)
	case "stats":
		a.statsCommand(args)
	case "usage":
		a.usageCommand()
	case "mcp":
		a.mcpCommand()
	case "branch", "rewind":
		a.rewindCommand()
	case "agents":
		if strings.TrimSpace(args) == "defined" {
			a.listAgentDefs()
		} else {
			a.agentsCommand()
		}
	case "tree":
		a.treeCommand()
	case "fork":
		a.forkCommand()
	case "clone":
		a.cloneCommand()
	case "skills":
		if len(a.skills) == 0 {
			fmt.Fprintln(w, "no skills found")
		}
		for _, s := range a.skills {
			fmt.Fprintf(w, "  %-24s %s\n", s.Name, s.Description)
		}
	default:
		if sk, ok := strings.CutPrefix(name, "skill:"); ok {
			a.runSkill(sk, args, line, done)
			return false
		}
		if c, ok := a.commands[name]; ok {
			prompt := c.Expand(args)
			a.echoPrompt(line)
			a.maybeTitle(line)
			a.startJob(done, func(ctx context.Context) error { return a.runTurn(ctx, prompt) })
			return false
		}
		a.ui.Errorf("unknown command /%s (try /help)", name)
	}
	a.refreshStatus()
	return false
}
