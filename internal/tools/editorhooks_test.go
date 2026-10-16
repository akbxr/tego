package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buffers is an editor whose buffers may differ from the disk.
type buffers struct {
	files  map[string]string
	writes int
}

func (b *buffers) ReadText(_ context.Context, p string) (string, error) {
	if s, ok := b.files[p]; ok {
		return s, nil
	}
	data, err := os.ReadFile(p)
	return string(data), err
}

func (b *buffers) WriteText(_ context.Context, p, content string) error {
	b.files[p] = content
	b.writes++
	return nil
}

// Through an editor, the tools see its buffer (unsaved changes included),
// and edits go to the buffer, not the disk.
func TestToolsThroughEditorBuffers(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p, []byte("package a\n\nfunc A() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ed := &buffers{files: map[string]string{p: "package a\n\nfunc A() int { return 2 } // unsaved\n"}}
	env := &Env{Cwd: dir, Files: ed}
	read, edit, write := &readTool{env: env}, &editTool{env: env}, &writeTool{env: env}

	out, err := run(t, read, map[string]any{"path": "a.go"})
	if err != nil || !strings.Contains(out, "return 2 } // unsaved") {
		t.Fatalf("read: %q %v", out, err)
	}
	// The buffer was read, so editing it is not refused as stale, although
	// the disk differs.
	if out, err = run(t, edit, map[string]any{"path": "a.go", "old_string": "return 2", "new_string": "return 3"}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if ed.files[p] != "package a\n\nfunc A() int { return 3 } // unsaved\n" {
		t.Fatalf("buffer after edit: %q", ed.files[p])
	}
	if disk, _ := os.ReadFile(p); !strings.Contains(string(disk), "return 1") {
		t.Fatalf("the disk was written: %q", disk)
	}
	// Overwriting goes through the editor too.
	if _, err = run(t, write, map[string]any{"path": "a.go", "content": "package a\n"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if ed.files[p] != "package a\n" || ed.writes != 2 {
		t.Fatalf("buffer after write: %q (%d writes)", ed.files[p], ed.writes)
	}
	// A change made in the editor since the last read is caught.
	ed.files[p] = "package b\n"
	if _, err = run(t, edit, map[string]any{"path": "a.go", "old_string": "package b", "new_string": "package c"}); err == nil ||
		!strings.Contains(err.Error(), "changed since it was last read") {
		t.Fatalf("stale edit: %v", err)
	}
}

type fakeTerminal struct {
	got struct {
		command, cwd string
		timeout      time.Duration
	}
	res TerminalResult
}

func (f *fakeTerminal) Run(_ context.Context, command, cwd string, timeout time.Duration, _ int) (TerminalResult, error) {
	f.got.command, f.got.cwd, f.got.timeout = command, cwd, timeout
	return f.res, nil
}

func TestBashThroughEditorTerminal(t *testing.T) {
	dir := t.TempDir()
	term := &fakeTerminal{res: TerminalResult{Output: "FAIL a_test.go:3\n", ExitCode: 1}}
	bash := &bashTool{env: &Env{Cwd: dir, Terminal: term}}
	out, err := run(t, bash, map[string]any{"command": "go test ./...", "timeout": 30})
	if err != nil || out != "FAIL a_test.go:3\n[exit code 1]" {
		t.Fatalf("out %q, err %v", out, err)
	}
	if term.got.command != "go test ./..." || term.got.cwd != dir || term.got.timeout != 30*time.Second {
		t.Fatalf("ran %+v", term.got)
	}
	term.res = TerminalResult{Output: "tail\n", Truncated: true, TimedOut: true, ExitCode: -1, Signal: "SIGKILL"}
	if out, _ = run(t, bash, map[string]any{"command": "sleep 999"}); !strings.HasPrefix(out, "[earlier output dropped]\ntail\n[timed out after") {
		t.Fatalf("timed out: %q", out)
	}
	// The guard still applies.
	if _, err = run(t, bash, map[string]any{"command": "rm -rf ~"}); err == nil {
		t.Fatal("blocked command ran in the terminal")
	}
}
