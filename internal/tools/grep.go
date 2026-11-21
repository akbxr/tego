package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/akbxr/tego/internal/llm"
)

// Every line of a result is resent with each later request, so the defaults
// are tight: on a 3.5M-line repository a broad pattern used to return ~50k
// characters per call (300 lines × 300 columns), five times what pi sends
// (20 matches) for the same searches. More pages come with offset.
const (
	grepMaxLines   = 100 // default page size (limit)
	grepMaxLimit   = 500
	grepMaxColumns = 200
	grepMaxFile    = 32 << 20
	grepTopFiles   = 15 // files listed when a result is cut
	// From this offset on rg sorts by path (one thread): out of order, every
	// line before the page has to be kept, and memory grows with offset.
	grepSortFrom = 10_000
)

type grepTool struct{ env *Env }

func (t *grepTool) ReadOnly() bool { return true }

func (t *grepTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "grep",
		Description: "Search file contents by regex (ripgrep syntax). Respects .gitignore. Output: path:line:text.",
		Schema: schema(`{"type":"object","properties":{` +
			`"pattern":{"type":"string","description":"Regular expression"},` +
			`"path":{"type":"string","description":"File or directory (default cwd)"},` +
			`"glob":{"type":"string","description":"Filter files, e.g. *.go or src/**/*.ts"},` +
			`"ignore_case":{"type":"boolean"},` +
			`"context":{"type":"integer","description":"Lines of context around matches"},` +
			`"output":{"type":"string","enum":["content","files","count"],"description":"Default content"},` +
			`"limit":{"type":"integer","description":"Lines to return (default 100, max 500)"},` +
			`"offset":{"type":"integer","description":"Lines to skip, for the next page"}},` +
			`"required":["pattern"]}`),
	}
}

type grepInput struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	IgnoreCase bool   `json:"ignore_case"`
	Context    int    `json:"context"`
	Output     string `json:"output"`
	Limit      int    `json:"limit"`
	Offset     int    `json:"offset"`
}

// grepPage keeps one page (offset, limit) of grep output in path order and
// counts the rest. Output comes one file at a time, each file's lines
// together and in order, but the files come in any order: rg searches them
// in parallel. Files that sort before a line can only push it further down,
// so once a line's place is at or past the end of the page it is dropped and
// only counted. That bounds memory to a few pages of lines, however large
// the result. When the output comes in path order (sorted), each line's place
// is known as it comes, and only the page's lines are kept.
type grepPage struct {
	offset, limit int
	fileSep       bool // a "--" line goes between files (output with context)
	files         map[string]*grepFile
	kept          []*grepFile // files whose lines may still reach the page
	keptLines     int
	bound         string // once bounded, files sorting from bound on are past the page
	bounded       bool
	sorted        bool // files come in path order
	placed        int  // lines so far, separators between files too (sorted)
}

// grepFile is the output of one file.
type grepFile struct {
	path    string
	n       int      // output lines
	matches int      // matching lines (in count output, the count)
	skip    int      // first lines placed before the page and not kept (sorted)
	lines   []string // the next output lines, as many as may reach the page
	room    int      // how many of those lines may reach the page
}

func newGrepPage(in *grepInput) *grepPage {
	limit := in.Limit
	if limit <= 0 {
		limit = grepMaxLines
	}
	return &grepPage{
		offset:  min(max(in.Offset, 0), math.MaxInt32),
		limit:   min(limit, grepMaxLimit),
		fileSep: in.Context > 0 && (in.Output == "" || in.Output == "content"),
		files:   map[string]*grepFile{},
	}
}

// file returns the output of path, starting it on first use.
func (p *grepPage) file(path string) *grepFile {
	if f := p.files[path]; f != nil {
		return f
	}
	f := &grepFile{path: path, room: math.MaxInt}
	if p.sorted && p.fileSep && len(p.files) > 0 {
		p.placed++ // the "--" before it
	}
	if p.bounded && !pathLess(path, p.bound) {
		f.room = 0
	} else {
		p.kept = append(p.kept, f)
	}
	p.files[path] = f
	return f
}

// keeps reports whether the next line of f may reach the page; when it
// does not, the caller need not build the line.
func (p *grepPage) keeps(f *grepFile) bool {
	if p.sorted {
		return p.placed >= p.offset && p.placed < p.offset+p.limit
	}
	return len(f.lines) < f.room
}

// add records the next output line of f, which stands for matches matching
// lines: 1 for a match, 0 for a context line or a separator. s is not used
// when p.keeps(f) is false.
func (p *grepPage) add(f *grepFile, s string, matches int) {
	keep := p.keeps(f)
	f.n++
	f.matches += matches
	if p.sorted {
		if p.placed < p.offset {
			f.skip++
		}
		p.placed++
	}
	if !keep {
		return
	}
	f.lines = append(f.lines, strings.ToValidUTF8(s, "\uFFFD"))
	p.keptLines++
	if p.keptLines > 2*(p.offset+p.limit)+4096 {
		p.compact()
	}
}

// compact sorts the kept files by path and drops the lines placed at or past
// the end of the page, and the files placed wholly past it.
func (p *grepPage) compact() {
	sort.Slice(p.kept, func(i, j int) bool { return pathLess(p.kept[i].path, p.kept[j].path) })
	end := p.offset + p.limit
	at, kept := 0, 0 // place of the next line, lines kept
	for i, f := range p.kept {
		if p.fileSep && i > 0 {
			at++
		}
		if at >= end {
			p.bound, p.bounded = f.path, true
			for _, g := range p.kept[i:] {
				g.lines, g.room = nil, 0
			}
			clear(p.kept[i:])
			p.kept = p.kept[:i]
			break
		}
		f.room = min(f.room, max(end-at-f.skip, 0))
		if len(f.lines) > f.room {
			clear(f.lines[f.room:])
			f.lines = f.lines[:f.room]
		}
		kept += len(f.lines)
		at += f.n
	}
	p.keptLines = kept
}

func (p *grepPage) empty() bool { return len(p.files) == 0 }

func (p *grepPage) String() string {
	if p.empty() {
		return "No matches found"
	}
	p.compact()
	total := 0
	for _, f := range p.files {
		total += f.n
	}
	if p.fileSep {
		total += len(p.files) - 1
	}
	var lines []string
	at := 0
	put := func(s string) {
		if at >= p.offset && len(lines) < p.limit {
			lines = append(lines, s)
		}
		at++
	}
	for i, f := range p.kept {
		if p.fileSep && i > 0 {
			put("--")
		}
		at += f.skip
		for _, s := range f.lines {
			put(s)
		}
		at += f.n - f.skip - len(f.lines)
	}
	if p.fileSep && len(p.kept) < len(p.files) {
		put("--") // before the first file past the page, which may end on it
	}
	if len(lines) == 0 {
		return fmt.Sprintf("No lines after offset %d (the result has %d lines)", p.offset, total)
	}
	s := strings.Join(lines, "\n")
	shown := p.offset + len(lines)
	if shown >= total {
		return s
	}
	s += fmt.Sprintf("\n... showing lines %d-%d of %d.", p.offset+1, shown, total)
	var files []*grepFile
	for _, f := range p.files {
		if f.matches > 0 {
			files = append(files, f)
		}
	}
	if n := len(files); n > 1 {
		sort.Slice(files, func(i, j int) bool {
			if files[i].matches != files[j].matches {
				return files[i].matches > files[j].matches
			}
			return pathLess(files[i].path, files[j].path)
		})
		s += fmt.Sprintf(" Matches per file (%d files, most first):", n)
		for _, f := range files[:min(n, grepTopFiles)] {
			s += fmt.Sprintf("\n  %s: %d", f.path, f.matches)
		}
		if n > grepTopFiles {
			s += fmt.Sprintf("\n  ... %d more files", n-grepTopFiles)
		}
		s += "\n"
	}
	s += fmt.Sprintf(" Narrow it with path, glob or a more specific pattern, use output \"files\" or \"count\", or get the next lines with offset %d.", shown)
	return s
}

// pathLess orders paths the way a sorted directory walk visits them
// (filepath.WalkDir, rg --sort path): name by name, so a/b comes before a.txt.
func pathLess(a, b string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		x, y := a[i], b[i]
		if x == '/' || x == filepath.Separator {
			x = 0
		}
		if y == '/' || y == filepath.Separator {
			y = 0
		}
		if x != y {
			return x < y
		}
	}
	return len(a) < len(b)
}

func (t *grepTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in grepInput
	if err := decode(input, &in); err != nil {
		return "", err
	}
	if in.Pattern == "" {
		return "", errors.New("pattern is required")
	}
	switch in.Output {
	case "", "content", "files", "count":
	default:
		return "", fmt.Errorf("output must be content, files or count")
	}
	if in.Context < 0 {
		in.Context = 0
	}
	root := t.env.abs(in.Path)
	st, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if rg := rgPath(); rg != "" {
		return t.runRg(ctx, rg, &in, root, st.IsDir())
	}
	return t.runGo(ctx, &in, root, st.IsDir())
}

func (t *grepTool) runRg(ctx context.Context, rg string, in *grepInput, root string, isDir bool) (string, error) {
	// No --sort path: it makes rg search on one thread, 4-8x slower on a
	// large tree with 8 cores. grepPage puts the files in path order instead,
	// but from a far offset on it would keep too many lines for that.
	args := []string{"--no-heading", "--color", "never", "--with-filename",
		"--max-columns", strconv.Itoa(grepMaxColumns), "--max-columns-preview"}
	sorted := in.Offset >= grepSortFrom
	if sorted {
		args = append(args, "--sort", "path")
	}
	switch in.Output {
	case "files":
		args = append(args, "--files-with-matches")
	case "count":
		args = append(args, "--count", "--null")
	default:
		args = append(args, "--line-number", "--null") // NUL after the path: paths may hold ':'
		if in.Context > 0 {
			args = append(args, "--context", strconv.Itoa(in.Context))
		}
	}
	if in.IgnoreCase {
		args = append(args, "--ignore-case")
	}
	target := t.env.rel(root)
	// For directories run rg inside the search root so globs are relative to
	// it, then prefix output paths to make them relative to Cwd.
	cmd := exec.CommandContext(ctx, rg)
	prefix := ""
	if isDir {
		if in.Glob != "" {
			args = append(args, "--glob", in.Glob)
		}
		args = append(append(args, hiddenArgs(root)...), "--regexp", in.Pattern)
		cmd.Dir = root
		if target != "." {
			prefix = target + string(filepath.Separator)
		}
	} else {
		args = append(args, "--regexp", in.Pattern, "--", target)
		cmd.Dir = t.env.Cwd
	}
	cmd.Args = append(cmd.Args, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	c := newGrepPage(in)
	c.sorted = sorted
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	// Each file's lines come together, so the file of the previous line
	// saves a lookup. A "--" between two lines of one file parts two context
	// hunks; one before a file's first line parts files, and grepPage puts
	// those back once the files are in order.
	var last *grepFile
	sep := false
	fileOf := func(path []byte) *grepFile {
		if last != nil && len(last.path) == len(prefix)+len(path) && last.path[len(prefix):] == string(path) {
			return last
		}
		return c.file(prefix + string(path))
	}
	for sc.Scan() {
		line := sc.Bytes()
		if in.Output == "files" { // one path per line
			last = fileOf(line)
			c.add(last, last.path, 1)
			continue
		}
		if string(line) == "--" {
			sep = true
			continue
		}
		path, rest, ok := bytes.Cut(line, []byte{0})
		if !ok { // "path: binary file matches (...)"
			last, sep = c.file(prefix+string(line)), false
			c.add(last, last.path, 1)
			continue
		}
		f := fileOf(path)
		if in.Output == "count" { // "path\x00count"
			n, _ := strconv.Atoi(string(rest))
			c.add(f, f.path+":"+string(rest), n)
			last = f
			continue
		}
		if sep && f == last {
			c.add(f, "--", 0)
		}
		last, sep = f, false
		// "12:text" for a match, "12-text" for a context line.
		d := 0
		for d < len(rest) && rest[d] >= '0' && rest[d] <= '9' {
			d++
		}
		mark, match := ":", 1
		if d < len(rest) && rest[d] == '-' {
			mark, match = "-", 0
		}
		s := ""
		if c.keeps(f) {
			s = f.path + mark + string(rest)
		}
		c.add(f, s, match)
	}
	werr := cmd.Wait()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	var ee *exec.ExitError
	if werr != nil && c.empty() && !(errors.As(werr, &ee) && ee.ExitCode() == 1) {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "No files were searched") {
			return c.String(), nil
		}
		if msg == "" {
			msg = werr.Error()
		}
		return "", errors.New(msg)
	}
	return c.String(), nil
}

func (t *grepTool) runGo(ctx context.Context, in *grepInput, root string, isDir bool) (string, error) {
	pat := in.Pattern
	if in.IgnoreCase {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return "", fmt.Errorf("invalid regex: %v", err)
	}
	var m *globMatcher
	if in.Glob != "" {
		if m, err = compileGlob(in.Glob); err != nil {
			return "", err
		}
	}
	c := newGrepPage(in)
	c.sorted = true // the walk goes in path order
	hidden := isDir && searchHidden(root)
	search := func(abs string) {
		data, err := os.ReadFile(abs)
		if err != nil || isBinary(data) {
			return
		}
		name := t.env.rel(abs)
		lines := strings.Split(string(data), "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		var hits []int
		for i, l := range lines {
			if re.MatchString(strings.TrimSuffix(l, "\r")) {
				hits = append(hits, i)
			}
		}
		if len(hits) == 0 {
			return
		}
		f := c.file(name)
		switch in.Output {
		case "files":
			c.add(f, name, 1)
			return
		case "count":
			c.add(f, name+":"+strconv.Itoa(len(hits)), len(hits))
			return
		}
		next := 0 // first line not yet printed in this file
		for k, h := range hits {
			from := max(h-in.Context, next)
			if in.Context > 0 && k > 0 && from > next { // grepPage parts files
				c.add(f, "--", 0)
			}
			for i := from; i <= h+in.Context && i < len(lines); i++ {
				sep, match := "-", 0
				if i == h || (i > h && re.MatchString(strings.TrimSuffix(lines[i], "\r"))) {
					sep, match = ":", 1
				}
				s := ""
				if c.keeps(f) {
					s = name + sep + strconv.Itoa(i+1) + sep + clipColumns(strings.TrimSuffix(lines[i], "\r"))
				}
				c.add(f, s, match)
				next = i + 1
			}
		}
	}
	if !isDir {
		search(root)
		return c.String(), nil
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return nil
		}
		if p != root && skipEntry(d, hidden) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if m != nil {
			rel, _ := filepath.Rel(root, p)
			if !m.Match(filepath.ToSlash(rel)) {
				return nil
			}
		}
		if info, err := d.Info(); err != nil || info.Size() > grepMaxFile {
			return nil
		}
		search(p)
		return nil
	})
	if err != nil {
		return "", err
	}
	return c.String(), nil
}

func clipColumns(s string) string {
	if len(s) <= grepMaxColumns {
		return s
	}
	return strings.ToValidUTF8(s[:grepMaxColumns], "") + " [... omitted end of long line]"
}
