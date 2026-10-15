package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/akbxr/tego/internal/images"
	"github.com/akbxr/tego/internal/lineedit"
	"github.com/akbxr/tego/internal/llm"
)

const (
	maxIndexedFiles = 20000
	maxMentionBytes = 100 << 10 // file content attached per @mention
	maxMentionLines = 2000
	maxMentionHits  = 50
)

// fileIndex is the project's file list for the @ picker. It is rebuilt in
// the background (startup, after each turn) and read on the TUI goroutine.
type fileIndex struct {
	mu    sync.Mutex
	items []lineedit.PickItem // Label = path ("dir/" for directories)
	top   []lineedit.PickItem // entries directly in cwd, for a bare "@"
}

func (x *fileIndex) snapshot() (all, top []lineedit.PickItem) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.items, x.top
}

// refreshFiles lists project files (gitignore-aware via rg or git, else a
// plain walk skipping heavy directories) without blocking the caller.
func (a *app) refreshFiles() {
	go func() {
		files := listFiles(a.cwd)
		dirs := map[string]bool{}
		for _, f := range files {
			for d := filepath.Dir(f); d != "." && !dirs[d]; d = filepath.Dir(d) {
				dirs[d] = true
			}
		}
		items := make([]lineedit.PickItem, 0, len(files)+len(dirs))
		var top []lineedit.PickItem
		for d := range dirs {
			it := lineedit.PickItem{Label: d + "/"}
			items = append(items, it)
			if !strings.Contains(d, "/") {
				top = append(top, it)
			}
		}
		for _, f := range files {
			it := lineedit.PickItem{Label: f}
			items = append(items, it)
			if !strings.Contains(f, "/") {
				top = append(top, it)
			}
		}
		// Shorter paths first so equally good matches favour shallow files.
		sort.SliceStable(items, func(i, j int) bool { return len(items[i].Label) < len(items[j].Label) })
		sort.Slice(top, func(i, j int) bool {
			di, dj := strings.HasSuffix(top[i].Label, "/"), strings.HasSuffix(top[j].Label, "/")
			if di != dj {
				return di
			}
			return top[i].Label < top[j].Label
		})
		a.files.mu.Lock()
		a.files.items, a.files.top = items, top
		a.files.mu.Unlock()
	}()
}

func listFiles(cwd string) []string {
	for _, args := range [][]string{
		{"rg", "--files", "--hidden", "--glob", "!.git"},
		{"git", "ls-files", "--cached", "--others", "--exclude-standard"},
	} {
		if _, err := exec.LookPath(args[0]); err != nil {
			continue
		}
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = cwd
		out, err := cmd.Output()
		if err != nil && len(out) == 0 {
			continue
		}
		var files []string
		sc := bufio.NewScanner(bytes.NewReader(out))
		for sc.Scan() && len(files) < maxIndexedFiles {
			files = append(files, filepath.ToSlash(strings.TrimPrefix(sc.Text(), "./")))
		}
		return files
	}
	var files []string
	_ = filepath.WalkDir(cwd, func(p string, d fs.DirEntry, err error) error {
		if err != nil || len(files) >= maxIndexedFiles {
			return filepath.SkipDir
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "target", "dist", ".venv", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		if rel, err := filepath.Rel(cwd, p); err == nil {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	return files
}

// suggestFiles completes an "@query" token at the end of the line with
// project paths. It runs on the TUI goroutine with its lock held: cached
// data only, no I/O.
func (a *app) suggestFiles(line string) []lineedit.Suggestion {
	i := strings.LastIndexAny(line, " \t\n")
	token := line[i+1:]
	if !strings.HasPrefix(token, "@") {
		return nil
	}
	prefix, query := line[:i+1], strings.Trim(token[1:], `"'`)
	all, top := a.files.snapshot()
	var hits []lineedit.PickItem
	if query == "" {
		hits = top
	} else {
		for _, h := range lineedit.Filter(all, query) {
			hits = append(hits, all[h])
			if len(hits) == maxMentionHits {
				break
			}
		}
	}
	out := make([]lineedit.Suggestion, 0, len(hits))
	for _, h := range hits {
		p := h.Label
		dir := strings.HasSuffix(p, "/")
		ref := "@" + p
		if strings.ContainsAny(p, " \t") {
			ref = `@"` + p + `"`
		}
		s := lineedit.Suggestion{Label: p, Icon: "▫", Value: prefix + ref + " "}
		if dir {
			s.Icon, s.Description, s.Value = "▸", "directory", prefix+ref // keep typing to go deeper
		}
		out = append(out, s)
	}
	return out
}

var (
	mentionRe     = regexp.MustCompile(`@(?:"([^"]+)"|'([^']+)'|([^\s@]+))`)
	mentionLead   = regexp.MustCompile("^[`\"'(\\[{<]+")
	mentionTrail  = regexp.MustCompile("[)\\]}>.,;:!?\"'`]+$")
	mentionBefore = " \t\n([{<\"'`"
)

// extractMentions finds @path references: @path, @"path with
// spaces", @'path'. An @ must start the text or follow whitespace or an
// opening bracket/quote, so emails and decorators are ignored.
func extractMentions(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range mentionRe.FindAllStringSubmatchIndex(text, -1) {
		if m[0] > 0 && !strings.ContainsRune(mentionBefore, rune(text[m[0]-1])) {
			continue
		}
		var p string
		switch {
		case m[2] >= 0:
			p = strings.TrimSpace(text[m[2]:m[3]])
		case m[4] >= 0:
			p = strings.TrimSpace(text[m[4]:m[5]])
		default:
			p = mentionTrail.ReplaceAllString(mentionLead.ReplaceAllString(text[m[6]:m[7]], ""), "")
		}
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// mentionPrefix marks attached file content in user messages.
const mentionPrefix = `<file path="`

// collectMentions reads the files and directories named by @mentions and
// returns blocks to send after the prompt: file contents (numbered lines,
// truncated), directory listings, and images. Unknown paths are left as
// plain text.
func (a *app) collectMentions(prompt string) []llm.Block {
	var blocks []llm.Block
	for _, ref := range extractMentions(prompt) {
		abs := ref
		if strings.HasPrefix(abs, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				abs = filepath.Join(home, abs[2:])
			}
		}
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(a.cwd, abs)
		}
		st, err := os.Stat(abs)
		if err != nil {
			continue
		}
		if st.IsDir() {
			listing := dirListing(abs)
			blocks = append(blocks, llm.Block{Type: llm.BlockText, Text: fmt.Sprintf("%s%s\">\n%s</file>", mentionPrefix, ref, listing)})
			a.ui.Dim("📎 @%s (directory, %d entries)", ref, strings.Count(listing, "\n"))
			continue
		}
		if head := readHead(abs, 16); images.MediaType(head) != "" {
			blk, info, err := images.Load(abs)
			if err != nil {
				a.ui.Errorf("@%s: %v", ref, err)
				continue
			}
			blocks = append(blocks, llm.Block{Type: llm.BlockText, Text: fmt.Sprintf("%s%s\" type=\"image\"></file>", mentionPrefix, ref)}, blk)
			a.ui.Dim("📎 @%s · %d×%d · %s", ref, info.Width, info.Height, fmtBytes(info.Bytes))
			continue
		}
		content, lines, note := readMention(abs, st.Size())
		blocks = append(blocks, llm.Block{Type: llm.BlockText, Text: fmt.Sprintf("%s%s\">\n%s%s</file>", mentionPrefix, ref, content, note)})
		if a.env != nil {
			a.env.MarkRead(abs) // the model has the content: editing it needs no extra read
		}
		if note != "" {
			a.ui.Dim("📎 @%s (%d lines, truncated)", ref, lines)
		} else {
			a.ui.Dim("📎 @%s (%d lines)", ref, lines)
		}
	}
	return blocks
}

func readHead(path string, n int) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	b := make([]byte, n)
	k, _ := f.Read(b)
	return b[:k]
}

// readMention returns the file with numbered lines (like the read tool),
// capped in bytes and lines, plus a truncation note.
func readMention(path string, size int64) (string, int, string) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, "(unreadable: " + err.Error() + ")\n"
	}
	defer f.Close()
	if bytes.IndexByte(readHead(path, 8192), 0) >= 0 {
		return "", 0, fmt.Sprintf("(binary file, %s; not attached)\n", fmtBytes(int(size)))
	}
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		if n > maxMentionLines || b.Len() > maxMentionBytes {
			for sc.Scan() {
				n++
			}
			return b.String(), n, fmt.Sprintf("(truncated: file has %d lines; use read with offset for the rest)\n", n)
		}
		fmt.Fprintf(&b, "%d\t%s\n", n, sc.Text())
	}
	return b.String(), n, ""
}

func dirListing(dir string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "(unreadable)\n"
	}
	var b strings.Builder
	for i, e := range ents {
		if i == 200 {
			fmt.Fprintf(&b, "… %d more\n", len(ents)-200)
			break
		}
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		b.WriteString(name + "\n")
	}
	return b.String()
}
