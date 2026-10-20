package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/akbxr/tego/internal/worktree"
)

// Subagents given "isolation": "worktree" work in a worktree of their own
// (under ~/.tego/worktrees, like tego -w), so several can edit the same
// files at once. One with nothing in it at the end is removed; one with
// work in it stays, and the report names its branch.

// worktreeMu serialises git's worktree bookkeeping across subagents.
var worktreeMu sync.Mutex

// inGitRepo says whether dir is inside a git work tree.
func inGitRepo(dir string) bool {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = dir
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// isolator is the task tool's Isolate for cwd, nil outside a git repo.
func isolator(home, cwd string) func(string) (string, string, func() string, error) {
	if !inGitRepo(cwd) {
		return nil
	}
	return func(task string) (string, string, func() string, error) {
		return isolate(home, cwd, task)
	}
}

var unsafeName = regexp.MustCompile(`[^a-z0-9._-]+`)

// subWorktreeName is a fresh worktree name for a task: never one that
// exists, which Open would hand back with someone's work in it.
func subWorktreeName(task string) string {
	base := strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(task), "-"), "-.")
	if base == "" {
		base = "task"
	}
	if len(base) > 40 {
		base = base[:40]
	}
	b := make([]byte, 3)
	rand.Read(b)
	return "sub-" + base + "-" + hex.EncodeToString(b)
}

func isolate(home, cwd, task string) (string, string, func() string, error) {
	worktreeMu.Lock()
	defer worktreeMu.Unlock()
	wt, _, err := worktree.Open(home, cwd, subWorktreeName(task))
	if err != nil {
		return "", "", nil, err
	}
	// The same folder of the project as the session's.
	dir := wt.Path
	cmd := exec.Command("git", "rev-parse", "--show-prefix")
	cmd.Dir = cwd
	if out, err := cmd.Output(); err == nil {
		if rel := strings.TrimSpace(string(out)); rel != "" {
			if fi, err := os.Stat(filepath.Join(wt.Path, rel)); err == nil && fi.IsDir() {
				dir = filepath.Join(wt.Path, rel)
			}
		}
	}
	note := fmt.Sprintf("You work in a git worktree of your own at %s, on branch %s, not in %s: use paths under %s. "+
		"It is a checkout of the project's last commit; changes not committed in %s are not in it. "+
		"Other subagents may change the same files in their own worktrees. "+
		"When you are done, commit your changes on this branch (git add -A && git commit -m \"...\") and say what you changed.",
		dir, wt.Branch, cwd, dir, cwd)
	finish := func() string {
		worktreeMu.Lock()
		defer worktreeMu.Unlock()
		st, err := wt.Status()
		if err != nil {
			return fmt.Sprintf("kept at %s (branch %s): %v", wt.Path, wt.Branch, err)
		}
		if st.Clean() {
			if err := wt.Remove(true); err != nil {
				return fmt.Sprintf("no changes, but removing %s failed: %v", wt.Path, err)
			}
			return "no changes; removed"
		}
		s := fmt.Sprintf("%s in %s, branch %s. ", st.Describe(), wt.Path, wt.Branch)
		if st.Unmerged > 0 {
			s += "Merge it with: git merge " + wt.Branch
		}
		if st.Changed > 0 {
			if st.Unmerged > 0 {
				s += "; "
			}
			s += "see the uncommitted changes with: git -C " + wt.Path + " diff HEAD"
		}
		return s + ". Remove it afterwards with: tego worktree rm " + wt.Name
	}
	return dir, note, finish, nil
}
