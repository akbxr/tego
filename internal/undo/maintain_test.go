package undo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
}

// project runs git in a project folder.
func project(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// local reports whether the snapshot store keeps object id itself.
func local(s *Store, id string) bool {
	_, err := os.Stat(filepath.Join(s.gitDir, "objects", id[:2], id[2:]))
	return err == nil
}

func blobID(t *testing.T, body string) string {
	t.Helper()
	cmd := exec.Command("git", "hash-object", "--stdin")
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestLinkSharesProjectObjects(t *testing.T) {
	needGit(t)
	ctx := context.Background()
	base, work := t.TempDir(), t.TempDir()
	project(t, work, "init", "-q")
	write(t, work, "big.png", "committed\n")
	project(t, work, "add", ".")
	project(t, work, "commit", "-qm", "1")
	s := Open(base, work)
	before, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	committed := blobID(t, "committed\n")
	if local(s, committed) {
		t.Fatal("a committed file's contents stored a second time")
	}
	write(t, work, "new.txt", "not committed\n")
	if _, err := s.Snapshot(ctx); err != nil || !local(s, blobID(t, "not committed\n")) {
		t.Fatalf("an uncommitted file's contents are not in the snapshot: %v", err)
	}

	// The project's git deletes a version its history no longer holds: the
	// snapshot can't restore it, and says so instead of restoring half.
	write(t, work, "big.png", "rendered again\n")
	cur, _ := s.Snapshot(ctx)
	if err := os.Remove(filepath.Join(work, ".git", "objects", committed[:2], committed[2:])); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Restore(ctx, before, cur); err == nil || !strings.Contains(err.Error(), "no longer has 1 of the files (big.png)") {
		t.Fatalf("restore: %v", err)
	}
	if read(t, work, "big.png") != "rendered again\n" || read(t, work, "new.txt") != "not committed\n" {
		t.Fatal("a failed restore changed files")
	}
}

func TestMaintain(t *testing.T) {
	needGit(t)
	defer func(g time.Duration) { pruneGrace = g }(pruneGrace)
	pruneGrace = 0
	ctx := context.Background()
	base := t.TempDir()
	dir := filepath.Join(base, "snapshots")
	now := time.Now()
	later := now.Add(time.Minute) // objects written now count as old enough

	// A project with a month-old snapshot and a new one.
	work := t.TempDir()
	project(t, work, "init", "-q")
	s := Open(base, work)
	write(t, work, "old.txt", "only in the old snapshot\n")
	old, _ := s.Snapshot(ctx)
	_ = s.Add("sess", Entry{Tree: old, Prompt: "old", Time: now.Add(-40 * 24 * time.Hour)})
	os.Remove(filepath.Join(work, "old.txt"))
	write(t, work, "a.txt", "snapshotted, then committed\n")
	cur, _ := s.Snapshot(ctx)
	_ = s.Add("sess", Entry{Tree: cur, Prompt: "new", Time: now})
	// The project commits a file after it was snapshotted.
	project(t, work, "add", ".")
	project(t, work, "commit", "-qm", "1")
	shared := blobID(t, "snapshotted, then committed\n")
	if !local(s, shared) || !local(s, blobID(t, "only in the old snapshot\n")) {
		t.Fatal("setup: contents not in the store")
	}
	// It was too big to snapshot more than a week ago.
	_ = os.WriteFile(filepath.Join(s.gitDir, tooBigFile), []byte(now.Add(-8*24*time.Hour).Format(time.RFC3339)), 0o600)

	// A folder that is gone, and one with only old snapshots.
	gone := filepath.Join(t.TempDir(), "gone")
	write(t, gone, "x.txt", "x\n")
	g := Open(base, gone)
	tree, _ := g.Snapshot(ctx)
	_ = g.Add("sess", Entry{Tree: tree, Time: now})
	os.RemoveAll(gone)
	stale := t.TempDir()
	write(t, stale, "y.txt", "y\n")
	st := Open(base, stale)
	tree, _ = st.Snapshot(ctx)
	_ = st.Add("sess", Entry{Tree: tree, Time: now.Add(-31 * 24 * time.Hour)})

	Maintain(ctx, dir, later)

	if isDir(g.gitDir) || isDir(st.gitDir) {
		t.Errorf("kept: gone folder %v, old snapshots only %v", isDir(g.gitDir), isDir(st.gitDir))
	}
	if e := s.Entries("sess"); len(e) != 1 || e[0].Tree != cur {
		t.Errorf("entries %+v", e)
	}
	if local(s, blobID(t, "only in the old snapshot\n")) {
		t.Error("contents only an old snapshot held were kept")
	}
	if local(s, shared) {
		t.Error("contents the project committed since are still stored twice")
	}
	if _, err := os.Stat(filepath.Join(s.gitDir, tooBigFile)); err == nil {
		t.Error("an expired too-big mark was kept")
	}
	if n, _ := s.git(ctx, nil, "rev-list", "--count", "refs/tego/latest"); strings.TrimSpace(n) != "1" {
		t.Errorf("chain of %s commits", n)
	}
	// What is kept still restores, the shared contents from the project.
	write(t, work, "a.txt", "changed\n")
	now2, _ := s.Snapshot(ctx)
	if _, _, err := s.Restore(ctx, cur, now2); err != nil || read(t, work, "a.txt") != "snapshotted, then committed\n" {
		t.Fatalf("restore after maintenance: %v %q", err, read(t, work, "a.txt"))
	}
}

func TestTooBig(t *testing.T) {
	needGit(t)
	ctx := context.Background()
	base, work := t.TempDir(), t.TempDir()
	write(t, work, "a.txt", "a\n")
	s := Open(base, work)
	if _, err := s.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	// A git killed mid-snapshot left its lock: the next snapshot goes on.
	lock := filepath.Join(s.gitDir, "index.lock")
	_ = os.WriteFile(lock, nil, 0o600)
	_ = os.Chtimes(lock, time.Now().Add(-time.Minute), time.Now().Add(-time.Minute))
	if _, err := s.Snapshot(ctx); err != nil {
		t.Fatalf("stale lock: %v", err)
	}

	_ = os.WriteFile(lock, nil, 0o600)
	s.giveUp()
	if !strings.Contains(s.Off(), "took over") {
		t.Fatalf("off: %q", s.Off())
	}
	if _, err := os.Stat(lock); err == nil {
		t.Error("the killed git's lock was kept")
	}
	// Other sessions of the folder don't try either, for a week.
	other := Open(base, work)
	if !strings.Contains(other.Off(), "took over") {
		t.Fatalf("another session: %q", other.Off())
	}
	_ = os.WriteFile(filepath.Join(s.gitDir, tooBigFile), []byte(time.Now().Add(-tooBigFor-time.Hour).Format(time.RFC3339)), 0o600)
	if off := Open(base, work).Off(); off != "" {
		t.Fatalf("still off after a week: %q", off)
	}
}
