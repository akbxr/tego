package lineedit

import (
	"bufio"
	"bytes"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// emu is a tiny terminal emulator: \r, \n, CUU/CUD/CUF/CUB/CHA/CUP, ED, EL,
// soft wrap with pending-wrap state and scrolling. SGR, OSC and private modes
// are ignored. Rows are kept for the whole session (scrollback included).
type emu struct {
	w, h     int
	rows     [][]rune
	row, col int
	top      int // first visible row
	pending  bool
	onlcr    bool // translate lone \n to \r\n (cooked output)
}

func newEmu(w, h int) *emu { return &emu{w: w, h: h, rows: [][]rune{nil}} }

func (e *emu) line() []rune {
	for len(e.rows) <= e.row {
		e.rows = append(e.rows, nil)
	}
	return e.rows[e.row]
}

func (e *emu) lf() {
	e.row++
	e.line()
	if e.row >= e.top+e.h {
		e.top = e.row - e.h + 1
	}
}

func (e *emu) put(r rune, rw int) {
	if e.pending {
		e.col = 0
		e.lf()
		e.pending = false
	}
	if e.col+rw > e.w {
		e.col = 0
		e.lf()
	}
	l := e.line()
	for len(l) < e.col+rw {
		l = append(l, ' ')
	}
	l[e.col] = r
	if rw == 2 {
		l[e.col+1] = 0
	}
	e.rows[e.row] = l
	e.col += rw
	if e.col >= e.w {
		e.col = e.w - 1
		e.pending = true
	}
}

func (e *emu) eraseLine(from int) {
	l := e.line()
	if from < len(l) {
		e.rows[e.row] = l[:from]
	}
}

func (e *emu) feed(b []byte) {
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0x1b:
			end, _ := escEnd(b, i)
			e.esc(b[i:end])
			i = end
			continue
		case c == '\r':
			e.col, e.pending = 0, false
		case c == '\n':
			if e.onlcr {
				e.col = 0
			}
			e.pending = false
			e.lf()
		case c == '\t':
			e.pending = false
			e.col = min((e.col/8+1)*8, e.w-1)
		case c < 0x20:
		default:
			r, size := utf8.DecodeRune(b[i:])
			e.put(r, widthCond.RuneWidth(r))
			i += size
			continue
		}
		i++
	}
}

func (e *emu) esc(seq []byte) {
	if len(seq) < 3 || seq[1] != '[' {
		return
	}
	params := seq[2 : len(seq)-1]
	if len(params) > 0 && params[0] == '?' {
		return
	}
	n1, n2 := csiArgs(params)
	n := max(n1, 1)
	switch seq[len(seq)-1] {
	case 'A':
		e.row = max(e.row-n, e.top)
	case 'B':
		e.row = min(e.row+n, e.top+e.h-1)
		e.line()
	case 'C':
		e.col = min(e.col+n, e.w-1)
	case 'D':
		e.col = max(e.col-n, 0)
	case 'G':
		e.col = min(n, e.w) - 1
	case 'H':
		e.row = e.top + max(n1, 1) - 1
		e.col = max(n2, 1) - 1
		e.line()
	case 'J':
		if n1 == 2 {
			for r := e.top; r < len(e.rows); r++ {
				e.rows[r] = nil
			}
			break
		}
		e.eraseLine(e.col)
		if len(e.rows) > e.row+1 {
			e.rows = e.rows[:e.row+1]
		}
	case 'K':
		e.eraseLine(e.col)
	default:
		return
	}
	e.pending = false
}

func (e *emu) text(r int) string {
	if r >= len(e.rows) {
		return ""
	}
	var sb strings.Builder
	for _, c := range e.rows[r] {
		if c != 0 {
			sb.WriteRune(c)
		}
	}
	return strings.TrimRight(sb.String(), " ")
}

// all returns every row (scrollback included), trailing empty rows trimmed.
func (e *emu) all() []string {
	out := make([]string, len(e.rows))
	for i := range e.rows {
		out[i] = e.text(i)
	}
	for len(out) > 0 && out[len(out)-1] == "" && len(out)-1 > e.row {
		out = out[:len(out)-1]
	}
	return out
}

// screen returns the visible rows.
func (e *emu) screen() []string {
	out := make([]string, e.h)
	for i := range out {
		out[i] = e.text(e.top + i)
	}
	return out
}

// testTUI is a started TUI drawing into an in-memory buffer.
type testTUI struct {
	*TUI
	out *syncBuf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}

func newTestTUI(o Options, w, h int) *testTUI {
	if o.Prompt == "" {
		o.Prompt = "> "
	}
	out := &syncBuf{}
	t := newTUI(o, out, w, h)
	t.color = false
	t.mu.Lock()
	t.started = true
	t.drawLocked()
	t.mu.Unlock()
	return &testTUI{TUI: t, out: out}
}

func (tt *testTUI) emu() *emu {
	e := newEmu(tt.width, tt.height)
	e.feed(tt.out.bytes())
	return e
}

// feed runs the input loop over input and returns the events, without the
// trailing end-of-input EvEOF.
func (tt *testTUI) feed(input string) []Event {
	done := make(chan struct{})
	go func() {
		tt.inputLoop(bufio.NewReader(strings.NewReader(input)))
		close(done)
	}()
	var evs []Event
	for {
		select {
		case ev := <-tt.events:
			evs = append(evs, ev)
		case <-done:
			for {
				select {
				case ev := <-tt.events:
					evs = append(evs, ev)
				default:
					return evs[:len(evs)-1]
				}
			}
		}
	}
}

func statusOpts() Options {
	return Options{Status: func(int) []string { return []string{"-- status --"} }}
}

// checkLayout asserts the session rows are exactly the reference content
// rows followed by the footer.
func checkLayout(t *testing.T, tt *testTUI, content string, footer []string) {
	t.Helper()
	ref := newEmu(tt.width, 1000)
	ref.onlcr = true
	ref.feed([]byte(content))
	want := append(ref.all(), footer...)
	got := tt.emu().all()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("screen mismatch\n got: %q\nwant: %q", got, want)
	}
}

const sampleContent = "hello \x1b[1mworld\x1b[0m\n" +
	"tab\tstop 中文 wide\n" +
	"link \x1b]8;;https://example.com\x1b\\here\x1b]8;;\x1b\\ end\n" +
	"exactly-twenty-cols!" + "after\n" +
	"exactly-twenty-cols!" + "\n" +
	"a line that is much longer than twenty columns wraps\n" +
	"partial" + " more" +
	"\r\x1b[1A\x1b[Jrewritten line\nnew partial" +
	"\r\x1b[J" + "streamed tail\n" +
	"exactly-twenty-cols!" + "\r\x1b[1A\x1b[J" + "redrawn after full\n" +
	"end"

func TestContentAboveFooterChunked(t *testing.T) {
	footer := []string{">", "-- status --"}
	for seed := int64(0); seed < 40; seed++ {
		tt := newTestTUI(statusOpts(), 20, 200)
		rng := rand.New(rand.NewPCG(uint64(seed), 1))
		data := []byte(sampleContent)
		for len(data) > 0 {
			n := 1 + rng.IntN(7)
			if seed == 0 {
				n = len(data) // one write
			}
			n = min(n, len(data))
			_, _ = tt.Writer().Write(data[:n])
			data = data[n:]
		}
		checkLayout(t, tt, sampleContent, footer)
	}
}

func TestContentScrollsFooterStaysAtBottom(t *testing.T) {
	tt := newTestTUI(statusOpts(), 20, 6)
	var sb strings.Builder
	for i := range 30 {
		sb.WriteString("row ")
		sb.WriteByte(byte('a' + i%26))
		sb.WriteString("\n")
	}
	sb.WriteString("tail")
	for _, ch := range []byte(sb.String()) {
		_, _ = tt.Writer().Write([]byte{ch})
	}
	tt.feed("typed")
	got := tt.emu().screen()
	want := []string{"row b", "row c", "row d", "tail", "> typed", "-- status --"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("screen %q want %q", got, want)
	}
	e := tt.emu()
	if e.row-e.top != 4 || e.col != 7 {
		t.Fatalf("cursor at (%d,%d), want editor cursor (4,7)", e.row-e.top, e.col)
	}
}

func TestExactWidthPendingWrap(t *testing.T) {
	footer := []string{">", "-- statu…"} // status truncated to width-1
	cases := [][]string{
		{"0123456789"},
		{"0123456789", "abc"},
		{"0123456789", "\n", "x"},
		{"0123456789\n"},
		{"0123456789", "\r\n"},
		{"0123456789", "\r", "\n"},
		{"0123456789", "\x1b[0m", "z"},
		{"0123456789", "\r\x1b[J", "short"},
		{"01234567中", "y"},
		{"012345678中"},
		{"0123456789", "\x1b[1A\x1b[J"},
		{"x\n0123456789012345678", "9", "\r\x1b[1A\x1b[Jdone\n"},
	}
	for _, chunks := range cases {
		tt := newTestTUI(statusOpts(), 10, 50)
		for _, c := range chunks {
			_, _ = tt.Writer().Write([]byte(c))
			// Interleaved footer redraws must not disturb the pending state.
			tt.mu.Lock()
			tt.drawLocked()
			tt.mu.Unlock()
		}
		t.Run(strings.Join(chunks, "+"), func(t *testing.T) {
			checkLayout(t, tt, strings.Join(chunks, ""), footer)
		})
	}
}

func TestMultiRowEditorAndContent(t *testing.T) {
	tt := newTestTUI(Options{
		Header: func(int) string {
			return "\x1b[2m── tego ──────────────────────────\x1b[0m"
		},
		Status: func(int) []string { return []string{"rule", "model · 12%"} },
	}, 20, 40)
	tt.feed("first\nsecond line that wraps around\x1b[A")
	// Cursor moved up to the first line (column 5).
	e := tt.emu()
	footer := []string{"── tego ──────────…", "> first", "  second line that w", "raps around", "rule", "model · 12%"}
	if got := e.all(); strings.Join(got[1:], "|") != strings.Join(footer, "|") {
		t.Fatalf("footer %q want %q", got, footer)
	}
	if e.row != 2 || e.col != 7 {
		t.Fatalf("cursor (%d,%d) want (2,7)", e.row, e.col)
	}
	content := "streamed \x1b[1mbold\x1b[0m\nmore"
	for _, ch := range []byte(content) {
		_, _ = tt.Writer().Write([]byte{ch})
	}
	checkLayout(t, tt, content, footer)
	e = tt.emu()
	if e.row != 3 || e.col != 7 {
		t.Fatalf("cursor after write (%d,%d) want (3,7)", e.row, e.col)
	}
}

func TestPickerInFooter(t *testing.T) {
	tt := newTestTUI(statusOpts(), 50, 30)
	tt.feed("draft text")
	items := []PickItem{{Label: "alpha"}, {Label: "beta", Detail: "current"}, {Label: "gamma"}}
	type res struct {
		idx int
		err error
	}
	ch := make(chan res, 1)
	go func() {
		idx, err := tt.Pick("Choose", items, "", 1)
		ch <- res{idx, err}
	}()
	waitPick(t, tt)
	// Content keeps streaming while the picker is open.
	_, _ = tt.Writer().Write([]byte("streaming\n"))
	got := tt.emu().all()
	want := []string{"streaming", "", "Choose", "›", "  alpha", "❯ beta  current", "  gamma",
		"  3/3 · ↑↓ move · enter select · esc cancel", "-- status --"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("picker footer %q\nwant %q", got, want)
	}
	if !bytes.Contains(tt.out.bytes(), []byte(hideCursor)) {
		t.Fatal("cursor not hidden while picking")
	}
	tt.feed("gam\r")
	r := <-ch
	if r.err != nil || r.idx != 2 {
		t.Fatalf("pick = %d, %v", r.idx, r.err)
	}
	checkLayout(t, tt, "streaming\n", []string{"> draft text", "-- status --"})
	if !bytes.HasSuffix(tt.out.bytes(), []byte(showCursor+syncEnd)) {
		t.Fatal("cursor not shown after picker closed")
	}

	// Esc cancels, the buffer still holds the draft.
	go func() {
		idx, err := tt.Pick("Choose", items, "x", 0)
		ch <- res{idx, err}
	}()
	waitPick(t, tt)
	tt.feed("\x1b")
	if r := <-ch; r.err != ErrCanceled {
		t.Fatalf("esc: %v", r.err)
	}
	if evs := tt.feed("\r"); len(evs) != 1 || evs[0].Text != "draft text" {
		t.Fatalf("buffer after picker: %+v", evs)
	}
}

func waitPick(t *testing.T, tt *testTUI) {
	t.Helper()
	for range 1000 {
		tt.mu.Lock()
		open := tt.pick != nil
		tt.mu.Unlock()
		if open {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("picker did not open")
}

func TestTruncateANSI(t *testing.T) {
	tests := []struct {
		s    string
		w    int
		want string
	}{
		{"short", 10, "short"},
		{"\x1b[1mexact\x1b[0m", 5, "\x1b[1mexact\x1b[0m"},
		{"abcdefgh", 5, "abcd…\x1b[0m"},
		{"\x1b[31mred\x1b[0m and more", 6, "\x1b[31mred\x1b[0m a…\x1b[0m"},
		{"中文字符", 5, "中文…\x1b[0m"},
		{"\x1b]8;;http://x\x1b\\linktext\x1b]8;;\x1b\\", 4, "\x1b]8;;http://x\x1b\\lin…\x1b]8;;\x1b\\\x1b[0m"},
		{"\x1b]8;;http://x\x07ab\x1b]8;;\x07cdefg", 4, "\x1b]8;;http://x\x07ab\x1b]8;;\x07c…\x1b[0m"},
		{"abc", 0, ""},
	}
	for _, tc := range tests {
		got := truncateANSI(tc.s, tc.w)
		if got != tc.want {
			t.Errorf("truncateANSI(%q, %d) = %q, want %q", tc.s, tc.w, got, tc.want)
		}
		if tc.w > 0 && visibleWidth(got) > tc.w {
			t.Errorf("truncateANSI(%q, %d) too wide: %d", tc.s, tc.w, visibleWidth(got))
		}
	}
}

func TestFooterLinesTruncatedToOneRow(t *testing.T) {
	long := strings.Repeat("=", 50)
	tt := newTestTUI(Options{
		Activity: func(int) string { return "⠋ working " + long },
		Header:   func(int) string { return long },
		Status:   func(int) []string { return []string{long, "ok"} },
	}, 20, 30)
	defer tt.Close()
	got := tt.emu().all()
	want := []string{"", "⠋ working ========…", strings.Repeat("=", 18) + "…", ">", strings.Repeat("=", 18) + "…", "ok"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("footer %q want %q", got, want)
	}
	if tt.ContentHeight() != 30-5 {
		t.Fatalf("ContentHeight = %d", tt.ContentHeight())
	}
	tt.mu.Lock()
	armed := tt.tickArmed
	tt.mu.Unlock()
	if !armed {
		t.Fatal("activity ticker not armed")
	}
}

func TestEvents(t *testing.T) {
	tt := newTestTUI(statusOpts(), 40, 20)
	if evs := tt.feed("\x03"); len(evs) != 1 || evs[0].Kind != EvInterrupt {
		t.Fatalf("ctrl-c empty: %+v", evs)
	}
	if evs := tt.feed("junk\x03"); len(evs) != 0 {
		t.Fatalf("ctrl-c non-empty must only clear: %+v", evs)
	}
	if evs := tt.feed("ok\r"); len(evs) != 1 || evs[0] != (Event{Kind: EvSubmit, Text: "ok"}) {
		t.Fatalf("submit after clear: %+v", evs)
	}
	if got := tt.emu().all(); got[len(got)-2] != ">" {
		t.Fatalf("buffer not cleared after submit: %q", got)
	}
	if evs := tt.feed("\x1b"); len(evs) != 1 || evs[0].Kind != EvEscape {
		t.Fatalf("lone esc: %+v", evs)
	}
	if evs := tt.feed("foo bar\x1bbx\r"); len(evs) != 1 || evs[0].Text != "foo xbar" {
		t.Fatalf("alt-b is not escape: %+v", evs)
	}
	if evs := tt.feed("\x04"); len(evs) != 1 || evs[0].Kind != EvEOF {
		t.Fatalf("ctrl-d empty: %+v", evs)
	}
	if evs := tt.feed("\r"); len(evs) != 1 || evs[0] != (Event{Kind: EvSubmit}) {
		t.Fatalf("empty submit: %+v", evs)
	}
}

func TestClearScreen(t *testing.T) {
	tt := newTestTUI(statusOpts(), 20, 10)
	_, _ = tt.Writer().Write([]byte("old content\nmore"))
	tt.feed("ab\x0c")
	got := tt.emu().screen()
	want := []string{"", "> ab", "-- status --", "", "", "", "", "", "", ""}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("after ctrl-l %q", got)
	}
	_, _ = tt.Writer().Write([]byte("fresh\n"))
	if got := tt.emu().screen(); got[0] != "fresh" || got[2] != "> ab" {
		t.Fatalf("content after clear %q", got)
	}
}

func TestCloseLeavesFreshLine(t *testing.T) {
	tt := newTestTUI(statusOpts(), 20, 10)
	_, _ = tt.Writer().Write([]byte("partial"))
	tt.Close()
	tt.Close()
	e := tt.emu()
	if got := e.all(); strings.Join(got, "|") != "partial|" || e.row != 1 || e.col != 0 {
		t.Fatalf("after close %q cursor (%d,%d)", got, e.row, e.col)
	}
	if _, ok := <-tt.Events(); ok {
		t.Fatal("events not closed")
	}
}

func TestSuspendResume(t *testing.T) {
	tt := newTestTUI(statusOpts(), 20, 10)
	tt.feed("keep")
	_, _ = tt.Writer().Write([]byte("before"))
	resume := tt.Suspend()
	_, _ = tt.Writer().Write([]byte("while suspended\n"))
	if got := tt.emu().all(); strings.Join(got, "|") != "before|while suspended|" {
		t.Fatalf("suspended screen %q", got)
	}
	tt.Redraw()
	resume()
	resume()
	checkLayout(t, tt, "before\nwhile suspended\n", []string{"> keep", "-- status --"})
}

func TestKeyboardModesFollowTheEditor(t *testing.T) {
	tt := newTestTUI(statusOpts(), 20, 10)
	tt.started = true
	start := len(tt.out.bytes())
	resume := tt.Suspend()
	if out := string(tt.out.bytes()[start:]); !strings.Contains(out, keysOff) || strings.Contains(out, keysOn) {
		t.Fatalf("suspend must hand the terminal back in its normal key mode: %q", out)
	}
	start = len(tt.out.bytes())
	resume()
	if out := string(tt.out.bytes()[start:]); !strings.Contains(out, keysOn) {
		t.Fatalf("resume must turn Shift+Enter reporting back on: %q", out)
	}
	start = len(tt.out.bytes())
	tt.Close()
	if out := string(tt.out.bytes()[start:]); !strings.Contains(out, keysOff) {
		t.Fatalf("close must restore the key mode: %q", out)
	}
}

func TestRedrawCoalesces(t *testing.T) {
	tt := newTestTUI(statusOpts(), 20, 10)
	before := bytes.Count(tt.out.bytes(), []byte(syncBegin))
	for range 10 {
		tt.Redraw()
	}
	time.Sleep(4 * frameInterval)
	if n := bytes.Count(tt.out.bytes(), []byte(syncBegin)) - before; n < 1 || n > 2 {
		t.Fatalf("10 redraws drew %d frames", n)
	}
}

func TestPickWithActions(t *testing.T) {
	tt := newTestTUI(statusOpts(), 90, 30)
	items := []PickItem{{Label: "alpha"}, {Label: "beta"}, {Label: "gamma"}}
	type res struct {
		r   PickResult
		err error
	}
	ch := make(chan res, 1)
	open := func(o PickOptions) {
		go func() {
			r, err := tt.PickWith(items, o)
			ch <- res{r, err}
		}()
		waitPick(t, tt)
	}
	o := PickOptions{Title: "Sessions", Delete: true, Tab: "all projects"}

	open(o)
	if got := strings.Join(tt.emu().all(), "|"); !strings.Contains(got, "⌫/ctrl+d delete · tab all projects · esc cancel") {
		t.Fatalf("footer %q", got)
	}
	tt.feed("gam\x1b[3~") // Delete on the filtered row
	if r := <-ch; r.err != nil || r.r != (PickResult{Index: 2, Action: PickDelete, Query: "gam"}) {
		t.Fatalf("delete: %+v %v", r.r, r.err)
	}
	open(o)
	tt.feed("\x1b[B\x04") // Down, then Ctrl+D
	if r := <-ch; r.r.Index != 1 || r.r.Action != PickDelete {
		t.Fatalf("ctrl+d: %+v", r.r)
	}
	open(o)
	tt.feed("x\x1b\x7f") // Alt+Backspace with a filter clears the filter only
	tt.feed("\x1b\x7f")  // on an empty filter it deletes, like pi's Ctrl+Backspace
	if r := <-ch; r.r != (PickResult{Index: 0, Action: PickDelete}) {
		t.Fatalf("alt+backspace: %+v", r.r)
	}
	open(o)
	tt.feed("ab\x7f\x7f") // Backspace erases the filter first ...
	tt.feed("\x7f")       // ... then, on an empty filter, deletes (the Mac "delete" key)
	if r := <-ch; r.r != (PickResult{Index: 0, Action: PickDelete}) {
		t.Fatalf("backspace: %+v", r.r)
	}
	open(o)
	tt.feed("\x15") // Cmd+Backspace (Ctrl+U) on an empty filter
	if r := <-ch; r.r.Action != PickDelete {
		t.Fatalf("cmd+backspace: %+v", r.r)
	}
	open(o)
	tt.feed("\t")
	if r := <-ch; r.r != (PickResult{Index: 0, Action: PickTab}) {
		t.Fatalf("tab: %+v", r.r)
	}
	open(o)
	tt.feed("zzz\x1b[3~\r\x1b") // nothing to delete or select: only Esc closes it
	if r := <-ch; r.err != ErrCanceled {
		t.Fatalf("empty filter: %+v %v", r.r, r.err)
	}

	// Plain Pick keeps its keys: Tab moves down and Ctrl+D cancels.
	idx := make(chan int, 1)
	go func() { i, _ := tt.Pick("Choose", items, "", 0); idx <- i }()
	waitPick(t, tt)
	tt.feed("\t\r")
	if i := <-idx; i != 1 {
		t.Fatalf("tab in Pick selected %d", i)
	}
	go func() { i, _ := tt.Pick("Choose", items, "", 0); idx <- i }()
	waitPick(t, tt)
	tt.feed("\x04")
	if i := <-idx; i != -1 {
		t.Fatalf("ctrl+d in Pick = %d", i)
	}
}

func TestPickRenameSortAndAsk(t *testing.T) {
	tt := newTestTUI(statusOpts(), 120, 30)
	items := []PickItem{{Label: "alpha"}, {Label: "beta"}}
	ch := make(chan PickResult, 1)
	open := func() {
		go func() { r, _ := tt.PickWith(items, PickOptions{Title: "S", Rename: true, Sort: "recent"}); ch <- r }()
		waitPick(t, tt)
	}
	open()
	if got := strings.Join(tt.emu().all(), "|"); !strings.Contains(got, "ctrl+r rename · ctrl+s sort: recent") {
		t.Fatalf("footer %q", got)
	}
	tt.feed("\x1b[B\x12") // Down, Ctrl+R
	if r := <-ch; r.Action != PickRename || r.Index != 1 {
		t.Fatalf("ctrl+r: %+v", r)
	}
	open()
	tt.feed("\x13") // Ctrl+S
	if r := <-ch; r.Action != PickSort || r.Index != 0 {
		t.Fatalf("ctrl+s: %+v", r)
	}

	type ans struct {
		s   string
		err error
	}
	ask := make(chan ans, 1)
	go func() { s, err := tt.Ask("Rename session", "old name"); ask <- ans{s, err} }()
	waitPick(t, tt)
	if got := strings.Join(tt.emu().all(), "|"); !strings.Contains(got, "Rename session") || !strings.Contains(got, "old name") || !strings.Contains(got, "enter save") {
		t.Fatalf("ask screen %q", got)
	}
	tt.feed("\x7f\x7f\x7f\x7f" + "fresh\r") // erase "name", type, Enter
	if a := <-ask; a.err != nil || a.s != "old fresh" {
		t.Fatalf("ask = %+v", a)
	}
	go func() { s, err := tt.Ask("Rename session", "x"); ask <- ans{s, err} }()
	waitPick(t, tt)
	tt.feed("\x1b")
	if a := <-ask; a.err != ErrCanceled {
		t.Fatalf("esc: %+v", a)
	}
}
