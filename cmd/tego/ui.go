package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	think     *markdown.Stream // thinking in the TUI; nil: raw
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

	// The edit or write call the model is writing now, for the footer's
	// live box (streamwrite.go). Guarded by runMu too.
	writing *liveWrite
	// The call waiting for the user's approval (toolbox.go), shown in the
	// footer. Guarded by runMu too.
	approval *pendingApproval
	// height is the content area's height (nil outside the TUI). It takes
	// the TUI's lock: never call it while the TUI draws.
	height func() int

	// live is what the turn has shown that the history does not hold yet,
	// guarded by mu: the response being streamed, then the results and
	// notices of its tool calls until they land in the history. A resize
	// mid-turn redraws the conversation and shows live again: before the
	// next streamed delta, on the agent's goroutine (beforeStream,
	// app.redrawIfResized), or while a tool runs (toolRun,
	// app.toolRunning).
	live         []liveSeg
	beforeStream func()
	// toolRun is told when a tool of the main agent starts running (after
	// OnToolCall) and when it is done (first thing in OnToolResult).
	toolRun func(running bool)
	// replaying: the history is being shown again, not run (mu).
	replaying bool
}

// liveSeg is a piece of the turn on screen but not in the history yet.
type liveSeg struct {
	kind  liveKind
	text  string // the text, thinking or notice; a result's output
	name  string // a result's tool, input and error flag
	input json.RawMessage
	isErr bool
}

type liveKind int

const (
	liveText liveKind = iota
	liveThinking
	liveResult // a tool call's result, before the batch's results land
	liveInfo   // a notice
)

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
	u := &termUI{out: w, diag: w, c: c, dc: c, atBOL: true, width: width, height: height}
	u.md = markdown.NewStream(w, markdown.Options{Width: width, Height: height, Color: c.reset != ""})
	// Thinking is plain, dim and italic, and word-wrapped like the answer.
	u.think = markdown.NewStream(styled{w, c.dim + c.italic, c.reset}, markdown.Options{Width: width, Height: height})
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
		if u.think != nil {
			u.think.Close() // its last line ends with a newline
		} else if !u.atBOL {
			fmt.Fprintln(u.diag)
		}
	}
	u.mode = modeNone
	u.atBOL = true
}

func (u *termUI) OnText(d string) {
	if h := u.beforeStream; h != nil {
		h()
	}
	u.streamed(len(d))
	u.mu.Lock()
	defer u.mu.Unlock()
	u.recordLive(liveText, d)
	u.textLocked(d)
}

// ReplayText shows text from the history: not counted for tok/s, not
// recorded as live.
func (u *termUI) ReplayText(d string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.textLocked(d)
}

func (u *termUI) textLocked(d string) {
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
	if h := u.beforeStream; h != nil {
		h()
	}
	u.streamed(len(d))
	u.mu.Lock()
	defer u.mu.Unlock()
	u.recordLive(liveThinking, d)
	u.thinkingLocked(d)
}

// ReplayThinking shows thinking from the history, like ReplayText.
func (u *termUI) ReplayThinking(d string) {
	if u.quiet {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.thinkingLocked(d)
}

func (u *termUI) thinkingLocked(d string) {
	if u.mode != modeThinking {
		u.endStream()
		u.mode = modeThinking
		d = strings.TrimLeft(d, "\n")
	}
	if d == "" {
		return
	}
	if u.think != nil {
		u.think.Write(d) // word-wrapped, styled by its writer
	} else {
		// Style every chunk: a pinned footer may redraw (and reset SGR) between writes.
		fmt.Fprint(u.diag, u.dc.dim+u.dc.italic+d+u.dc.reset)
	}
	u.atBOL = strings.HasSuffix(d, "\n")
}

// styled writes each chunk in its own style: a pinned footer may redraw
// (and reset SGR) between writes.
type styled struct {
	w          io.Writer
	start, end string
}

func (s styled) Write(p []byte) (int, error) {
	if _, err := io.WriteString(s.w, s.start+string(p)+s.end); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (u *termUI) recordLive(kind liveKind, d string) {
	if n := len(u.live); n > 0 && u.live[n-1].kind == kind {
		u.live[n-1].text += d
		return
	}
	u.live = append(u.live, liveSeg{kind: kind, text: d})
}

// takeLive is what the turn has shown that the history does not hold.
// Once the next response streams, the last batch's results are in the
// history: streaming keeps only the response's text and thinking.
func (u *termUI) takeLive(streaming bool) []liveSeg {
	u.mu.Lock()
	defer u.mu.Unlock()
	if streaming {
		return slices.DeleteFunc(slices.Clone(u.live), func(s liveSeg) bool { return s.kind > liveThinking })
	}
	return slices.Clone(u.live)
}

// resetLive starts a response: nothing streamed yet.
func (u *termUI) resetLive() {
	u.mu.Lock()
	u.live = nil
	u.mu.Unlock()
}

// showLive shows segs again, after a redraw.
func (u *termUI) showLive(segs []liveSeg) {
	for _, s := range segs {
		switch s.kind {
		case liveText:
			u.ReplayText(s.text)
		case liveThinking:
			u.ReplayThinking(s.text)
		case liveResult:
			u.ReplayToolResult(s.name, s.input, s.text, s.isErr)
		case liveInfo:
			u.mu.Lock()
			u.infoLocked(s.text)
			u.mu.Unlock()
		}
	}
}

// closeStream ends an open text or thinking stream, and nothing else: a
// command may be running.
func (u *termUI) closeStream() {
	u.mu.Lock()
	u.endStream()
	u.mu.Unlock()
}

func (u *termUI) OnToolStart(string) {
	u.mu.Lock()
	u.endStream()
	u.mu.Unlock()
}

func (u *termUI) OnToolCall(name string, input json.RawMessage) {
	u.endWrite() // the call is complete: its own box follows
	u.toolCall(name, input, false)
	if h := u.toolRun; h != nil {
		h(true)
	}
}

// ReplayToolCall shows a call from the history: nothing starts running.
func (u *termUI) ReplayToolCall(name string, input json.RawMessage) {
	u.toolCall(name, input, true)
}

func (u *termUI) toolCall(name string, input json.RawMessage, replay bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.endStream()
	if u.boxed(name) {
		if !replay {
			u.boxCallLocked(name, input)
		}
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
	if h := u.toolRun; h != nil {
		h(false) // waits for a redraw in progress
	}
	if hook := u.onTurnEnd; hook != nil && (name == "todo" || name == "rewind") {
		defer hook() // after mu is released (deferred calls run LIFO)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.live = append(u.live, liveSeg{kind: liveResult, name: name, input: input, text: output, isErr: isErr})
	u.toolResultLocked(name, input, output, isErr)
}

// ReplayToolResult shows a result from the history (or live): the command
// running now, if any, keeps its footer box.
func (u *termUI) ReplayToolResult(name string, input json.RawMessage, output string, isErr bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.replaying = true
	defer func() { u.replaying = false }()
	u.toolResultLocked(name, input, output, isErr)
}

func (u *termUI) toolResultLocked(name string, input json.RawMessage, output string, isErr bool) {
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
	u.endWrite() // the response is complete, or cut off: nothing is being written
	u.mu.Lock()
	u.endStream()
	u.live = nil // in the history now
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
	u.live = append(u.live, liveSeg{kind: liveInfo, text: msg})
	u.infoLocked(msg)
}

func (u *termUI) infoLocked(msg string) {
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
	u.endWrite()
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
