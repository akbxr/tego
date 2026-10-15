package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/markdown"
	"github.com/akbxr/tego/internal/tools"
)

type palette struct {
	reset, dim, bold, italic, red, green, yellow, blue, magenta, cyan string
}

func newPalette(f *os.File) palette {
	if !term.IsTerminal(int(f.Fd())) {
		return palette{}
	}
	return colorPalette()
}

// colorPalette is the ANSI palette unless NO_COLOR is set.
func colorPalette() palette {
	if os.Getenv("NO_COLOR") != "" {
		return palette{}
	}
	return palette{
		reset: "\x1b[0m", dim: "\x1b[2m", bold: "\x1b[1m", italic: "\x1b[3m",
		red: "\x1b[31m", green: "\x1b[32m", yellow: "\x1b[33m", blue: "\x1b[34m", magenta: "\x1b[35m", cyan: "\x1b[36m",
	}
}

const (
	modeNone = iota
	modeText
	modeThinking
)

// termUI renders agent output. Assistant text goes to out; everything else
// (thinking, tool activity, info) goes to diag. Both may be the same stream.
type termUI struct {
	mu        sync.Mutex
	out, diag io.Writer
	c, dc     palette
	mode      int
	atBOL     bool             // diag cursor at beginning of line
	quiet     bool             // print mode: suppress thinking and tool previews
	md        *markdown.Stream // nil when out is not a terminal: raw text
	width     func() int
	cwd, home string                        // for shortening paths in tool-call lines
	promptGap bool                          // a prompt block was just printed; see AfterPrompt
	onTurnEnd func()                        // called after each assistant message (status refresh)
	subagents func() []agent.SubagentStatus // the latest task call's subagents, for the Task box

	// tok/s measurement for the current user turn, guarded by rateMu
	rateMu     sync.Mutex
	firstAt    time.Time // first delta of the in-flight request
	chars      int
	doneTokens int
	doneDur    time.Duration

	// The bash command running now and since when, for the footer's live
	// row. Its own lock: the TUI reads it while drawing, and mu is held
	// while writing to the TUI.
	runMu    sync.Mutex
	running  string
	runSince time.Time
	runOut   []byte // the end of its output so far
}

// newPrintUI writes the answer to stdout and everything else to stderr,
// rendering markdown when stdout is a terminal.
func newPrintUI() *termUI {
	u := &termUI{out: os.Stdout, diag: os.Stderr, c: newPalette(os.Stdout), dc: newPalette(os.Stderr), atBOL: true, quiet: true}
	u.width = func() int {
		if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
			return w
		}
		return 80
	}
	if term.IsTerminal(int(os.Stdout.Fd())) {
		u.md = markdown.NewStream(os.Stdout, markdown.Options{Width: u.width, Color: u.c.reset != ""})
	}
	return u
}

// newTUIUI renders everything above the TUI footer.
func newTUIUI(w io.Writer, width, height func() int) *termUI {
	c := colorPalette()
	u := &termUI{out: w, diag: w, c: c, dc: c, atBOL: true, width: width}
	u.md = markdown.NewStream(w, markdown.Options{Width: width, Height: height, Color: c.reset != ""})
	return u
}

// AfterPrompt asks for one blank line between a prompt block and the next
// output (thinking, a tool call, a notice); text opens with one anyway.
func (u *termUI) AfterPrompt() {
	u.mu.Lock()
	u.promptGap = true
	u.mu.Unlock()
}

// endStream closes an open text/thinking stream with a newline.
func (u *termUI) endStream() {
	if u.promptGap {
		u.promptGap = false
		fmt.Fprintln(u.diag)
	}
	switch u.mode {
	case modeText:
		if u.md != nil {
			u.md.Close()
		}
		if !u.atBOL {
			fmt.Fprintln(u.out)
		}
	case modeThinking:
		if !u.atBOL {
			fmt.Fprintln(u.diag)
		}
	}
	u.mode = modeNone
	u.atBOL = true
}

func (u *termUI) OnText(d string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.streamed(len(d))
	if u.mode != modeText {
		u.promptGap = false // text already opens with a blank line
		u.endStream()
		if !u.quiet {
			fmt.Fprintln(u.out)
		}
		u.mode = modeText
		d = strings.TrimLeft(d, "\n")
	}
	if d == "" {
		return
	}
	if u.md != nil {
		u.md.Write(d)
	} else {
		fmt.Fprint(u.out, d)
	}
	u.atBOL = strings.HasSuffix(d, "\n")
}

func (u *termUI) OnThinking(d string) {
	if u.quiet {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.streamed(len(d))
	if u.mode != modeThinking {
		u.endStream()
		u.mode = modeThinking
		d = strings.TrimLeft(d, "\n")
	}
	if d == "" {
		return
	}
	// Style every chunk: a pinned footer may redraw (and reset SGR) between writes.
	fmt.Fprint(u.diag, u.dc.dim+u.dc.italic+d+u.dc.reset)
	u.atBOL = strings.HasSuffix(d, "\n")
}

func (u *termUI) OnToolStart(string) {
	u.mu.Lock()
	u.endStream()
	u.mu.Unlock()
}

func (u *termUI) OnToolCall(name string, input json.RawMessage) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.endStream()
	if u.boxed(name) {
		u.boxCallLocked(name, input)
		return
	}
	fmt.Fprintf(u.diag, "%s%s●%s %s%s\n", u.dc.cyan, u.dc.bold, u.dc.reset+u.dc.cyan, u.shortPaths(name, tools.Summary(name, input)), u.dc.reset)
}

// shortPaths keeps tool-call lines short: file paths under the
// working directory become relative, and anything else under $HOME is
// shown as ~/….
func (u *termUI) shortPaths(tool, s string) string {
	switch tool {
	case "read", "write", "edit":
		if u.cwd != "" {
			s = strings.Replace(s, " "+u.cwd+string(filepath.Separator), " ", 1)
		}
	}
	if u.home != "" {
		s = strings.ReplaceAll(s, u.home+string(filepath.Separator), "~"+string(filepath.Separator))
	}
	return s
}

// shortPath shortens one file path the same way: relative under the
// working directory, ~/… elsewhere under $HOME.
func (u *termUI) shortPath(p string) string {
	sep := string(filepath.Separator)
	switch {
	case u.cwd != "" && strings.HasPrefix(p, u.cwd+sep):
		return p[len(u.cwd)+1:]
	case u.home != "" && strings.HasPrefix(p, u.home+sep):
		return "~" + p[len(u.home):]
	}
	return p
}

func (u *termUI) OnToolResult(name string, input json.RawMessage, output string, isErr bool) {
	if hook := u.onTurnEnd; hook != nil && (name == "todo" || name == "rewind") {
		defer hook() // after mu is released (deferred calls run LIFO)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.boxed(name) {
		u.boxResultLocked(name, input, output, isErr)
		return
	}
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	limit := 3
	switch {
	case isErr:
		limit = 6
	case name == "edit" || name == "write":
		limit = 24
	case name == "todo":
		limit = 40
		lines = lines[1:] // the "Todo n/m" header duplicates the call summary
	case name == "task" || name == "checkpoint" || name == "rewind":
		limit = 0
	case u.quiet:
		limit = 0
	}
	shown := lines
	if len(shown) > limit {
		shown = shown[:limit]
	}
	for _, l := range shown {
		if r := []rune(l); len(r) > 160 {
			l = string(r[:160]) + "…"
		}
		color := u.dc.dim
		switch {
		case isErr:
			color = u.dc.red
		case name == "todo" && strings.HasPrefix(l, "✓"):
			color = u.dc.green
		case name == "todo" && strings.HasPrefix(l, "▸"):
			color = u.dc.yellow + u.dc.bold
		case name == "todo" && strings.HasPrefix(l, "○"):
			color = ""
		case name == "edit" && strings.HasPrefix(strings.TrimLeft(l, " 0123456789"), "+"):
			color = u.dc.green
		case name == "edit" && strings.HasPrefix(strings.TrimLeft(l, " 0123456789"), "-"):
			color = u.dc.red
		}
		fmt.Fprintf(u.diag, "  %s│ %s%s\n", u.dc.dim, color+l, u.dc.reset)
	}
	if extra := len(lines) - len(shown); extra > 0 && limit > 0 {
		fmt.Fprintf(u.diag, "  %s│ … %d more lines%s\n", u.dc.dim, extra, u.dc.reset)
	}
}

func (u *termUI) OnTurnEnd(m *llm.Message) {
	u.mu.Lock()
	u.endStream()
	hook := u.onTurnEnd
	u.mu.Unlock()
	u.rateMu.Lock()
	u.firstAt, u.chars = time.Time{}, 0 // requestDone already counted the request
	u.rateMu.Unlock()
	if hook != nil {
		hook()
	}
}

// requestDone adds a finished main-agent request to the tok/s figure, timed
// like tego stats: output tokens over the time since generation began, and
// only when that window is long enough to mean anything (a model that
// thinks silently and then sends everything at once, or a provider that
// buffers its answer, has no measurable speed).
func (u *termUI) requestDone(st llm.RequestStat) {
	u.rateMu.Lock()
	defer u.rateMu.Unlock()
	u.firstAt, u.chars = time.Time{}, 0 // the in-flight estimate ends with the request
	tokens := st.Usage.Output
	if st.GenOutput > 0 {
		tokens = st.GenOutput
	}
	if gen := st.Duration - st.GenStart; st.GenStart > 0 && st.Err == "" && tokens > 0 && gen >= minRateWindow {
		u.doneTokens += tokens
		u.doneDur += gen
	}
}

// minRateWindow matches the stats package: output arriving faster than
// this came in one burst.
const minRateWindow = 500 * time.Millisecond

// streamed records generated text for the tok/s estimate.
func (u *termUI) streamed(n int) {
	u.rateMu.Lock()
	if u.firstAt.IsZero() {
		u.firstAt = time.Now()
	}
	u.chars += n
	u.rateMu.Unlock()
}

// resetRate starts a new tok/s measurement (one per user turn).
func (u *termUI) resetRate() {
	u.rateMu.Lock()
	u.firstAt, u.chars, u.doneTokens, u.doneDur = time.Time{}, 0, 0, 0
	u.rateMu.Unlock()
}

// rate is output tokens per second of generation time: finished requests
// use reported usage, the in-flight one estimates ~4 chars per token.
// The footer calls it while holding the TUI lock, so it must only take
// rateMu (never mu, which is held while writing into the TUI).
func (u *termUI) rate() float64 {
	u.rateMu.Lock()
	defer u.rateMu.Unlock()
	tokens, dur := float64(u.doneTokens), u.doneDur
	if !u.firstAt.IsZero() {
		tokens += float64(u.chars) / 4
		dur += time.Since(u.firstAt)
	}
	if dur < 300*time.Millisecond || tokens == 0 {
		return 0
	}
	return tokens / dur.Seconds()
}

func (u *termUI) OnInfo(msg string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.endStream()
	fmt.Fprintf(u.diag, "%s%s%s\n", u.dc.yellow, msg, u.dc.reset)
}

func (u *termUI) Errorf(format string, a ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.endStream()
	fmt.Fprintf(u.diag, "%serror:%s %s\n", u.dc.red+u.dc.bold, u.dc.reset+u.dc.red, fmt.Sprintf(format, a...)+u.dc.reset)
}

func (u *termUI) Dim(format string, a ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.endStream()
	fmt.Fprintf(u.diag, "%s%s%s\n", u.dc.dim, fmt.Sprintf(format, a...), u.dc.reset)
}

func (u *termUI) Finish() {
	u.mu.Lock()
	u.endStream()
	u.mu.Unlock()
	u.setRunning("") // a turn that ended without the result (aborted, failed)
}

func fmtTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}
