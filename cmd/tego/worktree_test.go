package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/worktree"
)

func TestWorktreeFlag(t *testing.T) {
	for _, c := range []struct {
		args   []string
		name   string
		prompt string
	}{
		{[]string{"-w"}, "", ""},
		{[]string{"-w", "fix-login"}, "fix-login", ""},
		{[]string{"--worktree=fix-login", "do it"}, "fix-login", "do it"},
		{[]string{"-w", "-m", "x/y", "do it"}, "", "do it"},
		{[]string{"-w", "fix", "do", "it"}, "fix", "do it"},
	} {
		o, err := parseArgs(c.args)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if !o.worktree || o.worktreeName != c.name || strings.Join(o.prompt, " ") != c.prompt {
			t.Errorf("%v: worktree=%v name=%q prompt=%q", c.args, o.worktree, o.worktreeName, o.prompt)
		}
	}
}

// gitRepo makes a repository with one commit and returns it and a tego home.
func gitRepo(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	dir := filepath.Join(t.TempDir(), "proj")
	for _, args := range [][]string{{"init", "--quiet", "-b", "main", dir}, {"-C", dir, "commit", "--quiet", "--allow-empty", "-m", "first"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir, t.TempDir()
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestWorktreeCommand(t *testing.T) {
	dir, home := gitRepo(t)
	cmd := func(args ...string) (string, error) {
		var b bytes.Buffer
		err := worktreeCommand(&b, home, dir, args)
		return b.String(), err
	}
	if out, _ := cmd(); !strings.Contains(out, "No worktrees made by tego") {
		t.Fatalf("empty list: %q", out)
	}
	clean, _, _ := worktree.Open(home, dir, "clean")
	dirty, _, _ := worktree.Open(home, dir, "dirty")
	done, _, _ := worktree.Open(home, dir, "done")
	os.WriteFile(filepath.Join(dirty.Path, "wip.go"), []byte("package wip\n"), 0o644)
	os.WriteFile(filepath.Join(done.Path, "f.go"), []byte("package f\n"), 0o644)
	gitIn(t, done.Path, "add", ".")
	gitIn(t, done.Path, "commit", "--quiet", "-m", "done")

	out, err := cmd("list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"clean  ", "no changes", "1 uncommitted change", "1 commit on no other branch", "tego/done"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}

	// rm refuses to drop uncommitted work without --force.
	if _, err := cmd("rm", "dirty"); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("rm of a worktree with changes: %v", err)
	}
	// It keeps a branch that has commits of its own.
	out, err = cmd("rm", "done")
	if err != nil || !strings.Contains(out, "Kept branch tego/done") {
		t.Fatalf("rm done: %q %v", out, err)
	}
	// prune removes what has nothing in it and keeps the rest.
	out, err = cmd("prune")
	if err != nil || !strings.Contains(out, "Removed worktree clean") || !strings.Contains(out, "Kept 1 worktree") {
		t.Fatalf("prune: %q %v", out, err)
	}
	if _, err := os.Stat(clean.Path); err == nil {
		t.Fatal("prune left the clean worktree")
	}
	if out, err = cmd("rm", "dirty", "--force"); err != nil || !strings.Contains(out, "and branch tego/dirty") {
		t.Fatalf("rm --force: %q %v", out, err)
	}
	if _, err := cmd("rm"); err == nil {
		t.Fatal("rm without a name")
	}
}

// At the end of a -p run, a worktree with nothing in it goes, one with
// work stays: nobody is there to ask.
func TestFinishWorktreeInPrintMode(t *testing.T) {
	dir, home := gitRepo(t)
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })

	empty, _, _ := worktree.Open(home, dir, "empty")
	os.Chdir(empty.Path)
	(&app{worktree: empty}).finishWorktree()
	if _, err := os.Stat(empty.Path); err == nil {
		t.Fatal("a worktree with nothing in it was kept")
	}

	work, _, _ := worktree.Open(home, dir, "work")
	os.WriteFile(filepath.Join(work.Path, "a.go"), []byte("package a\n"), 0o644)
	(&app{worktree: work}).finishWorktree()
	if _, err := os.Stat(filepath.Join(work.Path, "a.go")); err != nil {
		t.Fatal("a worktree with work in it was removed")
	}
}
