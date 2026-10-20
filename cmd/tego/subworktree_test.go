package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubagentWorktree(t *testing.T) {
	dir, home := gitRepo(t)
	if isolator(home, t.TempDir()) != nil {
		t.Fatal("worktrees outside a git repository")
	}
	sub := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(sub, "a.go"), []byte("package pkg\n"), 0o644)
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "--quiet", "-m", "pkg")
	iso := isolator(home, sub)
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
	if got := finish(); got != "no changes; removed" {
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
	got := finish()
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
	f1()
	f2()
}
