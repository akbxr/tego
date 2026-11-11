package main

import (
	"cmp"
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/lineedit"
)

// /restart starts tego again on the same session: on the binary installed
// now, with config.json as it is now. The session's model, thinking level,
// permission mode, fast mode and worktree go with it; background tasks and
// shells stop (an ended subagent can be resumed after it, from the session
// file).

// restartCommand handles /restart; true: the REPL ends and main starts
// tego again.
func (a *app) restartCommand() bool {
	if a.noSession && len(a.ag.Messages) > 0 {
		a.ui.Errorf("this session isn't saved (--no-session): a restart would lose it")
		return false
	}
	if !a.confirmRestart() {
		return false
	}
	dir := a.cwd
	if a.worktree != nil {
		dir = a.worktree.Repo // -w opens it again from there
	}
	a.restart = &restartError{dir: dir, args: a.restartArgs(), keepWorktree: true}
	note := ""
	if a.plan.Load() {
		note = " (plan mode will be off: /plan turns it on again)"
	}
	a.ui.Dim("restarting%s", note)
	return true
}

// restartArgs are the flags that start this session again as it is.
func (a *app) restartArgs() []string {
	var args []string
	if a.worktree != nil {
		args = append(args, "-w", a.worktree.Name)
	}
	if s := a.ag.Session; s != nil && len(a.ag.Messages) > 0 {
		args = append(args, "-r", s.Path)
	}
	if a.ag.Provider != nil {
		args = append(args, "-m", a.ag.ProviderName+"/"+a.ag.Model, "-t", cmp.Or(a.ag.Thinking, "off"))
	}
	if a.ag.Perms != nil {
		args = append(args, "--permissions", a.ag.Perms.Mode())
	}
	if a.ag.Fast {
		args = append(args, "--fast")
	}
	if a.noSession {
		args = append(args, "--no-session")
	}
	return args
}

// confirmRestart asks before a restart stops what runs in the background.
func (a *app) confirmRestart() bool {
	var what []string
	if n := a.bg.Running(); n > 0 {
		what = append(what, plural(n, "background task"))
	}
	if a.env != nil {
		if n := a.env.RunningShells(); n > 0 {
			what = append(what, plural(n, "background shell"))
		}
	}
	if len(what) == 0 || a.tui == nil {
		return true
	}
	items := []lineedit.PickItem{
		{Label: "Stop them and restart", Detail: "a stopped subagent can be resumed after it"},
		{Label: "Keep working", Detail: "/tasks shows them"},
	}
	idx, err := a.tui.Pick(strings.Join(what, " and ")+" still running", items, "", 1)
	return err == nil && idx == 0
}

// staleWatch notices, while tego waits for a prompt, that the binary it
// runs from was replaced (an install or tego update) or that config.json
// changed, and says once that /restart takes them in.
type staleWatch struct {
	exe     string
	exeInfo os.FileInfo // the binary as it was at start
	saidExe os.FileInfo // the replacement already told about
	saidCfg string      // the config keys already told about
	next    time.Time
}

const staleEvery = 10 * time.Second

// startStaleWatch notes the binary this process runs from.
func (a *app) startStaleWatch() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if fi, err := os.Stat(exe); err == nil {
		a.stale = &staleWatch{exe: exe, exeInfo: fi, next: time.Now().Add(staleEvery)}
	}
}

// checkStale runs while tego is idle (every second): at most every
// staleEvery it looks at the binary and the config.
func (a *app) checkStale() {
	w := a.stale
	if w == nil || time.Now().Before(w.next) {
		return
	}
	w.next = time.Now().Add(staleEvery)
	if v, ok := w.replaced(); ok {
		a.ui.Dim("tego %s is installed (this session runs %s): /restart goes on with this session on it", v, version)
	}
	if keys := a.configChanged(); keys != "" && keys != w.saidCfg {
		w.saidCfg = keys
		a.ui.Dim("config.json changed (%s): /restart applies it to this session", keys)
	} else if keys == "" {
		w.saidCfg = ""
	}
}

// replaced reports the version of the binary that replaced this one, once.
// The same build installed again doesn't count.
func (w *staleWatch) replaced() (string, bool) {
	fi, err := os.Stat(w.exe)
	if err != nil || same(fi, w.exeInfo) || w.saidExe != nil && same(fi, w.saidExe) {
		return "", false
	}
	w.saidExe = fi
	v := binaryVersion(w.exe)
	if v == version {
		return "", false
	}
	return cmp.Or(v, "another build"), true
}

func same(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size()
}

// binaryVersion asks the tego binary at path for its version.
func binaryVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "tego ")
}

// sessionKeys are the config keys that are a new session's defaults: a
// restart keeps the session's own (another session's /model writes model).
var sessionKeys = map[string]bool{"model": true, "thinking": true, "fast": true, "permissions": true}

// configChanged lists the config keys a restart would apply.
func (a *app) configChanged() string {
	var keys []string
	for _, k := range a.cfg.Changed() {
		if !sessionKeys[k] {
			keys = append(keys, k)
		}
	}
	return strings.Join(keys, ", ")
}
