package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/akbxr/tego/internal/config"
	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/peer"
	"github.com/akbxr/tego/internal/session"
)

// session_start (with "session_messages": true, in the terminal): the
// model starts another tego session, interactive, where the user can watch
// it and type in it: a herdr tab when tego runs in herdr, a tmux window in
// tmux, else a detached tmux session. The new tego takes the session id
// chosen here (--session-id) and gets the prompt as a session message that
// asks for a notice, so its last answer comes back when its turn ends;
// session_send and sessions read reach it by that id.

const (
	maxStarted = 6                // sessions one session may have started and open at once
	startWait  = 45 * time.Second // for a started tego to take messages
)

type startTool struct {
	a *app
	// open opens a terminal running cmdline in dir (openPane; tests
	// replace it).
	open func(ctx context.Context, dir, label, id, cmdline string) (*pane, error)

	mu      sync.Mutex
	started []string // ids of the sessions it started
}

func (t *startTool) Def() llm.Tool {
	return llm.Tool{
		Name: "session_start",
		Description: "Start another tego session with a prompt, interactive, in a new terminal tab the user can switch to, watch and type in (a herdr tab or a tmux window). " +
			"It works on its own with a context of its own and keeps going after this turn ends. When its turn ends or it closes, you get a notice with its last answer, so don't poll it. " +
			"Use it when the user asks for a session of its own, or for long, independent work they may want to follow or steer; a subagent (task) is lighter for work that only reports back. " +
			"session_send messages it and sessions read shows what it did. It knows nothing of this conversation: write the prompt as for a colleague, self-contained. " +
			fmt.Sprintf("At most %d started sessions open at once.", maxStarted),
		Schema: json.RawMessage(`{"type":"object","properties":{` +
			`"prompt":{"type":"string","description":"what the session should do"},` +
			`"name":{"type":"string","description":"a short name for its tab"},` +
			`"folder":{"type":"string","description":"the folder it works in (default: this session's)"},` +
			`"worktree":{"type":"boolean","description":"work in a git worktree of its own, on a branch of its own"},` +
			`"model":{"type":"string","description":"default: this session's. A subagent model name, an alias (opus, sonnet, fable) or provider/model; ':level' sets the thinking level"},` +
			`"thinking":{"type":"string","description":"its thinking level (default: this session's)"}` +
			`},"required":["prompt"]}`),
	}
}

func (t *startTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Prompt, Name, Folder, Model, Thinking string
		Worktree                              bool
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	a := t.a
	if strings.TrimSpace(in.Prompt) == "" {
		return "", errors.New("the prompt is empty")
	}
	if len(in.Prompt) > peer.MaxText {
		return "", fmt.Errorf("the prompt is %d bytes; at most %d: put the details in a file and point to it", len(in.Prompt), peer.MaxText)
	}
	if n := t.openStarted(ctx); n >= maxStarted {
		return "", fmt.Errorf("%d sessions this one started are open: message one with session_send, or wait for one to close", n)
	}
	dir := cmp.Or(in.Folder, a.cwd)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(a.cwd, dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("no folder %s", dir)
	}
	model, thinking, err := a.startModel(in.Model, in.Thinking)
	if err != nil {
		return "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	id := session.NewID()
	args := []string{exe}
	if in.Worktree {
		args = append(args, "--worktree") // followed by a flag: a worktree of a new name
	}
	args = append(args, "--session-id", id, "-m", model, "-t", thinking, "--permissions", a.permMode())
	label := startLabel(in.Name, in.Prompt)
	open := t.open
	if open == nil {
		open = openPane
	}
	p, err := open(ctx, dir, label, id, shellLine(args))
	if err != nil {
		return "", err
	}
	if err := waitOpen(ctx, a.cfg.Dir, id); err != nil {
		out := p.read()
		p.close()
		if out != "" {
			err = fmt.Errorf("%w; its terminal showed:\n%s", err, out)
		}
		return "", err
	}
	t.mu.Lock()
	t.started = append(t.started, id)
	t.mu.Unlock()

	self := a.peerSelf()
	sent, err := peer.Send(ctx, a.cfg.Dir, peer.Message{To: id, From: self.ID, FromTitle: self.Title, FromCwd: self.Cwd,
		Text: in.Prompt, Hops: self.Hops + 1, CanReply: true, Notify: true, Sent: time.Now()})
	if err != nil {
		return "", fmt.Errorf("session %s started in %s, but the prompt didn't reach it: %w", id, p.where, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Started session %s in %s (%s, %s · %s).", id, p.where, shortHome(dir), model, thinking)
	if in.Worktree {
		b.WriteString(" It works in a git worktree of its own.")
	}
	if sent.Status == "held" {
		b.WriteString(" It holds the prompt for its user: " + sent.Note)
	} else {
		b.WriteString(" It is working on the prompt; when its turn ends or it closes, you get a notice with its last answer.")
	}
	if p.attach != "" {
		b.WriteString(" The user can watch it with: " + p.attach)
	}
	return b.String(), nil
}

// openStarted counts the sessions t started that are still open.
func (t *startTool) openStarted(ctx context.Context) int {
	t.mu.Lock()
	ids := append([]string(nil), t.started...)
	t.mu.Unlock()
	if len(ids) == 0 {
		return 0
	}
	n := 0
	for _, o := range peer.List(ctx, t.a.cfg.Dir) {
		for _, id := range ids {
			if o.ID == id {
				n++
			}
		}
	}
	return n
}

// waitOpen waits until a tego has session id open and takes messages.
func waitOpen(ctx context.Context, base, id string) error {
	ctx, cancel := context.WithTimeout(ctx, startWait)
	defer cancel()
	for {
		for _, o := range peer.List(ctx, base) {
			if o.ID == id {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("the new tego didn't open session %s in %s", id, startWait)
			}
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// startModel is the model and thinking level a started session runs on:
// this session's unless asked otherwise.
func (a *app) startModel(spec, thinking string) (string, string, error) {
	level := cmp.Or(a.ag.Thinking, llm.ThinkingOff)
	model := a.ag.ProviderName + "/" + a.ag.Model
	if spec = strings.TrimSpace(spec); spec != "" && spec != "main" {
		base, lvl := spec, ""
		if i := strings.LastIndexByte(spec, ':'); i >= 0 && llm.ThinkingRank(spec[i+1:]) >= 0 {
			base, lvl = spec[:i], spec[i+1:]
		}
		for _, m := range a.cfg.SubagentModelList() {
			if m.Name == base {
				base, lvl = config.SubagentModelSpec(m.SubagentModel, a.ag.ProviderName), cmp.Or(lvl, m.Thinking)
			}
		}
		if id := claudeAliases[strings.ToLower(base)]; id != "" {
			prov := "anthropic"
			if a.cfg.Kind(a.ag.ProviderName) == "anthropic" { // a gateway for Anthropic's API
				prov = a.ag.ProviderName
			}
			base = prov + "/" + id
		}
		prov, m, err := a.cfg.ParseModel(base, a.store)
		if err != nil {
			return "", "", err
		}
		if prov != a.ag.ProviderName && !a.cfg.Available(prov, a.store) {
			return "", "", fmt.Errorf("not logged in to %s (tego login %s)", prov, prov)
		}
		model, level = prov+"/"+m, cmp.Or(lvl, level)
	}
	level = cmp.Or(thinking, level)
	if !validThinking(level) {
		return "", "", fmt.Errorf("invalid thinking level %q", level)
	}
	return model, level, nil
}

// permMode is the session's permission mode now.
func (a *app) permMode() string {
	if a.ag.Perms != nil {
		return a.ag.Perms.Mode()
	}
	return a.perms
}

// startLabel names a started session's tab: name, else the prompt's first
// words.
func startLabel(name, prompt string) string {
	if name = strings.TrimSpace(name); name != "" {
		return clipRunes(name, 24)
	}
	label := ""
	for _, w := range strings.Fields(prompt) {
		if len([]rune(label+" "+w)) > 24 {
			if label == "" {
				return clipRunes(w, 24)
			}
			return label + "…"
		}
		label = strings.TrimSpace(label + " " + w)
	}
	return label
}

// shellLine quotes args for a POSIX shell.
func shellLine(args []string) string {
	q := make([]string, len(args))
	for i, s := range args {
		if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=@%+,") == "" {
			q[i] = s
		} else {
			q[i] = "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return strings.Join(q, " ")
}

// pane is a terminal session_start opened.
type pane struct {
	where  string        // what it is, for the model and the user
	attach string        // a command that shows it, when the user can't just switch to it
	read   func() string // what it shows now
	close  func()
}

// openPane opens a terminal in dir that runs cmdline in the user's shell
// (which stays, so that the user sees why if tego fails to start): a herdr
// tab, a tmux window, or a detached tmux session.
func openPane(ctx context.Context, dir, label, id, cmdline string) (*pane, error) {
	if bin := os.Getenv("HERDR_BIN_PATH"); bin != "" && os.Getenv("HERDR_PANE_ID") != "" {
		return herdrTab(ctx, bin, dir, label, cmdline)
	}
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		return nil, errors.New("session_start opens the session in a herdr tab or a tmux window, and there is neither: run tego in herdr or tmux, or install tmux")
	}
	if os.Getenv("TMUX") != "" {
		return tmuxPane(ctx, tmux, label, cmdline, "", "new-window", "-d", "-P", "-F", "#{pane_id}", "-c", dir, "-n", label)
	}
	name := "tego-" + id
	p, err := tmuxPane(ctx, tmux, label, cmdline, name, "new-session", "-d", "-P", "-F", "#{pane_id}", "-s", name, "-c", dir, "-x", "200", "-y", "50")
	if err == nil {
		p.where = "the detached tmux session " + name
		p.attach = "tmux attach -t " + name
	}
	return p, err
}

func herdrTab(ctx context.Context, bin, dir, label, cmdline string) (*pane, error) {
	args := []string{"tab", "create", "--cwd", dir, "--label", label, "--no-focus"}
	if ws := os.Getenv("HERDR_WORKSPACE_ID"); ws != "" {
		args = append(args, "--workspace", ws)
	}
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("herdr tab create: %w", cmdErr(err))
	}
	var r struct {
		Result struct {
			RootPane struct {
				PaneID string `json:"pane_id"`
			} `json:"root_pane"`
			Tab struct {
				TabID string `json:"tab_id"`
			} `json:"tab"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &r); err != nil || r.Result.RootPane.PaneID == "" {
		return nil, fmt.Errorf("herdr tab create said %q", strings.TrimSpace(string(out)))
	}
	paneID, tab := r.Result.RootPane.PaneID, r.Result.Tab.TabID
	closeTab := func() { _ = exec.Command(bin, "tab", "close", tab).Run() }
	if out, err := exec.CommandContext(ctx, bin, "pane", "run", paneID, cmdline).CombinedOutput(); err != nil {
		closeTab()
		return nil, fmt.Errorf("herdr pane run: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return &pane{
		where: fmt.Sprintf("the herdr tab %q", label),
		read: func() string {
			out, _ := exec.Command(bin, "pane", "read", paneID, "--lines", "30", "--source", "recent").Output()
			return strings.TrimSpace(string(out))
		},
		close: closeTab,
	}, nil
}

// tmuxPane runs tmux with args, which make a pane and print its id, and
// types cmdline into it. session: the tmux session it made, if any.
func tmuxPane(ctx context.Context, tmux, label, cmdline, session string, args ...string) (*pane, error) {
	cmd := exec.CommandContext(ctx, tmux, args...)
	cmd.Env = tmuxEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tmux %s: %w", args[0], cmdErr(err))
	}
	id := strings.TrimSpace(string(out))
	kill := func() {
		if session != "" {
			_ = exec.Command(tmux, "kill-session", "-t", session).Run()
		} else {
			_ = exec.Command(tmux, "kill-pane", "-t", id).Run()
		}
	}
	if err := exec.CommandContext(ctx, tmux, "send-keys", "-t", id, "-l", cmdline).Run(); err != nil {
		kill()
		return nil, fmt.Errorf("tmux send-keys: %w", err)
	}
	_ = exec.CommandContext(ctx, tmux, "send-keys", "-t", id, "Enter").Run()
	return &pane{
		where: fmt.Sprintf("the tmux window %q", label),
		read: func() string {
			out, _ := exec.Command(tmux, "capture-pane", "-p", "-t", id).Output()
			return strings.TrimSpace(string(out))
		},
		close: kill,
	}, nil
}

// tmuxEnv is the environment for a tmux server tego may start: without
// herdr's pane, which the new session doesn't run in.
func tmuxEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "HERDR_") {
			env = append(env, kv)
		}
	}
	return env
}

func cmdErr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}
