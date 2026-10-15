package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/akbxr/tego/internal/lineedit"
	"github.com/akbxr/tego/internal/session"
)

// pickSession resolves /resume [query]: a unique id prefix (in this folder,
// else in any project) resumes directly, otherwise the session picker opens.
// Tab switches between this folder's sessions and every project's; Ctrl+D
// (or Backspace on an empty filter, the Mac "delete" key, or Delete)
// deletes the selected session once Enter confirms it; Ctrl+R renames it
// and Ctrl+S changes the order.
func (a *app) pickSession(query string) {
	if query != "" {
		if path, err := session.Find(a.cfg.Dir, a.cwd, query); err == nil {
			a.openSession(path)
			return
		}
	}
	all, keep, order := false, "", sortRecent
	for {
		list, err := a.sessionList(all)
		if err != nil {
			a.ui.Errorf("%v", err)
			return
		}
		if len(list) == 0 {
			if all {
				a.ui.Dim("no sessions yet")
				return
			}
			all = true // nothing here: show every project's instead
			continue
		}
		sortSessions(list, order)
		items, sel := a.sessionItems(list, all, keep)
		title, tab := "Resume session · this folder", "all projects"
		if all {
			title, tab = "Resume session · all projects", "this folder"
		}
		r, err := a.tui.PickWith(items, lineedit.PickOptions{Title: title, Query: query, Selected: sel, Delete: true, Tab: tab,
			Rename: true, Sort: order.String()})
		if err != nil {
			if !errors.Is(err, lineedit.ErrCanceled) {
				a.ui.Errorf("%v", err)
			}
			return
		}
		query = r.Query
		switch r.Action {
		case lineedit.PickSelect:
			a.openSession(list[r.Index].Path)
			return
		case lineedit.PickTab:
			all = !all
			if r.Index >= 0 {
				keep = list[r.Index].Path
			}
		case lineedit.PickDelete:
			keep = neighbour(list, r.Index)
			a.deleteSession(list[r.Index])
		case lineedit.PickRename:
			keep = list[r.Index].Path
			a.renameSession(list[r.Index])
		case lineedit.PickSort:
			order = (order + 1) % sortOrders
			if r.Index >= 0 {
				keep = list[r.Index].Path
			}
		}
	}
}

// sessionOrder is how the resume picker sorts sessions (Ctrl+S cycles).
type sessionOrder int

const (
	sortRecent sessionOrder = iota // last changed first
	sortSize                       // biggest file first: what to clean up
	sortName                       // by title, A-Z
	sortOrders
)

func (o sessionOrder) String() string {
	return [...]string{"recent", "size", "name"}[o]
}

func sortSessions(list []session.Info, o sessionOrder) {
	sort.SliceStable(list, func(i, j int) bool {
		switch o {
		case sortSize:
			return list[i].Size > list[j].Size
		case sortName:
			return strings.ToLower(list[i].Preview) < strings.ToLower(list[j].Preview)
		}
		return list[i].Modified.After(list[j].Modified)
	})
}

// renameSession asks for a new title (prefilled with the current one) and
// records it. An empty title keeps the old one.
func (a *app) renameSession(s session.Info) {
	title, err := a.tui.Ask("Rename session", s.Preview)
	title = strings.TrimSpace(strings.ReplaceAll(title, "\n", " "))
	if err != nil || title == "" || title == s.Preview {
		return
	}
	if a.isCurrent(s.Path) {
		err = a.ag.Session.SetTitle(title) // the open file: one writer
		a.refreshStatus()                  // the footer shows the title
	} else {
		err = session.Rename(s.Path, title)
	}
	if err != nil {
		a.ui.Errorf("rename: %v", err)
	}
}

func (a *app) sessionList(all bool) ([]session.Info, error) {
	if all {
		return session.ListAll(a.cfg.Dir)
	}
	return session.List(a.cfg.Dir, a.cwd)
}

// sessionItems renders list for the picker and finds the row of keep.
func (a *app) sessionItems(list []session.Info, all bool, keep string) ([]lineedit.PickItem, int) {
	items := make([]lineedit.PickItem, len(list))
	sel := 0
	for i, s := range list {
		label := s.Preview
		if label == "" {
			label = "(no prompt)"
		}
		parts := []string{ago(s.Modified), fmt.Sprintf("%d msgs", s.Count), fmtBytes(int(s.Size))}
		if all {
			parts = append(parts, shortHome(s.Cwd))
		}
		if a.isCurrent(s.Path) {
			parts = append(parts, "current")
		}
		parts = append(parts, s.ID)
		items[i] = lineedit.PickItem{Label: label, Detail: strings.Join(parts, " · ")}
		if s.Path == keep {
			sel = i
		}
	}
	return items, sel
}

func (a *app) isCurrent(path string) bool {
	return a.ag.Session != nil && a.ag.Session.Path == path
}

// neighbour is the path the cursor moves to once list[i] is gone.
func neighbour(list []session.Info, i int) string {
	switch {
	case i+1 < len(list):
		return list[i+1].Path
	case i > 0:
		return list[i-1].Path
	}
	return ""
}

func (a *app) deleteSession(s session.Info) {
	if a.isCurrent(s.Path) {
		a.ui.Errorf("that is the session in use: start a /new one or resume another first")
		return
	}
	label := s.Preview
	if label == "" {
		label = s.ID
	}
	where := "The file is removed for good."
	if session.CanTrash() {
		where = "The file moves to the Trash."
	}
	// Like pi: Enter confirms, Esc keeps it.
	idx, err := a.tui.Pick(fmt.Sprintf("Delete %q? %s Enter deletes, Esc keeps it.", label, where),
		[]lineedit.PickItem{{Label: "Delete it", Detail: shortHome(s.Path)}, {Label: "Keep it"}}, "", 0)
	if err != nil || idx != 0 {
		return
	}
	if err := session.Delete(a.cfg.Dir, s.Path); err != nil {
		a.ui.Errorf("%v", err)
		return
	}
	a.ui.Dim("deleted session %s", s.ID)
}

// openSession resumes path here, or restarts tego in the session's folder
// when it belongs to another project, so the system prompt, AGENTS.md,
// project MCP servers, skills, git status and file index all match it.
func (a *app) openSession(path string) {
	dir := sessionDir(path)
	if dir == "" || sameDir(dir, a.cwd) {
		a.resumeAndShow(path)
		return
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		idx, err := a.tui.Pick(fmt.Sprintf("Its folder %s no longer exists. Resume it in %s?", shortHome(dir), shortHome(a.cwd)),
			[]lineedit.PickItem{{Label: "No"}, {Label: "Yes, resume it here"}}, "", 0)
		if err == nil && idx == 1 {
			a.resumeAndShow(path)
		}
		return
	}
	if a.cancel != nil {
		a.ui.Errorf("wait for the running turn to finish, or stop it, before switching projects")
		return
	}
	argv := []string{"-r", path}
	if a.ag.Provider != nil {
		argv = append(argv, "-m", a.ag.ProviderName+"/"+a.ag.Model, "-t", a.ag.Thinking)
	}
	a.restart = &restartError{dir: dir, args: argv}
	a.ui.Dim("switching to %s", shortHome(dir))
}

// restartError makes main re-exec tego in dir with args, after run's
// deferred cleanup (terminal mode, session file, MCP servers) has run.
type restartError struct {
	dir  string
	args []string
}

func (r *restartError) Error() string { return "restart in " + r.dir }

func (r *restartError) exec() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.Chdir(r.dir); err != nil {
		return err
	}
	// Replace PWD (with a duplicate the first one wins), so the new process
	// shows the folder as the session stored it, not symlink-resolved.
	env := []string{"PWD=" + r.dir}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PWD=") {
			env = append(env, kv)
		}
	}
	return syscall.Exec(exe, append([]string{os.Args[0]}, r.args...), env)
}

// sessionDir is the folder a session file was started in ("" if unknown).
func sessionDir(path string) string {
	s, _, err := session.Open(path)
	if err != nil {
		return ""
	}
	defer s.Close()
	return s.Cwd
}

func sameDir(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// shortHome writes paths under the home directory as ~/...
func shortHome(p string) string {
	if home, err := os.UserHomeDir(); err == nil && (p == home || strings.HasPrefix(p, home+string(filepath.Separator))) {
		return "~" + p[len(home):]
	}
	return p
}
