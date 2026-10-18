package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/akbxr/tego/internal/lineedit"
	"github.com/akbxr/tego/internal/worktree"
)

// finishWorktree runs as tego exits from a session started with -w: a
// worktree with nothing in it goes, with its branch; one with work in it
// stays unless the user says otherwise. Without a terminal (-p) it stays.
func (a *app) finishWorktree() {
	t := a.worktree
	if t == nil {
		return
	}
	say := func(format string, args ...any) {
		if a.interactive {
			a.ui.Dim(format, args...)
		} else {
			fmt.Fprintf(os.Stderr, "tego: "+format+"\n", args...)
		}
	}
	st, err := t.Status()
	if err != nil {
		say("kept worktree %s: %v", shortHome(t.Path), err)
		return
	}
	remove := func(deleteBranch bool) {
		_ = os.Chdir(t.Repo) // not inside the folder about to go
		if err := t.Remove(deleteBranch); err != nil {
			say("could not remove worktree %s: %v", shortHome(t.Path), err)
			return
		}
		if deleteBranch {
			say("removed worktree %s and branch %s", t.Name, t.Branch)
		} else {
			say("removed worktree %s, kept branch %s", t.Name, t.Branch)
		}
	}
	if st.Clean() {
		remove(true)
		return
	}
	keep := func() {
		say("kept worktree %s (%s): tego -w %s goes back to it, tego worktree rm %s removes it",
			shortHome(t.Path), st.Describe(), t.Name, t.Name)
	}
	if !a.interactive || a.tui == nil {
		keep()
		return
	}
	items := []lineedit.PickItem{{Label: "Keep it", Detail: "tego -w " + t.Name + " goes back to it"}}
	actions := []func(){keep}
	if st.Unmerged > 0 {
		items = append(items, lineedit.PickItem{Label: "Remove the worktree, keep branch " + t.Branch,
			Detail: lostChanges(st.Changed)})
		actions = append(actions, func() { remove(false) })
	}
	items = append(items, lineedit.PickItem{Label: "Remove it", Detail: "and lose " + st.Describe()})
	actions = append(actions, func() { remove(true) })
	idx, err := a.tui.Pick(fmt.Sprintf("Worktree %s has %s. Remove it?", t.Name, st.Describe()), items, "", 0)
	if err != nil || idx < 0 || idx >= len(actions) {
		idx = 0
	}
	actions[idx]()
}

func lostChanges(n int) string {
	if n == 0 {
		return "its commits stay on the branch"
	}
	return fmt.Sprintf("and lose %s", plural(n, "uncommitted change"))
}

// cmdWorktree is `tego worktree`: list the worktrees tego made for this
// repository, remove one, or remove every one with nothing in it.
func cmdWorktree(home string, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	return worktreeCommand(os.Stdout, home, cwd, args)
}

const worktreeUsage = "usage: tego worktree [list | rm <name> [--force] | prune]"

func worktreeCommand(w io.Writer, home, cwd string, args []string) error {
	action := "list"
	if len(args) > 0 {
		action, args = args[0], args[1:]
	}
	switch action {
	case "list", "ls":
		trees, err := worktree.List(home, cwd)
		if err != nil {
			return err
		}
		if len(trees) == 0 {
			fmt.Fprintln(w, "No worktrees made by tego for this repository. tego -w [name] makes one.")
			return nil
		}
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tBRANCH\tIN IT\tPATH")
		for _, t := range trees {
			in := "?"
			if st, err := t.Status(); err == nil {
				in = st.Describe()
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.Name, t.Branch, in, shortHome(t.Path))
		}
		return tw.Flush()

	case "rm", "remove":
		var name string
		force := false
		for _, a := range args {
			switch a {
			case "-f", "--force":
				force = true
			default:
				if name != "" {
					return errors.New(worktreeUsage)
				}
				name = a
			}
		}
		if name == "" {
			return errors.New(worktreeUsage)
		}
		t, err := worktree.Find(home, cwd, name)
		if err != nil {
			return err
		}
		st, err := t.Status()
		if err != nil {
			return err
		}
		if st.Changed > 0 && !force {
			return fmt.Errorf("worktree %s has %s: tego worktree rm %s --force discards them", name, plural(st.Changed, "uncommitted change"), name)
		}
		// Commits are never lost here: a branch with some of its own stays.
		deleteBranch := st.Unmerged == 0
		if sameDir(cwd, t.Path) {
			_ = os.Chdir(t.Repo)
		}
		if err := t.Remove(deleteBranch); err != nil {
			return err
		}
		if deleteBranch {
			fmt.Fprintf(w, "Removed worktree %s and branch %s.\n", name, t.Branch)
		} else {
			fmt.Fprintf(w, "Removed worktree %s. Kept branch %s: it has %s (git branch -D %s deletes it).\n",
				name, t.Branch, plural(st.Unmerged, "commit")+" on no other branch", t.Branch)
		}
		return nil

	case "prune":
		trees, err := worktree.List(home, cwd)
		if err != nil {
			return err
		}
		removed, kept := 0, 0
		for _, t := range trees {
			st, err := t.Status()
			if err != nil || !st.Clean() {
				kept++
				continue
			}
			if sameDir(cwd, t.Path) {
				_ = os.Chdir(t.Repo)
			}
			if err := t.Remove(true); err != nil {
				return err
			}
			fmt.Fprintf(w, "Removed worktree %s and branch %s.\n", t.Name, t.Branch)
			removed++
		}
		switch {
		case removed == 0 && kept == 0:
			fmt.Fprintln(w, "No worktrees made by tego for this repository.")
		case kept > 0:
			fmt.Fprintf(w, "Kept %s with work in it (tego worktree lists them).\n", plural(kept, "worktree"))
		}
		return nil
	}
	return errors.New(worktreeUsage)
}
