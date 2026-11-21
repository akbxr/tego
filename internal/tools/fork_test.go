package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type nopFiles struct{}

func (nopFiles) ReadText(context.Context, string) (string, error) { return "", os.ErrNotExist }
func (nopFiles) WriteText(context.Context, string, string) error  { return nil }

type nopTerminal struct{}

func (nopTerminal) Run(context.Context, string, string, time.Duration, int) (TerminalResult, error) {
	return TerminalResult{}, nil
}

// Agents that share a folder keep their own record of what they read: A
// reads a file, B changes it, and A's write from its stale copy is refused
// with the current content instead of undoing B's change.
func TestForkKeepsReadsPerAgent(t *testing.T) {
	dir := t.TempDir()
	session := &Env{Cwd: dir}
	a, b := session.Fork(), session.Fork()
	p := filepath.Join(dir, "f.go")
	_ = os.WriteFile(p, []byte("one\ntwo\n"), 0o644)

	if _, err := run(t, &readTool{a}, map[string]any{"path": "f.go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, &readTool{b}, map[string]any{"path": "f.go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, &editTool{b}, map[string]any{"path": "f.go", "old_string": "two", "new_string": "TWO"}); err != nil {
		t.Fatalf("B's edit: %v", err)
	}
	stale := map[string]any{"path": "f.go", "content": "one\ntwo\nthree\n"}
	_, err := run(t, &writeTool{a}, stale)
	if err == nil || !strings.Contains(err.Error(), "changed since it was last read") || !strings.Contains(err.Error(), "2\tTWO") {
		t.Fatalf("A's stale write: %v", err)
	}
	if got, _ := os.ReadFile(p); string(got) != "one\nTWO\n" {
		t.Fatalf("B's change was overwritten: %q", got)
	}
	// The error showed A the current content: a retry goes through.
	if _, err := run(t, &writeTool{a}, map[string]any{"path": "f.go", "content": "one\nTWO\nthree\n"}); err != nil {
		t.Fatalf("A's retry: %v", err)
	}
	// What A read is not B's, nor the session's.
	q := filepath.Join(dir, "q.go")
	_ = os.WriteFile(q, []byte("q\n"), 0o644)
	if _, err := run(t, &readTool{a}, map[string]any{"path": "q.go"}); err != nil {
		t.Fatal(err)
	}
	for name, env := range map[string]*Env{"B": b, "the session": session} {
		if env.wasSeen(q) {
			t.Errorf("A's read counts for %s", name)
		}
	}
}

// A fork shares everything but the read record: the editor's files and
// terminal, the language server and the background shells.
func TestForkShares(t *testing.T) {
	lsp := &fakeChecker{}
	e := &Env{Cwd: t.TempDir(), LSP: lsp}
	p := filepath.Join(e.Cwd, "x.go")
	_ = os.WriteFile(p, []byte("x\n"), 0o644)
	e.MarkRead(p)
	e.Files, e.Terminal = nopFiles{}, nopTerminal{}
	f := e.Fork()
	if f.Cwd != e.Cwd || f.Files != e.Files || f.Terminal != e.Terminal || f.LSP != e.LSP || f.reg() != e.reg() {
		t.Fatalf("fork %+v of %+v", f, e)
	}
	if !e.wasSeen(p) || f.wasSeen(p) || f.shellWrites != 0 {
		t.Fatalf("fork shares the read record: %v", f.seen)
	}
	if (*Env)(nil).Fork() != nil {
		t.Fatal("nil fork")
	}
	// A worktree's Env shares the shells and the language server only.
	w := e.At(t.TempDir())
	if w.Files != nil || w.Terminal != nil || w.LSP != e.LSP || w.reg() != e.reg() || w.seen != nil {
		t.Fatalf("At: %+v", w)
	}
}
