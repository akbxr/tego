package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// repo makes a git repository with one commit on main and returns its
// folder and a tego home.
func repo(t *testing.T) (dir, home string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}
	dir = filepath.Join(t.TempDir(), "proj")
	home = t.TempDir()
	run(t, "", "init", "--quiet", "-b", "main", dir)
	write(t, dir, ".gitignore", ".env\n*.log\nnode_modules/\n")
	write(t, dir, "main.go", "package main\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "--quiet", "-m", "first")
	return dir, home
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOpenMakesABranchOutsideTheRepository(t *testing.T) {
	dir, home := repo(t)
	tr, created, err := Open(home, dir, "feature")
	if err != nil || !created {
		t.Fatalf("Open: %v, created %v", err, created)
	}
	if tr.Branch != "tego/feature" || tr.Name != "feature" {
		t.Fatalf("tree %+v", tr)
	}
	if !strings.HasPrefix(tr.Path, Root(home)+string(filepath.Separator)) || strings.HasPrefix(tr.Path, dir) {
		t.Fatalf("worktree at %s, want it under %s and outside the repository", tr.Path, Root(home))
	}
	if b := run(t, tr.Path, "branch", "--show-current"); b != "tego/feature" {
		t.Fatalf("checked out %q", b)
	}
	if _, err := os.Stat(filepath.Join(tr.Path, "main.go")); err != nil {
		t.Fatal("the checkout lacks the repository's files")
	}
	if !sameFile(tr.Repo, dir) {
		t.Fatalf("repo %s, want %s", tr.Repo, dir)
	}

	// The same name again enters it rather than failing.
	again, created, err := Open(home, dir, "feature")
	if err != nil || created || !sameFile(again.Path, tr.Path) {
		t.Fatalf("second Open: %+v created=%v err=%v", again, created, err)
	}
	// From inside the worktree, a new one belongs to the same repository.
	other, _, err := Open(home, tr.Path, "other")
	if err != nil || filepath.Dir(other.Path) != filepath.Dir(tr.Path) {
		t.Fatalf("from inside a worktree: %+v, %v", other, err)
	}
}

func TestOpenNamesAndChecks(t *testing.T) {
	dir, home := repo(t)
	for _, bad := range []string{"a b", "../x", "x/y", "-x", "x.lock", "a..b"} {
		if _, _, err := Open(home, dir, bad); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	tr, _, err := Open(home, dir, "")
	if err != nil || !strings.HasPrefix(tr.Branch, BranchPrefix) || len(tr.Name) < 9 {
		t.Fatalf("unnamed: %+v %v", tr, err)
	}
	if got := freeName(filepath.Dir(tr.Path), time.Now()); got == tr.Name {
		t.Fatalf("freeName gave a name in use: %s", got)
	}
	if _, _, err := Open(home, t.TempDir(), "x"); err == nil || !strings.Contains(err.Error(), "not in a git repository") {
		t.Fatalf("outside a repository: %v", err)
	}
	// An existing tego/ branch is checked out, not made again.
	run(t, dir, "branch", "tego/kept")
	tr, created, err := Open(home, dir, "kept")
	if err != nil || !created || run(t, tr.Path, "branch", "--show-current") != "tego/kept" {
		t.Fatalf("existing branch: %+v %v", tr, err)
	}
}

func TestOpenCopiesTheIgnoredFilesListedInWorktreeinclude(t *testing.T) {
	dir, home := repo(t)
	write(t, dir, ".env", "SECRET=1\n")
	write(t, dir, "config/.env", "NESTED=1\n")
	write(t, dir, "debug.log", "ignored but not listed\n")
	write(t, dir, "notes.txt", "untracked, not ignored\n")
	write(t, dir, "node_modules/x/index.js", "big\n")
	write(t, dir, IncludeFile, ".env\nnotes.txt\n")
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, "config", "link.env")); err != nil {
		t.Fatal(err)
	}
	write(t, dir, IncludeFile, ".env\n*.env\nnotes.txt\n")

	tr, _, err := Open(home, dir, "inc")
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]bool{
		".env": true, "config/.env": true,
		"debug.log": false, "notes.txt": false, "node_modules/x/index.js": false, "config/link.env": false,
	} {
		_, err := os.Lstat(filepath.Join(tr.Path, rel))
		if got := err == nil; got != want {
			t.Errorf("%s copied: %v, want %v", rel, got, want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(tr.Path, ".env")); string(b) != "SECRET=1\n" {
		t.Fatalf(".env = %q", b)
	}
}

func TestStatusCountsWhatRemovingWouldLose(t *testing.T) {
	dir, home := repo(t)
	tr, _, err := Open(home, dir, "work")
	if err != nil {
		t.Fatal(err)
	}
	check := func(want Status) {
		t.Helper()
		got, err := tr.Status()
		if err != nil || got != want {
			t.Fatalf("status %+v (%v), want %+v", got, err, want)
		}
	}
	check(Status{})
	write(t, tr.Path, "main.go", "package main // changed\n")
	write(t, tr.Path, "new/file.go", "package new\n")
	write(t, tr.Path, "x.log", "ignored: not counted\n")
	check(Status{Changed: 2})
	run(t, tr.Path, "add", ".")
	run(t, tr.Path, "commit", "--quiet", "-m", "work")
	check(Status{Unmerged: 1})
	if s := (Status{Changed: 1, Unmerged: 2}).Describe(); s != "1 uncommitted change, 2 commits on no other branch" {
		t.Fatalf("Describe: %q", s)
	}
	// Merged into main: nothing would be lost.
	run(t, dir, "merge", "--quiet", "--ff-only", "tego/work")
	check(Status{})
}

func TestRemoveAndList(t *testing.T) {
	dir, home := repo(t)
	a, _, _ := Open(home, dir, "a")
	b, _, _ := Open(home, dir, "b")
	// A worktree tego did not make is not listed.
	run(t, dir, "worktree", "add", "--quiet", "-b", "mine", filepath.Join(t.TempDir(), "mine"))

	names := func() string {
		trees, err := List(home, dir)
		if err != nil {
			t.Fatal(err)
		}
		var n []string
		for _, tr := range trees {
			n = append(n, tr.Name+"="+tr.Branch)
		}
		return strings.Join(n, ",")
	}
	if got := names(); got != "a=tego/a,b=tego/b" {
		t.Fatalf("listed %q", got)
	}
	if err := a.Remove(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.Path); err == nil {
		t.Fatal("folder still there")
	}
	if strings.Contains(run(t, dir, "branch"), "tego/a") {
		t.Fatal("branch kept")
	}
	if err := b.Remove(false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(run(t, dir, "branch"), "tego/b") {
		t.Fatal("branch deleted though asked to keep it")
	}
	if got := names(); got != "" {
		t.Fatalf("listed %q after removing both", got)
	}
	if _, err := os.Stat(filepath.Dir(a.Path)); err == nil {
		t.Fatal("the empty repository folder was left")
	}

	// A folder deleted by hand: Find no longer sees it, Remove still cleans up.
	c, _, _ := Open(home, dir, "c")
	os.RemoveAll(c.Path)
	if err := c.Remove(true); err != nil {
		t.Fatalf("removing a deleted folder: %v", err)
	}
	if _, err := Find(home, dir, "c"); err == nil {
		t.Fatal("found a removed worktree")
	}
}
