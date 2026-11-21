package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// writeFiles writes name → content files under dir.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, s := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

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

// grepEngines lists the engines to test: the Go fallback, and ripgrep when
// it is installed.
func grepEngines() []string {
	if rgPath() != "" {
		return []string{"go", "rg"}
	}
	return []string{"go"}
}

// grepRun runs grep on env with engine "rg" or "go".
func grepRun(t *testing.T, env *Env, engine string, in map[string]any) string {
	t.Helper()
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
	g := &grepTool{env}
	var out string
	if engine == "rg" {
		out, err = g.runRg(context.Background(), rgPath(), &gi, root, st.IsDir())
	} else {
		out, err = g.runGo(context.Background(), &gi, root, st.IsDir())
	}
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// eachEngine runs f with ripgrep (when installed) and with the Go fallback,
// each on a fresh fixture.
func eachEngine(t *testing.T, fixture func(*testing.T) *Env, f func(t *testing.T, engine string, run func(in map[string]any) string)) {
	for _, engine := range grepEngines() {
		t.Run(engine, func(t *testing.T) {
			env := fixture(t)
			f(t, engine, func(in map[string]any) string { return grepRun(t, env, engine, in) })
		})
	}
}

func TestGrepPagesAndSummarises(t *testing.T) {
	eachEngine(t, grepFixture, func(t *testing.T, _ string, run func(map[string]any) string) {
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
	eachEngine(t, grepFixture, func(t *testing.T, _ string, run func(map[string]any) string) {
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
	eachEngine(t, grepFixture, func(t *testing.T, _ string, run func(map[string]any) string) {
		out := run(map[string]any{"pattern": "config.one", "path": "src/f00.ts", "context": 1})
		want := "src/f00.ts-1-x\nsrc/f00.ts:2:config.one\nsrc/f00.ts-3-y"
		if out != want {
			t.Fatalf("got\n%s\nwant\n%s", out, want)
		}
	})
}

// hiddenFixture: hidden files and folders beside a .git folder, a .git link
// file of a submodule, and a file that .gitignore leaves out. Each file but
// .gitignore holds "needle".
func hiddenFixture(t *testing.T) *Env {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		".git/HEAD":                 "ref: refs/heads/main needle\n",
		".git/hooks/pre-commit.yml": "needle\n",
		".github/workflows/ci.yml":  "run: go test # needle\n",
		".env.example":              "TOKEN=needle\n",
		".gitignore":                "ignored.txt\n",
		"ignored.txt":               "needle\n",
		"src/main.go":               "// needle\n",
		"sub/.git":                  "gitdir: ../.git/modules/sub needle\n",
	})
	return &Env{Cwd: dir}
}

func TestGrepHiddenFiles(t *testing.T) {
	eachEngine(t, hiddenFixture, func(t *testing.T, engine string, run func(map[string]any) string) {
		want := ".env.example\n.github/workflows/ci.yml\nsrc/main.go"
		if engine == "go" { // the fallback does not read .gitignore
			want = ".env.example\n.github/workflows/ci.yml\nignored.txt\nsrc/main.go"
		}
		if got := run(map[string]any{"pattern": "needle", "output": "files"}); got != want {
			t.Fatalf("files:\n%s\nwant\n%s", got, want)
		}
		if got := run(map[string]any{"pattern": "TOKEN"}); got != ".env.example:1:TOKEN=needle" {
			t.Fatalf("content: %q", got)
		}
		// A glob reaches into hidden folders, but not into .git.
		if got := run(map[string]any{"pattern": "needle", "glob": "*.yml", "output": "files"}); got != ".github/workflows/ci.yml" {
			t.Fatalf("glob: %q", got)
		}
		// .git is still searched when asked for by path.
		if got := run(map[string]any{"pattern": "needle", "path": ".git", "output": "files"}); got != ".git/HEAD\n.git/hooks/pre-commit.yml" {
			t.Fatalf("path .git: %q", got)
		}
	})
}

// orderNames are the files of orderFixture in walk order: name by name, so
// a/b/c.ts comes before a/b-c.ts and a.ts, unlike in plain string order.
func orderNames() []string {
	names := []string{".hidden.ts", "Z.ts", "a/b/c.ts", "a/b-c.ts", "a/b.ts", "a-b.ts", "a.ts", "b.ts"}
	for i := range 9 {
		names = append(names, fmt.Sprintf("d/%02d/f.ts", i))
	}
	for i := range 9 {
		names = append(names, fmt.Sprintf("e%02d.ts", i))
	}
	return append(names, "z/y/x.ts")
}

// orderFixture: enough files that rg, searching in parallel, finds them in
// a different order from run to run, each with 3-6 hits placed so that
// context hunks both part (a "--" line) and merge.
func orderFixture(t *testing.T) *Env {
	dir := t.TempDir()
	files := map[string]string{}
	for i, name := range orderNames() {
		var b strings.Builder
		b.WriteString("top\nhit one\n" + strings.Repeat("pad\n", 6) + "hit two\nhit three\npad\n")
		for j := range i % 4 {
			fmt.Fprintf(&b, "pad\nhit extra %d\n", j)
		}
		b.WriteString("end\n")
		files[name] = b.String()
	}
	files[".git/HEAD"] = "ref: refs/heads/main\n" // a repository: hidden files are searched
	writeFiles(t, dir, files)
	return &Env{Cwd: dir}
}

// fileOfLine returns the index in names of the file a content line is from:
// the name followed by ":" or "-" and a line number.
func fileOfLine(names []string, line string) int {
	for i, n := range names {
		if rest, ok := strings.CutPrefix(line, n); ok && len(rest) > 1 && (rest[0] == ':' || rest[0] == '-') && rest[1] >= '0' && rest[1] <= '9' {
			return i
		}
	}
	return -1
}

func TestGrepSortedAndPaged(t *testing.T) {
	env := orderFixture(t)
	names := orderNames()
	with := func(in map[string]any, kv ...any) map[string]any {
		out := maps.Clone(in)
		for i := 0; i < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	// Exact match counts per file, from count output.
	counts := map[string]int{}
	for _, l := range strings.Split(grepRun(t, env, "go", map[string]any{"pattern": "hit", "output": "count", "limit": 500}), "\n") {
		i := strings.LastIndexByte(l, ':')
		n, err := strconv.Atoi(l[i+1:])
		if err != nil {
			t.Fatalf("count line %q", l)
		}
		counts[l[:i]] = n
	}
	if len(counts) != len(names) {
		t.Fatalf("counts for %d files, want %d", len(counts), len(names))
	}
	// The summary of a cut result: most matches first, then in path order.
	// Files output knows no counts: each file stands for one match.
	summary := func(output string) string {
		count := func(n string) int {
			if output == "files" {
				return 1
			}
			return counts[n]
		}
		busiest := slices.Clone(names)
		sort.SliceStable(busiest, func(i, j int) bool { return count(busiest[i]) > count(busiest[j]) })
		s := fmt.Sprintf(" Matches per file (%d files, most first):", len(names))
		for _, n := range busiest[:grepTopFiles] {
			s += fmt.Sprintf("\n  %s: %d", n, count(n))
		}
		return s + fmt.Sprintf("\n  ... %d more files\n", len(names)-grepTopFiles)
	}

	for _, in := range []map[string]any{
		{"pattern": "hit"},
		{"pattern": "hit", "context": 1},
		{"pattern": "hit", "context": 2},
		{"pattern": "hit", "output": "files"},
		{"pattern": "hit", "output": "count"},
	} {
		ctxLines, _ := in["context"].(int)
		output, _ := in["output"].(string)
		t.Run(fmt.Sprintf("%s-context%d", cmpOr(output, "content"), ctxLines), func(t *testing.T) {
			var first string
			for _, engine := range grepEngines() {
				full := grepRun(t, env, engine, with(in, "limit", 500))
				if strings.Contains(full, "showing lines") {
					t.Fatalf("%s: fixture too large for one page", engine)
				}
				// Both engines agree, and rg, searching in parallel, gives the
				// same result each run.
				if first == "" {
					first = full
				} else if full != first {
					t.Fatalf("engines differ:\n%s\n--- vs ---\n%s", first, full)
				}
				for range 3 {
					if again := grepRun(t, env, engine, with(in, "limit", 500)); again != full {
						t.Fatalf("%s: result changed between runs:\n%s\n--- vs ---\n%s", engine, full, again)
					}
				}
				lines := strings.Split(full, "\n")
				switch output {
				case "files":
					if !slices.Equal(lines, names) {
						t.Fatalf("%s: files\n%s", engine, full)
					}
				case "count":
					for i, n := range names {
						if lines[i] != n+":"+strconv.Itoa(counts[n]) {
							t.Fatalf("%s: count line %d is %q, want %s:%d", engine, i, lines[i], n, counts[n])
						}
					}
				default:
					// Files in walk order, each file's lines together, "--" only
					// with context, and then between files and between hunks.
					at, matches, parts := 0, map[string]int{}, 0
					for i, l := range lines {
						if l == "--" {
							if ctxLines == 0 || i == 0 || i == len(lines)-1 || lines[i-1] == "--" {
								t.Fatalf("%s: stray -- at line %d:\n%s", engine, i, full)
							}
							if fileOfLine(names, lines[i+1]) == at {
								parts++
							}
							continue
						}
						f := fileOfLine(names, l)
						switch {
						case f == at+1 && (ctxLines == 0 || lines[i-1] == "--"):
							at = f
						case f != at:
							t.Fatalf("%s: line %d %q is out of order (file %d after %d)", engine, i, l, f, at)
						}
						if strings.HasPrefix(l[len(names[f]):], ":") {
							matches[names[f]]++
						}
					}
					if at != len(names)-1 || !maps.Equal(matches, counts) {
						t.Fatalf("%s: ended at file %d of %d, matches %v, want %v", engine, at, len(names), matches, counts)
					}
					if ctxLines > 0 && parts != len(names) {
						t.Fatalf("%s: %d hunks parted within files, want one per file:\n%s", engine, parts, full)
					}
				}
				// Pages of any size put together give the whole result; every
				// page but the last says where it is and sums up the files.
				for _, limit := range []int{7, 40} {
					var got []string
					for off := 0; off < len(lines); off += limit {
						page := strings.Split(grepRun(t, env, engine, with(in, "offset", off, "limit", limit)), "\n")
						n := min(limit, len(lines)-off)
						if off+n < len(lines) {
							note := fmt.Sprintf("... showing lines %d-%d of %d.", off+1, off+n, len(lines)) + summary(output)
							if tail := strings.Join(page[n:], "\n"); !strings.HasPrefix(tail, note) {
								t.Fatalf("%s: page at %d ends\n%s\nwant\n%s", engine, off, tail, note)
							}
						} else if len(page) != n {
							t.Fatalf("%s: last page at %d has %d lines, want %d", engine, off, len(page), n)
						}
						got = append(got, page[:n]...)
					}
					if strings.Join(got, "\n") != full {
						t.Fatalf("%s: pages of %d differ from the whole:\n%s", engine, limit, strings.Join(got, "\n"))
					}
				}
			}
		})
	}
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// segLess is the reference order for pathLess: by slash-separated name.
func segLess(a, b string) bool {
	return slices.Compare(strings.Split(a, "/"), strings.Split(b, "/")) < 0
}

func TestPathLess(t *testing.T) {
	parts := []string{"a", "a-b", "a.b", "ab", "B", ".x", "a/b", "a/b/c", "_"}
	for _, a := range parts {
		for _, b := range parts {
			for _, sfx := range []string{"", ".go", "/z"} {
				if got, want := pathLess(a+sfx, b), segLess(a+sfx, b); got != want {
					t.Errorf("pathLess(%q, %q) = %v, want %v", a+sfx, b, got, want)
				}
			}
		}
	}
}

// TestGrepPageAnyOrder feeds grepPage files in shuffled order, enough lines
// that it drops lines past the page several times on the way (also from a
// file that is still arriving), and checks each page against the sorted
// result.
func TestGrepPageAnyOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	segs := []string{"a", "a-b", "a.b", "B", ".x", "z"}
	type file struct {
		path  string
		lines []string
		match []int
	}
	seen := map[string]bool{}
	var files []file
	for len(files) < 2500 {
		var p []string
		for range 1 + rng.Intn(3) {
			p = append(p, segs[rng.Intn(len(segs))])
		}
		path := strings.Join(p, "/") + fmt.Sprintf("%d.go", rng.Intn(50))
		if seen[path] {
			continue
		}
		seen[path] = true
		n := 1 + rng.Intn(8)
		if len(files) == 100 {
			n = 9000 // one file larger than a compaction
		}
		f := file{path: path}
		for j := range n {
			f.lines = append(f.lines, fmt.Sprintf("%s:%d:x", path, j+1))
			f.match = append(f.match, rng.Intn(2))
		}
		files = append(files, f)
	}
	sorted := slices.Clone(files)
	sort.Slice(sorted, func(i, j int) bool { return segLess(sorted[i].path, sorted[j].path) })
	for _, fileSep := range []bool{false, true} {
		var all []string
		for i, f := range sorted {
			if fileSep && i > 0 {
				all = append(all, "--")
			}
			all = append(all, f.lines...)
		}
		pages := [][2]int{{0, 100}, {0, 500}, {1234, 37}, {4000, 500}, {8000, 500}, {len(all) - 10, 100}, {len(all), 10}}
		if fileSep { // pages that end and start on the "--" before a file
			sep := 3000 + slices.Index(all[3000:], "--")
			pages = append(pages, [2]int{sep - 4, 5}, [2]int{sep, 5})
		}
		for _, pg := range pages {
			kept := 0 // lines held at the end, in path order
			feed := func(order []file, inOrder bool) string {
				in := &grepInput{Offset: pg[0], Limit: pg[1]}
				if fileSep {
					in.Context = 1
				}
				p := newGrepPage(in)
				p.sorted = inOrder
				for _, f := range order {
					gf := p.file(f.path)
					for j, l := range f.lines {
						if !p.keeps(gf) {
							l = ""
						}
						p.add(gf, l, f.match[j])
					}
				}
				kept = p.keptLines
				return p.String()
			}
			want := feed(sorted, false)
			// Told the files come in order, it gives the same page and holds
			// no more than the page.
			if got := feed(sorted, true); got != want || kept > pg[1] {
				t.Fatalf("sep %v page %v: in order, %d lines held, gives\n%s\nwant\n%s", fileSep, pg, kept, tail(got), tail(want))
			}
			end := min(pg[0]+pg[1], len(all))
			if pg[0] >= len(all) {
				if w := fmt.Sprintf("No lines after offset %d (the result has %d lines)", pg[0], len(all)); want != w {
					t.Fatalf("sep %v page %v: %q, want %q", fileSep, pg, want, w)
				}
			} else if page := strings.Join(all[pg[0]:end], "\n"); !strings.HasPrefix(want, page) ||
				(end < len(all) && !strings.HasPrefix(want[len(page):], fmt.Sprintf("\n... showing lines %d-%d of %d.", pg[0]+1, end, len(all)))) ||
				(end == len(all) && want != page) {
				t.Fatalf("sep %v page %v:\n%s", fileSep, pg, tail(want))
			}
			for range 3 {
				shuffled := slices.Clone(files)
				rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
				if got := feed(shuffled, false); got != want {
					t.Fatalf("sep %v page %v: shuffled order gives\n%s\nwant\n%s", fileSep, pg, tail(got), tail(want))
				}
			}
		}
	}
}

func tail(s string) string {
	if len(s) > 1200 {
		return "…" + s[len(s)-1200:]
	}
	return s
}
