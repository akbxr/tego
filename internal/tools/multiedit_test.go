package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditSeveralChangesAtOnce(t *testing.T) {
	dir := t.TempDir()
	env := &Env{Cwd: dir}
	read, edit := &readTool{env}, &editTool{env}
	p := filepath.Join(dir, "a.txt")
	orig := "alpha\nbeta\ngamma\ndelta\n"
	_ = os.WriteFile(p, []byte(orig), 0o644)
	if _, err := run(t, read, map[string]any{"path": "a.txt"}); err != nil {
		t.Fatal(err)
	}

	// The second change fails: nothing is written.
	_, err := run(t, edit, map[string]any{"path": "a.txt", "edits": []map[string]any{
		{"old_string": "alpha", "new_string": "ALPHA"},
		{"old_string": "missing", "new_string": "x"},
		{"old_string": "delta", "new_string": "DELTA"},
	}})
	if err == nil || !strings.Contains(err.Error(), "change 2 of 3") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != orig {
		t.Fatalf("file changed by a failed multi-edit: %q", b)
	}

	// In order, each to the result of the previous one.
	out, err := run(t, edit, map[string]any{"path": "a.txt", "edits": []map[string]any{
		{"old_string": "alpha", "new_string": "ALPHA"},
		{"old_string": "ALPHA\nbeta", "new_string": "ALPHA\nBETA"},
		{"old_string": "delta", "new_string": "DELTA"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "ALPHA\nBETA\ngamma\nDELTA\n" {
		t.Fatalf("content %q", b)
	}
	if !strings.Contains(out, "3 replacements in 3 changes") || strings.Count(out, "... next change") != 2 || !strings.Contains(out, "+4\tDELTA") {
		t.Fatalf("output:\n%s", out)
	}

	// The single form still works, and the two forms can't be mixed.
	if _, err := run(t, edit, map[string]any{"path": "a.txt", "old_string": "gamma", "new_string": "GAMMA"}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, edit, map[string]any{"path": "a.txt", "old_string": "x", "new_string": "y",
		"edits": []map[string]any{{"old_string": "a", "new_string": "b"}}}); err == nil {
		t.Fatal("both forms accepted")
	}
}

func TestWritesSource(t *testing.T) {
	for cmd, want := range map[string]bool{
		"sed -i '' 's/a/b/' main.go":                                        true,
		"sed -i.bak 's/a/b/' x.py":                                          true,
		"perl -pi -e 's/a/b/' lib.rs":                                       true,
		"cat > internal/agent/x.go <<'EOF'\npackage x\nEOF":                 true,
		"echo '{}' >> config.json":                                          true,
		"python3 - <<'EOF'\nopen('cmd/main.go','w').write(s)\nEOF":          true,
		"python3 -c \"import pathlib; pathlib.Path('a.py').write_text(s)\"": true,
		"go test ./... > /tmp/out.txt":                                      false,
		"cat > /tmp/scratch.go <<'EOF'\nEOF":                                false,
		"go build ./... 2>&1 | tail -3":                                     false,
		"echo hi > /dev/null":                                               false,
		"grep -n 'sed -i' README.md":                                        false,
		"ls -la > files.txt":                                                false,
		"python3 - <<'EOF'\nprint(open('a.go').read())\nEOF":                false,
		// Targeted edits that the edit tool does better still count.
		"cd internal && sed -i '' -e 's/a/b/' agent/x.go && go test ./...": true,
		"sed -i 's/a/b/' a.go b.go":                                        true,
		"perl -pe 's/a/b/' -i lib.rs":                                      true,
		// Bulk mechanical changes are the shell's job.
		"sed -i '' 's/Old/New/g' $(grep -rl Old --include=*.go .)": false,
		"sed -i 's/Old/New/g' internal/*/*.go":                     false,
		"sed -i 's/Old/New/g' a.go b.go c.go":                      false,
		"find . -name '*.go' -exec sed -i '' 's/a/b/' {} +":        false,
		"grep -rl foo . | xargs sed -i 's/foo/bar/'":               false,
		"python3 - <<'EOF'\nimport glob\nfor f in glob.glob('**/*.py', recursive=True):\n    open(f,'w').write(fix(open(f).read()))\nEOF": false,
		"gofmt -w ./internal":                    false,
		"gopls rename -w internal/x.go:#120 New": false,
		"go generate ./...":                      false,
		"sed 's/a/b/' main.go > /tmp/out.go":     false, // not in place, output to a temp file
		// Scratch files.
		"cat > internal/tools/zz_probe_test.go <<'EOF'\npackage tools\nEOF\ngo test ./internal/tools": false,
		"cat > internal/x_test.go <<'EOF'\nEOF\ngo test ./internal; rm internal/x_test.go":            false,
		"python3 -c \"open('gen_tmp.go','w').write(s)\"":                                              false,
	} {
		if got := writesSource(cmd); got != want {
			t.Errorf("writesSource(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestShellWriteNoteIsRationed(t *testing.T) {
	env := &Env{Cwd: t.TempDir()}
	n := 0
	for range 4 {
		if env.noteShellWrite("sed -i 's/a/b/' x.go") != "" {
			n++
		}
	}
	if n != shellWriteNotes || env.noteShellWrite("go test ./...") != "" {
		t.Fatalf("notes given %d", n)
	}
}
