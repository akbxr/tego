package lineedit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bufAt(s string) *buffer {
	i := strings.IndexByte(s, '|')
	b := &buffer{}
	b.set(s[:i] + s[i+1:])
	b.pos = len([]rune(s[:i]))
	return b
}

func show(b *buffer) string {
	return string(b.text[:b.pos]) + "|" + string(b.text[b.pos:])
}

func TestWordOps(t *testing.T) {
	tests := []struct {
		in   string
		op   func(*buffer)
		want string
	}{
		{"foo bar|", (*buffer).killWordBack, "foo |"},
		{"foo bar  |", (*buffer).killWordBack, "foo |"},
		{"foo.bar|", (*buffer).killWordBack, "foo.|"},
		{"foo...|", (*buffer).killWordBack, "foo|"},
		{"|foo", (*buffer).killWordBack, "|foo"},
		{"a\n  b|", (*buffer).killWordBack, "a\n  |"},
		{"|  foo bar", (*buffer).killWordForward, "| bar"},
		{"héllo wörld|", (*buffer).wordLeft, "héllo |wörld"},
		{"|héllo wörld", (*buffer).wordRight, "héllo| wörld"},
		{"x = foo_bar|", (*buffer).wordLeft, "x = |foo_bar"},
	}
	for _, tt := range tests {
		b := bufAt(tt.in)
		tt.op(b)
		if got := show(b); got != tt.want {
			t.Errorf("%q: got %q want %q", tt.in, got, tt.want)
		}
	}
}

func TestKillLine(t *testing.T) {
	b := bufAt("one\ntw|o")
	b.killLineStart()
	if got := show(b); got != "one\n|o" {
		t.Fatalf("killLineStart: %q", got)
	}
	b.killLineStart() // at line start: join with previous line
	if got := show(b); got != "one|o" {
		t.Fatalf("killLineStart join: %q", got)
	}
	b = bufAt("o|ne\ntwo")
	b.killLineEnd()
	if got := show(b); got != "o|\ntwo" {
		t.Fatalf("killLineEnd: %q", got)
	}
	b.killLineEnd()
	if got := show(b); got != "o|two" {
		t.Fatalf("killLineEnd join: %q", got)
	}
}

func TestLineNavigation(t *testing.T) {
	b := bufAt("abcdef\nxy\n中文字|")
	if !b.up() || show(b) != "abcdef\nxy|\n中文字" {
		t.Fatalf("up clamps to shorter line: %q", show(b))
	}
	if !b.up() || show(b) != "ab|cdef\nxy\n中文字" {
		t.Fatalf("up keeps column: %q", show(b))
	}
	if b.up() {
		t.Fatal("up on first line must report false")
	}
	b.pos = 3 // column 3
	b.down()
	b.down()
	// Column 3 falls inside the second wide rune: stay before it.
	if show(b) != "abcdef\nxy\n中|文字" {
		t.Fatalf("down onto wide runes: %q", show(b))
	}
	if b.down() {
		t.Fatal("down on last line must report false")
	}
	b.home()
	if show(b) != "abcdef\nxy\n|中文字" {
		t.Fatalf("home: %q", show(b))
	}
	b.up()
	b.end()
	if show(b) != "abcdef\nxy|\n中文字" {
		t.Fatalf("end: %q", show(b))
	}
}

func TestNormalizePaste(t *testing.T) {
	got := normalizePaste("a\r\nb\rc\x1b[31md\x07\te\xff")
	if want := "a\nb\nc[31md\te"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestVisibleWidth(t *testing.T) {
	tests := []struct {
		s    string
		want int
	}{
		{"> ", 2},
		{"\x1b[1;32m> \x1b[0m", 2},
		{"\x1b]8;;http://x\x07link\x1b]8;;\x1b\\ ", 5},
		{"中>", 3},
		{"\x1b(Bab", 2},
	}
	for _, tt := range tests {
		if got := visibleWidth(tt.s); got != tt.want {
			t.Errorf("visibleWidth(%q) = %d, want %d", tt.s, got, tt.want)
		}
	}
}

func TestLayout(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		pos      int // -1 = end
		width    int
		rows     int
		row, col int
	}{
		{"fits", "abc", -1, 10, 1, 0, 5},
		{"wraps", "abcdefghi", -1, 10, 2, 1, 1},
		{"exactly full adds cursor row", "abcdefgh", -1, 10, 2, 1, 0},
		{"exactly full cursor inside", "abcdefgh", 3, 10, 1, 0, 5},
		{"wide rune wraps early", "abc中", -1, 5, 2, 1, 2},
		{"newline prefix", "ab\ncd", -1, 10, 2, 1, 4},
		{"full line then newline", "abcdefgh\nx", 8, 10, 3, 1, 0},
		{"cursor on wrapped rune", "abcdefghij", 9, 10, 2, 1, 1},
		{"tab", "\tx", -1, 10, 1, 0, 7},
	}
	for _, tt := range tests {
		buf := []rune(tt.text)
		pos := tt.pos
		if pos < 0 {
			pos = len(buf)
		}
		l := computeLayout("> ", "· ", 2, buf, pos, tt.width)
		if len(l.rows) != tt.rows || l.curRow != tt.row || l.curCol != tt.col {
			t.Errorf("%s: rows=%d cursor=(%d,%d), want rows=%d cursor=(%d,%d); %q",
				tt.name, len(l.rows), l.curRow, l.curCol, tt.rows, tt.row, tt.col, l.rows)
		}
	}
}

func TestLayoutRowText(t *testing.T) {
	l := computeLayout("> ", "· ", 2, []rune("abcdefghij\nk"), 0, 10)
	want := []string{"> abcdefgh", "ij", "· k"}
	if strings.Join(l.rows, "|") != strings.Join(want, "|") {
		t.Fatalf("rows %q want %q", l.rows, want)
	}
}

func TestContinuationPrefixWidth(t *testing.T) {
	for _, w := range []int{0, 1, 2, 5} {
		if got := visibleWidth(continuationPrefix(w)); got != w {
			t.Errorf("width %d: prefix width %d", w, got)
		}
	}
}

func submitted(t *testing.T, tt *testTUI, input string) string {
	t.Helper()
	evs := tt.feed(input)
	if len(evs) != 1 || evs[0].Kind != EvSubmit {
		t.Fatalf("%q: events %+v", input, evs)
	}
	return evs[0].Text
}

func TestEditKeys(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain", "hello\r", "hello"},
		{"bracketed paste", "x\x1b[200~line1\r\nline2\rline3\x1b[201~\r", "xline1\nline2\nline3"},
		{"alt enter", "a\x1b\rb\r", "a\nb"},
		{"ctrl j", "a\nb\r", "a\nb"},
		{"kitty shift enter", "a\x1b[13;2ub\r", "a\nb"},
		{"xterm shift enter", "a\x1b[27;2;13~b\r", "a\nb"},
		{"backslash enter", "a\\\rb\r", "a\nb"},
		{"utf8", "héllo 中\r", "héllo 中"},
		{"left insert", "ac\x1b[Db\r", "abc"},
		{"ss3 home", "bc\x1bOHa\r", "abc"},
		{"ctrl word left", "foo bar\x1b[1;5Dx\r", "foo xbar"},
		{"alt b", "foo bar\x1bbx\r", "foo xbar"},
		{"esc esc left", "foo bar\x1b\x1b[Dx\r", "foo xbar"},
		{"delete", "abc\x01\x1b[3~\r", "bc"},
		{"ctrl d deletes", "abc\x01\x04\r", "bc"},
		{"ctrl c clears", "junk\x03ok\r", "ok"},
		{"ctrl w", "foo bar\x17\r", "foo "},
		{"alt backspace", "foo bar\x1b\x7f\r", "foo "},
		{"unknown csi ignored", "a\x1b[99;99zb\x1b[<0;1;1Mc\r", "abc"},
		{"up within buffer", "ab\ncd\x1b[Ax\r", "abx\ncd"},
		{"ctrl e end", "ab\x01\x05c\r", "abc"},
		{"backspace", "abd\x7fc\r", "abc"},
		// The kitty protocol (flag 1) and modifyOtherKeys report Ctrl/Alt
		// keys and Escape as CSI sequences: they must act as before.
		{"kitty shift enter caps lock", "a\x1b[13;66ub\r", "a\nb"},
		{"kitty ctrl enter", "a\x1b[13;5ub\r", "a\nb"},
		{"kitty ctrl a e", "bc\x1b[97;5ua\x1b[101;5ud\r", "abcd"},
		{"kitty ctrl c clears", "junk\x1b[99;5uok\r", "ok"},
		{"kitty ctrl w", "foo bar\x1b[119;5u\r", "foo "},
		{"kitty ctrl u", "foo\x1b[117;5ux\r", "x"},
		{"kitty ctrl j", "a\x1b[106;5ub\r", "a\nb"},
		{"kitty alt b", "foo bar\x1b[98;3ux\r", "foo xbar"},
		{"kitty alt backspace", "foo bar\x1b[127;3u\r", "foo "},
		{"kitty shift letter", "\x1b[97;2u\r", "A"},
		{"xterm ctrl a", "bc\x1b[27;5;97~a\r", "abc"},
	}
	for _, tc := range tests {
		tt := newTestTUI(Options{}, 80, 24)
		if got := submitted(t, tt, tc.in); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestShiftTab(t *testing.T) {
	tt := newTestTUI(Options{}, 80, 24)
	for _, seq := range []string{"\x1b[Z", "\x1b[9;2u"} { // legacy, kitty
		if evs := tt.feed(seq); len(evs) != 1 || evs[0].Kind != EvBackTab {
			t.Fatalf("%q: events %+v", seq, evs)
		}
	}
	// Plain Tab is still Tab (completion), not Shift+Tab.
	if evs := tt.feed("\x1b[9u"); len(evs) != 0 {
		t.Fatalf("tab: events %+v", evs)
	}
}

func TestKittyEscapeIsEscape(t *testing.T) {
	// The kitty protocol sends Escape as CSI 27 u, no longer a lone ESC.
	tt := newTestTUI(Options{}, 80, 24)
	if evs := tt.feed("\x1b[27u"); len(evs) != 1 || evs[0].Kind != EvEscape {
		t.Fatalf("events %+v", evs)
	}
}

func TestBusyKeys(t *testing.T) {
	tt := newTestTUI(Options{}, 80, 24)
	tt.SetBusy(true)
	// While busy, Alt+Enter (ESC CR, or kitty's CSI 13;3u) sends a follow-up
	// and clears the editor; Shift+Enter still inserts a newline.
	for _, alt := range []string{"\x1b\r", "\x1b[13;3u"} {
		evs := tt.feed("a\x1b[13;2ub" + alt)
		if len(evs) != 1 || evs[0].Kind != EvFollowUp || evs[0].Text != "a\nb" || tt.Input() != "" {
			t.Fatalf("%q: events %+v, input %q", alt, evs, tt.Input())
		}
	}
	// An empty editor has nothing to queue: Alt+Enter is a newline.
	if evs := tt.feed("\x1b\r"); len(evs) != 0 || tt.Input() != "\n" {
		t.Fatalf("empty alt+enter: %+v %q", evs, tt.Input())
	}
	tt.SetInput("draft")
	// Alt+Up asks for the queued messages, passing the editor along.
	for _, up := range []string{"\x1b[1;3A", "\x1b\x1b[A"} {
		if evs := tt.feed(up); len(evs) != 1 || evs[0].Kind != EvDequeue || evs[0].Text != "draft" {
			t.Fatalf("%q: %+v", up, evs)
		}
	}
	tt.SetBusy(false)
	if got := submitted(t, tt, "\x1b\rx\r"); got != "draft\nx" {
		t.Fatalf("idle alt+enter: %q", got)
	}
}

func TestHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "history")
	tt := newTestTUI(Options{HistoryPath: path}, 80, 24)
	tt.AddHistory("one")
	tt.AddHistory("two\nlines")
	tt.AddHistory("two\nlines") // consecutive duplicate
	tt.AddHistory("   ")
	tt.AddHistory("three")

	tt2 := newTestTUI(Options{HistoryPath: path}, 80, 24)
	if got := strings.Join(tt2.hist.entries, "|"); got != "one|two\nlines|three" {
		t.Fatalf("persisted entries: %q", got)
	}
	// Up recalls newest; Up on a multi-line entry first walks its lines.
	if got := submitted(t, tt2, "\x1b[A\r"); got != "three" {
		t.Fatalf("up: %q", got)
	}
	if got := submitted(t, tt2, "\x1b[A\x1b[A\x1b[A\x1b[A\r"); got != "one" {
		t.Fatalf("up x4: %q", got)
	}
	if got := submitted(t, tt2, "draft\x1b[A\x1b[B\r"); got != "draft" {
		t.Fatalf("draft restore: %q", got)
	}
	// A line added while editing the draft is recalled next.
	tt2.AddHistory("four")
	if got := submitted(t, tt2, "\x1b[A\r"); got != "four" {
		t.Fatalf("after AddHistory: %q", got)
	}
}

func TestHistoryCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	var sb strings.Builder
	for i := range historyCap + 50 {
		sb.WriteString("\"e")
		sb.WriteString(strings.Repeat("x", i%7))
		sb.WriteString(string(rune('a' + i%26)))
		sb.WriteString("\"\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	h := history{path: path}
	h.load()
	if len(h.entries) != historyCap {
		t.Fatalf("entries=%d", len(h.entries))
	}
	data, _ := os.ReadFile(path)
	if n := strings.Count(string(data), "\n"); n != historyCap {
		t.Fatalf("file not trimmed: %d lines", n)
	}
}

func TestCtrlV(t *testing.T) {
	if got := submitted(t, newTestTUI(Options{}, 80, 24), "a\x16b\r"); got != "ab" {
		t.Fatalf("no hook: %q", got)
	}
	calls := 0
	tt := newTestTUI(Options{CtrlV: func() string {
		calls++
		if calls == 2 {
			return ""
		}
		return "[Image #1]"
	}}, 80, 24)
	// Inserted at the cursor (after Left), and "" is a no-op.
	if got := submitted(t, tt, "ab\x1b[D\x16\x16\r"); got != "a[Image #1]b" || calls != 2 {
		t.Fatalf("hook: %q calls=%d", got, calls)
	}
}
