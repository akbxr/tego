package tools

import (
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
