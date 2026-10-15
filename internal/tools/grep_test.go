package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// grepFixture: many.ts has 150 matches, 30 small files have 2 each, and one
// file name holds a colon.
func grepFixture(t *testing.T) *Env {
	t.Helper()
	dir := t.TempDir()
	write := func(name, s string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var b strings.Builder
	for i := range 150 {
		fmt.Fprintf(&b, "const a%d = config.value%d\n", i, i)
	}
	write("src/many.ts", b.String())
	for i := range 30 {
		write(fmt.Sprintf("src/f%02d.ts", i), "x\nconfig.one\ny\nconfig.two "+strings.Repeat("z", 400)+"\n")
	}
	write("src/we:ird.ts", "config.colon\n")
	return &Env{Cwd: dir}
}

// eachEngine runs f with ripgrep (when installed) and with the Go fallback.
func eachEngine(t *testing.T, f func(t *testing.T, run func(in map[string]any) string)) {
	engines := map[string]bool{"go": false}
	if rgPath() != "" {
		engines["rg"] = true
	}
	for name, useRg := range engines {
		t.Run(name, func(t *testing.T) {
			env := grepFixture(t)
			g := &grepTool{env}
			f(t, func(in map[string]any) string {
				raw, _ := json.Marshal(in)
				var gi grepInput
				if err := json.Unmarshal(raw, &gi); err != nil {
					t.Fatal(err)
				}
				root := env.abs(gi.Path)
				st, err := os.Stat(root)
				if err != nil {
					t.Fatal(err)
				}
				var out string
				if useRg {
					out, err = g.runRg(context.Background(), rgPath(), &gi, root, st.IsDir())
				} else {
					out, err = g.runGo(context.Background(), &gi, root, st.IsDir())
				}
				if err != nil {
					t.Fatal(err)
				}
				return out
			})
		})
	}
}

func TestGrepPagesAndSummarises(t *testing.T) {
	eachEngine(t, func(t *testing.T, run func(map[string]any) string) {
		out := run(map[string]any{"pattern": `config\.`, "path": "src"})
		lines := strings.Split(out, "\n")
		if !strings.Contains(out, "showing lines 1-100 of 211.") {
			t.Fatalf("no page note:\n%s", tail(out))
		}
		// The summary lists the busiest file first and counts every file.
		if !strings.Contains(out, "Matches per file (32 files, most first):\n  src/many.ts: 150") {
			t.Fatalf("summary:\n%s", tail(out))
		}
		if !strings.Contains(out, "... 17 more files") || !strings.Contains(out, "offset 100") {
			t.Fatalf("hints:\n%s", tail(out))
		}
		if len(lines) > 100+grepTopFiles+4 {
			t.Fatalf("%d lines", len(lines))
		}
		// Long lines are clipped.
		for _, l := range lines {
			if len(l) > grepMaxColumns+80 {
				t.Fatalf("line not clipped (%d): %.80s", len(l), l)
			}
		}
		// The next page starts where the first ended; the last page has no note.
		page2 := run(map[string]any{"pattern": `config\.`, "path": "src", "offset": 100, "limit": 200})
		all := strings.Split(run(map[string]any{"pattern": `config\.`, "path": "src", "limit": 500}), "\n")
		if !strings.HasPrefix(page2, all[100]+"\n") || strings.Contains(page2, "showing lines") {
			t.Fatalf("last page starts %.80q, want %.80q", page2, all[100])
		}
		if strings.Count(page2, "\n")+1 != 111 {
			t.Fatalf("last page has %d lines", strings.Count(page2, "\n")+1)
		}
		if got := run(map[string]any{"pattern": `config\.`, "path": "src", "offset": 500}); !strings.Contains(got, "No lines after offset 500 (the result has 211 lines)") {
			t.Fatalf("past the end: %q", got)
		}
	})
}

func TestGrepPathWithColon(t *testing.T) {
	eachEngine(t, func(t *testing.T, run func(map[string]any) string) {
		out := run(map[string]any{"pattern": "colon", "path": "src"})
		if out != "src/we:ird.ts:1:config.colon" {
			t.Fatalf("%q", out)
		}
		count := run(map[string]any{"pattern": "colon", "path": "src", "output": "count"})
		if count != "src/we:ird.ts:1" {
			t.Fatalf("count %q", count)
		}
	})
}

func TestGrepContextLines(t *testing.T) {
	eachEngine(t, func(t *testing.T, run func(map[string]any) string) {
		out := run(map[string]any{"pattern": "config.one", "path": "src/f00.ts", "context": 1})
		want := "src/f00.ts-1-x\nsrc/f00.ts:2:config.one\nsrc/f00.ts-3-y"
		if out != want {
			t.Fatalf("got\n%s\nwant\n%s", out, want)
		}
	})
}

func tail(s string) string {
	if len(s) > 1200 {
		return "…" + s[len(s)-1200:]
	}
	return s
}
