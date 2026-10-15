package lineedit

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// EventKind identifies an input event.
type EventKind int

const (
	EvSubmit    EventKind = iota // Enter: Text is the buffer (buffer cleared, history cursor reset)
	EvInterrupt                  // Ctrl+C on an empty buffer (otherwise Ctrl+C clears the buffer)
	EvEscape                     // lone Esc while no picker or suggestion popup is open
	EvEOF                        // Ctrl+D on an empty buffer, or end of input
	EvResize                     // the terminal width changed (content may need re-rendering)
	EvLeft                       // Left arrow on an empty buffer (otherwise it moves the cursor)
	EvFollowUp                   // Alt+Enter while busy (SetBusy): Text is the buffer (buffer cleared)
	EvDequeue                    // Alt+Up: Text is the buffer (left as is), for restoring queued messages
	EvBackTab                    // Shift+Tab while no suggestion popup is open
)

// Event is one input event delivered on TUI.Events.
type Event struct {
	Kind EventKind
	Text string
}

// Options configures the TUI. The Header, Status, Activity and Suggest
// callbacks run with the TUI lock held: they must be cheap and must not call
// TUI methods.
type Options struct {
	HistoryPath string                         // "" = no persistence
	Prompt      string                         // e.g. "\x1b[35;1m❯\x1b[0m " (may contain SGR)
	Suggest     func(line string) []Suggestion // popup under a single-line buffer, queried on every change; nil/empty = no popup
	CtrlV       func() string                  // Ctrl+V hook; returned text inserted at cursor (may block briefly)
	Paste       func(text string) string       // rewrites pasted text (e.g. dragged image paths) before it is inserted; may block briefly
	Header      func(width int) string         // styled line drawn above the input; "" = none
	Status      func(width int) []string       // styled lines drawn below the input
	Activity    func(width int) string         // styled line drawn above Header while busy; "" = no line (animated every 100 ms while non-empty)
	Panel       func(width int) []string       // styled lines drawn above Activity (at most half the screen); nil/empty = none
	// Margin keeps content (not the footer) this many cells away from the
	// left and right edge: every row starts indented and wraps early.
	Margin int
}

// ErrNotTerminal is returned by Start when stdin or stdout is not a terminal.
var ErrNotTerminal = errors.New("stdin/stdout is not a terminal")

const (
	bracketedPasteOn  = "\x1b[?2004h"
	bracketedPasteOff = "\x1b[?2004l"
	// Keyboard reporting that tells Shift+Enter (a newline) from Enter: the
	// kitty protocol's "disambiguate" flag (kitty, Ghostty, WezTerm, iTerm2,
	// foot, Alacritty) and xterm's modifyOtherKeys level 1 (xterm, tmux).
	// Terminals that know neither ignore both and send Enter for Shift+Enter;
	// Alt+Enter, Ctrl+J and "\" before Enter still insert a newline there.
	keysOn        = "\x1b[>1u\x1b[>4;1m"
	keysOff       = "\x1b[<u\x1b[>4m"
	termOn        = bracketedPasteOn + keysOn
	termOff       = keysOff + bracketedPasteOff
	defaultWidth  = 80
	defaultHeight = 24
	eventBuffer   = 16
)

// TUI is the inline terminal UI: content written through Writer scrolls in
// the normal screen above a footer holding the editor and status lines.
type TUI struct {
	opts       Options
	out        io.Writer
	promptW    int
	contPrefix string
	color      bool

	mu           sync.Mutex
	busy         bool // see SetBusy
	started      bool
	suspended    bool
	closed       bool
	width        int
	height       int
	sess         session
	hist         history
	pick         *pickState
	dirty        bool     // editor changed but not yet drawn (more input queued)
	col          int      // content cursor column; == width: pending wrap
	footerStart  int      // footer row 0 is this many rows below the content position
	cursorDown   int      // rows from the content position down to the real cursor
	curCol       int      // real cursor column after the last footer draw
	footerRows   int      // lines in the last footer
	drawn        bool     // footer currently on screen
	cursorHidden bool     // cursor hidden by us
	lines        []string // last footer lines (reused)
	buf          []byte   // output buffer (reused)
	carry        []byte   // incomplete escape/UTF-8 tail held back from a Write
	lastDraw     time.Time
	drawTimer    *time.Timer
	drawPending  bool
	tickTimer    *time.Timer
	tickArmed    bool

	// terminal
	inFd, outFd  int
	oldState     *term.State
	wakeR, wakeW int
	winch        chan os.Signal

	// input reader pause/stop, guarded by inMu (never held with mu acquired after it)
	inMu     sync.Mutex
	inCond   *sync.Cond
	paused   bool
	inClosed bool

	events chan Event
	done   chan struct{}
	wg     sync.WaitGroup
}

// newTUI builds a TUI drawing to out at the given size, without touching a
// terminal or starting goroutines.
func newTUI(o Options, out io.Writer, width, height int) *TUI {
	pw := visibleWidth(o.Prompt)
	t := &TUI{
		opts:       o,
		out:        out,
		promptW:    pw,
		contPrefix: continuationPrefix(pw),
		color:      os.Getenv("NO_COLOR") == "",
		width:      max(width, 1),
		height:     max(height, 1),
		hist:       history{path: o.HistoryPath},
		events:     make(chan Event, eventBuffer),
		done:       make(chan struct{}),
		inFd:       -1,
		outFd:      -1,
	}
	t.inCond = sync.NewCond(&t.inMu)
	t.hist.load()
	t.sess = session{hist: &t.hist, histIdx: len(t.hist.entries), sug: suggestState{fn: o.Suggest}}
	return t
}

// Start puts stdin in raw mode, enables bracketed paste, draws the footer
// and starts reading input.
func Start(o Options) (*TUI, error) {
	inFd, outFd := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	if !term.IsTerminal(inFd) || !term.IsTerminal(outFd) {
		return nil, ErrNotTerminal
	}
	w, h := termSize(outFd)
	t := newTUI(o, os.Stdout, w, h)
	t.inFd, t.outFd = inFd, outFd
	var p [2]int
	if err := unix.Pipe(p[:]); err != nil {
		return nil, err
	}
	t.wakeR, t.wakeW = p[0], p[1]
	for _, fd := range p {
		unix.CloseOnExec(fd)
		_ = unix.SetNonblock(fd, true)
	}
	old, err := term.MakeRaw(inFd)
	if err != nil {
		unix.Close(p[0])
		unix.Close(p[1])
		return nil, err
	}
	t.oldState = old
	t.winch = make(chan os.Signal, 1)
	signal.Notify(t.winch, syscall.SIGWINCH)

	t.mu.Lock()
	t.started = true
	t.flush(append(t.buf[:0], termOn...))
	t.drawLocked()
	t.mu.Unlock()

	t.wg.Add(2)
	go func() {
		defer t.wg.Done()
		t.inputLoop(bufio.NewReader(ttyReader{t}))
	}()
	go t.winchLoop()
	return t, nil
}

func termSize(fd int) (int, int) {
	w, h, err := term.GetSize(fd)
	if err != nil || w <= 0 || h <= 0 {
		return defaultWidth, defaultHeight
	}
	return w, h
}

// Events delivers input events. It is closed by Close.
func (t *TUI) Events() <-chan Event { return t.events }

// Writer returns a goroutine-safe writer whose output appears above the footer.
func (t *TUI) Writer() io.Writer { return contentWriter{t} }

type contentWriter struct{ t *TUI }

func (w contentWriter) Write(p []byte) (int, error) {
	w.t.mu.Lock()
	defer w.t.mu.Unlock()
	w.t.write(p)
	return len(p), nil
}

// ContentHeight is the number of terminal rows not used by the footer (min 1).
func (t *TUI) ContentHeight() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return max(t.height-t.footerRows, 1)
}

// Width is the terminal width in cells.
func (t *TUI) Width() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.width
}

// ContentWidth is the width available to content between the margins.
func (t *TUI) ContentWidth() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return max(t.width-2*t.opts.Margin, 1)
}

// Clear wipes the screen and the terminal's scrollback (CSI 2J + 3J), then
// redraws the footer at the top. Callers re-print whatever content should
// remain (e.g. the conversation after a rewind).
func (t *TUI) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.live() {
		return
	}
	t.flush(append(t.buf[:0], "\x1b[H\x1b[2J\x1b[3J"...))
	t.col, t.drawn = 0, false
	t.drawLocked()
	t.dirty = false
}

// Redraw re-renders the footer (e.g. after status or activity changed).
// Calls within ~16 ms are coalesced into one draw.
func (t *TUI) Redraw() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.requestDrawLocked()
}

// WriteRaw sends s straight to the terminal (a notification escape or BEL).
// s must not move the cursor or print visible text.
func (t *TUI) WriteRaw(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.live() {
		t.flush(append(t.buf[:0], s...))
	}
}

// AddHistory appends line to the history and persists it. Blank lines and
// repeats of the most recent entry are skipped.
func (t *TUI) AddHistory(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	atDraft := t.sess.histIdx >= len(t.hist.entries)
	t.hist.add(line)
	if atDraft || t.sess.histIdx > len(t.hist.entries) {
		t.sess.histIdx = len(t.hist.entries)
	}
}

// SetInput replaces the editor buffer with text (cursor at the end) and
// redraws.
func (t *TUI) SetInput(text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sess.set(text)
	t.drawLocked()
}

// Input returns the editor buffer.
func (t *TUI) Input() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sess.buf.String()
}

// SetBusy tells the editor the agent is working: Alt+Enter then sends the
// buffer as EvFollowUp instead of inserting a newline.
func (t *TUI) SetBusy(busy bool) {
	t.mu.Lock()
	t.busy = busy
	t.mu.Unlock()
}

// Pick shows a filterable list in the footer in place of the editor and
// blocks until the user selects (index into items) or cancels (Esc/Ctrl+C:
// ErrCanceled). Typing filters, Up/Down/Tab move, Enter selects. It starts on
// selected with query prefilled. The editor buffer is kept. Pick must not be
// called from an Options callback.
func (t *TUI) Pick(title string, items []PickItem, query string, selected int) (int, error) {
	r, err := t.PickWith(items, PickOptions{Title: title, Query: query, Selected: selected})
	return r.Index, err
}

// Ask shows a one-line text field (prefilled with initial) in the picker's
// place and returns what the user typed when they press Enter.
func (t *TUI) Ask(title, initial string) (string, error) {
	t.mu.Lock()
	if !t.live() || t.pick != nil {
		t.mu.Unlock()
		return "", errors.New("prompt unavailable")
	}
	ps := &pickState{picker: picker{query: []rune(initial)}, title: title, res: make(chan pickResult, 1), input: true}
	t.pick = ps
	t.drawLocked()
	t.mu.Unlock()
	select {
	case r := <-ps.res:
		return string(ps.query), r.err
	case <-t.done:
		return "", ErrCanceled
	}
}

// PickWith is Pick with extra actions (see PickOptions). Esc and Ctrl+C
// still return ErrCanceled.
func (t *TUI) PickWith(items []PickItem, o PickOptions) (PickResult, error) {
	if len(items) == 0 {
		return PickResult{Index: -1}, errors.New("nothing to pick from")
	}
	t.mu.Lock()
	if !t.live() || t.pick != nil {
		t.mu.Unlock()
		return PickResult{Index: -1}, errors.New("picker unavailable")
	}
	ps := &pickState{
		picker:   picker{items: items, query: []rune(o.Query)},
		title:    o.Title,
		selected: o.Selected,
		res:      make(chan pickResult, 1),
		del:      o.Delete,
		tab:      o.Tab,
		rename:   o.Rename,
		sort:     o.Sort,
	}
	ps.refilter(o.Selected)
	t.pick = ps
	t.drawLocked()
	t.mu.Unlock()
	select {
	case r := <-ps.res:
		return PickResult{Index: r.idx, Action: r.action, Query: string(ps.query)}, r.err
	case <-t.done:
		return PickResult{Index: -1}, ErrCanceled
	}
}

// Suspend erases the footer, restores the original terminal mode and stops
// reading input so a child process can use the terminal. resume re-enters
// raw mode and redraws.
func (t *TUI) Suspend() (resume func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.live() {
		return func() {}
	}
	b := t.eraseLocked(t.buf[:0])
	b = append(b, termOff...)
	b = append(b, showCursor...)
	t.cursorHidden = false
	t.flush(b)
	t.suspended = true
	t.inMu.Lock()
	t.paused = true
	t.inMu.Unlock()
	if t.oldState != nil {
		_ = term.Restore(t.inFd, t.oldState)
	}
	var once sync.Once
	return func() {
		once.Do(t.resume)
	}
}

func (t *TUI) resume() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || !t.suspended {
		return
	}
	if t.oldState != nil {
		_, _ = term.MakeRaw(t.inFd)
		t.width, t.height = termSize(t.outFd)
	}
	t.suspended = false
	t.inMu.Lock()
	t.paused = false
	t.inCond.Broadcast()
	t.inMu.Unlock()
	t.flush(append(t.buf[:0], termOn...))
	t.drawLocked()
}

// Close erases the footer, leaves the cursor on a fresh line after the
// content and restores the terminal. It is idempotent.
func (t *TUI) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	wasSuspended := t.suspended
	var b []byte
	if !wasSuspended {
		b = t.eraseLocked(t.buf[:0])
	} else {
		b = t.buf[:0]
	}
	t.closed = true
	if t.started && !wasSuspended {
		b = append(b, termOff...)
		b = append(b, showCursor...)
	}
	t.flush(b)
	if t.drawTimer != nil {
		t.drawTimer.Stop()
	}
	if t.tickTimer != nil {
		t.tickTimer.Stop()
	}
	t.pick = nil
	if t.oldState != nil && !wasSuspended {
		_ = term.Restore(t.inFd, t.oldState)
	}
	t.mu.Unlock()

	close(t.done)
	if t.winch != nil {
		signal.Stop(t.winch)
	}
	t.inMu.Lock()
	t.inClosed = true
	t.inCond.Broadcast()
	t.inMu.Unlock()
	if t.wakeW > 0 {
		_, _ = unix.Write(t.wakeW, []byte{0})
	}
	t.wg.Wait()
	if t.wakeW > 0 {
		unix.Close(t.wakeR)
		unix.Close(t.wakeW)
	}
	close(t.events)
}

func (t *TUI) winchLoop() {
	defer t.wg.Done()
	for {
		select {
		case <-t.done:
			return
		case <-t.winch:
			w, h := termSize(t.outFd)
			t.mu.Lock()
			old := t.width
			t.resizeLocked(w, h)
			changed := t.width != old
			t.mu.Unlock()
			if changed {
				// Content already printed was laid out for the old width; let
				// the app re-render it. Dropped when a resize is already queued.
				select {
				case t.events <- Event{Kind: EvResize}:
				default:
				}
			}
		}
	}
}

// resizeLocked adapts to a new terminal size. Terminals that reflow wrap the
// old footer lines at the new width; the climb back to the footer top
// assumes that. Partial content lines continue on a fresh line.
func (t *TUI) resizeLocked(w, h int) {
	w, h = max(w, 1), max(h, 1)
	if w == t.width && h == t.height {
		return
	}
	if !t.live() {
		t.width, t.height = w, h
		return
	}
	b := append(t.buf[:0], syncBegin...)
	if t.drawn {
		up := 0
		cur := t.cursorDown - t.footerStart
		for i := range min(cur, len(t.lines)) {
			lw := visibleWidth(t.lines[i])
			up += max((lw+w-1)/w, 1)
		}
		up += t.curCol / w
		b = appendCSI(b, up, 'A')
		b = append(b, "\r\x1b[J"...)
		if t.footerStart == 1 && t.col == 0 {
			b = append(b, "\x1b[A"...)
		}
		t.drawn = false
	} else if t.col > 0 {
		b = append(b, '\r', '\n')
	}
	t.col = 0
	t.width, t.height = w, h
	b = append(b, '\r', '\n')
	t.footerStart = 1
	b = t.appendFooter(b)
	b = append(b, syncEnd...)
	t.flush(b)
}

// inputLoop decodes keys from r and dispatches them until r fails or the TUI
// closes.
func (t *TUI) inputLoop(r *bufio.Reader) {
	kp := &keyParser{r: r}
	for {
		var k key
		peek, err := r.Peek(1)
		if err == nil {
			// A lone ESC (nothing queued behind it) is Escape, not an Alt prefix.
			if peek[0] == 0x1b && r.Buffered() == 1 {
				_, _ = r.ReadByte()
				k = key{kind: keyEscape}
			} else {
				k, err = kp.next()
			}
		}
		if err != nil {
			select {
			case <-t.done:
			case t.events <- Event{Kind: EvEOF}:
			}
			return
		}
		switch {
		case k.kind == keyCtrlV:
			k = t.ctrlV()
		case k.kind == keyPaste && t.opts.Paste != nil && k.text != "":
			k.text = t.opts.Paste(k.text) // without the lock: it may load files
		}
		if ev, ok := t.dispatch(k, r.Buffered() > 0); ok {
			select {
			case t.events <- ev:
			case <-t.done:
				return
			}
		}
	}
}

// ctrlV runs the Ctrl+V hook without holding the lock and turns its text into
// a paste.
func (t *TUI) ctrlV() key {
	if t.opts.CtrlV == nil {
		return key{}
	}
	return key{kind: keyPaste, text: normalizePaste(t.opts.CtrlV())}
}

// dispatch applies k to the picker or editor. more reports queued input, in
// which case the redraw is deferred to the last queued key.
func (t *TUI) dispatch(k key, more bool) (Event, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return Event{}, false
	}
	if ps := t.pick; ps != nil {
		if res, done := ps.handle(k); done {
			t.pick = nil
			ps.res <- res
		}
		t.dirty = true
		t.drawIfIdle(more)
		return Event{}, false
	}
	if k.kind == keyEscape && !t.sess.sug.visible() {
		return Event{Kind: EvEscape}, true
	}
	if k.kind == keyLeft && len(t.sess.buf.text) == 0 && !t.sess.sug.visible() {
		return Event{Kind: EvLeft}, true
	}
	if k.kind == keyAltUp {
		return Event{Kind: EvDequeue, Text: t.sess.buf.String()}, true
	}
	if k.kind == keyBackTab && !t.sess.sug.visible() {
		return Event{Kind: EvBackTab}, true
	}
	if k.kind == keyAltEnter {
		if t.busy && strings.TrimSpace(t.sess.buf.String()) != "" {
			ev := Event{Kind: EvFollowUp, Text: t.sess.buf.String()}
			t.sess.reset()
			t.dirty = true
			t.drawIfIdle(false)
			return ev, true
		}
		k.kind = keyNewline
	}
	var ev Event
	ok := false
	switch t.sess.handle(k) {
	case actNone:
	case actRedraw:
		t.dirty = true
	case actClearScreen:
		if t.live() {
			t.flush(append(t.buf[:0], "\x1b[H\x1b[2J"...))
			t.col, t.drawn = 0, false
			t.drawLocked()
			t.dirty = false
		}
	case actSubmit:
		ev, ok = Event{Kind: EvSubmit, Text: t.sess.buf.String()}, true
		t.sess.reset()
		t.dirty = true
		more = false
	case actInterrupt:
		ev, ok = Event{Kind: EvInterrupt}, true
	case actEOF:
		ev, ok = Event{Kind: EvEOF}, true
	}
	t.drawIfIdle(more)
	return ev, ok
}

func (t *TUI) drawIfIdle(more bool) {
	if t.dirty && !more {
		t.dirty = false
		t.drawLocked()
	}
}

// ttyReader reads stdin for the input goroutine. It waits for readability
// with select(2) on stdin and a wake pipe and only reads while not paused,
// so a suspended TUI never consumes bytes meant for a child process.
type ttyReader struct{ t *TUI }

func (r ttyReader) Read(p []byte) (int, error) {
	t := r.t
	for {
		t.inMu.Lock()
		for t.paused && !t.inClosed {
			t.inCond.Wait()
		}
		closed := t.inClosed
		t.inMu.Unlock()
		if closed {
			return 0, io.EOF
		}
		ready, err := waitReadable(t.inFd, t.wakeR)
		if err != nil {
			return 0, err
		}
		if !ready {
			continue
		}
		t.inMu.Lock()
		if t.paused || t.inClosed {
			t.inMu.Unlock()
			continue
		}
		n, err := unix.Read(t.inFd, p)
		t.inMu.Unlock()
		switch {
		case err == unix.EINTR || err == unix.EAGAIN:
			continue
		case err != nil:
			return 0, err
		case n == 0:
			return 0, io.EOF
		}
		return n, nil
	}
}

// waitReadable blocks until fd is readable (true) or the wake pipe fired
// (false, pipe drained).
func waitReadable(fd, wake int) (bool, error) {
	for {
		var rs unix.FdSet
		rs.Set(fd)
		rs.Set(wake)
		_, err := unix.Select(max(fd, wake)+1, &rs, nil, nil, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, err
		}
		if rs.IsSet(wake) {
			var buf [64]byte
			for {
				if n, _ := unix.Read(wake, buf[:]); n <= 0 {
					break
				}
			}
			return false, nil
		}
		return rs.IsSet(fd), nil
	}
}
