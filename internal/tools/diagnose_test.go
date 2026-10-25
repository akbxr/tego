package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteReportsSyntaxErrors(t *testing.T) {
	dir := t.TempDir()
	write := &writeTool{&Env{Cwd: dir}}
	cases := []struct {
		path, content, checker, want string
	}{
		{"ok.go", "package x\n\nfunc F() {}\n", "gofmt", ""},
		{"bad.go", "package x\n\nfunc F() {\n", "gofmt", "bad.go:3:"},
		{"bad.json", "{\n  \"a\": 1,\n  \"b\": \n}\n", "", "bad.json:4"},
		{"bad.py", "def f(:\n    pass\n", "python3", "bad.py"},
		{"bad.sh", "if then\n", "bash", "bad.sh"},
		// Types are parsed, not checked; imports are not resolved.
		{"ok.ts", "import { x } from \"./missing\"\nexport const y: number = x + 1\n", "bun", ""},
		{"bad.ts", "function f(a: string {\n}\n", "bun", "Expected \")\""},
		{"ok.tsx", "export const A = (p: { n: number }) => <div>{p.n}</div>\n", "bun", ""},
		{"bad.tsx", "export const B = () => <div>{1</div>\n", "bun", "bad.tsx:1"},
		{"ok.luau", "local x: number = 1\nlocal function f(a: string): string\n\treturn a\nend\nx += 1\n", "luau-compile", ""},
		{"bad.luau", "local x: number =\nlocal function f(a: string\n", "luau-compile", "bad.luau(2,1): SyntaxError"},
		{"bad.lua", "local function f(a)\n  return a +\nend\n", "luac", "bad.lua:3:"},
		// In a Rojo project .lua files are Luau: type annotations are fine…
		{"rbx/typed.lua", "local x: number = 1\nx += 1\nreturn x\n", "luau-compile", ""},
		// …elsewhere they are plain Lua, where the same file is an error.
		{"plain/typed.lua", "local x: number = 1\nx += 1\nreturn x\n", "luac", "typed.lua:1:"},
	}
	_ = os.MkdirAll(filepath.Join(dir, "rbx"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "rbx", "default.project.json"), []byte(`{"name":"game","tree":{}}`), 0o644)
	for _, c := range cases {
		if c.checker != "" {
			if _, err := exec.LookPath(c.checker); err != nil {
				t.Logf("skipping %s: %s not installed", c.path, c.checker)
				continue
			}
		}
		out, err := run(t, write, map[string]any{"path": c.path, "content": c.content})
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		note := strings.Contains(out, "Syntax check")
		switch {
		case c.want == "" && note:
			t.Errorf("%s: clean file reported: %q", c.path, out)
		case c.want != "" && (!note || !strings.Contains(out, c.want)):
			t.Errorf("%s: want a note mentioning %q, got %q", c.path, c.want, out)
		case strings.Contains(out, dir):
			t.Errorf("%s: note shows the absolute path: %q", c.path, out)
		}
	}
}

type fakeChecker struct {
	ok    bool
	texts []string // before|after
}

func (f *fakeChecker) Check(_ context.Context, path, before, after string, rel func(string) string) (string, bool) {
	f.texts = append(f.texts, before+"|"+after)
	if !f.ok {
		return "", false
	}
	return "\n\nfake reports errors after this edit:\n" + rel(path) + ":1:1: no", true
}

// With a language server, its note replaces the syntax check and gets the
// content written; without an answer the syntax check runs.
func TestEditsGoToTheLanguageServer(t *testing.T) {
	if _, err := exec.LookPath("gofmt"); err != nil {
		t.Skip("gofmt not installed")
	}
	dir := t.TempDir()
	lsp := &fakeChecker{ok: true}
	env := &Env{Cwd: dir, LSP: lsp}
	read, edit, write := &readTool{env}, &editTool{env}, &writeTool{env}

	out, err := run(t, write, map[string]any{"path": "a.go", "content": "package x\n\nfunc F() {\n"})
	if err != nil || !strings.HasSuffix(out, "\n\nfake reports errors after this edit:\na.go:1:1: no") || strings.Contains(out, "Syntax check") {
		t.Fatalf("write: %q %v", out, err)
	}
	run(t, read, map[string]any{"path": "a.go"})
	if _, err := run(t, edit, map[string]any{"path": "a.go", "old_string": "F() {", "new_string": "G() {"}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, write, map[string]any{"path": "a.go", "content": "package y\n"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"|package x\n\nfunc F() {\n", "package x\n\nfunc F() {\n|package x\n\nfunc G() {\n", "package x\n\nfunc G() {\n|package y\n"}
	if strings.Join(lsp.texts, "\n--\n") != strings.Join(want, "\n--\n") {
		t.Fatalf("server got %q", lsp.texts)
	}
	run(t, write, map[string]any{"path": "a.go", "content": "package x\n\nfunc G() {\n"})
	// A subagent's worktree Env uses the same servers.
	if env.At(dir).LSP != env.LSP {
		t.Fatal("At dropped the language servers")
	}

	lsp.ok = false
	out, _ = run(t, edit, map[string]any{"path": "a.go", "old_string": "G() {", "new_string": "H() {"})
	if !strings.Contains(out, "Syntax check (gofmt)") {
		t.Fatalf("no syntax check when the server did not answer: %q", out)
	}
}
