package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

// lineCap keeps one page of output lines (offset, limit), counts the rest,
// and counts matching lines per file for the summary of a cut result.
type lineCap struct {
	offset, limit int
	lines         []string
	total         int
	perFile       map[string]int
	fileOrder     []string
}

func newLineCap(in *grepInput) *lineCap {
	limit := in.Limit
	if limit <= 0 {
		limit = grepMaxLines
	}
	return &lineCap{offset: max(in.Offset, 0), limit: min(limit, grepMaxLimit), perFile: map[string]int{}}
}

// add records an output line; match is true for a matching line of file
// (not a context line or separator).
func (c *lineCap) add(s, file string, match bool) {
	c.total++
	if match && file != "" {
		if c.perFile[file] == 0 {
			c.fileOrder = append(c.fileOrder, file)
		}
		c.perFile[file]++
	}
	if c.total > c.offset && len(c.lines) < c.limit {
		c.lines = append(c.lines, strings.ToValidUTF8(s, "\uFFFD"))
	}
}

func (c *lineCap) String() string {
	if c.total == 0 {
		return "No matches found"
	}
	if len(c.lines) == 0 {
		return fmt.Sprintf("No lines after offset %d (the result has %d lines)", c.offset, c.total)
	}
	s := strings.Join(c.lines, "\n")
	shown := c.offset + len(c.lines)
	if shown >= c.total {
		return s
	}
	s += fmt.Sprintf("\n... showing lines %d-%d of %d.", c.offset+1, shown, c.total)
	if n := len(c.fileOrder); n > 1 {
		files := append([]string(nil), c.fileOrder...)
		sort.SliceStable(files, func(i, j int) bool { return c.perFile[files[i]] > c.perFile[files[j]] })
		s += fmt.Sprintf(" Matches per file (%d files, most first):", n)
		for _, f := range files[:min(n, grepTopFiles)] {
			s += fmt.Sprintf("\n  %s: %d", f, c.perFile[f])
		}
		if n > grepTopFiles {
			s += fmt.Sprintf("\n  ... %d more files", n-grepTopFiles)
		}
		s += "\n"
	}
	s += fmt.Sprintf(" Narrow it with path, glob or a more specific pattern, use output \"files\" or \"count\", or get the next lines with offset %d.", shown)
	return s
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
	args := []string{"--no-heading", "--color", "never", "--with-filename", "--sort", "path",
		"--max-columns", strconv.Itoa(grepMaxColumns), "--max-columns-preview"}
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
		args = append(args, "--regexp", in.Pattern)
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
	c := newLineCap(in)
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		path, rest, ok := strings.Cut(line, "\x00")
		switch {
		case line == "--":
			c.add(line, "", false)
		case !ok: // files mode: one path per line
			c.add(prefix+line, prefix+line, true)
		case in.Output == "count":
			c.add(prefix+path+":"+rest, prefix+path, true)
		default:
			// "12:text" for a match, "12-text" for a context line.
			digits := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
			sep := ":"
			if digits >= 0 && rest[digits] == '-' {
				sep = "-"
			}
			if digits >= 0 {
				rest = rest[:digits] + sep + rest[digits+1:]
			}
			c.add(prefix+path+sep+rest, prefix+path, sep == ":")
		}
	}
	werr := cmd.Wait()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	var ee *exec.ExitError
	if werr != nil && c.total == 0 && !(errors.As(werr, &ee) && ee.ExitCode() == 1) {
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
	c := newLineCap(in)
	emitted := false
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
		switch in.Output {
		case "files":
			c.add(name, name, true)
			return
		case "count":
			c.add(name+":"+strconv.Itoa(len(hits)), name, true)
			return
		}
		next := 0 // first line not yet printed in this file
		for k, h := range hits {
			from := max(h-in.Context, next)
			if in.Context > 0 && emitted && (k == 0 || from > next) {
				c.add("--", "", false)
			}
			for i := from; i <= h+in.Context && i < len(lines); i++ {
				sep := "-"
				if i == h || (i > h && re.MatchString(strings.TrimSuffix(lines[i], "\r"))) {
					sep = ":"
				}
				c.add(name+sep+strconv.Itoa(i+1)+sep+clipColumns(strings.TrimSuffix(lines[i], "\r")), name, sep == ":")
				next = i + 1
			}
			emitted = true
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
		if d.IsDir() {
			if p != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
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
