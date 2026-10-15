package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, tool Tool, args map[string]any) (string, error) {
	t.Helper()
	in, _ := json.Marshal(args)
	return tool.Run(context.Background(), in)
}

func TestEditRequiresFreshRead(t *testing.T) {
	dir := t.TempDir()
	env := &Env{Cwd: dir}
	read, edit := &readTool{env}, &editTool{env}
	p := filepath.Join(dir, "a.go")
	_ = os.WriteFile(p, []byte("one\ntwo\nthree\n"), 0o644)
	change := map[string]any{"path": "a.go", "old_string": "two", "new_string": "TWO"}

	// Unread, and the text is not there exactly once: nothing changes, and
	// the error carries the numbered content.
	miss := map[string]any{"path": "a.go", "old_string": "TWO", "new_string": "2"}
	_, err := run(t, edit, miss)
	if err == nil || !strings.Contains(err.Error(), "was not read in this session and did not match it exactly once") || !strings.Contains(err.Error(), "2\ttwo") {
		t.Fatalf("edit before read: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "one\ntwo\nthree\n" {
		t.Fatalf("file changed by a rejected edit: %q", b)
	}
	// That content counts as read: an edit now succeeds.
	if _, err := run(t, edit, change); err != nil {
		t.Fatalf("retry after rejection: %v", err)
	}
	// A partial read is enough too: edit matches text, not line numbers.
	q := filepath.Join(dir, "q.go")
	_ = os.WriteFile(q, []byte("a\nb\nc\n"), 0o644)
	if _, err := run(t, read, map[string]any{"path": "q.go", "offset": 3, "limit": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, edit, map[string]any{"path": "q.go", "old_string": "a", "new_string": "A"}); err != nil {
		t.Fatalf("edit after partial read: %v", err)
	}
	// The tool's own edit keeps the record current: a second edit needs no re-read.
	if _, err := run(t, edit, map[string]any{"path": "a.go", "old_string": "one", "new_string": "ONE"}); err != nil {
		t.Fatalf("second edit: %v", err)
	}
	// A change made outside the tools (formatter, user, git) is detected, and
	// the error shows the new content so the edit can be retried directly.
	_ = os.WriteFile(p, []byte("ONE\nTWO\nthree\nfour\n"), 0o644)
	four := map[string]any{"path": "a.go", "old_string": "four", "new_string": "4"}
	if _, err := run(t, edit, four); err == nil || !strings.Contains(err.Error(), "changed since it was last read") || !strings.Contains(err.Error(), "4\tfour") {
		t.Fatalf("edit after external change: %v", err)
	}
	if _, err := run(t, edit, four); err != nil {
		t.Fatalf("retry after the content was shown: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "ONE\nTWO\nthree\n4\n" {
		t.Fatalf("content = %q", b)
	}
}

func TestEditUnreadFileNeedsExactUniqueMatches(t *testing.T) {
	dir := t.TempDir()
	env := &Env{Cwd: dir}
	edit := &editTool{env}
	file := func(name, s string) string {
		_ = os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644)
		return name
	}
	content := func(name string) string { b, _ := os.ReadFile(filepath.Join(dir, name)); return string(b) }

	// Seen only through grep: exact, unique matches go through, all of them.
	f := file("g.go", "func a() {\n\treturn 1\n}\n\nfunc b() {\n\treturn 2\n}\n")
	if _, err := run(t, edit, map[string]any{"path": f, "edits": []map[string]any{
		{"old_string": "\treturn 1", "new_string": "\treturn 10"},
		{"old_string": "\treturn 2", "new_string": "\treturn 20"},
	}}); err != nil {
		t.Fatalf("exact unique edits on an unread file: %v", err)
	}
	if !strings.Contains(content(f), "return 10") || !strings.Contains(content(f), "return 20") {
		t.Fatalf("content %q", content(f))
	}

	// Refused on an unread file, with the content shown, and nothing written:
	for name, c := range map[string]map[string]any{
		"ambiguous": {"old_string": "x", "new_string": "y"},
		"loose":     {"old_string": "if ok {\nreturn x\n}", "new_string": "if ok {\n\treturn y\n}"}, // matches only ignoring indentation
		"all":       {"old_string": "x", "new_string": "y", "replace_all": true},
	} {
		orig := "if ok {\n\t\treturn x\n}\nx := 1\n"
		n := file(name+".go", orig)
		c["path"] = n
		_, err := run(t, edit, c)
		if err == nil || !strings.Contains(err.Error(), "was not read in this session") || content(n) != orig {
			t.Fatalf("%s: err %v, content %q", name, err, content(n))
		}
	}

	// One bad change in a multi-edit refuses all of it.
	m := file("m.go", "alpha\nbeta\n")
	_, err := run(t, edit, map[string]any{"path": m, "edits": []map[string]any{
		{"old_string": "alpha", "new_string": "A"}, {"old_string": "gamma", "new_string": "G"},
	}})
	if err == nil || !strings.Contains(err.Error(), "change 2 of 2 did not match") || content(m) != "alpha\nbeta\n" {
		t.Fatalf("multi-edit: %v, %q", err, content(m))
	}
}

func TestWriteOverwriteRequiresRead(t *testing.T) {
	dir := t.TempDir()
	env := &Env{Cwd: dir}
	write, edit := &writeTool{env}, &editTool{env}
	// New files need no read, and what write wrote can be edited right away.
	if _, err := run(t, write, map[string]any{"path": "new/b.txt", "content": "hello\n"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := run(t, edit, map[string]any{"path": "new/b.txt", "old_string": "hello", "new_string": "hi"}); err != nil {
		t.Fatalf("edit after write: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "c.txt"), []byte("keep me\n"), 0o644)
	_, err := run(t, write, map[string]any{"path": "c.txt", "content": "gone\n"})
	if err == nil || !strings.Contains(err.Error(), "1\tkeep me") {
		t.Fatalf("overwrite of an unread file: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "c.txt")); string(b) != "keep me\n" {
		t.Fatalf("file changed: %q", b)
	}
	// The rejection showed the content, so the retry goes through.
	if _, err := run(t, write, map[string]any{"path": "c.txt", "content": "replaced\n"}); err != nil {
		t.Fatalf("overwrite retry: %v", err)
	}
	// A binary file cannot be shown, but the retry is still allowed.
	_ = os.WriteFile(filepath.Join(dir, "d.bin"), []byte{0, 1, 2, 0, 3}, 0o644)
	if _, err := run(t, write, map[string]any{"path": "d.bin", "content": "text\n"}); err == nil || !strings.Contains(err.Error(), "cannot be shown") {
		t.Fatalf("binary overwrite: %v", err)
	}
	if _, err := run(t, write, map[string]any{"path": "d.bin", "content": "text\n"}); err != nil {
		t.Fatalf("binary overwrite retry: %v", err)
	}
}
