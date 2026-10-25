// Package tools implements the built-in tools exposed to the model:
// read, write, edit, bash, grep and glob.
package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/akbxr/tego/internal/llm"
)

// Tool is a model-callable tool. A returned error is reported to the model
// as an is_error tool result with err.Error() as content.
type Tool interface {
	Def() llm.Tool
	Run(ctx context.Context, input json.RawMessage) (string, error)
}

// ImageTool is optionally implemented by tools that can return images
// (e.g. read on an image file, MCP screenshots). When present the agent
// calls RunImages instead of Run; images become tool_result Images.
type ImageTool interface {
	Tool
	RunImages(ctx context.Context, input json.RawMessage) (string, []llm.Block, error)
}

// ReadOnly is implemented by tools that never change files or state; the
// agent may run several of their calls at once.
type ReadOnly interface {
	ReadOnly() bool
}

// Env is the shared execution environment. Relative paths resolve against
// Cwd. It is shared by the main agent and its subagents.
type Env struct {
	Cwd string
	// Files, when set, reads and writes text files for read, edit and write
	// (an editor through ACP: its buffers, unsaved changes included).
	Files TextFiles
	// Terminal, when set, runs bash commands (an editor's terminal).
	Terminal Terminal
	// LSP, when set, checks a file after edit and write with its language
	// server; the syntax check runs when it does not answer.
	LSP Checker

	mu          sync.Mutex
	seen        map[string][sha256.Size]byte // abs path -> content hash when a tool last read or wrote it
	shellWrites int                          // notes given about edits made from the shell

	bg *shells // background shells, shared with At's Envs; made at first use (reg)
}

// MarkRead records p as read by the model, e.g. a file attached to the
// prompt with @path.
func (e *Env) MarkRead(p string) {
	p = e.abs(p)
	if sum, err := e.hash(context.Background(), p); err == nil {
		e.markSeen(p, sum)
	}
}

// TextFiles reads and writes whole text files somewhere other than the disk
// (an editor's buffers). Paths are absolute.
type TextFiles interface {
	ReadText(ctx context.Context, path string) (string, error)
	WriteText(ctx context.Context, path, content string) error
}

// Terminal runs a shell command somewhere the user watches it (an editor's
// terminal) and returns its output, as much as was kept, and how it ended.
type Terminal interface {
	Run(ctx context.Context, command, cwd string, timeout time.Duration, outputLimit int) (TerminalResult, error)
}

// TerminalResult is how a Terminal command ended.
type TerminalResult struct {
	Output    string
	Truncated bool // the start of the output was dropped
	ExitCode  int  // -1 when it ended by a signal
	Signal    string
	TimedOut  bool
}

// readText reads a whole text file, through Files when set.
func (e *Env) readText(ctx context.Context, p string) ([]byte, error) {
	if e.Files == nil || isBinaryFile(p) {
		return os.ReadFile(p)
	}
	s, err := e.Files.ReadText(ctx, p)
	return []byte(s), err
}

// writeText writes a whole text file, through Files when set (the editor
// keeps the file's mode).
func (e *Env) writeText(ctx context.Context, p string, data []byte, mode os.FileMode) error {
	if e.Files == nil {
		return os.WriteFile(p, data, mode)
	}
	return e.Files.WriteText(ctx, p, string(data))
}

// hash hashes a file's content as the tools see it (through Files when
// set: an unsaved buffer differs from the disk).
func (e *Env) hash(ctx context.Context, p string) ([sha256.Size]byte, error) {
	if e.Files == nil || isBinaryFile(p) {
		return hashFile(p)
	}
	data, err := e.readText(ctx, p)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(data), nil
}

// readFile is the package's readFile, through Files when set.
func (e *Env) readFile(ctx context.Context, p string, offset, limit int, sum *[sha256.Size]byte) (string, error) {
	if e.Files == nil || isBinaryFile(p) {
		return readFile(p, offset, limit, sum)
	}
	data, err := e.readText(ctx, p)
	if err != nil {
		return "", err
	}
	return numberLines(p, bytes.NewReader(data), func() int64 { return int64(len(data)) }, offset, limit, sum)
}

// isBinaryFile reports whether p, on disk, starts like a binary file: such
// files are never read or written as text through an editor.
func isBinaryFile(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 8192)
	n, _ := io.ReadFull(f, head)
	return isBinary(head[:n])
}

// markSeen records the content hash of p as known to the model.
func (e *Env) markSeen(p string, sum [sha256.Size]byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.seen == nil {
		e.seen = map[string][sha256.Size]byte{}
	}
	e.seen[p] = sum
}

// checkSeen enforces read-before-write: an existing file may only be
// edited or overwritten after a tool read it, and only if it has not
// changed since (a command, a formatter, the user or another agent).
// When the check fails, the error carries the file's current content
// (as read would show it) and the file counts as read from then on, so
// the model can retry the same call right away instead of spending a
// round on a separate read.
func (e *Env) checkSeen(p string, current [sha256.Size]byte, verb string) error {
	e.mu.Lock()
	sum, ok := e.seen[p]
	e.mu.Unlock()
	var why string
	switch {
	case !ok:
		why = fmt.Sprintf("%s was not read in this session (shell output such as cat does not count)", e.rel(p))
	case sum != current:
		why = fmt.Sprintf("%s changed since it was last read (by a command, a formatter, the user or another agent)", e.rel(p))
	default:
		return nil
	}
	return e.showFresh(p, current, why, verb)
}

// wasSeen reports whether a tool read or wrote p in this session.
func (e *Env) wasSeen(p string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.seen[p]
	return ok
}

// showFresh is the error for a refused change: why, plus the file's current
// content, which now counts as read so the retry can go through.
func (e *Env) showFresh(p string, current [sha256.Size]byte, why, verb string) error {
	var fresh [sha256.Size]byte
	text, err := e.readFile(context.Background(), p, 1, 0, &fresh)
	if err != nil { // binary or unreadable: nothing to show, but don't block the retry
		e.markSeen(p, current)
		return fmt.Errorf("%s, so nothing was changed. It cannot be shown as text (%v); retry the %s if that is still intended", why, err, verb)
	}
	e.markSeen(p, fresh)
	return fmt.Errorf("%s, so nothing was changed. Its current content follows and now counts as read: "+
		"check your change against it and retry the %s.\n%s", why, verb, text)
}

// hashFile hashes a file's content without loading it whole.
func hashFile(p string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	f, err := os.Open(p)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// Builtins returns the built-in tools in a stable order.
func Builtins(env *Env) []Tool {
	return []Tool{
		&readTool{env},
		&writeTool{env},
		&editTool{env},
		&bashTool{env},
		&bashOutputTool{env},
		&grepTool{env},
		&globTool{env},
	}
}

// abs resolves p (absolute, ~/..., or relative to Cwd) to a clean absolute path.
func (e *Env) abs(p string) string {
	switch {
	case p == "" || p == ".":
		return filepath.Clean(e.Cwd)
	case p == "~" || strings.HasPrefix(p, "~/"):
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	case filepath.IsAbs(p):
		return filepath.Clean(p)
	}
	return filepath.Join(e.Cwd, p)
}

// rel renders an absolute path relative to Cwd when it lies under Cwd.
func (e *Env) rel(p string) string {
	r, err := filepath.Rel(e.Cwd, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return p
	}
	return r
}

func decode(input json.RawMessage, v any) error {
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	if err := json.Unmarshal(input, v); err != nil {
		return fmt.Errorf("invalid input: %v", err)
	}
	return nil
}

func schema(s string) json.RawMessage { return json.RawMessage(s) }

// hasRg reports whether ripgrep is available on PATH.
func rgPath() string {
	p, err := exec.LookPath("rg")
	if err != nil {
		return ""
	}
	return p
}

// isBinary reports whether b (a file prefix) looks binary.
func isBinary(b []byte) bool {
	if len(b) > 8192 {
		b = b[:8192]
	}
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}

// Summary renders a one-line human description of a tool call.
func Summary(name string, input json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(input, &m)
	str := func(k string) string {
		s, _ := m[k].(string)
		return s
	}
	num := func(k string) int {
		f, _ := m[k].(float64)
		return int(f)
	}
	var s string
	switch name {
	case "read":
		s = "read " + str("path")
		if off, lim := num("offset"), num("limit"); off > 0 || lim > 0 {
			if off < 1 {
				off = 1
			}
			s += fmt.Sprintf(":%d-", off)
			if lim > 0 {
				s += fmt.Sprint(off + lim - 1)
			}
		}
	case "write":
		c := str("content")
		s = fmt.Sprintf("write %s (%d lines)", str("path"), countLines(c))
	case "edit":
		s = "edit " + str("path")
		if b, _ := m["replace_all"].(bool); b {
			s += " (all)"
		}
	case "bash":
		cmd := strings.TrimSpace(str("command"))
		if i := strings.IndexByte(cmd, '\n'); i >= 0 {
			cmd = strings.TrimSpace(cmd[:i]) + " …"
		}
		s = "bash " + cmd
	case "grep":
		s = fmt.Sprintf("grep %q", str("pattern"))
		if p := str("path"); p != "" {
			s += " " + p
		}
		if g := str("glob"); g != "" {
			s += " (" + g + ")"
		}
	case "glob":
		s = "glob " + str("pattern")
		if p := str("path"); p != "" {
			s += " in " + p
		}
	case "todo":
		done, total := 0, 0
		if ts, ok := m["todos"].([]any); ok {
			for _, t := range ts {
				total++
				if tm, ok := t.(map[string]any); ok && tm["status"] == "completed" {
					done++
				}
			}
		}
		s = fmt.Sprintf("todo %d/%d", done, total)
	case "checkpoint":
		s = "checkpoint: " + str("goal")
	case "rewind":
		s = "rewind: " + str("report")
	case "web_search":
		s = fmt.Sprintf("web_search %q", str("query"))
	case "tool_search":
		s = fmt.Sprintf("tool_search %q", str("query"))
	case "task":
		var names []string
		if ts, ok := m["tasks"].([]any); ok {
			for _, t := range ts {
				if tm, ok := t.(map[string]any); ok {
					n, _ := tm["name"].(string)
					names = append(names, n)
				}
			}
		}
		s = fmt.Sprintf("task ×%d: %s", len(names), strings.Join(names, ", "))
	default:
		s = name + " " + string(input)
	}
	return truncRunes(strings.Join(strings.Fields(s), " "), 100)
}

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}
