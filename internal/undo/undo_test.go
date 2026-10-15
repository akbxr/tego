package undo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func TestSnapshotAndRestore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ctx := context.Background()
	base, work := t.TempDir(), t.TempDir()
	write(t, work, "a.go", "package a // v1\n")
	write(t, work, "b.go", "package b\n")
	write(t, work, ".gitignore", "*.log\n")
	write(t, work, "keep.log", "ignored v1\n")
	write(t, work, "node_modules/x/index.js", "dep\n")
	s := Open(base, work)
	if s.Off() != "" {
		t.Fatalf("off: %s", s.Off())
	}
	before, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The agent edits, deletes and creates files; a log file and a dependency change too.
	write(t, work, "a.go", "package a // v2\n")
	os.Remove(filepath.Join(work, "b.go"))
	write(t, work, "new/deep/c.go", "package c\n")
	write(t, work, "keep.log", "ignored v2\n")
	write(t, work, "node_modules/x/index.js", "dep v2\n")
	now, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restore, remove, err := s.Changes(ctx, before, now)
	sort.Strings(restore)
	if err != nil || !reflect.DeepEqual(restore, []string{"a.go", "b.go"}) || !reflect.DeepEqual(remove, []string{"new/deep/c.go"}) {
		t.Fatalf("changes %v %v %v", restore, remove, err)
	}
	if _, _, err := s.Restore(ctx, before, now); err != nil {
		t.Fatal(err)
	}
	if read(t, work, "a.go") != "package a // v1\n" || read(t, work, "b.go") != "package b\n" || read(t, work, "new/deep/c.go") != "<missing>" {
		t.Fatalf("not restored: a=%q b=%q c=%q", read(t, work, "a.go"), read(t, work, "b.go"), read(t, work, "new/deep/c.go"))
	}
	if _, err := os.Stat(filepath.Join(work, "new")); !os.IsNotExist(err) {
		t.Fatal("empty folders of a removed file kept")
	}
	// Ignored and excluded files are not snapshotted, so they are left alone.
	if read(t, work, "keep.log") != "ignored v2\n" || read(t, work, "node_modules/x/index.js") != "dep v2\n" {
		t.Fatal("ignored files were touched")
	}
	// The undo itself can be undone: restore the snapshot taken before it.
	again, _ := s.Snapshot(ctx)
	if _, _, err := s.Restore(ctx, now, again); err != nil || read(t, work, "a.go") != "package a // v2\n" || read(t, work, "new/deep/c.go") != "package c\n" {
		t.Fatalf("redo: %v a=%q", err, read(t, work, "a.go"))
	}
}

func TestEntriesLog(t *testing.T) {
	s := Open(t.TempDir(), t.TempDir())
	now := time.Now().Round(time.Second)
	_ = s.Add("sess", Entry{Tree: "t1", Prompt: "fix it", Time: now})
	_ = s.Add("sess", Entry{Tree: "t2", Time: now})
	_ = s.Add("sess", Entry{Tree: "t2", Time: now}) // a repeated /undo of the same state
	got := s.Entries("sess")
	if len(got) != 2 || got[0].Prompt != "fix it" || got[1].Tree != "t2" || !got[0].Time.Equal(now) {
		t.Fatalf("entries %+v", got)
	}
	if len(s.Entries("other")) != 0 {
		t.Fatal("sessions mixed")
	}
}

func TestOffForHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if Open(t.TempDir(), home).Off() == "" || Open(t.TempDir(), filepath.Dir(home)).Off() == "" {
		t.Fatal("home folder snapshotted")
	}
	if Open(t.TempDir(), filepath.Join(home, "proj")).Off() != "" && func() bool { _, err := exec.LookPath("git"); return err == nil }() {
		t.Fatal("project under home not snapshotted")
	}
}
