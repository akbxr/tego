package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubagentWorktree(t *testing.T) {
	dir, home := gitRepo(t)
	if isolator(home, t.TempDir(), false) != nil {
		t.Fatal("worktrees outside a git repository")
	}
	sub := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(sub, "a.go"), []byte("package pkg\n"), 0o644)
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "--quiet", "-m", "pkg")
	iso := isolator(home, sub, false)
	if iso == nil {
		t.Fatal("no worktrees in a git repository")
	}

	// Nothing done: removed with its branch.
	wd, note, finish, err := iso("Look Around")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(wd) != "pkg" || !strings.Contains(wd, string(filepath.Separator)+"sub-look-around-") {
		t.Fatalf("worked in %s, want the worktree's pkg folder", wd)
	}
	if !strings.Contains(note, "on branch tego/sub-look-around-") || !strings.Contains(note, "commit your changes") {
		t.Fatalf("note %q", note)
	}
	if got := finish(true); got != "no changes; removed" {
		t.Fatalf("finish %q", got)
	}
	if _, err := os.Stat(wd); !os.IsNotExist(err) {
		t.Fatal("empty worktree left behind")
	}

	// A commit and an uncommitted change: kept, with how to merge and look.
	wd, _, finish, err = iso("Fix")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wd, "a.go"), []byte("package pkg\n\nfunc A() {}\n"), 0o644)
	gitIn(t, wd, "commit", "--quiet", "-am", "A")
	os.WriteFile(filepath.Join(wd, "b.go"), []byte("package pkg\n"), 0o644)
	got := finish(true)
	for _, want := range []string{"1 uncommitted change, 1 commit on no other branch", "Merge it with: git merge tego/sub-fix-", "diff HEAD", "tego worktree rm sub-fix-"} {
		if !strings.Contains(got, want) {
			t.Errorf("finish %q lacks %q", got, want)
		}
	}
	if _, err := os.Stat(wd); err != nil {
		t.Fatal("worktree with work in it removed")
	}
	// Two tasks of one name get worktrees of their own.
	w1, _, f1, _ := iso("Same")
	w2, _, f2, _ := iso("Same")
	if w1 == w2 {
		t.Fatal("two tasks share a worktree")
	}
	f1(true)
	f2(true)
}

// subagent_merge: the commits of a subagent that succeeded are merged into
// the session's branch and its worktree removed; a failed one, one with
// uncommitted changes, or a merge that conflicts leaves things as they were.
func TestSubagentWorktreeMerge(t *testing.T) {
	dir, home := gitRepo(t)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644)
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "--quiet", "-m", "a")
	iso := isolator(home, dir, true)
	commit := func(wd, file, text string) {
		os.WriteFile(filepath.Join(wd, file), []byte(text), 0o644)
		gitIn(t, wd, "add", "-A")
		gitIn(t, wd, "commit", "--quiet", "-m", file)
	}

	wd, note, finish, err := iso("Add B")
	if err != nil || !strings.Contains(note, "your commits are merged into the session's branch") {
		t.Fatalf("%v %q", err, note)
	}
	commit(wd, "b.txt", "b\n")
	if got := finish(true); !strings.HasPrefix(got, "merged into ") || !strings.Contains(got, "(1 commit); removed") {
		t.Fatalf("finish %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "b.txt")); string(b) != "b\n" {
		t.Fatal("b.txt not merged")
	}
	if _, err := os.Stat(wd); !os.IsNotExist(err) {
		t.Fatal("merged worktree left behind")
	}

	// Failed: kept, not merged.
	wd, _, finish, _ = iso("Failed")
	commit(wd, "c.txt", "c\n")
	if got := finish(false); !strings.Contains(got, "Merge it with: git merge") {
		t.Fatalf("failed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "c.txt")); err == nil {
		t.Fatal("a failed subagent's work merged")
	}

	// Uncommitted work: kept, not merged.
	wd, _, finish, _ = iso("Dirty")
	commit(wd, "d.txt", "d\n")
	os.WriteFile(filepath.Join(wd, "e.txt"), []byte("e\n"), 0o644)
	if got := finish(true); !strings.Contains(got, "1 uncommitted change") || strings.Contains(got, "merged into") {
		t.Fatalf("dirty: %q", got)
	}

	// A conflict: the merge is undone, the worktree kept.
	wd, _, finish, _ = iso("Clash")
	commit(wd, "a.txt", "theirs\n")
	commit(dir, "a.txt", "ours\n")
	got := finish(true)
	if !strings.HasPrefix(got, "not merged (git merge: ") || !strings.Contains(got, "Merge it with: git merge") {
		t.Fatalf("conflict: %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "ours\n" {
		t.Fatalf("a.txt after a conflict: %q", b)
	}
	if out, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output(); len(out) != 0 {
		t.Fatalf("session's checkout left dirty: %s", out)
	}
}
