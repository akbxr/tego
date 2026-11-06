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
// work in it stays, and the report names its branch. With subagent_merge,
// the commits of one that succeeded are merged into the session's branch.

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
func isolator(home, cwd string, merge bool) func(string) (string, string, func(bool) string, error) {
	if !inGitRepo(cwd) {
		return nil
	}
	return func(task string) (string, string, func(bool) string, error) {
		return isolate(home, cwd, task, merge)
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

func isolate(home, cwd, task string, merge bool) (string, string, func(bool) string, error) {
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
	if merge {
		note += " If you end without an error, your commits are merged into the session's branch; changes you didn't commit are not."
	}
	finish := func(ok bool) string {
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
		s := ""
		if merge && ok && st.Changed == 0 && st.Unmerged > 0 {
			done, err := mergeBranch(cwd, wt.Branch)
			if err == nil {
				if err := wt.Remove(true); err != nil {
					return done + fmt.Sprintf("; removing %s failed: %v", wt.Path, err)
				}
				return done + "; removed"
			}
			s = fmt.Sprintf("not merged (%v). ", err)
		}
		s += fmt.Sprintf("%s in %s, branch %s. ", st.Describe(), wt.Path, wt.Branch)
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

// mergeBranch merges branch into the branch checked out in cwd, or leaves
// cwd as it was.
func mergeBranch(cwd, branch string) (string, error) {
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if _, err := git("rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil {
		return "", fmt.Errorf("a merge is in progress in %s", cwd)
	}
	into, err := git("symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		into = "the detached HEAD"
	}
	n, _ := git("rev-list", "--count", "HEAD.."+branch)
	if out, err := git("merge", "--no-edit", branch); err != nil {
		if _, conflict := git("rev-parse", "-q", "--verify", "MERGE_HEAD"); conflict == nil {
			git("merge", "--abort")
		}
		why, _, _ := strings.Cut(out, "\n")
		if why == "" {
			why = err.Error()
		}
		return "", fmt.Errorf("git merge: %s", why)
	}
	commits := "commits"
	if n == "1" {
		commits = "commit"
	}
	return fmt.Sprintf("merged into %s (%s %s)", into, n, commits), nil
}
