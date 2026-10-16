package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreview(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc A() int { return 1 }\n"), 0o644)
	env := &Env{Cwd: dir}
	in := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

	edit := Preview(env, "edit", in(map[string]any{"path": "a.go", "edits": []map[string]any{
		{"old_string": "package a", "new_string": "package b"},
		{"old_string": "return 1", "new_string": "return 2"},
	}}))
	for _, want := range []string{"-1\tpackage a", "+1\tpackage b", "... next change", "-3\tfunc A() int { return 1 }", "+3\tfunc A() int { return 2 }"} {
		if !strings.Contains(edit, want) {
			t.Fatalf("edit preview lacks %q:\n%s", want, edit)
		}
	}
	if got := Preview(env, "write", in(map[string]any{"path": "a.go", "content": "package a\n"})); !strings.Contains(got, "-3\tfunc A()") {
		t.Fatalf("overwrite preview:\n%s", got)
	}
	// Nothing to show: a new file, an edit that doesn't apply, other tools.
	for name, input := range map[string]json.RawMessage{
		"write": in(map[string]any{"path": "new.go", "content": "x"}),
		"edit":  in(map[string]any{"path": "a.go", "old_string": "nope", "new_string": "x"}),
		"bash":  in(map[string]any{"command": "ls"}),
	} {
		if got := Preview(env, name, input); got != "" {
			t.Fatalf("%s: preview %q", name, got)
		}
	}
	// Previewing changes nothing.
	if b, _ := os.ReadFile(filepath.Join(dir, "a.go")); string(b) != "package a\n\nfunc A() int { return 1 }\n" {
		t.Fatalf("file changed: %q", b)
	}
}
